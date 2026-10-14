//go:build integration

package httpserver

// ADR 0119 S3 integration harness: a real HTTP server whose pool is the igaming_runtime role
// (TEST_RUNTIME_DATABASE_URL - every request runs as the runtime role), the table-owner pool for fixtures and
// inspection only, the shared test staff MFA keys, and a log buffer that captures EVERYTHING the server logs. A
// skip is NOT evidence: a missing runtime URL fails the test.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/totp"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

var (
	mfaRTOnce sync.Once
	mfaRTPool *db.Pool
	mfaRTErr  error
)

func mfaRuntimePool(t *testing.T) *db.Pool {
	t.Helper()
	u := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if u == "" {
		t.Fatal("TEST_RUNTIME_DATABASE_URL must be set: the ADR 0119 S3 HTTP tests run as the runtime role")
	}
	mfaRTOnce.Do(func() { mfaRTPool, mfaRTErr = db.Connect(context.Background(), u, 16, 10*time.Second) })
	if mfaRTErr != nil {
		t.Fatalf("connect runtime pool: %v", mfaRTErr)
	}
	return mfaRTPool
}

type mfaW struct {
	t      *testing.T
	rt     *db.Pool
	owner  *db.Pool
	issuer *auth.Issuer
	m      *StaffMFA
	srv    *httptest.Server
	logs   *syncBuffer
	tenant identity.Tenant
	other  identity.Tenant

	mu      sync.Mutex
	secrets map[uuid.UUID][]byte
	steps   map[uuid.UUID]int64
}

type mfaStaff struct {
	ref      staffmfa.StaffRef
	email    string
	password string
	slug     string
	role     string
}

func newMFAW(t *testing.T, mutate ...func(*Deps)) *mfaW {
	t.Helper()
	rt := mfaRuntimePool(t)
	owner := launchfix.OwnerPool(t)
	launchfix.SeedOperators(t)
	ctx := context.Background()
	w := &mfaW{t: t, rt: rt, owner: owner, logs: &syncBuffer{}, secrets: map[uuid.UUID][]byte{}, steps: map[uuid.UUID]int64{}}
	if err := owner.WithPlatformAdmin(ctx, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		for _, p := range []*identity.Tenant{&w.tenant, &w.other} {
			tn, err := launchfix.CreateTenant(ctx, tx, "S3 MFA", "s3mfa-"+uuid.NewString()[:13], "under_platform_licence")
			if err != nil {
				return err
			}
			*p = tn
		}
		return nil
	}); err != nil {
		t.Fatalf("seed tenants: %v", err)
	}
	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": testJWTSecret})
	if err != nil {
		t.Fatal(err)
	}
	w.issuer = auth.NewIssuer(keys, "platform-api-test", "platform-api-test")
	w.m = newTestStaffMFA(15*time.Minute, 30*time.Minute, 12*time.Hour)
	deps := Deps{
		Logger:                 slog.New(slog.NewJSONHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		DB:                     rt,
		AuthIssuer:             w.issuer,
		ServiceName:            "s3-mfa-test",
		AccessTokenTTL:         5 * time.Minute,
		RefreshTokenTTL:        time.Hour,
		StaffMFA:               w.m,
		PersonResolver:         identityresolution.NewMockPersonResolver(),
		AuthRateLimitPerMinute: -1,
	}
	for _, f := range mutate {
		f(&deps)
	}
	w.srv = httptest.NewServer(New(deps))
	t.Cleanup(w.srv.Close)
	return w
}

// staff provisions an active staff member as the table owner. tenant nil = platform staff (platform_admin).
func (w *mfaW) staff(tenant *identity.Tenant, role string) mfaStaff {
	w.t.Helper()
	id := uuid.New()
	s := mfaStaff{email: "s3mfa-" + id.String() + "@fixture.invalid", password: "pw-" + uuid.NewString(), role: role}
	hash, err := auth.HashPassword(s.password)
	if err != nil {
		w.t.Fatal(err)
	}
	ctx := context.Background()
	ins := func(ctx context.Context, tx pgx.Tx) error {
		var tid any
		if tenant != nil {
			tid = tenant.ID
		}
		_, err := tx.Exec(ctx, `WITH s56p AS (SELECT gen_random_uuid() AS id), s56i AS (INSERT INTO persons (id) SELECT id FROM s56p) INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id) VALUES ($1, $2, $3, $4, $5, 'active', (SELECT id FROM s56p))`,
			id, tid, s.email, hash, role)
		return err
	}
	if tenant != nil {
		err = w.owner.WithTenant(ctx, tenant.ID, ins)
		s.ref = staffmfa.StaffRef{StaffID: id, TenantID: tenant.ID}
		s.slug = tenant.Slug
	} else {
		err = w.owner.WithoutTenant(ctx, ins)
		s.ref = staffmfa.StaffRef{StaffID: id}
	}
	if err != nil {
		w.t.Fatalf("provision staff: %v", err)
	}
	return s
}

type mfaResp struct {
	status int
	body   []byte
	header http.Header
}

func (r mfaResp) code() string {
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(r.body, &e)
	return e.Code
}

func (r mfaResp) want(t *testing.T, status int, code string) {
	t.Helper()
	if r.status != status || (code != "" && r.code() != code) {
		t.Fatalf("want %d %s, got %d %s", status, code, r.status, r.body)
	}
}

func (r mfaResp) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

func (w *mfaW) do(method, path, bearer string, body any) mfaResp {
	w.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			w.t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, w.srv.URL+path, rdr)
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return mfaResp{status: resp.StatusCode, body: b, header: resp.Header}
}

func (w *mfaW) post(path string, body any) mfaResp { return w.do(http.MethodPost, path, "", body) }

func (s mfaStaff) loginBody() map[string]string {
	b := map[string]string{"email": s.email, "password": s.password}
	if s.slug != "" {
		b["tenant_slug"] = s.slug
	}
	return b
}

// bootstrapToken issues a bootstrap enrolment token for s exactly as cmd/seed-admin does (owner, store, test keys).
func (w *mfaW) bootstrapToken(s mfaStaff) string {
	w.t.Helper()
	st, err := store.New(testStaffMFAKeys())
	if err != nil {
		w.t.Fatal(err)
	}
	var tok staffmfa.Token
	if err := store.WithScope(context.Background(), w.owner, s.ref, func(ctx context.Context, tx pgx.Tx) error {
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			return err
		}
		tok, err = st.IssueEnrolmentToken(ctx, tx, s.ref, store.EnrolBootstrap, nil, nil, now)
		return err
	}); err != nil {
		w.t.Fatalf("bootstrap token: %v", err)
	}
	return tok.RevealForDelivery()
}

func (w *mfaW) redeem(s mfaStaff, token string) mfaResp {
	body := map[string]string{"token": token, "email": s.email, "password": s.password}
	if s.slug != "" {
		body["tenant_slug"] = s.slug
	}
	return w.post("/v1/staff/auth/enrol/redeem", body)
}

func challengeOf(t *testing.T, r mfaResp, typ string) string {
	t.Helper()
	var ch staffChallengeWire
	r.decode(t, &ch)
	if ch.Challenge.Type != typ || ch.Challenge.Token == "" || ch.Challenge.ExpiresIn != 300 {
		t.Fatalf("want a %q challenge, got %s", typ, r.body)
	}
	return ch.Challenge.Token
}

type mfaEnrolled struct {
	pair     tokenPairResponse
	codes    []string
	factorID uuid.UUID
	start    enrolStartWire
}

// enrolWith runs start + confirm on an `enrol` challenge and records the secret.
func (w *mfaW) enrolWith(s mfaStaff, enrolChallenge string) mfaEnrolled {
	w.t.Helper()
	sr := w.post("/v1/staff/auth/enrol/start", map[string]string{"challenge_token": enrolChallenge})
	sr.want(w.t, http.StatusOK, "")
	var start enrolStartWire
	sr.decode(w.t, &start)
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(start.SecretBase32)
	if err != nil {
		w.t.Fatal(err)
	}
	w.mu.Lock()
	w.secrets[s.ref.StaffID] = secret
	w.steps[s.ref.StaffID] = 0
	w.mu.Unlock()
	cr := w.post("/v1/staff/auth/enrol/confirm", map[string]string{"challenge_token": enrolChallenge, "factor_id": start.FactorID, "code": w.code(s)})
	cr.want(w.t, http.StatusOK, "")
	var conf enrolConfirmWire
	cr.decode(w.t, &conf)
	return mfaEnrolled{pair: conf.TokenPair, codes: conf.RecoveryCodes, factorID: uuid.MustParse(start.FactorID), start: start}
}

// enrol takes a staff member from no factor to an active factor through redeem / start / confirm.
func (w *mfaW) enrol(s mfaStaff) mfaEnrolled {
	w.t.Helper()
	r := w.redeem(s, w.bootstrapToken(s))
	r.want(w.t, http.StatusOK, "")
	return w.enrolWith(s, challengeOf(w.t, r, "enrol"))
}

// code returns the next TOTP code the server accepts for s (strictly after the last step used), waiting for the
// next 30 s step when needed.
func (w *mfaW) code(s mfaStaff) string {
	w.t.Helper()
	for {
		cur, err := totp.StepAt(time.Now())
		if err != nil {
			w.t.Fatal(err)
		}
		w.mu.Lock()
		secret, last := w.secrets[s.ref.StaffID], w.steps[s.ref.StaffID]
		w.mu.Unlock()
		if secret == nil {
			w.t.Fatal("no secret recorded for the staff member")
		}
		step := cur
		if last+1 > step {
			step = last + 1
		}
		if step <= cur+1 {
			c, err := totp.CodeAtStep(secret, step)
			if err != nil {
				w.t.Fatal(err)
			}
			w.mu.Lock()
			w.steps[s.ref.StaffID] = step
			w.mu.Unlock()
			return c
		}
		time.Sleep(time.Until(time.Unix((cur+1)*30, 0)) + 50*time.Millisecond)
	}
}

// wrong returns a 6-digit code valid at no step within two steps of now.
func (w *mfaW) wrong(s mfaStaff) string {
	w.t.Helper()
	w.mu.Lock()
	secret := w.secrets[s.ref.StaffID]
	w.mu.Unlock()
	cur, _ := totp.StepAt(time.Now())
	valid := map[string]bool{}
	for d := int64(-3); d <= 3; d++ {
		c, _ := totp.CodeAtStep(secret, cur+d)
		valid[c] = true
	}
	for i := 0; ; i++ {
		c := fmt.Sprintf("%06d", (i*7919+12345)%1000000)
		if !valid[c] {
			return c
		}
	}
}

// login runs the password step and returns the `mfa` challenge token.
func (w *mfaW) login(s mfaStaff) string {
	w.t.Helper()
	r := w.post("/v1/staff/auth/login", s.loginBody())
	r.want(w.t, http.StatusOK, "")
	return challengeOf(w.t, r, "mfa")
}

func (w *mfaW) verify(challenge, code string) mfaResp {
	return w.post("/v1/staff/auth/mfa/verify", map[string]string{"challenge_token": challenge, "code": code})
}

// fullLogin is login + verify with the next valid code.
func (w *mfaW) fullLogin(s mfaStaff) tokenPairResponse {
	w.t.Helper()
	r := w.verify(w.login(s), w.code(s))
	r.want(w.t, http.StatusOK, "")
	var p tokenPairResponse
	r.decode(w.t, &p)
	return p
}

// ---- inspection (table owner, in the staff member's own scope) -----------------------------------------------

func (w *mfaW) inScope(s mfaStaff, fn func(ctx context.Context, tx pgx.Tx) error) {
	w.t.Helper()
	if err := store.WithScope(context.Background(), w.owner, s.ref, fn); err != nil {
		w.t.Fatalf("inspect: %v", err)
	}
}

type mfaFactorRow struct {
	ID            uuid.UUID
	Status        string
	LastUsedStep  int64
	FailedCount   int
	Failed24h     int
	LockedUntil   *time.Time
	HardLockedAt  *time.Time
	RevokedReason *string
}

func (w *mfaW) factors(s mfaStaff) []mfaFactorRow {
	w.t.Helper()
	var out []mfaFactorRow
	w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, status, last_used_step, failed_count, failed_24h_count, locked_until, hard_locked_at, revoked_reason
			FROM staff_mfa_factors WHERE staff_user_id = $1 ORDER BY created_at`, s.ref.StaffID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f mfaFactorRow
			if err := rows.Scan(&f.ID, &f.Status, &f.LastUsedStep, &f.FailedCount, &f.Failed24h, &f.LockedUntil, &f.HardLockedAt, &f.RevokedReason); err != nil {
				return err
			}
			out = append(out, f)
		}
		return rows.Err()
	})
	return out
}

func (w *mfaW) activeFactor(s mfaStaff) mfaFactorRow {
	w.t.Helper()
	for _, f := range w.factors(s) {
		if f.Status == "active" {
			return f
		}
	}
	w.t.Fatal("no active factor")
	return mfaFactorRow{}
}

// challengeState reads a challenge by its token (attempts_left, consumed).
func (w *mfaW) challengeState(s mfaStaff, token string, purpose store.ChallengePurpose) (attempts int, consumed bool) {
	w.t.Helper()
	tok, err := staffmfa.ParseToken(token)
	if err != nil {
		w.t.Fatal(err)
	}
	kind := staffmfa.MACChallengeMFA
	if purpose == store.ChallengeEnrol {
		kind = staffmfa.MACChallengeEnrol
	}
	h, err := testStaffMFAKeys().HashToken(kind, tok)
	if err != nil {
		w.t.Fatal(err)
	}
	w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
		var c *time.Time
		if err := tx.QueryRow(ctx, `SELECT attempts_left, consumed_at FROM staff_auth_challenges WHERE token_mac = $1`, h.MAC).Scan(&attempts, &c); err != nil {
			return err
		}
		consumed = c != nil
		return nil
	})
	return attempts, consumed
}

func (w *mfaW) countChallenges(s mfaStaff) int {
	w.t.Helper()
	var n int
	w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM staff_auth_challenges WHERE staff_user_id = $1`, s.ref.StaffID).Scan(&n)
	})
	return n
}

type mfaSessionRow struct {
	ID          uuid.UUID
	AAL         *int16
	AMR         []string
	AuthTime    *time.Time
	MFAAt       *time.Time
	AbsExpires  *time.Time
	Expires     time.Time
	Revoked     *time.Time
	ReplacedBy  *uuid.UUID
	Kid         *string
	Seal        []byte
	CreatedAt   time.Time
	PrincipalID uuid.UUID
	TenantID    *uuid.UUID
	// RefreshHash is refresh_token_hash (bound by seal format v2 since ADR 0119 S4b).
	RefreshHash string
}

func (w *mfaW) sessions(s mfaStaff) []mfaSessionRow {
	w.t.Helper()
	var out []mfaSessionRow
	if err := w.owner.WithPrincipalScope(context.Background(), s.ref.TenantID, s.ref.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, aal, amr, auth_time, mfa_at, absolute_expires_at, expires_at, revoked_at, replaced_by_session_id,
				assurance_kid, assurance_seal, created_at, principal_id, tenant_id, refresh_token_hash
			FROM sessions WHERE principal_id = $1 ORDER BY created_at`, s.ref.StaffID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r mfaSessionRow
			if err := rows.Scan(&r.ID, &r.AAL, &r.AMR, &r.AuthTime, &r.MFAAt, &r.AbsExpires, &r.Expires, &r.Revoked, &r.ReplacedBy,
				&r.Kid, &r.Seal, &r.CreatedAt, &r.PrincipalID, &r.TenantID, &r.RefreshHash); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	}); err != nil {
		w.t.Fatalf("sessions: %v", err)
	}
	return out
}

func (w *mfaW) session(s mfaStaff, id uuid.UUID) mfaSessionRow {
	w.t.Helper()
	for _, r := range w.sessions(s) {
		if r.ID == id {
			return r
		}
	}
	w.t.Fatalf("no session %s", id)
	return mfaSessionRow{}
}

// sealOK verifies a session row's seal with the test keys.
func sealOK(r mfaSessionRow) bool {
	if r.AAL == nil || r.AuthTime == nil || r.MFAAt == nil || r.AbsExpires == nil || r.Kid == nil {
		return false
	}
	tenant := uuid.Nil
	if r.TenantID != nil {
		tenant = *r.TenantID
	}
	return testStaffMFAKeys().VerifySessionSeal(staffmfa.SessionAssurance{SessionID: r.ID, PrincipalType: "staff", PrincipalID: r.PrincipalID,
		TenantID: tenant, AAL: int(*r.AAL), AMR: r.AMR, AuthTime: *r.AuthTime, MFAAt: *r.MFAAt, AbsoluteExpiresAt: *r.AbsExpires,
		RefreshTokenHash: r.RefreshHash, ExpiresAt: r.Expires},
		staffmfa.KeyedHash{KID: *r.Kid, MAC: r.Seal})
}

// auditCount counts audit rows of action for the staff member (actor), in its scope.
func (w *mfaW) auditCount(s mfaStaff, action string) int {
	w.t.Helper()
	var n int
	w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE actor_id = $1 AND action = $2`, s.ref.StaffID, action).Scan(&n)
	})
	return n
}

// auditMetadata returns every metadata JSON written for the staff member (as actor or target).
func (w *mfaW) auditMetadata(s mfaStaff) []string {
	w.t.Helper()
	var out []string
	w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT action || ' ' || metadata::text FROM audit_log WHERE actor_id = $1 OR target_id = $2`, s.ref.StaffID, s.ref.StaffID.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m string
			if err := rows.Scan(&m); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out
}

// alertCount counts alerts of kind about the staff member (any state), read as a platform admin.
func (w *mfaW) alertCount(kind string, staffID uuid.UUID) int {
	w.t.Helper()
	var n int
	if err := w.owner.WithPlatformAdmin(context.Background(), launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = $1 AND attributes->>'staff_id' = $2`, kind, staffID.String()).Scan(&n)
	}); err != nil {
		w.t.Fatalf("alerts: %v", err)
	}
	return n
}

func (w *mfaW) alertAttrs(kind string, staffID uuid.UUID) map[string]any {
	w.t.Helper()
	var raw []byte
	if err := w.owner.WithPlatformAdmin(context.Background(), launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT attributes FROM alerts WHERE kind = $1 AND attributes->>'staff_id' = $2 ORDER BY created_at DESC LIMIT 1`, kind, staffID.String()).Scan(&raw)
	}); err != nil {
		w.t.Fatalf("alert attributes: %v", err)
	}
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	return m
}

// plantSession inserts a sealed staff session as the RUNTIME role in the staff member's scope and returns its raw
// refresh token and id. tamper is applied to the seal before the INSERT (nil = a valid seal).
func (w *mfaW) plantSession(s mfaStaff, authTime, absolute, expires time.Time, tamper func([]byte) []byte) (string, uuid.UUID) {
	w.t.Helper()
	id := uuid.New()
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	token := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	// Seal format v2 (ADR 0119 S4b, S3-R10): the refresh token hash and the idle expiry are bound.
	a := staffmfa.SessionAssurance{SessionID: id, PrincipalType: "staff", PrincipalID: s.ref.StaffID, TenantID: s.ref.TenantID,
		AAL: 2, AMR: []string{"pwd", "otp"}, AuthTime: staffmfa.SessionTime(authTime), MFAAt: staffmfa.SessionTime(authTime),
		AbsoluteExpiresAt: staffmfa.SessionTime(absolute), RefreshTokenHash: hex.EncodeToString(sum[:]), ExpiresAt: staffmfa.SessionTime(expires)}
	seal, err := testStaffMFAKeys().SealSession(a)
	if err != nil {
		w.t.Fatal(err)
	}
	mac := seal.MAC
	if tamper != nil {
		mac = tamper(append([]byte(nil), mac...))
	}
	var tenantArg any
	if s.ref.TenantID != uuid.Nil {
		tenantArg = s.ref.TenantID
	}
	if err := store.WithScope(context.Background(), w.rt, s.ref, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO sessions (id, principal_type, principal_id, tenant_id, refresh_token_hash, expires_at,
				aal, amr, auth_time, mfa_at, absolute_expires_at, assurance_kid, assurance_seal)
			VALUES ($1, 'staff', $2, $3, $4, $5, 2, $6, $7, $8, $9, $10, $11)`,
			id, s.ref.StaffID, tenantArg, hex.EncodeToString(sum[:]), staffmfa.SessionTime(expires), a.AMR, a.AuthTime, a.MFAAt, a.AbsoluteExpiresAt, seal.KID, mac)
		return err
	}); err != nil {
		w.t.Fatalf("plant session: %v", err)
	}
	return token, id
}

// runtimeSessionExec runs sql (args: the session id first) as the RUNTIME role with the session row made visible
// through app.session_internal_op_id (the ADR 0016 technique), in the staff member's scope.
func (w *mfaW) runtimeSessionExec(s mfaStaff, id uuid.UUID, sql string, args ...any) error {
	return store.WithScope(context.Background(), w.rt, s.ref, func(ctx context.Context, tx pgx.Tx) error {
		if err := db.SetSessionInternalOpID(ctx, tx, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, sql, append([]any{id}, args...)...)
		return err
	})
}

// ownerSessionExec is runtimeSessionExec as the table owner (not bound by the runtime column grants; SF040 still
// applies).
func (w *mfaW) ownerSessionExec(s mfaStaff, id uuid.UUID, sql string, args ...any) error {
	return store.WithScope(context.Background(), w.owner, s.ref, func(ctx context.Context, tx pgx.Tx) error {
		if err := db.SetSessionInternalOpID(ctx, tx, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, sql, append([]any{id}, args...)...)
		return err
	})
}

func (w *mfaW) refresh(token string) mfaResp {
	return w.post("/v1/auth/refresh", map[string]string{"refresh_token": token})
}

func containsAny(hay string, needles []string) []string {
	var found []string
	for _, n := range needles {
		if n != "" && strings.Contains(hay, n) {
			found = append(found, n)
		}
	}
	return found
}

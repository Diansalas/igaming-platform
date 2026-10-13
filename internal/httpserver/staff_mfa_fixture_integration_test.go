//go:build integration

package httpserver

// ADR 0119 S3 fixture migration: every staff login of the integration suite (mustLoginStaff, ~300 call sites) now
// runs the REAL two-step flow over HTTP against the test server:
//
//   - the first login of a staff member gets 403 mfa_enrolment_required at the password step; the fixture then issues
//     a bootstrap enrolment token (the cmd/seed-admin path, through the store and the shared test keys, as the table
//     owner), and drives POST /v1/staff/auth/enrol/redeem -> /enrol/start -> /enrol/confirm, whose aal2 token pair it
//     returns;
//   - every later login gets the `mfa` challenge and completes POST /v1/staff/auth/mfa/verify with a TOTP code
//     computed from the secret the enrolment returned, for a step strictly after the last one used (the replay rule),
//     waiting for the next 30 s step only when a staff member logs in more than twice within one step.
//
// A 429 from the per-IP buckets is retried after Retry-After (a fixture concern only; the limiter itself is covered
// by its own tests).

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/totp"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

type staffFixtureFactor struct {
	secret   []byte
	lastStep int64
}

var staffFixtures = struct {
	sync.Mutex
	m map[uuid.UUID]*staffFixtureFactor
}{m: map[uuid.UUID]*staffFixtureFactor{}}

// staffFixtureResp is a decoded fixture response.
type staffFixtureResp struct {
	status int
	body   []byte
}

func (r staffFixtureResp) code() string {
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(r.body, &e)
	return e.Code
}

// staffFixturePost POSTs body to path, retrying a 429 after its Retry-After.
func staffFixturePost(t *testing.T, srv *httptest.Server, path string, body any) staffFixtureResp {
	t.Helper()
	for attempt := 0; ; attempt++ {
		resp := postJSON(t, srv, path, "", body)
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 3 && path != "/v1/staff/auth/login" {
			wait, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			time.Sleep(time.Duration(wait+1) * time.Second)
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 3 && path == "/v1/staff/auth/login" && resp.Header.Get("Retry-After") != "" {
			// the per-IP limiter (not the per-account password lockout, which sets no Retry-After)
			wait, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			time.Sleep(time.Duration(wait+1) * time.Second)
			continue
		}
		return staffFixtureResp{status: resp.StatusCode, body: b}
	}
}

// mustLoginStaff logs a staff member in through the real two-step MFA flow (enrolling on first use) and returns the
// aal2 token pair.
func mustLoginStaff(t *testing.T, srv *httptest.Server, tenantSlug, email, password string) tokenPairResponse {
	t.Helper()
	body := map[string]string{"email": email, "password": password}
	if tenantSlug != "" {
		body["tenant_slug"] = tenantSlug
	}
	r := staffFixturePost(t, srv, "/v1/staff/auth/login", body)
	switch {
	case r.status == http.StatusOK:
		var ch staffChallengeWire
		if err := json.Unmarshal(r.body, &ch); err != nil || ch.Challenge.Type != "mfa" {
			t.Fatalf("staff login: unexpected challenge %s", r.body)
		}
		tok, err := staffmfa.ParseToken(ch.Challenge.Token)
		if err != nil {
			t.Fatalf("staff login: challenge token: %v", err)
		}
		code := staffFixtureNextCode(t, tok.StaffID())
		v := staffFixturePost(t, srv, "/v1/staff/auth/mfa/verify", map[string]string{"challenge_token": ch.Challenge.Token, "code": code})
		if v.status != http.StatusOK {
			t.Fatalf("staff login: mfa/verify answered %d %s", v.status, v.body)
		}
		var pair tokenPairResponse
		if err := json.Unmarshal(v.body, &pair); err != nil {
			t.Fatal(err)
		}
		return pair
	case r.status == http.StatusForbidden && r.code() == "mfa_enrolment_required":
		return staffFixtureEnrol(t, srv, tenantSlug, email, password)
	default:
		t.Fatalf("staff login failed with status %d %s", r.status, r.body)
		return tokenPairResponse{}
	}
}

// staffFixtureRef resolves the staff member the way the login route does (tenant by slug, staff by email in scope).
func staffFixtureRef(t *testing.T, tenantSlug, email string) staffmfa.StaffRef {
	t.Helper()
	owner := launchfix.OwnerPool(t)
	ctx := context.Background()
	var ref staffmfa.StaffRef
	if tenantSlug != "" {
		tn, err := identity.GetTenantBySlug(ctx, owner, tenantSlug)
		if err != nil {
			t.Fatalf("fixture: tenant %q: %v", tenantSlug, err)
		}
		ref.TenantID = tn.ID
	}
	scope := func(fn func(ctx context.Context, tx pgx.Tx) error) error {
		if ref.TenantID != uuid.Nil {
			return owner.WithTenant(ctx, ref.TenantID, fn)
		}
		return owner.WithoutTenant(ctx, fn)
	}
	if err := scope(func(ctx context.Context, tx pgx.Tx) error {
		s, err := identity.GetStaffUserByEmail(ctx, tx, email)
		ref.StaffID = s.ID
		return err
	}); err != nil {
		t.Fatalf("fixture: staff %q: %v", email, err)
	}
	return ref
}

// staffFixtureBootstrapToken issues a bootstrap enrolment token as cmd/seed-admin does (the table owner, the store,
// the shared test keys).
func staffFixtureBootstrapToken(t *testing.T, ref staffmfa.StaffRef) string {
	t.Helper()
	st, err := store.New(testStaffMFAKeys())
	if err != nil {
		t.Fatal(err)
	}
	var tok staffmfa.Token
	if err := store.WithScope(context.Background(), launchfix.OwnerPool(t), ref, func(ctx context.Context, tx pgx.Tx) error {
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			return err
		}
		tok, err = st.IssueEnrolmentToken(ctx, tx, ref, store.EnrolBootstrap, nil, nil, now)
		return err
	}); err != nil {
		t.Fatalf("fixture: bootstrap token: %v", err)
	}
	return tok.RevealForDelivery()
}

// staffFixtureEnrol redeems a bootstrap token and enrols a TOTP factor over HTTP; it returns the confirm token pair.
func staffFixtureEnrol(t *testing.T, srv *httptest.Server, tenantSlug, email, password string) tokenPairResponse {
	t.Helper()
	ref := staffFixtureRef(t, tenantSlug, email)
	redeem := map[string]string{"token": staffFixtureBootstrapToken(t, ref), "email": email, "password": password}
	if tenantSlug != "" {
		redeem["tenant_slug"] = tenantSlug
	}
	r := staffFixturePost(t, srv, "/v1/staff/auth/enrol/redeem", redeem)
	if r.status != http.StatusOK {
		t.Fatalf("fixture: enrol/redeem answered %d %s", r.status, r.body)
	}
	var ch staffChallengeWire
	if err := json.Unmarshal(r.body, &ch); err != nil || ch.Challenge.Type != "enrol" {
		t.Fatalf("fixture: enrol challenge %s", r.body)
	}
	s := staffFixturePost(t, srv, "/v1/staff/auth/enrol/start", map[string]string{"challenge_token": ch.Challenge.Token})
	if s.status != http.StatusOK {
		t.Fatalf("fixture: enrol/start answered %d %s", s.status, s.body)
	}
	var start enrolStartWire
	if err := json.Unmarshal(s.body, &start); err != nil {
		t.Fatal(err)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(start.SecretBase32)
	if err != nil {
		t.Fatalf("fixture: secret: %v", err)
	}
	staffFixtures.Lock()
	staffFixtures.m[ref.StaffID] = &staffFixtureFactor{secret: secret}
	staffFixtures.Unlock()
	c := staffFixturePost(t, srv, "/v1/staff/auth/enrol/confirm", map[string]string{
		"challenge_token": ch.Challenge.Token, "factor_id": start.FactorID, "code": staffFixtureNextCode(t, ref.StaffID)})
	if c.status != http.StatusOK {
		t.Fatalf("fixture: enrol/confirm answered %d %s", c.status, c.body)
	}
	var conf enrolConfirmWire
	if err := json.Unmarshal(c.body, &conf); err != nil || len(conf.RecoveryCodes) != staffmfa.RecoveryCodesPerBatch {
		t.Fatalf("fixture: enrol/confirm body: %v", err)
	}
	return conf.TokenPair
}

// staffFixtureNextCode returns a TOTP code for the earliest step after the last one used that the server accepts now
// (window T-1..T+1), waiting for the next step when needed.
func staffFixtureNextCode(t *testing.T, staffID uuid.UUID) string {
	t.Helper()
	staffFixtures.Lock()
	f, ok := staffFixtures.m[staffID]
	staffFixtures.Unlock()
	if !ok {
		t.Fatalf("fixture: no enrolled test factor for staff %s (enrol through mustLoginStaff first)", staffID)
	}
	for {
		cur, err := totp.StepAt(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		step := cur
		if f.lastStep+1 > step {
			step = f.lastStep + 1
		}
		if step <= cur+1 {
			code, err := totp.CodeAtStep(f.secret, step)
			if err != nil {
				t.Fatal(err)
			}
			staffFixtures.Lock()
			f.lastStep = step
			staffFixtures.Unlock()
			return code
		}
		time.Sleep(time.Until(time.Unix((cur+1)*30, 0)) + 50*time.Millisecond)
	}
}

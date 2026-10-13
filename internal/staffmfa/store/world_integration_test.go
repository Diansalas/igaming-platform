//go:build integration

// Shared integration fixtures for the ADR 0119 S2 store tests: the table-owner pool (TEST_DATABASE_URL, used
// ONLY to provision tenants and staff) and the igaming_runtime pool (TEST_RUNTIME_DATABASE_URL, the role
// every assertion runs as). Every test provisions its own tenants and staff with unique ids, so tests are
// independent and re-runnable on a shared database. A skip is NOT evidence.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/totp"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// t0 is the injected clock origin of every store test (no time.Now in assertions). ADR 0119 S3: it lies a week in
// the PAST (minute-aligned, so 30 s TOTP steps align): migration 0138's SF008 rule lets failed_24h_count decrease only
// once its window is 24 hours old by the DATABASE clock, so a test that moves the injected clock past a 24 hour
// window must stay behind the real clock (the largest offset used is about 48 h 16 min).
var t0 = time.Now().UTC().Add(-7 * 24 * time.Hour).Truncate(time.Minute)

var (
	rtOnce sync.Once
	rtPool *db.Pool
	rtErr  error
)

func runtimePool(t testing.TB) *db.Pool {
	t.Helper()
	u := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set; skipping ADR 0119 S2 runtime-role tests")
	}
	rtOnce.Do(func() { rtPool, rtErr = db.Connect(context.Background(), u, 12, 10*time.Second) })
	if rtErr != nil {
		t.Fatalf("connect runtime pool: %v", rtErr)
	}
	return rtPool
}

// testKeys builds deterministic non-degenerate keys; salt makes a distinct key set.
func testKeys(t testing.TB, salt string) *staffmfa.Keys {
	t.Helper()
	k := func(label string) []byte {
		h := sha256.Sum256([]byte("adr0119-s2-test|" + salt + "|" + label))
		return h[:]
	}
	keys, err := staffmfa.NewKeys("f1", map[string][]byte{"f1": k("factor-1"), "f0": k("factor-0")},
		"m1", map[string][]byte{"m1": k("mac-1"), "m0": k("mac-0")})
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	return keys
}

type world struct {
	t                testing.TB
	owner, rt        *db.Pool
	keys             *staffmfa.Keys
	factor           *TOTPFactor
	st               *Store
	tenantA, tenantB uuid.UUID
	platform         staffmfa.StaffRef
	a1, a2, b1       staffmfa.StaffRef
}

func newWorld(t testing.TB) *world {
	t.Helper()
	rt := runtimePool(t)
	owner := launchfix.OwnerPool(t)
	launchfix.SeedOperators(t)
	ctx := context.Background()
	w := &world{t: t, owner: owner, rt: rt, keys: testKeys(t, "world")}
	var err error
	if w.factor, err = NewTOTPFactor(w.keys, "iGaming Test Back Office"); err != nil {
		t.Fatal(err)
	}
	if w.st, err = New(w.keys); err != nil {
		t.Fatal(err)
	}
	if err := owner.WithPlatformAdmin(ctx, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		for i, p := range []*uuid.UUID{&w.tenantA, &w.tenantB} {
			tn, err := launchfix.CreateTenant(ctx, tx, "sfmfa", "sfmfa-"+uuid.NewString()[:12], "under_platform_licence")
			if err != nil {
				return err
			}
			*p = tn.ID
			_ = i
		}
		return nil
	}); err != nil {
		t.Fatalf("seed tenants: %v", err)
	}
	w.platform = w.newStaff(nil, "platform_admin")
	w.a1 = w.newStaff(&w.tenantA, "support")
	w.a2 = w.newStaff(&w.tenantA, "support")
	w.b1 = w.newStaff(&w.tenantB, "support")
	return w
}

// newStaff provisions an active staff member as the table owner (the staff tables are not S2's to write).
func (w *world) newStaff(tenant *uuid.UUID, role string) staffmfa.StaffRef {
	w.t.Helper()
	id := uuid.New()
	email := "sfmfa-" + id.String() + "@fixture.invalid"
	ctx := context.Background()
	ins := func(ctx context.Context, tx pgx.Tx) error {
		var tid any
		if tenant != nil {
			tid = *tenant
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, $2, $3, '!x', $4)`,
			id, tid, email, role)
		return err
	}
	var err error
	if tenant != nil {
		err = w.owner.WithTenant(ctx, *tenant, ins)
	} else {
		err = w.owner.WithoutTenant(ctx, ins)
	}
	if err != nil {
		w.t.Fatalf("seed staff: %v", err)
	}
	ref := staffmfa.StaffRef{StaffID: id}
	if tenant != nil {
		ref.TenantID = *tenant
	}
	return ref
}

// run executes fn as igaming_runtime in the scope of ref (WithTenant / WithoutTenant) and commits.
func (w *world) run(ref staffmfa.StaffRef, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return WithScope(context.Background(), w.rt, ref, fn)
}

// mustRun is run, failing the test on error.
func (w *world) mustRun(ref staffmfa.StaffRef, fn func(ctx context.Context, tx pgx.Tx) error) {
	w.t.Helper()
	if err := w.run(ref, fn); err != nil {
		w.t.Fatalf("run: %v", err)
	}
}

// ownerRun executes fn as the table owner in the scope of ref (fixtures and tamper plants).
func (w *world) ownerRun(ref staffmfa.StaffRef, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return WithScope(context.Background(), w.owner, ref, fn)
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// secretFrom decodes the base32 secret revealed by an enrolment.
func secretFrom(t testing.TB, m staffmfa.EnrolmentMaterial) []byte {
	t.Helper()
	_, s := m.RevealForEnrolment()
	b, err := b32.DecodeString(s)
	if err != nil || len(b) != totp.SecretBytes {
		t.Fatalf("enrolment secret: %v", err)
	}
	return b
}

func codeAt(t testing.TB, secret []byte, at time.Time) string {
	t.Helper()
	c, err := totp.CodeAt(secret, at)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// wrongCode returns a 6-digit code that is NOT valid at any step in the window around at.
func wrongCode(t testing.TB, secret []byte, at time.Time) string {
	t.Helper()
	valid := map[string]bool{}
	for d := -3; d <= 3; d++ {
		valid[codeAt(t, secret, at.Add(time.Duration(d)*30*time.Second))] = true
	}
	for i := 0; i < 1000; i++ {
		c := strings.Repeat("0", 6-len(itoa(i))) + itoa(i)
		if !valid[c] {
			return c
		}
	}
	t.Fatal("no wrong code")
	return ""
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// enrol begins and confirms a TOTP factor for ref at time at (committed) and returns its secret.
func (w *world) enrol(ref staffmfa.StaffRef, at time.Time) (id uuid.UUID, secret []byte) {
	w.t.Helper()
	w.mustRun(ref, func(ctx context.Context, tx pgx.Tx) error {
		var m staffmfa.EnrolmentMaterial
		var err error
		id, m, err = w.factor.Begin(ctx, tx, ref, at)
		if err != nil {
			return err
		}
		secret = secretFrom(w.t, m)
		return w.factor.Confirm(ctx, tx, ref, id, staffmfa.NewTOTPAssertion(codeAt(w.t, secret, at)), at)
	})
	return id, secret
}

// verify runs VerifyDetailed in its own committed transaction.
func (w *world) verify(ref staffmfa.StaffRef, code string, at time.Time) (VerifyResult, error) {
	var vr VerifyResult
	err := w.run(ref, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		vr, err = w.factor.VerifyDetailed(ctx, tx, ref, staffmfa.NewTOTPAssertion(code), at)
		return err
	})
	return vr, err
}

// factorRowOf reads the staff member's single row of the given status (as the owner, no RLS blindness).
type factorState struct {
	ID                                     uuid.UUID
	Status                                 string
	LastUsedStep                           int64
	FailedCount, Failed24h                 int
	LockedUntil, HardLockedAt, ActivatedAt *time.Time
	RevokedReason                          *string
}

func (w *world) factors(ref staffmfa.StaffRef) []factorState {
	w.t.Helper()
	var out []factorState
	w.mustOwner(ref, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, status, last_used_step, failed_count, failed_24h_count, locked_until, hard_locked_at, activated_at, revoked_reason
			FROM staff_mfa_factors WHERE staff_user_id = $1 ORDER BY created_at, id`, ref.StaffID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f factorState
			if err := rows.Scan(&f.ID, &f.Status, &f.LastUsedStep, &f.FailedCount, &f.Failed24h, &f.LockedUntil, &f.HardLockedAt, &f.ActivatedAt, &f.RevokedReason); err != nil {
				return err
			}
			out = append(out, f)
		}
		return rows.Err()
	})
	return out
}

func (w *world) mustOwner(ref staffmfa.StaffRef, fn func(ctx context.Context, tx pgx.Tx) error) {
	w.t.Helper()
	if err := w.ownerRun(ref, fn); err != nil {
		w.t.Fatalf("owner run: %v", err)
	}
}

// active returns the single active factor of ref.
func (w *world) active(ref staffmfa.StaffRef) factorState {
	w.t.Helper()
	var act []factorState
	for _, f := range w.factors(ref) {
		if f.Status == "active" {
			act = append(act, f)
		}
	}
	if len(act) != 1 {
		w.t.Fatalf("want exactly one active factor, got %d", len(act))
	}
	return act[0]
}

// pgCode extracts the SQLSTATE of err ("" when none).
func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func wantCode(t testing.TB, what string, err error, code string) {
	t.Helper()
	if got := pgCode(err); got != code {
		t.Fatalf("%s: want SQLSTATE %s, got %q (%v)", what, code, got, err)
	}
}

func wantOK(t testing.TB, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

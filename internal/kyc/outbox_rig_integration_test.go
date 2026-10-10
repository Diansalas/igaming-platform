//go:build integration

package kyc

// The shared rig for the PRH-2 E1 worker tests (ADR 0106 section 10): a tenant
// fixture, a counting spy provider, a worker over the SAME orchestrator and
// MOCK resolver the HTTP path uses, and small readers for alerts, audit rows
// and verification snapshots.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// rig is one tenant plus a worker whose vendor calls are counted.
type rig struct {
	t     *testing.T
	pool  *db.Pool // runtime role
	owner *db.Pool // owner role (alertinject DDL only)
	f     fixture
	spy   *spyKYCProvider
	w     *OutboxWorker

	mu          sync.Mutex
	createCalls int
	submitCalls int
	createKeys  []string
	submitKeys  []string

	// Overridable behaviour (called after counting).
	createImpl func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error)
	submitImpl func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error)
}

func newRig(t *testing.T) *rig {
	t.Helper()
	return newRigOn(t, rtPool(t, 10), testPool(t))
}

// newRigOn builds the rig over an explicit pool pair. Tests that must add a
// database constraint or otherwise change the schema use a SCRATCH database
// (scratchThrough0114) here, never the shared one.
func newRigOn(t *testing.T, pool, owner *db.Pool) *rig {
	t.Helper()
	resetScope(t) // rows an earlier (sub)test left behind are out of this test's claim scope
	r := &rig{t: t, pool: pool, owner: owner}
	r.f = seedFixture(t, pool)
	base := NewMockKYCProvider()
	r.createImpl = func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
		return base.CreateVerification(ctx, in)
	}
	r.submitImpl = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		base.mu.Lock()
		base.created[ref] = true // the rig's mock accepts any reference the worker was handed
		base.mu.Unlock()
		return base.SubmitVerification(ctx, ref, docs, call)
	}
	r.spy = &spyKYCProvider{MockKYCProvider: base}
	r.spy.onCreateVerification = func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
		r.mu.Lock()
		r.createCalls++
		r.createKeys = append(r.createKeys, in.Call.IdempotencyKey)
		impl := r.createImpl
		r.mu.Unlock()
		return impl(ctx, in)
	}
	r.spy.onSubmitVerification = func(ctx context.Context, ref string, docs []SubmittedDocument, call CallContext) (ProviderResult, error) {
		r.mu.Lock()
		r.submitCalls++
		r.submitKeys = append(r.submitKeys, call.IdempotencyKey)
		impl := r.submitImpl
		r.mu.Unlock()
		return impl(ctx, ref, docs, call)
	}
	r.w = workerFor(pool, NewMockOutboundResolver(), r.spy)
	return r
}

func (r *rig) calls() (create, submit int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.createCalls, r.submitCalls
}

// create runs phase A for the rig's player and returns the orphan.
func (r *rig) create() Verification {
	r.t.Helper()
	v, existing := requestCreate(r.t, r.pool, r.f, "mock")
	if existing {
		r.t.Fatal("expected a new verification")
	}
	return v
}

// createSent runs phase A and the worker until the create row is sent.
func (r *rig) createSent() Verification {
	r.t.Helper()
	v := r.create()
	passUntilQuiet(r.t, r.w)
	if row := onlyRow(r.t, r.pool, r.f.tenantID, v.ID, OpCreate); row.State != OutboxSent {
		r.t.Fatalf("create row = %s, want sent", row.State)
	}
	return r.reload(v.ID)
}

func (r *rig) reload(id uuid.UUID) Verification {
	r.t.Helper()
	var v Verification
	if err := r.pool.WithTenant(context.Background(), r.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		v, err = GetVerificationByID(ctx, tx, id)
		return err
	}); err != nil {
		r.t.Fatalf("reload verification: %v", err)
	}
	return v
}

// pass runs one worker pass.
func (r *rig) pass() PassStats { return r.w.RunPass(context.Background()) }

// snapshot returns the verification row as JSON text (for byte-identical checks).
func (r *rig) snapshot(id uuid.UUID) string {
	r.t.Helper()
	var s string
	if err := r.pool.WithTenant(context.Background(), r.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT to_jsonb(v)::text FROM kyc_verifications v WHERE id = $1`, id).Scan(&s)
	}); err != nil {
		r.t.Fatalf("snapshot verification: %v", err)
	}
	return s
}

// alerts returns the tenant's own (subject) alerts of the KYC Kind.
func (r *rig) alerts() []alertinject.Row {
	r.t.Helper()
	return alertinject.Find(alertinject.ForSubject(r.t, r.pool, r.f.tenantID), "kyc.submission_failed_terminal")
}

// auditRows returns (action, metadata) for every audit row targeting id.
type auditRow struct {
	Action  string
	Actor   string
	Outcome string
	Meta    map[string]any
}

func (r *rig) auditFor(targetID uuid.UUID) []auditRow {
	r.t.Helper()
	var out []auditRow
	if err := r.pool.WithTenant(context.Background(), r.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT action, actor_type, outcome, metadata FROM audit_log WHERE tenant_id = $1 AND target_id = $2 ORDER BY created_at, id`,
			r.f.tenantID, targetID.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a auditRow
			var raw []byte
			if err := rows.Scan(&a.Action, &a.Actor, &a.Outcome, &raw); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &a.Meta); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	}); err != nil {
		r.t.Fatalf("read audit rows: %v", err)
	}
	return out
}

func auditCount(rows []auditRow, action string) int {
	n := 0
	for _, a := range rows {
		if a.Action == action {
			n++
		}
	}
	return n
}

func auditByAction(rows []auditRow, action string) []auditRow {
	var out []auditRow
	for _, a := range rows {
		if a.Action == action {
			out = append(out, a)
		}
	}
	return out
}

// setTenantStatus changes the tenant's status through a platform-admin session
// (the only sanctioned writer of tenants).
func setTenantStatus(t *testing.T, pool *db.Pool, tenantID uuid.UUID, status string) {
	t.Helper()
	// ADR 0112: governed fixture (real launch guards), not a raw UPDATE.
	if err := launchfix.TrySetTenantStatusOn(context.Background(), t, pool, tenantID, status); err != nil {
		t.Fatalf("set tenant status %s: %v", status, err)
	}
}

// within reports whether got is within tol of want.
func within(got, want, tol time.Duration) bool {
	d := got - want
	if d < 0 {
		d = -d
	}
	return d <= tol
}

// nextAttemptIn returns next_attempt_at - now() of the row, by the DATABASE clock.
func nextAttemptIn(t *testing.T, pool *db.Pool, tenantID, rowID uuid.UUID) time.Duration {
	t.Helper()
	var secs float64
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT extract(epoch FROM (next_attempt_at - now())) FROM kyc_submission_outbox WHERE id = $1`, rowID).Scan(&secs)
	}); err != nil {
		t.Fatalf("read next_attempt_at: %v", err)
	}
	return time.Duration(secs * float64(time.Second))
}

// seedStaffReview rejects/approves a verification as a compliance reviewer.
func (r *rig) staffReview(id uuid.UUID, status VerificationStatus) uuid.UUID {
	r.t.Helper()
	staffID := seedComplianceStaff(r.t, r.pool, r.f)
	if err := r.pool.WithTenant(context.Background(), r.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewVerification(ctx, tx, ReviewVerificationParams{VerificationID: id, StaffID: staffID, NewStatus: status, Reason: "staff decision"})
		return err
	}); err != nil {
		r.t.Fatalf("staff review: %v", err)
	}
	return staffID
}

//go:build integration

// lic11 (migration 0182): the governed write side of the withdrawal restriction policy. Every assertion about what the
// application can do runs as igaming_runtime (non-superuser, non-BYPASSRLS, asserted); the owner pool provisions fixtures
// (tenants, extra platform staff) and reads for assertions. Requires TEST_DATABASE_URL and TEST_RUNTIME_DATABASE_URL (a
// skip is NOT evidence). No real money, no real provider.
package withdrawalpolicy

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

type gworld struct {
	t                           *testing.T
	owner, rt                   *db.Pool
	svc                         *Service
	ops                         launchfix.OperatorIDs
	req, ap1, ap2, ap3, ap1Twin Actor
}

func connect(t *testing.T, u string) *db.Pool {
	t.Helper()
	p, err := db.Connect(context.Background(), u, 10, 5*time.Second)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func newGWorld(t *testing.T) *gworld {
	t.Helper()
	ou, ru := os.Getenv("TEST_DATABASE_URL"), os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if ou == "" || ru == "" {
		t.Skip("TEST_DATABASE_URL and TEST_RUNTIME_DATABASE_URL are required (a skip is not evidence)")
	}
	w := &gworld{t: t, owner: connect(t, ou), rt: connect(t, ru)}
	var who string
	var super, bypass bool
	if err := w.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_user::text, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&who, &super, &bypass)
	}); err != nil {
		t.Fatal(err)
	}
	if who != "igaming_runtime" || super || bypass {
		t.Fatalf("the runtime pool must be igaming_runtime, non-superuser, non-BYPASSRLS; got %q super=%v bypass=%v", who, super, bypass)
	}
	prooftest.Install(t, ou)
	launchfix.SeedOperators(t)
	w.ops = launchfix.Operators()
	plat := func(id uuid.UUID) Actor {
		return Actor{StaffID: id, Scope: ScopePlatform, Role: string(auth.RolePlatformAdmin), RequestID: "wrp-" + id.String()[:8], IPAddress: "10.0.0.7"}
	}
	w.req, w.ap1, w.ap2 = plat(w.ops.Requester), plat(w.ops.Approver1), plat(w.ops.Approver2)
	w.ap3 = plat(w.platformStaff(uuid.Nil))
	w.ap1Twin = plat(w.platformStaff(w.ops.Persons[1]))
	s, err := New(Config{Pool: w.rt, LockTimeout: "3s"})
	if err != nil {
		t.Fatal(err)
	}
	w.svc = s
	return w
}

func (w *gworld) platformStaff(person uuid.UUID) uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if person == uuid.Nil {
			person = uuid.New()
			if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id)
			VALUES ($1, NULL, $2, '!wrp-fixture-not-a-login', 'platform_admin', $3)`, id, "wrp-"+id.String()+"@fixture.invalid", person)
		return err
	}); err != nil {
		w.t.Fatalf("seed platform staff: %v", err)
	}
	return id
}

func (w *gworld) tenant() uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "wrp", "wrp-"+uuid.NewString(), "under_platform_licence")
		id = tn.ID
		return err
	}); err != nil {
		w.t.Fatalf("seed tenant: %v", err)
	}
	return id
}

func (w *gworld) tenantStaff(tenantID uuid.UUID, role string) Actor {
	w.t.Helper()
	id, person := uuid.New(), uuid.New()
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id) VALUES ($1, $2, $3, '!x', $4, $5)`,
			id, tenantID, "wrp-"+id.String()+"@fixture.invalid", role, person)
		return err
	}); err != nil {
		w.t.Fatalf("seed tenant staff: %v", err)
	}
	return Actor{StaffID: id, Scope: ScopeTenant, TenantID: tenantID, Role: role}
}

func (w *gworld) ownerInt(sql string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	}); err != nil {
		w.t.Fatalf("owner query %q: %v", sql, err)
	}
	return n
}

func sqlstate(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return GuardCode(err)
}

// Tightening (default allow -> block) is SINGLE ACTOR with a reason code: it executes in the request's own transaction,
// writes version 1, and the audit trail has requested + executed with before / after, the IP and the reason.
func TestGovernance_Tightening_IsSingleActor(t *testing.T) {
	w := newGWorld(t)
	tn := w.tenant()
	res, err := w.svc.Request(context.Background(), w.req, RequestInput{TenantID: tn, Cause: "licence_suspended", Action: ActionBlock, ReasonCode: "licence_suspension_hold"})
	if err != nil || !res.Executed || res.Policy == nil || res.Policy.SetVersion != 1 || res.Change.Widening || res.Change.RequiredApprovals != 0 {
		t.Fatalf("a tightening executes at once: %+v %v", res, err)
	}
	if n := w.ownerInt(`SELECT count(*) FROM audit_log WHERE subject_tenant_id = $1 AND action = 'withdrawal_restriction_policy.executed'
		AND host(ip_address) = '10.0.0.7' AND metadata->>'reason_code' = 'licence_suspension_hold'
		AND metadata->'before'->0->>'source' = 'default_unconfigured' AND (metadata->'after'->>'set_version')::int = 1`, tn); n != 1 {
		t.Fatalf("the executed audit must carry actor, IP, reason, before and after, got %d", n)
	}
	if n := w.ownerInt(`SELECT count(*) FROM audit_log WHERE subject_tenant_id = $1 AND action = 'withdrawal_restriction_policy.requested'`, tn); n != 1 {
		t.Fatalf("one requested audit, got %d", n)
	}
	// hold_for_review over block is ALSO a loosening (block is stricter): it needs approvals.
	res, err = w.svc.Request(context.Background(), w.req, RequestInput{TenantID: tn, Cause: "licence_suspended", Action: ActionHoldForReview, ReasonCode: "partial_relax"})
	if err != nil || res.Executed || !res.Change.Widening || res.Change.RequiredApprovals != 2 {
		t.Fatalf("hold_for_review after block widens: %+v %v", res, err)
	}
}

// Loosening (block -> allow) needs TWO approvals by distinct Persons that are neither the requester's Person nor the
// requester's account: self-approval is refused, a second account of an approver's Person is refused, ONE approver cannot
// loosen (the change stays requested and the policy unchanged), the second distinct approver executes it.
func TestGovernance_Loosening_NeedsTwoDistinctApprovers(t *testing.T) {
	w := newGWorld(t)
	tn := w.tenant()
	if _, err := w.svc.Request(context.Background(), w.req, RequestInput{TenantID: tn, Cause: "licence_expired", Action: ActionBlock, ReasonCode: "tighten"}); err != nil {
		t.Fatal(err)
	}
	res, err := w.svc.Request(context.Background(), w.req, RequestInput{TenantID: tn, Cause: "licence_expired", Action: ActionAllow, ReasonCode: "q12_owner_approved"})
	if err != nil || res.Executed || !res.Change.Widening || res.Change.RequiredApprovals != 2 {
		t.Fatalf("a loosening waits for approvals: %+v %v", res, err)
	}
	id := res.Change.ID
	if _, err := w.svc.Approve(context.Background(), w.req, id, "self"); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("self-approval must be refused, got %v", err)
	}
	r1, err := w.svc.Approve(context.Background(), w.ap1, id, "ok")
	if err != nil || r1.Executed {
		t.Fatalf("one approval does not execute: %+v %v", r1, err)
	}
	if _, err := w.svc.Approve(context.Background(), w.ap1Twin, id, "ok"); !errors.Is(err, ErrAlreadyDecided) && sqlstate(err) != "23505" {
		t.Fatalf("a second account of the same Person must not count, got %v", err)
	}
	if v := w.ownerInt(`SELECT max(set_version) FROM withdrawal_restriction_policies WHERE tenant_id = $1`, tn); v != 1 {
		t.Fatalf("one approver cannot loosen: set version %d", v)
	}
	// One approver cannot force the execution through raw SQL either (WR013: requested -> executed needs approved; WR020).
	err = w.rt.WithPlatformAdmin(context.Background(), w.ap1.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE withdrawal_restriction_policy_changes SET status = 'approved' WHERE id = $1`, id)
		return err
	})
	if c := sqlstate(err); c != "WR020" {
		t.Fatalf("approved with one approval must be WR020, got %v", err)
	}
	r2, err := w.svc.Approve(context.Background(), w.ap2, id, "ok")
	if err != nil || !r2.Executed || r2.Policy == nil || r2.Policy.Action != ActionAllow || r2.Policy.SetVersion != 2 {
		t.Fatalf("the second distinct approver executes: %+v %v", r2, err)
	}
	if n := w.ownerInt(`SELECT count(*) FROM withdrawal_restriction_policy_approvals WHERE change_id = $1`, id); n != 2 {
		t.Fatalf("two approvals recorded, got %d", n)
	}
}

// Only a platform_admin PLATFORM session writes: a tenant actor (any role) is refused in Go and, through raw SQL, by RLS;
// a non-configurable cause (owner decision 13-18) is refused (Go and WR002); a change without a proof is refused (AP001); a
// policy row without an executed change of the transaction is refused (WR040).
func TestGovernance_WriteScope_AndGuards(t *testing.T) {
	w := newGWorld(t)
	tn := w.tenant()
	ta := w.tenantStaff(tn, string(auth.RoleTenantAdmin))
	if _, err := w.svc.Request(context.Background(), ta, RequestInput{TenantID: tn, Cause: "licence_suspended", Action: ActionAllow, ReasonCode: "x"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a tenant may not change its policy, got %v", err)
	}
	err := w.rt.WithPrincipalScope(context.Background(), tn, ta.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO withdrawal_restriction_policies (tenant_id, cause, action, reason_code) VALUES ($1, 'licence_suspended', 'allow', 'x')`, tn)
		return err
	})
	if sqlstate(err) != "42501" {
		t.Fatalf("RLS refuses a tenant write, got %v", err)
	}
	if _, err := w.svc.Request(context.Background(), w.req, RequestInput{TenantID: tn, Cause: "tenant_suspended", Action: ActionAllow, ReasonCode: "x"}); !errors.Is(err, ErrNotConfigurable) {
		t.Fatalf("tenant_suspended is owner-decided, got %v", err)
	}
	err = w.rt.WithPlatformAdmin(context.Background(), w.req.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO withdrawal_restriction_policy_changes (tenant_id, cause, action, reason_code) VALUES ($1, 'brand_closed', 'allow', 'x')`, tn)
		return err
	})
	if sqlstate(err) != "WR002" {
		t.Fatalf("a non-configurable cause is WR002 in SQL, got %v", err)
	}
	err = w.rt.WithPlatformAdmin(context.Background(), w.req.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO withdrawal_restriction_policy_changes (tenant_id, cause, action, reason_code) VALUES ($1, 'licence_missing', 'block', 'x')`, tn)
		return err
	})
	if sqlstate(err) != "AP001" {
		t.Fatalf("a change without a proof is AP001, got %v", err)
	}
	err = w.rt.WithPlatformAdmin(context.Background(), w.req.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO withdrawal_restriction_policies (tenant_id, cause, action, reason_code) VALUES ($1, 'licence_missing', 'block', 'x')`, tn)
		return err
	})
	if sqlstate(err) != "WR040" {
		t.Fatalf("a policy row outside an executed change is WR040, got %v", err)
	}
	// A rejected / cancelled change admits no policy row and cannot be revived.
	res, err := w.svc.Request(context.Background(), w.req, RequestInput{TenantID: tn, Cause: "licence_missing", Action: ActionBlock, ReasonCode: "tighten"})
	if err != nil {
		t.Fatal(err)
	}
	loosen, err := w.svc.Request(context.Background(), w.req, RequestInput{TenantID: tn, Cause: "licence_missing", Action: ActionAllow, ReasonCode: "relax"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Reject(context.Background(), w.ap1, loosen.Change.ID, "not_approved"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if _, err := w.svc.Approve(context.Background(), w.ap2, loosen.Change.ID, "late"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("a rejected change cannot be approved, got %v", err)
	}
	if v := w.ownerInt(`SELECT max(set_version) FROM withdrawal_restriction_policies WHERE tenant_id = $1`, tn); v != int(res.Policy.SetVersion) {
		t.Fatalf("a rejected change writes nothing: version %d", v)
	}
}

// READ API: a tenant reads its OWN effective policy (never another tenant's); an unconfigured tenant shows every
// configurable cause as default_unconfigured with the UNCONFIGURED notice and set version 0, and the non-configurable
// causes with their governing reference; after a change the cause shows configured.
func TestRead_Effective_UnconfiguredVisible_TenantScoped(t *testing.T) {
	w := newGWorld(t)
	tn, other := w.tenant(), w.tenant()
	reader := w.tenantStaff(tn, string(auth.RoleCompliance))
	v, err := w.svc.Effective(context.Background(), reader, tn)
	if err != nil {
		t.Fatal(err)
	}
	if v.Configured || v.SetVersion != 0 || v.Disclaimer == "" {
		t.Fatalf("an unconfigured tenant: %+v", v)
	}
	seen := map[string]bool{}
	for _, c := range v.Causes {
		seen[c.Cause] = true
		if c.Configurable {
			if !c.Unconfigured || c.Notice != UnconfiguredNotice || len(c.Stages) != 3 {
				t.Fatalf("%s must be visibly unconfigured: %+v", c.Cause, c)
			}
			for _, st := range c.Stages {
				if st.Source != SourceDefault || st.Action != ActionAllow {
					t.Fatalf("%s/%s: the interim default is allow / default_unconfigured, got %+v", c.Cause, st.Stage, st)
				}
			}
		} else if c.GoverningReference == "" || len(c.Stages) != 0 {
			t.Fatalf("%s: a non-configurable cause shows its governing reference and no stages: %+v", c.Cause, c)
		}
	}
	for _, want := range []string{"licence_suspended", "licence_expired", "licence_not_yet_issued", "licence_missing", "tenant_suspended", "tenant_closed", "brand_suspended", "brand_closed", "market_closed"} {
		if !seen[want] {
			t.Fatalf("the read API must list %s", want)
		}
	}
	if _, err := w.svc.Effective(context.Background(), reader, other); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a foreign tenant is not found, got %v", err)
	}
	if _, err := w.svc.Request(context.Background(), w.req, RequestInput{TenantID: tn, Cause: "licence_suspended", Action: ActionHoldForReview, ReasonCode: "tighten"}); err != nil {
		t.Fatal(err)
	}
	v, err = w.svc.Effective(context.Background(), reader, tn)
	if err != nil || !v.Configured || v.SetVersion != 1 {
		t.Fatalf("configured view: %+v %v", v, err)
	}
	for _, c := range v.Causes {
		if c.Cause == "licence_suspended" && (c.Unconfigured || c.Stages[0].Source != SourceConfigured || c.Stages[0].Action != ActionHoldForReview) {
			t.Fatalf("licence_suspended must show configured hold_for_review: %+v", c)
		}
	}
	hist, err := w.svc.History(context.Background(), reader, tn)
	if err != nil || len(hist) != 1 || hist[0].Action != ActionHoldForReview {
		t.Fatalf("history: %+v %v", hist, err)
	}
	// Change requests are platform records: a tenant reader gets nothing.
	if _, err := w.svc.ListChanges(context.Background(), reader, ListFilter{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a tenant may not list change requests, got %v", err)
	}
}

// Concurrency: two simultaneous tightenings of the same (tenant, cause) never both stay open, and set versions stay
// gap-free and unique per tenant (the tenant key serialises them).
func TestGovernance_ConcurrentChanges_SerialiseOnTheTenantKey(t *testing.T) {
	w := newGWorld(t)
	tn := w.tenant()
	errs := make(chan error, 4)
	for i, cause := range []string{"licence_suspended", "licence_expired", "licence_missing", "licence_not_yet_issued"} {
		go func(i int, cause string) {
			_, err := w.svc.Request(context.Background(), w.req, RequestInput{TenantID: tn, Cause: cause, Action: ActionBlock, ReasonCode: "tighten"})
			errs <- err
		}(i, cause)
	}
	for range 4 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent tightening: %v", err)
		}
	}
	if n := w.ownerInt(`SELECT count(DISTINCT set_version) FROM withdrawal_restriction_policies WHERE tenant_id = $1 AND set_version BETWEEN 1 AND 4`, tn); n != 4 {
		t.Fatalf("set versions must be 1..4 without gaps or duplicates, got %d distinct", n)
	}
}

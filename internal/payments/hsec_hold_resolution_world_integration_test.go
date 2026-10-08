//go:build integration

// HSEC-APPROVED-HOLD-RELEASE-1 (ADR 0111 section 6, migration 0124) shared
// fixtures. Every world is a fully isolated, fully migrated PRIVATE scratch
// database (its own tenant, player, platform staff, grants and policy), and every
// assertion runs as the REAL runtime role (TEST_RUNTIME_DATABASE_URL, asserted
// neither superuser nor BYPASSRLS, T-1 vacuity) through the real service. All
// policy values are synthetic and test-only (HD-PRH2-3). The owner pool (w.pool)
// is used only for fixtures and read-backs.
package payments

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/adjustment"
	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

const hsrReason = "tenant_suspended_release"

type hsr struct {
	*k3World
	rt     *db.Pool
	svc    *WithdrawalHoldResolutionService
	forger prooftest.Forger
	// Platform principals with DISTINCT Persons. reqA holds request+approve; apprB,
	// apprC, apprD hold approve only. noHR holds an acting session (a K3 grant for
	// the tenant) but NO withdrawal_hold_resolution capability.
	reqA, apprB, apprC, apprD, noHR k3Staff
}

// newHSR builds the world. base is the platform baseline requirement of the
// withdrawal_hold_resolution policy; 0 means "no baseline" (the operation is
// DISABLED, HD-PRH2-3).
func newHSR(t *testing.T, base int) *hsr {
	t.Helper()
	w, rt, forger := k3RuntimeWorld(t, k3Opts{})
	h := &hsr{k3World: w, rt: rt, forger: forger, svc: NewWithdrawalHoldResolutionService(rt)}
	h.reqA = w.staffMember(uuid.Nil, "platform_admin")
	h.apprB = w.staffMember(uuid.Nil, "platform_admin")
	h.apprC = w.staffMember(uuid.Nil, "platform_admin")
	h.apprD = w.staffMember(uuid.Nil, "platform_admin")
	h.noHR = w.staffMember(uuid.Nil, "platform_admin")
	w.grantActing(h.reqA, capability.CapabilityWithdrawalHoldResolutionRequest)
	w.grantActing(h.reqA, capability.CapabilityWithdrawalHoldResolutionApprove)
	for _, s := range []k3Staff{h.apprB, h.apprC, h.apprD} {
		w.grantActing(s, capability.CapabilityWithdrawalHoldResolutionApprove)
	}
	w.grantActing(h.noHR, capability.CapabilityPaymentForceResolveApprove)
	if base > 0 {
		w.approvePolicy(adjustment.PolicyChangeInput{
			ChangeKind: adjustment.ChangeKindPolicy, OperationKind: OperationKindHoldResolution, Level: adjustment.LevelPlatform,
			AssetCode: k3StrPtr("EUR"), BaseRequiredApprovals: k3IntPtr(base),
		}, w.adminA, w.adminB)
	}
	w.ensureWithdrawalPolicy()
	return h
}

// hold creates an APPROVED withdrawal (hold placed) on the still-active tenant.
func (h *hsr) hold(amount int64) withdrawal.WithdrawalRequest {
	h.t.Helper()
	return h.approveWithdrawal(amount, "hsr-"+uuid.NewString())
}

func (h *hsr) suspendTenant()  { h.t.Helper(); h.setTenantStatus("suspended") }
func (h *hsr) activateTenant() { h.t.Helper(); h.setTenantStatus("active") }
func (h *hsr) suspendBrand() {
	h.t.Helper()
	setBrandStatus(h.t, h.pool, h.f.tenantID, h.f.brandID, "suspended")
}
func (h *hsr) activateBrand() {
	h.t.Helper()
	setBrandStatus(h.t, h.pool, h.f.tenantID, h.f.brandID, "active")
}
func (h *hsr) meta() ResolutionMeta {
	return ResolutionMeta{RequestID: "hsr-test", IPAddress: "203.0.113.9", UserAgent: "hsr-test-agent"}
}

func (h *hsr) reqIn(wrID uuid.UUID) HoldResolutionRequestInput {
	return HoldResolutionRequestInput{WithdrawalRequestID: wrID, Kind: HoldReleaseToPlayer, ReasonCode: hsrReason,
		EvidenceRefHash: k3EvidenceHash(), Note: "hsr test"}
}

func (h *hsr) request(actor k3Staff, wrID uuid.UUID) (HoldResolution, error) {
	return h.svc.Request(k3Ctx(actor), h.target(actor), h.reqIn(wrID), h.meta())
}

func (h *hsr) mustRequest(actor k3Staff, wrID uuid.UUID) HoldResolution {
	h.t.Helper()
	r, err := h.request(actor, wrID)
	if err != nil {
		h.t.Fatalf("request: %v", err)
	}
	return r
}

func (h *hsr) decide(actor k3Staff, r HoldResolution, d ResolutionDecision) (HoldResolutionOutcome, error) {
	return h.svc.Decide(k3Ctx(actor), h.target(actor), r.ID,
		HoldResolutionDecisionInput{Decision: d, PayloadHash: r.PayloadHash, ReasonCode: "hsr_test"}, h.meta())
}

func (h *hsr) mustDecide(actor k3Staff, r HoldResolution, d ResolutionDecision) HoldResolutionOutcome {
	h.t.Helper()
	out, err := h.decide(actor, r, d)
	if err != nil {
		h.t.Fatalf("decide %s by %s: %v", d, actor.ID, err)
	}
	return out
}

// execute drives request (reqA) -> approvals to execution.
func (h *hsr) execute(wrID uuid.UUID, approvers ...k3Staff) HoldResolution {
	h.t.Helper()
	r := h.mustRequest(h.reqA, wrID)
	var out HoldResolutionOutcome
	for _, a := range approvers {
		out = h.mustDecide(a, r, ResolutionApprove)
	}
	if !out.Executed {
		h.t.Fatalf("hold resolution did not execute (counted %d of %d, refused=%v)", out.Counted, out.Required, out.Refused)
	}
	return out.Resolution
}

func (h *hsr) res(id uuid.UUID) HoldResolution {
	h.t.Helper()
	r, err := h.svc.Get(k3Ctx(h.reqA), h.target(h.reqA), id, h.meta())
	if err != nil {
		h.t.Fatalf("get resolution: %v", err)
	}
	return r
}

func (h *hsr) wr(id uuid.UUID) withdrawal.WithdrawalRequest {
	h.t.Helper()
	var out withdrawal.WithdrawalRequest
	h.tx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = withdrawal.GetByID(ctx, tx, id)
		return err
	})
	return out
}

func (h *hsr) count(sql string, args ...any) int {
	h.t.Helper()
	var n int
	h.tx(func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, sql, args...).Scan(&n) })
	return n
}

func (h *hsr) governedTxCount(wrID uuid.UUID) int {
	return h.count(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND idempotency_key = $2`,
		h.f.tenantID, wrID.String()+":governed_hold_released")
}

func (h *hsr) auditCount(action string, targetID uuid.UUID) int {
	return h.count(`SELECT count(*) FROM audit_log WHERE action = $1 AND target_id = $2`, action, targetID.String())
}

// acting runs fn in a raw ACTING session as actor (no proof attached: the caller
// attaches what it wants), on the RUNTIME role.
func (h *hsr) acting(actor k3Staff, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return h.rt.WithPlatformActingInTenant(context.Background(), actor.ID, h.f.tenantID, uuid.New(), OperationKindHoldResolution, fn)
}

// proof attaches a genuine, server-issued proof for the session's actor.
func (h *hsr) proof(ctx context.Context, tx pgx.Tx, op, target, payload string) error {
	return prooftest.AttachForSession(ctx, tx, op, target, payload)
}

func hsrIs(err error, codes ...string) bool {
	c := k3Code(err)
	for _, want := range codes {
		if c == want {
			return true
		}
	}
	return false
}

// expireNow backdates a pending resolution's expires_at in this throwaway scratch
// database: the owner disables the resolution guard for ONE statement (the K3 X10
// pattern; tests do not wait 24 h).
func (h *hsr) backdate(id uuid.UUID) {
	h.t.Helper()
	ctx := context.Background()
	if err := h.pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `ALTER TABLE withdrawal_hold_resolutions DISABLE TRIGGER withdrawal_hold_resolutions_guard`)
		return err
	}); err != nil {
		h.t.Fatalf("scratch DB: cannot disable the guard to backdate expiry: %v", err)
	}
	err := h.acting(h.reqA, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE withdrawal_hold_resolutions SET expires_at = now() - interval '1 hour' WHERE id = $1`, id)
		if err == nil && tag.RowsAffected() != 1 {
			h.t.Fatalf("backdate matched %d rows", tag.RowsAffected())
		}
		return err
	})
	if err2 := h.pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `ALTER TABLE withdrawal_hold_resolutions ENABLE TRIGGER withdrawal_hold_resolutions_guard`)
		return err
	}); err2 != nil {
		h.t.Fatalf("re-enable the guard: %v", err2)
	}
	if err != nil {
		h.t.Fatalf("backdate: %v", err)
	}
}

var _ = time.Second

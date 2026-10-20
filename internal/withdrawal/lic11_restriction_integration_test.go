//go:build integration

// lic11 (migration 0182): the governed per-tenant withdrawal restriction policy on the money paths of this package (the
// request stage, and the dispatch evaluation + decision row). Every assertion about what the application does runs as
// igaming_runtime (a13World.rt); the owner pool only provisions fixtures (licences and policy rows through the documented
// owner path) and reads for assertions. MOCK providers only; no real money.
package withdrawal

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/withdrawalgate"
	"github.com/Diansalas/igaming-platform/internal/withdrawalgate/standard"
)

// licence states a fixture can put the tenant in.
type l11Licence string

const (
	l11Valid         l11Licence = "valid"
	l11Suspended     l11Licence = "suspended"
	l11StatusExpired l11Licence = "status_expired"
	l11DateExpired   l11Licence = "date_expired"
	l11NotYetIssued  l11Licence = "not_yet_issued"
	l11None          l11Licence = "none"
)

// l11CauseOf is the cause the platform must derive for each state ("" for a valid licence).
var l11CauseOf = map[l11Licence]RestrictionCause{
	l11Valid: "", l11Suspended: CauseLicenceSuspended, l11StatusExpired: CauseLicenceExpired, l11DateExpired: CauseLicenceExpired,
	l11NotYetIssued: CauseLicenceNotYetIssued, l11None: CauseLicenceMissing,
}

// l11SetLicence binds a fresh licence in the given state to the tenant (owner path: the governed licensing writers are the
// owner-exempt 0177 / 0178 guards), or unbinds every licence (l11None).
func l11SetLicence(t *testing.T, owner *db.Pool, tenantID uuid.UUID, st l11Licence) {
	t.Helper()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if err := owner.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		if st == l11None {
			_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = NULL WHERE id = $1`, tenantID)
			return err
		}
		jid, lid := uuid.New(), uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'lic11 test jurisdiction')`, jid, "L11-"+jid.String()[:8]); err != nil {
			return err
		}
		status := "active"
		var issued, expires *time.Time
		switch st {
		case l11Suspended:
			status = "suspended"
		case l11StatusExpired:
			status = "expired"
		case l11DateExpired:
			e := today // invalid ON its expiry date (half-open interval)
			expires = &e
		case l11NotYetIssued:
			i := today.AddDate(0, 0, 3)
			issued = &i
		}
		if _, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number, status, issued_at, expires_at)
			VALUES ($1, $2, 'platform', $3, $4, $5, $6)`, lid, jid, "L11-"+lid.String()[:8], status, issued, expires); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, lid, tenantID)
		return err
	}); err != nil {
		t.Fatalf("set licence %s: %v", st, err)
	}
}

// l11Policy writes a policy row through the OWNER path (the fixture path of the 0182 guard; the governed path is tested in
// internal/withdrawalpolicy) and returns its set version.
func l11Policy(t *testing.T, owner *db.Pool, tenantID uuid.UUID, cause RestrictionCause, action RestrictionAction) int64 {
	t.Helper()
	var v int64
	if err := owner.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO withdrawal_restriction_policies (tenant_id, cause, action, reason_code)
			VALUES ($1, $2, $3, 'lic11_fixture') RETURNING set_version`, tenantID, string(cause), string(action)).Scan(&v)
	}); err != nil {
		t.Fatalf("write policy %s=%s: %v", cause, action, err)
	}
	return v
}

// l11Request runs RequestWithdrawal as the runtime role with the handler's commit pattern (a gate / KYC denial commits
// its audit) and returns the request and error.
func (w *a13World) l11Request(t *testing.T, key string, amount int64, mutate ...func(*RequestParams)) (WithdrawalRequest, error) {
	t.Helper()
	p := baseParams(w.f, key, amount)
	for _, m := range mutate {
		m(&p)
	}
	var wr WithdrawalRequest
	var gate *GateDeniedError
	var kycDenied *KYCDeniedError
	err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		var e error
		wr, e = RequestWithdrawal(ctx, tx, p)
		if errors.As(e, &gate) || errors.As(e, &kycDenied) {
			return nil
		}
		return e
	})
	if gate != nil {
		return wr, gate
	}
	if kycDenied != nil {
		return wr, kycDenied
	}
	return wr, err
}

func (w *a13World) l11Audit(t *testing.T, action, requestOrNil string) []map[string]any {
	t.Helper()
	rows := auditRows(t, w.owner, w.f, action)
	if requestOrNil == "" {
		return rows
	}
	var out []map[string]any
	for _, r := range rows {
		if r["withdrawal_request_id"] == requestOrNil {
			out = append(out, r)
		}
	}
	return out
}

func (w *a13World) l11RequestedAudit(t *testing.T) map[string]any {
	t.Helper()
	rows := auditRows(t, w.owner, w.f, "withdrawal.requested")
	if len(rows) == 0 {
		t.Fatal("no withdrawal.requested audit row")
	}
	return rows[len(rows)-1]
}

func num(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	}
	return -1
}

// DEFAULT PARITY: with NO policy row, every licence state behaves exactly as before 0182 (the request is accepted, the hold
// posted, no extra row is written), and the request's own audit row records source default_unconfigured with the cause
// (a cause in force) or none (valid licence), with set version 0.
func TestLic11_Request_DefaultParity_EveryLicenceState(t *testing.T) {
	for _, st := range []l11Licence{l11Valid, l11Suspended, l11StatusExpired, l11DateExpired, l11NotYetIssued, l11None} {
		w := newA13(t)
		l11SetLicence(t, w.owner, w.f.tenantID, st)
		cashBefore := cashBalance(t, w.owner, w.f)
		wr, err := w.l11Request(t, "l11-default-"+string(st), 700)
		if err != nil || wr.State != StateRequested {
			t.Fatalf("%s: the default must accept the request exactly as before 0182, got %v", st, err)
		}
		if cashBalance(t, w.owner, w.f) != cashBefore-700 || holdBalance(t, w.owner, w.f) != 700 {
			t.Fatalf("%s: the hold must be posted", st)
		}
		md := w.l11RequestedAudit(t)
		wantSource := RestrictionSourceDefault
		if l11CauseOf[st] == "" {
			wantSource = RestrictionSourceNone
		}
		if md["restriction_policy_source"] != wantSource || num(md["restriction_policy_set_version"]) != 0 || md["restriction_action"] != "allow" {
			t.Fatalf("%s: the request audit must carry the provenance, got %v", st, md)
		}
		if want := l11CauseOf[st]; want != "" && md["restriction_cause"] != string(want) || want == "" && md["restriction_cause"] != nil {
			t.Fatalf("%s: the request audit must name the cause in force, got %v", st, md)
		}
		if n := len(w.l11Audit(t, "withdrawal.restriction_cause_in_force", "")) + len(w.l11Audit(t, "withdrawal.held_for_restriction_review", "")); n != 0 {
			t.Fatalf("%s: the default path writes no extra audit row, got %d", st, n)
		}
		assertLedgerBalanced(t, w.owner, w.f.tenantID)
	}
}

// CONFIGURED MATRIX at the request stage: every configurable cause x action. allow -> accepted; hold_for_review ->
// accepted + withdrawal.held_for_restriction_review; block -> WD056 GateDeniedError, the denial audit commits and NOTHING
// else (no row, no hold, the key is not consumed: after loosening, the same key succeeds).
func TestLic11_Request_ConfiguredCauseActionMatrix(t *testing.T) {
	for _, st := range []l11Licence{l11Suspended, l11StatusExpired, l11NotYetIssued, l11None} {
		cause := l11CauseOf[st]
		for _, action := range []RestrictionAction{RestrictionAllow, RestrictionHoldForReview, RestrictionBlock} {
			name := fmt.Sprintf("%s/%s", cause, action)
			w := newA13(t)
			l11SetLicence(t, w.owner, w.f.tenantID, st)
			v := l11Policy(t, w.owner, w.f.tenantID, cause, action)
			key := "l11-matrix-" + uuid.NewString()
			cashBefore := cashBalance(t, w.owner, w.f)
			wr, err := w.l11Request(t, key, 900)
			switch action {
			case RestrictionBlock:
				var denied *GateDeniedError
				if !errors.As(err, &denied) || denied.GateCode != GateCodeRestriction || denied.Reason != string(cause) {
					t.Fatalf("%s: want a WD056 denial, got %v", name, err)
				}
				if n := countRows(t, w.owner, w.f.tenantID, `SELECT count(*) FROM withdrawal_requests`); n != 0 {
					t.Fatalf("%s: no request row may exist, got %d", name, n)
				}
				if cashBalance(t, w.owner, w.f) != cashBefore || holdBalance(t, w.owner, w.f) != 0 {
					t.Fatalf("%s: a refusal moves no money", name)
				}
				rows := auditRows(t, w.owner, w.f, "withdrawal.denied_by_restriction_policy")
				if len(rows) != 1 || rows[0]["gate_code"] != "WD056" || rows[0]["restriction_policy_source"] != RestrictionSourceConfigured ||
					num(rows[0]["restriction_policy_set_version"]) != v || rows[0]["restriction_cause"] != string(cause) {
					t.Fatalf("%s: the denial audit must carry the configured provenance, got %v", name, rows)
				}
				// Loosen (a later version); the same key now succeeds: it was never consumed.
				l11Policy(t, w.owner, w.f.tenantID, cause, RestrictionAllow)
				if wr2, err := w.l11Request(t, key, 900); err != nil || wr2.State != StateRequested {
					t.Fatalf("%s: after loosening the same key must succeed, got %v", name, err)
				}
			default:
				if err != nil || wr.State != StateRequested {
					t.Fatalf("%s: want the request accepted, got %v", name, err)
				}
				if holdBalance(t, w.owner, w.f) != 900 {
					t.Fatalf("%s: the hold must be posted", name)
				}
				md := w.l11RequestedAudit(t)
				if md["restriction_policy_source"] != RestrictionSourceConfigured || md["restriction_action"] != string(action) ||
					num(md["restriction_policy_set_version"]) != v || md["restriction_cause"] != string(cause) {
					t.Fatalf("%s: request audit provenance %v", name, md)
				}
				held := w.l11Audit(t, "withdrawal.held_for_restriction_review", wr.ID.String())
				inForce := w.l11Audit(t, "withdrawal.restriction_cause_in_force", wr.ID.String())
				if (action == RestrictionHoldForReview) != (len(held) == 1) || (action == RestrictionAllow) != (len(inForce) == 1) {
					t.Fatalf("%s: configured-cause audit rows held=%v in_force=%v", name, held, inForce)
				}
			}
			assertLedgerBalanced(t, w.owner, w.f.tenantID)
		}
	}
}

// A configured block of a cause that is NOT in force changes nothing (valid licence: source none).
func TestLic11_Request_PolicyAppliesOnlyWhileTheCauseIsInForce(t *testing.T) {
	w := newA13(t)
	l11SetLicence(t, w.owner, w.f.tenantID, l11Valid)
	for _, c := range ConfigurableCauses {
		l11Policy(t, w.owner, w.f.tenantID, c, RestrictionBlock)
	}
	if wr, err := w.l11Request(t, "l11-valid-"+uuid.NewString(), 500); err != nil || wr.State != StateRequested {
		t.Fatalf("a valid licence is not restricted by any configured cause, got %v", err)
	}
	md := w.l11RequestedAudit(t)
	if md["restriction_policy_source"] != RestrictionSourceNone || num(md["restriction_policy_set_version"]) != 4 {
		t.Fatalf("provenance: %v", md)
	}
}

// HARD RESTRICTIONS are never overridable: a configured ALLOW for the cause in force never skips the H-SEC-11 status gate,
// the RG half (account status), the KYC / AML gate, the risk half or the payout-instrument gate. Each refusal is the
// existing one, unchanged.
func TestLic11_HardRestrictions_NotOverridableByAConfiguredAllow(t *testing.T) {
	setup := func(t *testing.T) *a13World {
		w := newA13(t)
		l11SetLicence(t, w.owner, w.f.tenantID, l11Suspended)
		l11Policy(t, w.owner, w.f.tenantID, CauseLicenceSuspended, RestrictionAllow)
		return w
	}
	t.Run("account_suspended", func(t *testing.T) {
		w := setup(t)
		setAccountStatus(t, w.owner, w.f, identity.PlayerStatusSuspended)
		_, err := w.l11Request(t, "l11-hard-acct", 500)
		var denied *GateDeniedError
		if !errors.As(err, &denied) || denied.GateCode != GateCodeRG || denied.Reason != rg.CodePlayerAccountNotActive {
			t.Fatalf("want WD050, got %v", err)
		}
	})
	t.Run("risk_deny", func(t *testing.T) {
		w := setup(t)
		_, err := w.l11Request(t, "l11-hard-risk", 500, func(p *RequestParams) { p.Gate = l11DenyRisk{} })
		var denied *GateDeniedError
		if !errors.As(err, &denied) || denied.GateCode != GateCodeRisk {
			t.Fatalf("want WD051, got %v", err)
		}
	})
	t.Run("kyc_rejected", func(t *testing.T) {
		w := setup(t)
		setFixtureVerification(t, w.owner, w.f, "rejected")
		_, err := w.l11Request(t, "l11-hard-kyc", 500)
		var kd *KYCDeniedError
		if !errors.As(err, &kd) {
			t.Fatalf("want a KYC denial, got %v", err)
		}
	})
	t.Run("aml_hold", func(t *testing.T) {
		w := setup(t)
		w.holdAll(t)
		_, err := w.l11Request(t, "l11-hard-aml", 500)
		var kd *KYCDeniedError
		if !errors.As(err, &kd) {
			t.Fatalf("an AML hold must still deny, got %v", err)
		}
	})
	t.Run("instrument_unknown", func(t *testing.T) {
		w := setup(t)
		_, err := w.l11Request(t, "l11-hard-instr", 500, func(p *RequestParams) { p.PayoutInstrumentID = uuid.New() })
		if !errors.Is(err, ErrPayoutInstrumentNotUsable) {
			t.Fatalf("want the instrument refusal, got %v", err)
		}
	})
	for _, subject := range []string{"tenant", "brand"} {
		t.Run(subject+"_suspended", func(t *testing.T) {
			w := setup(t)
			if subject == "tenant" {
				launchfix.SetTenantStatus(t, w.f.tenantID, "suspended")
			} else {
				launchfix.SetBrandStatus(t, w.f.tenantID, w.f.brandID, "suspended")
			}
			_, err := w.l11Request(t, "l11-hard-"+subject, 500)
			if !errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
				t.Fatalf("H-SEC-11 (owner decisions 13-18) keeps its refusal under any policy, got %v", err)
			}
		})
	}
}

type l11DenyRisk struct{}

func (l11DenyRisk) Evaluate(context.Context, pgx.Tx, withdrawalgate.Input) (withdrawalgate.Decision, error) {
	return withdrawalgate.Decision{Allowed: false, GateCode: withdrawalgate.CodeRisk, Reason: "hard_limit_breach", Outcome: "deny"}, nil
}

// DISPATCH (the decision this package records): EvaluateDispatchGate returns a WD056 denial for a configured block /
// hold_for_review WITHOUT consulting the gate; the recorded denying row carries the provenance and vetoes approved ->
// submitted in the same transaction (WD053). With no policy, the decision is the gate's with source
// default_unconfigured; with a configured allow, source configured.
func TestLic11_Dispatch_EvaluateAndRecord(t *testing.T) {
	for _, action := range []RestrictionAction{RestrictionHoldForReview, RestrictionBlock} {
		w := newA13(t)
		l11SetLicence(t, w.owner, w.f.tenantID, l11Suspended)
		wr := w.at(t, StateApproved)
		v := l11Policy(t, w.owner, w.f.tenantID, CauseLicenceSuspended, action)
		g := &l11CountingGate{}
		err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
			d, err := EvaluateDispatchGate(ctx, tx, g, wr)
			if err != nil {
				return err
			}
			if d.Allowed || d.GateCode != GateCodeRestriction || d.Reason != string(CauseLicenceSuspended) || d.Restriction == nil ||
				d.Restriction.Source != RestrictionSourceConfigured || d.Restriction.SetVersion != v || d.Restriction.Action != string(action) {
				return fmt.Errorf("unexpected decision %+v / %+v", d, d.Restriction)
			}
			if err := f4Record(ctx, tx, wr.ID, d); err != nil {
				return err
			}
			// The F4 backstop still holds: a denying decision of this transaction vetoes E7.
			e7 := MarkSubmittedPending(ctx, tx, wr.ID, "lic11-mock")
			if a13Code(e7) != SQLStateDispatchNoDecision {
				return fmt.Errorf("E7 after a WD056 denial must be WD053, got %v", e7)
			}
			return errors.New("rollback")
		})
		if err == nil || err.Error() != "rollback" {
			t.Fatalf("%s: %v", action, err)
		}
		if g.calls != 0 {
			t.Fatalf("%s: a policy refusal must not consult the WD-RG-1 gate", action)
		}
	}
	// Default and configured allow: the gate decides, the provenance is attached and recorded.
	w := newA13(t)
	l11SetLicence(t, w.owner, w.f.tenantID, l11DateExpired)
	wr := w.at(t, StateApproved)
	check := func(wantSource string, wantVersion int64) {
		t.Helper()
		if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
			d, err := EvaluateDispatchGate(ctx, tx, standard.New(), wr)
			if err != nil {
				return err
			}
			if !d.Allowed || d.Restriction == nil || d.Restriction.Source != wantSource || d.Restriction.SetVersion != wantVersion ||
				d.Restriction.Cause != string(CauseLicenceExpired) || d.Restriction.Action != "allow" {
				return fmt.Errorf("decision %+v / %+v", d, d.Restriction)
			}
			return RecordDispatchGateDecision(ctx, tx, wr, DispatchStageDispatch, nil, d)
		}); err != nil {
			t.Fatalf("%s: %v", wantSource, err)
		}
		if n := countRows(t, w.owner, w.f.tenantID, `SELECT count(*) FROM withdrawal_gate_decisions WHERE withdrawal_request_id = $1 AND allowed
			AND restriction_policy_source = $2 AND restriction_policy_set_version = $3 AND restriction_cause = 'licence_expired' AND restriction_action = 'allow'`,
			wr.ID, wantSource, wantVersion); n != 1 {
			t.Fatalf("%s: the allowing decision row must carry the provenance, got %d", wantSource, n)
		}
	}
	check(RestrictionSourceDefault, 0)
	v := l11Policy(t, w.owner, w.f.tenantID, CauseLicenceExpired, RestrictionAllow)
	check(RestrictionSourceConfigured, v)
}

type l11CountingGate struct{ calls int }

func (g *l11CountingGate) Evaluate(context.Context, pgx.Tx, withdrawalgate.Input) (withdrawalgate.Decision, error) {
	g.calls++
	return withdrawalgate.Decision{Allowed: true, Outcome: "allow"}, nil
}

// The database refuses a WD056 row that is not a configured hold / block of the named cause (CHECK
// withdrawal_gate_decisions_wd056_shape), and the outcome CHECK still refuses an unknown gate code.
func TestLic11_DB_WD056Shape(t *testing.T) {
	w := newA13(t)
	wr := w.at(t, StateApproved)
	for _, c := range []struct {
		name, sql string
	}{
		{"no provenance", `INSERT INTO withdrawal_gate_decisions (tenant_id, withdrawal_request_id, stage, allowed, gate_code, reason_code)
			VALUES ($1, $2, 'dispatch', false, 'WD056', 'licence_suspended')`},
		{"default source", `INSERT INTO withdrawal_gate_decisions (tenant_id, withdrawal_request_id, stage, allowed, gate_code, reason_code,
			restriction_policy_source, restriction_policy_set_version, restriction_cause, restriction_action)
			VALUES ($1, $2, 'dispatch', false, 'WD056', 'licence_suspended', 'default_unconfigured', 0, 'licence_suspended', 'block')`},
		{"allow action", `INSERT INTO withdrawal_gate_decisions (tenant_id, withdrawal_request_id, stage, allowed, gate_code, reason_code,
			restriction_policy_source, restriction_policy_set_version, restriction_cause, restriction_action)
			VALUES ($1, $2, 'dispatch', false, 'WD056', 'licence_suspended', 'configured', 1, 'licence_suspended', 'allow')`},
		{"reason differs from cause", `INSERT INTO withdrawal_gate_decisions (tenant_id, withdrawal_request_id, stage, allowed, gate_code, reason_code,
			restriction_policy_source, restriction_policy_set_version, restriction_cause, restriction_action)
			VALUES ($1, $2, 'dispatch', false, 'WD056', 'licence_expired', 'configured', 1, 'licence_suspended', 'block')`},
		{"unknown gate code", `INSERT INTO withdrawal_gate_decisions (tenant_id, withdrawal_request_id, stage, allowed, gate_code, reason_code)
			VALUES ($1, $2, 'dispatch', false, 'WD057', 'x')`},
		{"half provenance", `INSERT INTO withdrawal_gate_decisions (tenant_id, withdrawal_request_id, stage, allowed, gate_code, reason_code,
			restriction_policy_source) VALUES ($1, $2, 'dispatch', false, 'WD050', 'x', 'none')`},
	} {
		err := w.rtExec(c.sql, w.f.tenantID, wr.ID)
		if a13Code(err) != "23514" {
			t.Fatalf("%s: want a CHECK violation, got %v", c.name, err)
		}
	}
}

// TENANT ISOLATION: a policy of tenant A never applies to tenant B; B's session reads none of A's rows; no tenant session
// can write a policy row (FORCE RLS: platform sessions only); a non-configurable cause is refused even on the owner path.
func TestLic11_TenantIsolation_AndWriteScope(t *testing.T) {
	a, b := newA13(t), newA13(t)
	for _, w := range []*a13World{a, b} {
		l11SetLicence(t, w.owner, w.f.tenantID, l11Suspended)
	}
	l11Policy(t, a.owner, a.f.tenantID, CauseLicenceSuspended, RestrictionBlock)
	if _, err := b.l11Request(t, "l11-iso-b", 500); err != nil {
		t.Fatalf("tenant B is not restricted by tenant A's policy, got %v", err)
	}
	var denied *GateDeniedError
	if _, err := a.l11Request(t, "l11-iso-a", 500); !errors.As(err, &denied) {
		t.Fatalf("tenant A is restricted, got %v", err)
	}
	var n int
	if err := b.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_restriction_policies`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("tenant B must see no policy rows (A has one), got %d %v", n, err)
	}
	if err := a.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_restriction_policies`).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("tenant A reads its own row, got %d %v", n, err)
	}
	for _, p := range []*db.Pool{a.rt, a.owner} {
		err := p.WithTenant(context.Background(), a.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO withdrawal_restriction_policies (tenant_id, cause, action, reason_code) VALUES ($1, 'licence_suspended', 'allow', 'x')`, a.f.tenantID)
			return err
		})
		// The guard fires before the RLS WITH CHECK: the runtime role is refused WR040 (no executed change visible), the
		// owner (guard-exempt) by RLS (42501). Either way nothing is written.
		if code := a13Code(err); code != "42501" && code != "WR040" {
			t.Fatalf("a tenant session must not write a policy row, got %v", err)
		}
	}
	err := a.owner.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO withdrawal_restriction_policies (tenant_id, cause, action, reason_code) VALUES ($1, 'tenant_suspended', 'allow', 'x')`, a.f.tenantID)
		return err
	})
	if a13Code(err) != "WR002" {
		t.Fatalf("a non-configurable cause (owner decision 13-18) must be refused, got %v", err)
	}
	// The runtime role in a PLATFORM session, without an executed change of the transaction: WR040.
	err = a.rt.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO withdrawal_restriction_policies (tenant_id, cause, action, reason_code) VALUES ($1, 'licence_suspended', 'allow', 'x')`, a.f.tenantID)
		return err
	})
	if a13Code(err) != "WR040" {
		t.Fatalf("a runtime write outside a governed change must be WR040, got %v", err)
	}
}

// APPEND-ONLY: a policy row is never updated or deleted. Under FORCE RLS no session has an UPDATE / DELETE policy, so the
// statement reaches no row; the guard trigger (WR055) is the second layer and TRUNCATE (statement level) hits it directly.
// Either way the row is unchanged.
func TestLic11_Policy_AppendOnly(t *testing.T) {
	w := newA13(t)
	l11Policy(t, w.owner, w.f.tenantID, CauseLicenceMissing, RestrictionBlock)
	for _, sql := range []string{
		`UPDATE withdrawal_restriction_policies SET action = 'allow' WHERE tenant_id = $1`,
		`DELETE FROM withdrawal_restriction_policies WHERE tenant_id = $1`,
	} {
		for _, p := range []*db.Pool{w.owner, w.rt} {
			err := p.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
				tag, err := tx.Exec(ctx, sql, w.f.tenantID)
				if err == nil && tag.RowsAffected() != 0 {
					return fmt.Errorf("%d rows affected", tag.RowsAffected())
				}
				return err
			})
			if err != nil && a13Code(err) != "WR055" && a13Code(err) != "42501" {
				t.Fatalf("%q: want no row affected or WR055 / 42501, got %v", sql, err)
			}
		}
	}
	err := w.owner.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE withdrawal_restriction_policies`)
		return err
	})
	if a13Code(err) != "WR055" {
		t.Fatalf("TRUNCATE must be WR055, got %v", err)
	}
	var action string
	if err := w.owner.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT action FROM withdrawal_restriction_policies WHERE tenant_id = $1`, w.f.tenantID).Scan(&action)
	}); err != nil || action != string(RestrictionBlock) {
		t.Fatalf("the row must be unchanged, got %q %v", action, err)
	}
}

// LOCK ORDER (ADR 0082; the lockorder harness): a policy write takes the tenant status gate key EXCLUSIVE; a request takes
// it SHARED (H-SEC-11) before every row lock. Both orders serialise without a deadlock, and the request decides on the
// policy committed before it: (1) policy write first -> the request waits, then sees the block; (2) request first -> the
// policy write waits for the request's commit.
func TestLic11_LockOrder_PolicyChangeVsRequest(t *testing.T) {
	ctx := context.Background()
	t.Run("policy_first", func(t *testing.T) {
		w := newA13(t)
		l11SetLicence(t, w.owner, w.f.tenantID, l11Suspended)
		policyTx, err := w.owner.Raw().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer policyTx.Rollback(ctx) //nolint:errcheck
		if _, err := policyTx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		if _, err := policyTx.Exec(ctx, `INSERT INTO withdrawal_restriction_policies (tenant_id, cause, action, reason_code) VALUES ($1, 'licence_suspended', 'block', 'lic11_lo')`, w.f.tenantID); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		finished := make(chan struct{})
		var pid int
		go func() {
			defer close(finished)
			_, err := w.l11RequestWithPID(t, "l11-lo-1", 500, &pid)
			done <- err
		}()
		var policyPID int
		if err := policyTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&policyPID); err != nil {
			t.Fatal(err)
		}
		if !w.l11WaitPID(t, &pid) {
			t.Fatal("the request never started")
		}
		l11PIDMu.Lock()
		reqPID := pid
		l11PIDMu.Unlock()
		if !loWaitBlockedByAnyOf(t, w.owner, reqPID, []int{policyPID}, finished) {
			t.Fatal("the request must wait for the policy writer's tenant key")
		}
		if err := policyTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		err = <-done
		var denied *GateDeniedError
		if !errors.As(err, &denied) || denied.GateCode != GateCodeRestriction || loIsDeadlock(err) {
			t.Fatalf("the request must see the committed block, got %v", err)
		}
	})
	t.Run("request_first", func(t *testing.T) {
		w := newA13(t)
		l11SetLicence(t, w.owner, w.f.tenantID, l11Suspended)
		// The request runs in its own WithTenant transaction (the production shape) and holds it open until released.
		reqReady, reqRelease, reqDone := make(chan int, 1), make(chan struct{}), make(chan error, 1)
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(reqRelease) }) }
		defer release()
		go func() {
			reqDone <- w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
				if _, err := RequestWithdrawal(ctx, tx, baseParams(w.f, "l11-lo-2", 500)); err != nil {
					return err
				}
				var p int
				if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&p); err != nil {
					return err
				}
				reqReady <- p
				<-reqRelease
				return nil
			})
		}()
		var reqPID int
		select {
		case reqPID = <-reqReady:
		case err := <-reqDone:
			t.Fatalf("request (default allow): %v", err)
		case <-time.After(loLockWaitTimeout):
			t.Fatal("the request did not reach its hold")
		}
		done := make(chan error, 1)
		finished := make(chan struct{})
		var pid int
		var mu sync.Mutex
		go func() {
			defer close(finished)
			done <- w.owner.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
				var p int
				if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&p); err != nil {
					return err
				}
				mu.Lock()
				pid = p
				mu.Unlock()
				_, err := tx.Exec(ctx, `INSERT INTO withdrawal_restriction_policies (tenant_id, cause, action, reason_code) VALUES ($1, 'licence_suspended', 'block', 'lic11_lo')`, w.f.tenantID)
				return err
			})
		}()
		deadline := time.Now().Add(loLockWaitTimeout)
		for {
			mu.Lock()
			p := pid
			mu.Unlock()
			if p != 0 && loWaitBlockedByAnyOf(t, w.owner, p, []int{reqPID}, finished) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the policy writer must wait for the in-flight request")
			}
			time.Sleep(10 * time.Millisecond)
		}
		select {
		case <-finished:
			t.Fatal("the policy writer must not complete while the request holds the tenant key SHARED")
		default:
		}
		release()
		if err := <-reqDone; err != nil {
			t.Fatalf("the request commits under the policy it read: %v", err)
		}
		if err := <-done; err != nil || loIsDeadlock(err) {
			t.Fatalf("the policy writer completes after the request, got %v", err)
		}
		assertLedgerBalanced(t, w.owner, w.f.tenantID)
	})
}

// l11RequestWithPID runs a request as the runtime role and publishes its backend pid before calling RequestWithdrawal.
func (w *a13World) l11RequestWithPID(t *testing.T, key string, amount int64, pid *int) (WithdrawalRequest, error) {
	var wr WithdrawalRequest
	var gate *GateDeniedError
	err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		var p int
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&p); err != nil {
			return err
		}
		l11PIDMu.Lock()
		*pid = p
		l11PIDMu.Unlock()
		var e error
		wr, e = RequestWithdrawal(ctx, tx, baseParams(w.f, key, amount))
		if errors.As(e, &gate) {
			return nil
		}
		return e
	})
	if gate != nil {
		return wr, gate
	}
	return wr, err
}

var l11PIDMu sync.Mutex

func (w *a13World) l11WaitPID(t *testing.T, pid *int) bool {
	t.Helper()
	deadline := time.Now().Add(loLockWaitTimeout)
	for time.Now().Before(deadline) {
		l11PIDMu.Lock()
		p := *pid
		l11PIDMu.Unlock()
		if p != 0 {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

//go:build integration

package licensingchange

// c4 (migration 0190): the governed screening policy loosening. Every assertion about what the application can do runs as
// igaming_runtime (the shared-database world); the owner pool only provisions fixtures. TEST FIXTURE VALUES - NOT LEGAL THRESHOLDS: the
// intervals, graces and cooling periods below exist to exercise the mechanism; the platform ships none.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// lw is a licensing change world plus one tenant that operates in one jurisdiction and has a first screening policy version.
type lw struct {
	*world
	tenant, jur uuid.UUID
}

const (
	fixtureInterval int64 = 7200 // TEST FIXTURE VALUE, not a threshold
	fixtureGrace    int64 = 3600 // TEST FIXTURE VALUE, not a threshold
)

func newLW(t *testing.T) *lw {
	t.Helper()
	return lwOn(newWorld(t))
}

// newLWNoOfficers is a world in which NOBODY holds an aml_officer grant (the shape of three plain platform admins).
func newLWNoOfficers(t *testing.T) *lw {
	t.Helper()
	return lwBuild(newWorld(t), false)
}

// lwOn builds the world with an officer grant (platform scope, one hour) for every fixture platform account, which is what the
// governed loosening now requires of the requester, every counted approver and the executor (security HIGH-1).
func lwOn(w *world) *lw { return lwBuild(w, true) }

func lwBuild(w *world, officers bool) *lw {
	w.t.Helper()
	l := &lw{world: w, tenant: w.tenant("under_platform_licence"), jur: w.jurisdiction()}
	if officers {
		for _, a := range []Actor{w.req, w.ap1, w.ap2, w.ap3, w.reqTwin, w.ap1Twin} {
			if a.StaffID == uuid.Nil {
				continue
			}
			amlfix.GrantOfficerAs(w.t, w.owner, l.tenant, a.StaffID, "platform")
		}
	}
	l.ownerTenant(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenant_jurisdiction_configs (tenant_id, jurisdiction_id, kyc_ruleset_id) VALUES ($1, $2, 'k')`, l.tenant, l.jur)
		return err
	})
	l.seedPolicy(l.tenant, l.jur, "{pep,sanctions}", fixtureInterval, fixtureGrace)
	return l
}

func (l *lw) ownerTenant(fn func(ctx context.Context, tx pgx.Tx) error) {
	l.t.Helper()
	l.ownerTenantOf(l.tenant, fn)
}

func (l *lw) ownerTenantOf(tenant uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) {
	l.t.Helper()
	if err := l.owner.WithTenant(context.Background(), tenant, fn); err != nil {
		l.t.Fatalf("owner tenant session: %v", err)
	}
}

// seedPolicy writes a screening policy version as the owner (the guard is disabled for the fixture only: the version-authoring paths have
// their own tests in internal/aml).
func (l *lw) seedPolicy(tenant, jur uuid.UUID, lists string, resc, grace int64) {
	l.t.Helper()
	l.ownerTenantOf(tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE aml_screening_policies DISABLE TRIGGER aml_screening_policies_10_guard`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO aml_screening_policies (tenant_id, licensing_jurisdiction_id, lists, rescreen_interval_seconds, backfill_grace_seconds,
				effective_from, legal_review_reference, reason_code, created_by, created_by_person_id, created_by_scope)
			VALUES ($1, $2, $3::text[], $4, $5, now() - interval '1 hour', 'TEST FIXTURE REF', 'fixture', $6, $7, 'platform_acting')`,
			tenant, jur, lists, resc, grace, l.ops.Requester, l.ops.Persons[0]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE aml_screening_policies ENABLE TRIGGER aml_screening_policies_10_guard`)
		return err
	})
}

// seedTrigger writes a trigger policy version as the owner (fixture; the guard is disabled for it only).
func (l *lw) seedTrigger(gate bool, alert int64) {
	l.t.Helper()
	l.ownerTenant(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE aml_screening_trigger_policies DISABLE TRIGGER aml_screening_trigger_policies_10_guard`); err != nil {
			return err
		}
		var alertArg any
		events := "{registration}"
		if gate {
			alertArg, events = alert, "{registration,scheduled}"
		}
		if _, err := tx.Exec(ctx, `INSERT INTO aml_screening_trigger_policies (tenant_id, licensing_jurisdiction_id, events, gate_withdrawal, gate_alert_after_seconds,
				effective_from, legal_review_reference, reason_code, created_by, created_by_person_id, created_by_scope)
			VALUES ($1, $2, $3::text[], $4, $5, now() - interval '1 hour', 'TEST FIXTURE REF', 'fixture', $6, $7, 'platform_acting')`,
			l.tenant, l.jur, events, gate, alertArg, l.ops.Requester, l.ops.Persons[0]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE aml_screening_trigger_policies ENABLE TRIGGER aml_screening_trigger_policies_10_guard`)
		return err
	})
}

type policyRow struct {
	ID         uuid.UUID
	Lists      []string
	Rescreen   int64
	Grace      int64
	Effective  time.Time
	Scope      string
	Request    *uuid.UUID
	LegalRef   string
	ReasonCode string
	CreatedBy  uuid.UUID
}

func (l *lw) policies() []policyRow {
	l.t.Helper()
	var out []policyRow
	l.ownerTenant(func(ctx context.Context, tx pgx.Tx) error {
		out = nil
		rows, err := tx.Query(ctx, `SELECT id, lists, rescreen_interval_seconds, backfill_grace_seconds, effective_from, created_by_scope, change_request_id,
				legal_review_reference, reason_code, created_by
			FROM aml_screening_policies WHERE tenant_id = $1 AND licensing_jurisdiction_id = $2 ORDER BY created_at, id`, l.tenant, l.jur)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r policyRow
			if err := rows.Scan(&r.ID, &r.Lists, &r.Rescreen, &r.Grace, &r.Effective, &r.Scope, &r.Request, &r.LegalRef, &r.ReasonCode, &r.CreatedBy); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out
}

type settingsRow struct {
	Enabled bool
	Cooling *int64
	Request uuid.UUID
}

func (l *lw) settings(tenant uuid.UUID) []settingsRow {
	l.t.Helper()
	var out []settingsRow
	l.ownerTenantOf(tenant, func(ctx context.Context, tx pgx.Tx) error {
		out = nil
		rows, err := tx.Query(ctx, `SELECT enabled, cooling_seconds, change_request_id FROM aml_screening_loosening_settings WHERE tenant_id = $1 ORDER BY created_at, id`, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r settingsRow
			if err := rows.Scan(&r.Enabled, &r.Cooling, &r.Request); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out
}

func (l *lw) loosenPayload(resc, grace int64, lists ...string) ScreeningPolicyLoosenPayload {
	if len(lists) == 0 {
		lists = []string{"pep", "sanctions"}
	}
	return ScreeningPolicyLoosenPayload{TenantID: l.tenant, JurisdictionID: l.jur, Lists: lists, RescreenIntervalSeconds: resc, BackfillGraceSeconds: grace,
		LegalReviewReference: "TEST FIXTURE LEGAL REF", EvidenceReference: "EVID-" + uuid.NewString()[:8]}
}

func (l *lw) cfgPayload(tenant uuid.UUID, enabled bool, cooling int64) ScreeningLooseningConfigurePayload {
	p := ScreeningLooseningConfigurePayload{TenantID: tenant, Enabled: enabled, EvidenceReference: "EVID-" + uuid.NewString()[:8]}
	if enabled {
		p.CoolingSeconds = &cooling
	}
	return p
}

// enable configures the capability for the tenant with two approvals (widening).
func (l *lw) enable(tenant uuid.UUID, cooling int64) {
	l.t.Helper()
	l.widen(KindAMLScreeningLooseningConfigure, l.cfgPayload(tenant, true, cooling), "loosening_enable_decision")
}

func (l *lw) request(kind Kind, p any) (*Result, error) {
	return l.svc.Request(context.Background(), l.req, RequestInput{Kind: kind, Payload: payload(l.t, p), ReasonCode: "screening_loosening_decision"})
}

func hintOf(err error) string {
	var g *GuardError
	if errors.As(err, &g) {
		return g.Hint
	}
	return ""
}

func wantHint(t *testing.T, err error, hint, what string) {
	t.Helper()
	if GuardCode(err) != "LC050" || hintOf(err) != hint {
		t.Fatalf("%s: want LC050 / %s, got %v (code %q hint %q)", what, hint, err, GuardCode(err), hintOf(err))
	}
}

func (l *lw) requestCount(kind Kind) int {
	return l.ownerInt(`SELECT count(*) FROM licensing_change_requests WHERE kind = $1 AND payload ->> 'tenant_id' = $2`, string(kind), l.tenant.String())
}

func (l *lw) auditCount(action, target string) int {
	return l.ownerInt(`SELECT count(*) FROM audit_log WHERE action = $1 AND target_id = $2`, action, target)
}

// age moves time for a request: the owner path with the append-only guard disabled for the one statement (a fixture clock, never a
// production path).
func (l *lw) age(table, guard, set string, args ...any) {
	l.t.Helper()
	if err := l.owner.WithPlatformAdmin(context.Background(), l.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		for _, stmt := range []string{`ALTER TABLE ` + table + ` DISABLE TRIGGER ` + guard, `ALTER TABLE ` + table + ` NO FORCE ROW LEVEL SECURITY`} {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, set, args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errors.New("the fixture clock moved no row")
		}
		for _, stmt := range []string{`ALTER TABLE ` + table + ` FORCE ROW LEVEL SECURITY`, `ALTER TABLE ` + table + ` ENABLE TRIGGER ` + guard} {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		l.t.Fatalf("age: %v", err)
	}
}

// ---- inert by default ------------------------------------------------------------------------------------------------------

func TestC4_Unconfigured_LooseningIsRefused_AndNothingIsWritten(t *testing.T) {
	l := newLW(t)
	before := l.policies()
	_, err := l.request(KindAMLScreeningPolicyLoosen, l.loosenPayload(14400, fixtureGrace))
	wantHint(t, err, "loosening_not_configured", "no capability row")
	if n := l.requestCount(KindAMLScreeningPolicyLoosen); n != 0 {
		t.Fatalf("a refused request leaves no row, found %d", n)
	}
	if len(l.settings(l.tenant)) != 0 {
		t.Fatal("no capability row ships")
	}
	if got := l.policies(); len(got) != len(before) {
		t.Fatalf("policy versions changed: %d -> %d", len(before), len(got))
	}
	// Disabled is refused exactly like unconfigured (with its own closed code).
	l.enable(l.tenant, 0)
	disabled := l.mustRequest(l.req, KindAMLScreeningLooseningConfigure, l.cfgPayload(l.tenant, false, 0), "loosening_disable_decision")
	if !disabled.Executed || disabled.Request.Widening || disabled.Request.RequiredApprovals != 0 {
		t.Fatalf("disabling is a single-actor narrowing: %+v", disabled.Request)
	}
	_, err = l.request(KindAMLScreeningPolicyLoosen, l.loosenPayload(14400, fixtureGrace))
	wantHint(t, err, "loosening_disabled", "capability disabled")
	if rows := l.settings(l.tenant); len(rows) != 2 || rows[1].Enabled || rows[1].Cooling != nil {
		t.Fatalf("settings versions: %+v", rows)
	}
	// Another tenant is unaffected by this tenant's configuration (per-tenant capability).
	other := l.world.tenant("under_platform_licence")
	p := l.loosenPayload(14400, fixtureGrace)
	p.TenantID = other
	_, err = l.request(KindAMLScreeningPolicyLoosen, p)
	wantHint(t, err, "loosening_not_configured", "wrong tenant: another tenant has no capability")
}

// ---- the governed path end to end --------------------------------------------------------------------------------------------

func TestC4_ConfigureThenLoosen_EndToEnd_TwoPersons_SameTransaction_Audited(t *testing.T) {
	l := newLW(t)
	// Enabling is WIDENING: a single approver does not execute; the requester cannot approve; the same Person cannot approve twice.
	r := l.mustRequest(l.req, KindAMLScreeningLooseningConfigure, l.cfgPayload(l.tenant, true, 0), "loosening_enable_decision")
	if r.Executed || !r.Request.Widening || r.Request.RequiredApprovals != 2 || r.Request.SubjectTenantID != nil {
		t.Fatalf("enable must be filed as widening, 2 approvals, no tenant projection: %+v", r.Request)
	}
	if _, err := l.approve(l.req, r.Request.ID); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("the requester approving: %v", err)
	}
	if _, err := l.approve(l.reqTwin, r.Request.ID); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("another account of the requester's Person approving: %v", err)
	}
	first := l.mustApprove(l.ap1, r.Request.ID)
	if first.Executed || len(l.settings(l.tenant)) != 0 {
		t.Fatal("one approval must not execute")
	}
	if _, err := l.approve(l.ap1, r.Request.ID); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("a duplicate approval by the same account: %v", err)
	}
	if _, err := l.approve(l.ap1Twin, r.Request.ID); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("a duplicate approval by another account of the same Person: %v", err)
	}
	done := l.mustApprove(l.ap2, r.Request.ID)
	if !done.Executed || done.Outcome["settings_id"] == nil {
		t.Fatalf("the second distinct Person executes: %+v", done)
	}
	rows := l.settings(l.tenant)
	if len(rows) != 1 || !rows[0].Enabled || rows[0].Cooling == nil || *rows[0].Cooling != 0 || rows[0].Request != r.Request.ID {
		t.Fatalf("capability row: %+v", rows)
	}
	for _, ev := range []string{"licensing_change.requested", "licensing_change.approved", "licensing_change.executed", "aml.screening_loosening_configured"} {
		if l.auditCount(ev, r.Request.ID.String()) == 0 {
			t.Fatalf("no audit row %s", ev)
		}
	}

	// Loosen: longer interval and grace, a list kept. Two Persons; the policy version is written in the executing transaction.
	before := l.policies()
	p := l.loosenPayload(14400, 7200)
	p.EffectiveOffsetSeconds = 600
	lr := l.mustRequest(l.req, KindAMLScreeningPolicyLoosen, p, "screening_loosening_decision").Request
	if !lr.Widening || lr.RequiredApprovals != 2 {
		t.Fatalf("loosening is always widening with 2 approvals: %+v", lr)
	}
	if _, err := l.request(KindAMLScreeningPolicyLoosen, p); !errors.Is(err, ErrDuplicatePending) {
		t.Fatalf("an identical open request: %v", err)
	}
	l.mustApprove(l.ap1, lr.ID)
	if got := l.policies(); len(got) != len(before) {
		t.Fatal("no policy version before the second approval")
	}
	exec := l.mustApprove(l.ap2, lr.ID)
	if !exec.Executed {
		t.Fatalf("not executed: %+v", exec)
	}
	after := l.policies()
	if len(after) != len(before)+1 {
		t.Fatalf("exactly one new version, got %d -> %d", len(before), len(after))
	}
	for i := range before { // append-only: the earlier versions are untouched
		if before[i].ID != after[i].ID || before[i].Rescreen != after[i].Rescreen || before[i].Grace != after[i].Grace {
			t.Fatalf("an earlier version changed: %+v -> %+v", before[i], after[i])
		}
	}
	nv := after[len(after)-1]
	if nv.Scope != "platform" || nv.Request == nil || *nv.Request != lr.ID || nv.Rescreen != 14400 || nv.Grace != 7200 ||
		strings.Join(nv.Lists, ",") != "pep,sanctions" || nv.LegalRef != "TEST FIXTURE LEGAL REF" || nv.ReasonCode != "governed_loosening" || nv.CreatedBy != l.ap2.StaffID {
		t.Fatalf("new version: %+v", nv)
	}
	// effective_from = execution time + the offset: inside [now, now + 30 d].
	if d := time.Until(nv.Effective); d < 9*time.Minute || d > 11*time.Minute {
		t.Fatalf("effective_from should be about 10 minutes ahead, got %s", d)
	}
	for _, ev := range []string{"licensing_change.requested", "licensing_change.approved", "licensing_change.executed", "aml.screening_policy_loosened"} {
		if l.auditCount(ev, lr.ID.String()) == 0 {
			t.Fatalf("no audit row %s", ev)
		}
	}
	// The audit row carries before and after.
	var md []byte
	if err := l.owner.WithPlatformAdmin(context.Background(), l.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata FROM audit_log WHERE action = 'aml.screening_policy_loosened' AND target_id = $1`, lr.ID.String()).Scan(&md)
	}); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(md, &m)
	out, _ := m["outcome"].(map[string]any)
	if out["previous_rescreen_interval_seconds"] != float64(fixtureInterval) || out["rescreen_interval_seconds"] != float64(14400) {
		t.Fatalf("audit before/after: %s", md)
	}
	// A request of the executed decision cannot be approved or executed again.
	if _, err := l.approve(l.ap3, lr.ID); !errors.Is(err, ErrNotPending) {
		t.Fatalf("approving an executed request: %v", err)
	}
}

// A request that is not a loosening is refused: the existing fast path (the screening policy route) stays the way to tighten.
func TestC4_NotALoosening_IsRefused_TightenStaysOnTheFastPath(t *testing.T) {
	l := newLW(t)
	l.enable(l.tenant, 0)
	_, err := l.request(KindAMLScreeningPolicyLoosen, l.loosenPayload(fixtureInterval, fixtureGrace))
	wantHint(t, err, "not_a_loosening", "identical to the latest version")
	_, err = l.request(KindAMLScreeningPolicyLoosen, l.loosenPayload(3600*2-1000, fixtureGrace-1))
	wantHint(t, err, "not_a_loosening", "strictly tighter")
	// No policy to loosen: a jurisdiction with no version.
	j2 := l.jurisdiction()
	l.ownerTenant(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenant_jurisdiction_configs (tenant_id, jurisdiction_id, kyc_ruleset_id) VALUES ($1, $2, 'k')`, l.tenant, j2)
		return err
	})
	p := l.loosenPayload(14400, fixtureGrace)
	p.JurisdictionID = j2
	_, err = l.request(KindAMLScreeningPolicyLoosen, p)
	wantHint(t, err, "no_screening_policy_to_loosen", "first version is not a loosening")
	// A jurisdiction the tenant does not operate in.
	p.JurisdictionID = l.jurisdiction()
	_, err = l.request(KindAMLScreeningPolicyLoosen, p)
	wantHint(t, err, "tenant_not_in_jurisdiction", "tenant not configured in the jurisdiction")
	if n := l.requestCount(KindAMLScreeningPolicyLoosen); n != 0 {
		t.Fatalf("refusals leave no request: %d", n)
	}
	// Dropping a list is a loosening on its own, except the sanctions list, which this path never removes (a safe default).
	_, err = l.request(KindAMLScreeningPolicyLoosen, l.loosenPayload(fixtureInterval, fixtureGrace, "pep"))
	wantHint(t, err, "sanctions_list_removal_refused", "removing the sanctions list")
	_, err = l.request(KindAMLScreeningPolicyLoosen, l.loosenPayload(14400, fixtureGrace, "pep"))
	wantHint(t, err, "sanctions_list_removal_refused", "removing the sanctions list while loosening the interval")
	r := l.mustRequest(l.req, KindAMLScreeningPolicyLoosen, l.loosenPayload(fixtureInterval, fixtureGrace, "sanctions"), "screening_loosening_decision")
	if !r.Request.Widening {
		t.Fatal("removing the pep list loosens")
	}
	// A mix of loosening and tightening is refused: every dimension must be looser or equal.
	_, err = l.request(KindAMLScreeningPolicyLoosen, l.loosenPayload(14400, fixtureGrace/2))
	wantHint(t, err, "loosening_mixes_tightening", "longer interval but shorter grace")
	l.seedPolicy(l.tenant, l.jur, "{pep}", fixtureInterval, fixtureGrace)
	_, err = l.request(KindAMLScreeningPolicyLoosen, l.loosenPayload(14400, fixtureGrace)) // adds the sanctions list back while loosening
	wantHint(t, err, "loosening_mixes_tightening", "a list added while the interval is lengthened")
}

// ---- floors, window, caps ----------------------------------------------------------------------------------------------------

func TestC4_Loosening_NeverGoesBelowTheFloors_AndRespectsTheEffectiveWindow(t *testing.T) {
	l := newLW(t)
	l.enable(l.tenant, 0)
	// A longer grace with an interval under the engineering floor: refused (the floor applies to every version).
	_, err := l.request(KindAMLScreeningPolicyLoosen, l.loosenPayload(3599, fixtureGrace*2))
	wantHint(t, err, "rescreen_interval_below_floor", "below 3600 s")
	_, err = l.request(KindAMLScreeningPolicyLoosen, l.loosenPayload(1, fixtureGrace*2))
	wantHint(t, err, "rescreen_interval_below_floor", "1 s")
	// A gating trigger policy with an alert threshold: the interval must be at least twice it.
	l.seedTrigger(true, 7200)
	_, err = l.request(KindAMLScreeningPolicyLoosen, l.loosenPayload(10000, fixtureGrace*4))
	wantHint(t, err, "rescreen_interval_below_alert_threshold", "below twice the gate alert threshold")
	// Window and caps: Go refuses what the SQL refuses (the SQL is tested directly below).
	bad := []ScreeningPolicyLoosenPayload{
		func() ScreeningPolicyLoosenPayload {
			p := l.loosenPayload(14400, 7200)
			p.EffectiveOffsetSeconds = -1
			return p
		}(),
		func() ScreeningPolicyLoosenPayload {
			p := l.loosenPayload(14400, 7200)
			p.EffectiveOffsetSeconds = 2592001
			return p
		}(),
		l.loosenPayload(31536001, 0), l.loosenPayload(14400, 31536001), l.loosenPayload(0, 0),
	}
	for i, p := range bad {
		if _, err := l.request(KindAMLScreeningPolicyLoosen, p); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("bad payload %d: %v", i, err)
		}
	}
	// The SQL applies the same bounds when Go is bypassed (a raw classify call over a hand-written payload).
	classify := func(body string) error {
		return l.rt.WithPlatformAdmin(context.Background(), l.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT widening FROM licensing_change_classify('aml_screening_policy_loosen', $1::jsonb)`, body)
			return err
		})
	}
	body := func(resc, grace, off string, lists string) string {
		return `{"tenant_id":"` + l.tenant.String() + `","jurisdiction_id":"` + l.jur.String() + `","lists":` + lists + `,"rescreen_interval_seconds":` + resc +
			`,"backfill_grace_seconds":` + grace + `,"effective_offset_seconds":` + off + `,"legal_review_reference":"REF","evidence_reference":"EVID"}`
	}
	l.seedTrigger(false, 0) // the latest trigger version no longer gates: the alert threshold is the LAST GATING version's, which remains
	if pgCode(classify(body("31536001", "0", "0", `["pep"]`))) != "LC010" || pgCode(classify(body("14400", "31536001", "0", `["pep"]`))) != "LC010" {
		t.Fatal("the SQL caps the interval and the grace at one year")
	}
	if err := classify(body("14400", "7200", "2592001", `["pep","sanctions"]`)); pgCode(err) != "LC050" || !strings.Contains(err.Error(), "between now and 30 days") {
		t.Fatalf("the SQL bounds the offset: %v", err)
	}
	for _, lists := range []string{`["sanctions","pep"]`, `["pep","pep"]`, `[]`, `["watch"]`, `["pep","sanctions","adverse"]`} {
		if pgCode(classify(body("14400", "7200", "0", lists))) != "LC010" {
			t.Fatalf("lists %s must be refused as invalid (canonical form only)", lists)
		}
	}
	if pgCode(classify(`{"tenant_id":"`+l.tenant.String()+`"}`)) != "LC010" {
		t.Fatal("a payload with missing keys is invalid")
	}
	// Window: offset 0 is "at execution", the maximum is 30 days after it.
	p := l.loosenPayload(20000, fixtureGrace)
	p.EffectiveOffsetSeconds = 2592000
	l.widen(KindAMLScreeningPolicyLoosen, p, "screening_loosening_decision")
	pol := l.policies()
	if d := time.Until(pol[len(pol)-1].Effective); d < 29*24*time.Hour || d > 30*24*time.Hour+time.Minute {
		t.Fatalf("a 30-day offset takes effect about 30 days ahead: %s", d)
	}
}

// ---- cooling period ----------------------------------------------------------------------------------------------------------

func TestC4_CoolingPeriod_IsEnforcedByTheDatabase_FromFiling(t *testing.T) {
	l := newLW(t)
	l.enable(l.tenant, 3)
	before := l.policies()
	r := l.mustRequest(l.req, KindAMLScreeningPolicyLoosen, l.loosenPayload(14400, 7200), "screening_loosening_decision").Request
	l.mustApprove(l.ap1, r.ID)
	_, err := l.approve(l.ap2, r.ID)
	wantHint(t, err, "loosening_cooling_period", "approval inside the cooling period")
	if got := l.policies(); len(got) != len(before) {
		t.Fatal("nothing is written inside the cooling period")
	}
	if s := l.status(r.ID); s != StatusRequested {
		t.Fatalf("the refused execution rolls everything back, status %s", s)
	}
	if n := l.ownerInt(`SELECT count(*) FROM licensing_change_approvals WHERE request_id = $1`, r.ID); n != 1 {
		t.Fatalf("only the first approval stands, found %d", n)
	}
	time.Sleep(3200 * time.Millisecond)
	done := l.mustApprove(l.ap2, r.ID)
	if !done.Executed || len(l.policies()) != len(before)+1 {
		t.Fatalf("after the cooling period the second approval executes: %+v", done)
	}
	// A longer cooling period is NARROWING (single actor); a shorter one is WIDENING (two Persons).
	longer := l.mustRequest(l.req, KindAMLScreeningLooseningConfigure, l.cfgPayload(l.tenant, true, 100), "loosening_cooling_longer")
	if !longer.Executed || longer.Request.Widening {
		t.Fatalf("a longer cooling period narrows: %+v", longer.Request)
	}
	shorter := l.mustRequest(l.req, KindAMLScreeningLooseningConfigure, l.cfgPayload(l.tenant, true, 50), "loosening_cooling_shorter")
	if shorter.Executed || !shorter.Request.Widening || shorter.Request.RequiredApprovals != 2 {
		t.Fatalf("a shorter cooling period widens: %+v", shorter.Request)
	}
	// An unchanged capability is refused; the structural bound holds.
	if _, err := l.request(KindAMLScreeningLooseningConfigure, l.cfgPayload(l.tenant, true, 100)); GuardCode(err) != "LC010" {
		t.Fatalf("unchanged capability: %v", err)
	}
	over := l.cfgPayload(l.tenant, true, MaxLooseningCoolingSeconds+1)
	if _, err := l.request(KindAMLScreeningLooseningConfigure, over); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("a cooling period beyond the request lifetime cannot be satisfied: %v", err)
	}
}

// ---- freshness, expiry, concurrency ------------------------------------------------------------------------------------------

func TestC4_StaleApprovals_AndExpiredRequests_AreRefused(t *testing.T) {
	l := newLW(t)
	l.enable(l.tenant, 0)
	before := l.policies()

	// An approval older than 24 h does not count: the Go count refuses; with the Go window widened the DATABASE refuses (LC020).
	r := l.mustRequest(l.req, KindAMLScreeningPolicyLoosen, l.loosenPayload(14400, 7200), "screening_loosening_decision").Request
	l.mustApprove(l.ap1, r.ID)
	l.age("licensing_change_approvals", "licensing_change_approvals_guard", `UPDATE licensing_change_approvals SET decided_at = decided_at - interval '25 hours' WHERE request_id = $1`, r.ID)
	if _, err := l.approve(l.ap2, r.ID); !errors.Is(err, ErrApprovalsStale) {
		t.Fatalf("a >24 h old approval: %v", err)
	}
	lax := l.service(Config{ApprovalMaxAge: 1000 * time.Hour})
	if _, err := lax.Approve(context.Background(), l.ap2, r.ID, "reviewed_ok"); GuardCode(err) != "LC020" {
		t.Fatalf("the database refuses stale approvals even when Go does not: %v", err)
	}
	if len(l.policies()) != len(before) {
		t.Fatal("no policy version from stale approvals")
	}

	// An expired request (72 h) can no longer be approved.
	e := l.mustRequest(l.req, KindAMLScreeningPolicyLoosen, l.loosenPayload(20000, fixtureGrace), "screening_loosening_decision").Request
	l.age("licensing_change_requests", "licensing_change_requests_guard", `UPDATE licensing_change_requests SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, e.ID)
	if _, err := l.approve(l.ap1, e.ID); !errors.Is(err, ErrNotPending) {
		t.Fatalf("an expired request: %v", err)
	}
	if len(l.policies()) != len(before) {
		t.Fatal("no policy version from an expired request")
	}
}

func TestC4_ConcurrentApprovers_ExecuteExactlyOnce(t *testing.T) {
	l := newLW(t)
	l.enable(l.tenant, 0)
	before := len(l.policies())
	r := l.mustRequest(l.req, KindAMLScreeningPolicyLoosen, l.loosenPayload(14400, 7200), "screening_loosening_decision").Request
	var wg sync.WaitGroup
	type out struct {
		res *Result
		err error
	}
	results := make([]out, 3)
	for i, a := range []Actor{l.ap1, l.ap2, l.ap3} {
		wg.Add(1)
		go func(i int, a Actor) {
			defer wg.Done()
			res, err := l.approve(a, r.ID)
			results[i] = out{res, err}
		}(i, a)
	}
	wg.Wait()
	executed := 0
	for _, o := range results {
		switch {
		case o.err == nil && o.res.Executed:
			executed++
		case o.err == nil:
		case errors.Is(o.err, ErrNotPending), errors.Is(o.err, ErrRetryableLockTimeout):
		default:
			t.Fatalf("unexpected concurrent outcome: %v", o.err)
		}
	}
	if executed != 1 || len(l.policies()) != before+1 {
		t.Fatalf("exactly one execution and one new version: executed=%d versions %d -> %d", executed, before, len(l.policies()))
	}
	// The same Person from two accounts, concurrently: only one approval is recorded.
	r2 := l.mustRequest(l.req, KindAMLScreeningPolicyLoosen, l.loosenPayload(30000, 7200), "screening_loosening_decision").Request
	var errs [2]error
	wg.Add(2)
	for i, a := range []Actor{l.ap1, l.ap1Twin} {
		go func(i int, a Actor) {
			defer wg.Done()
			_, errs[i] = l.approve(a, r2.ID)
		}(i, a)
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("exactly one of the two accounts of one Person records an approval: %v / %v", errs[0], errs[1])
	}
	if n := l.ownerInt(`SELECT count(*) FROM licensing_change_approvals WHERE request_id = $1`, r2.ID); n != 1 {
		t.Fatalf("one Person, one approval: %d", n)
	}
}

// ---- tenant staff, RLS, direct SQL -------------------------------------------------------------------------------------------

func TestC4_TenantStaff_CannotFileSeeApproveOrWriteAnything(t *testing.T) {
	l := newLW(t)
	l.enable(l.tenant, 0)
	staff := l.tenantStaff(l.tenant, string(auth.RoleTenantAdmin))
	for _, k := range []struct {
		kind Kind
		p    any
	}{{KindAMLScreeningPolicyLoosen, l.loosenPayload(14400, 7200)}, {KindAMLScreeningLooseningConfigure, l.cfgPayload(l.tenant, false, 0)}} {
		_, err := l.svc.Request(context.Background(), staff, RequestInput{Kind: k.kind, Payload: payload(t, k.p), ReasonCode: "tenant_attempt"})
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("tenant staff filing %s: %v", k.kind, err)
		}
	}
	// A platform request is invisible to tenant staff (no subject tenant projection) and cannot be approved by them.
	r := l.mustRequest(l.req, KindAMLScreeningPolicyLoosen, l.loosenPayload(14400, 7200), "screening_loosening_decision").Request
	if _, err := l.svc.Get(context.Background(), staff, r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant staff reading the request: %v", err)
	}
	list, err := l.svc.List(context.Background(), staff, ListFilter{Kind: KindAMLScreeningPolicyLoosen})
	if err != nil || len(list) != 0 {
		t.Fatalf("tenant staff listing: %v %v", list, err)
	}
	if _, err := l.svc.Approve(context.Background(), staff, r.ID, "reviewed_ok"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("tenant staff approving: %v", err)
	}
	// Direct SQL as the tenant principal and as the runtime role: no privilege on the tables, no file of the kind, no state read.
	tenantSQL := func(sql string, args ...any) error {
		return l.rt.WithPrincipalScope(context.Background(), l.tenant, staff.StaffID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, args...)
			return err
		})
	}
	for _, sql := range []string{`SELECT count(*) FROM aml_screening_loosening_settings`, `INSERT INTO aml_screening_loosening_settings (tenant_id, enabled, cooling_seconds, evidence_reference, reason_code, change_request_id, created_by, created_by_person_id)
		VALUES ('` + l.tenant.String() + `', true, 0, 'E', 'x', '` + r.ID.String() + `', '` + staff.StaffID.String() + `', '` + uuid.NewString() + `')`,
		`UPDATE aml_screening_policies SET rescreen_interval_seconds = 999999`, `INSERT INTO aml_screening_policies (tenant_id) VALUES ('` + l.tenant.String() + `')`} {
		if pgCode(tenantSQL(sql)) != "42501" {
			t.Fatalf("tenant principal: %q must be a permission error, got %v", sql, tenantSQL(sql))
		}
	}
	if err := tenantSQL(`SELECT screening_loosening_state($1)`, l.tenant); pgCode(err) != "LC002" {
		t.Fatalf("a tenant session reading the capability: %v", err)
	}
	if err := tenantSQL(`SELECT screening_policy_loosen_assert($1::jsonb, NULL)`, `{}`); pgCode(err) != "LC002" {
		t.Fatalf("a tenant session calling the validator: %v", err)
	}
	if err := tenantSQL(`SELECT screening_policy_loosen_apply($1)`, r.ID); pgCode(err) != "LC002" && pgCode(err) != "LC030" {
		t.Fatalf("a tenant session calling the executor: %v", err)
	}
	if err := tenantSQL(`INSERT INTO licensing_change_requests (kind, payload, payload_hash, reason_code) VALUES ('aml_screening_policy_loosen', '{}'::jsonb, repeat('0', 64), 'x')`); pgCode(err) != "LC002" {
		t.Fatalf("a tenant session filing the kind at the database: %v", err)
	}
	// The runtime role has no privilege on either table, with or without a platform session.
	for _, sql := range []string{`SELECT 1 FROM aml_screening_loosening_settings`, `INSERT INTO aml_screening_policies (tenant_id) VALUES (gen_random_uuid())`, `UPDATE aml_screening_policies SET reason_code = 'x'`, `DELETE FROM aml_screening_loosening_settings`} {
		err := l.rtPlatform(l.ops.Requester, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, sql); return err })
		if pgCode(err) != "42501" {
			t.Fatalf("runtime %q: want 42501, got %v", sql, err)
		}
	}
	// The cross-tenant case: tenant B's session reads nothing of tenant A, and the owner policy needs the session tenant.
	other := l.world.tenant("under_platform_licence")
	var visible int
	if err := l.owner.WithTenant(context.Background(), other, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM aml_screening_loosening_settings`).Scan(&visible)
	}); err != nil || visible != 0 {
		t.Fatalf("RLS: tenant B sees %d capability rows of tenant A: %v", visible, err)
	}
	var visiblePol int
	if err := l.owner.WithTenant(context.Background(), other, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM aml_screening_policies WHERE tenant_id = $1`, l.tenant).Scan(&visiblePol)
	}); err != nil || visiblePol != 0 {
		t.Fatalf("RLS: tenant B sees %d policy versions of tenant A: %v", visiblePol, err)
	}
}

// The executors and the table guards are not a back door: without an executed request of THIS transaction nothing is written.
func TestC4_ExecutorsAndGuards_NeedAnExecutedRequestOfThisTransaction(t *testing.T) {
	l := newLW(t)
	l.enable(l.tenant, 0)
	open := l.mustRequest(l.req, KindAMLScreeningPolicyLoosen, l.loosenPayload(14400, 7200), "screening_loosening_decision").Request
	before := len(l.policies())
	// The executor for a request that is still open, and for an unknown id.
	for _, id := range []uuid.UUID{open.ID, uuid.New()} {
		err := l.rtPlatform(l.ap1.StaffID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT screening_policy_loosen_apply($1)`, id)
			return err
		})
		if pgCode(err) != "LC040" {
			t.Fatalf("executor for %s: want LC040, got %v", id, err)
		}
	}
	// The executor for an EXECUTED request of an earlier transaction (a replay).
	done := l.widen(KindAMLScreeningPolicyLoosen, l.loosenPayload(30000, 7200), "screening_loosening_decision")
	after := len(l.policies())
	// By the very actor that executed it (ap2 gave the final approval), so only the "this transaction" rule can refuse it.
	err := l.rtPlatform(l.ap2.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT screening_policy_loosen_apply($1)`, done.Request.ID)
		return err
	})
	if pgCode(err) != "LC040" || len(l.policies()) != after || after != before+1 {
		t.Fatalf("a replayed executor call: %v (versions %d -> %d)", err, before, after)
	}
	// The wrong kind: a configure request cannot drive the policy executor and vice versa.
	cfg := l.mustRequest(l.req, KindAMLScreeningLooseningConfigure, l.cfgPayload(l.tenant, true, 10), "loosening_cooling_shorter").Request
	err = l.rtPlatform(l.ap1.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT screening_policy_loosen_apply($1)`, cfg.ID)
		return err
	})
	if pgCode(err) != "LC040" {
		t.Fatalf("wrong kind: %v", err)
	}
	// The table guards behind the executors (owner path, direct INSERT with no request): LC040.
	var forged error
	_ = l.owner.WithTenant(context.Background(), l.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, forged = tx.Exec(ctx, `INSERT INTO aml_screening_policies (tenant_id, licensing_jurisdiction_id, lists, rescreen_interval_seconds, backfill_grace_seconds, effective_from,
				legal_review_reference, reason_code, created_by, created_by_person_id, created_by_scope, change_request_id)
			VALUES ($1, $2, '{pep}', 99999, 0, now(), 'R', 'x', $3, $4, 'platform', $5)`, l.tenant, l.jur, l.ap1.StaffID, l.ops.Persons[1], done.Request.ID)
		return errors.New("rollback")
	})
	if pgCode(forged) != "LC040" {
		t.Fatalf("a platform-authored version without an executing request: %v", forged)
	}
	var forgedCfg error
	_ = l.owner.WithTenant(context.Background(), l.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, forgedCfg = tx.Exec(ctx, `INSERT INTO aml_screening_loosening_settings (tenant_id, enabled, cooling_seconds, evidence_reference, reason_code, change_request_id, created_by, created_by_person_id)
			VALUES ($1, true, 0, 'E', 'x', $2, $3, $4)`, l.tenant, cfg.ID, l.ap1.StaffID, l.ops.Persons[1])
		return errors.New("rollback")
	})
	if pgCode(forgedCfg) != "LC040" {
		t.Fatalf("a capability row without an executing request of this transaction: %v", forgedCfg)
	}
	if n := l.ownerInt(`SELECT count(*) FROM aml_screening_loosening_settings WHERE reason_code = 'x'`); n != 0 {
		t.Fatalf("no forged capability row: %d", n)
	}
	// Scope/request coupling and append-only (CHECKs and triggers).
	if err := l.owner.WithTenant(context.Background(), l.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE aml_screening_policies SET rescreen_interval_seconds = 1 WHERE tenant_id = $1`, l.tenant)
		return err
	}); pgCode(err) != "AM005" {
		t.Fatalf("policy versions stay append-only: %v", err)
	}
	for _, stmt := range []string{`DELETE FROM aml_screening_loosening_settings WHERE tenant_id = $1`, `UPDATE aml_screening_loosening_settings SET enabled = false, cooling_seconds = NULL WHERE tenant_id = $1`} {
		if err := l.owner.WithTenant(context.Background(), l.tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt, l.tenant)
			return err
		}); pgCode(err) != "AM005" {
			t.Fatalf("capability versions stay append-only (%s): %v", stmt, err)
		}
	}
	if err := l.owner.WithTenant(context.Background(), l.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE aml_screening_loosening_settings CASCADE`)
		return err
	}); pgCode(err) != "AM005" {
		t.Fatalf("capability versions cannot be truncated: %v", err)
	}
}

// ---- proofs ------------------------------------------------------------------------------------------------------------------

func TestC4_TamperedPayload_ForgedAndReplayedProofs_AreRefused(t *testing.T) {
	l := newLW(t)
	l.enable(l.tenant, 0)
	r := l.mustRequest(l.req, KindAMLScreeningPolicyLoosen, l.loosenPayload(14400, 7200), "screening_loosening_decision").Request
	// The content is immutable, even for the owner, once filed.
	err := l.owner.WithPlatformAdmin(context.Background(), l.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE licensing_change_requests SET payload = jsonb_set(payload, '{rescreen_interval_seconds}', '99999999') WHERE id = $1`, r.ID)
		return err
	})
	if pgCode(err) != "LC012" {
		t.Fatalf("a payload change after filing: %v", err)
	}
	good := prooftest.ProcessForger(t, l.ownerURL)
	// An approval proof bound to another payload hash (the payload as tampered): AP004.
	err = l.rtPlatform(l.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error {
		if err := setProof(ctx, tx, good.Valid(t, l.ops.Approver1, "platform", uuid.Nil, actorproof.OpLicensingChangeApprove, r.ID.String(), strings.Repeat("ef", 32))); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO licensing_change_approvals (request_id, reason_code) VALUES ($1, 'ok')`, r.ID)
		return err
	})
	if pgCode(err) != "AP004" {
		t.Fatalf("tampered-hash proof: %v", err)
	}
	// A proof for another actor and a forged key.
	err = l.rtPlatform(l.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error {
		if err := setProof(ctx, tx, good.Valid(t, l.ops.Approver2, "platform", uuid.Nil, actorproof.OpLicensingChangeApprove, r.ID.String(), r.PayloadHash)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO licensing_change_approvals (request_id, reason_code) VALUES ($1, 'ok')`, r.ID)
		return err
	})
	if pgCode(err) != "AP004" {
		t.Fatalf("another actor's proof: %v", err)
	}
	wrong := prooftest.Forger{KID: "c4-wrong", Key: []byte(strings.Repeat("w", 32))}
	err = l.rtPlatform(l.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error {
		if err := setProof(ctx, tx, wrong.Valid(t, l.ops.Approver1, "platform", uuid.Nil, actorproof.OpLicensingChangeApprove, r.ID.String(), r.PayloadHash)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO licensing_change_approvals (request_id, reason_code) VALUES ($1, 'ok')`, r.ID)
		return err
	})
	if pgCode(err) != "AP002" {
		t.Fatalf("forged key: %v", err)
	}
	// A valid proof is single-use: the first approval commits, the replay of the SAME token is AP005.
	tok := good.Valid(t, l.ops.Approver1, "platform", uuid.Nil, actorproof.OpLicensingChangeApprove, r.ID.String(), r.PayloadHash)
	insert := func(ctx context.Context, tx pgx.Tx) error {
		if err := setProof(ctx, tx, tok); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO licensing_change_approvals (request_id, reason_code) VALUES ($1, 'ok')`, r.ID)
		return err
	}
	if err := l.rtPlatform(l.ops.Approver1, insert); err != nil {
		t.Fatalf("the first use of a valid proof: %v", err)
	}
	if err := l.rtPlatform(l.ops.Approver1, insert); pgCode(err) != "AP005" {
		t.Fatalf("a replayed proof: want AP005, got %v", err)
	}
	// An execution proof cannot be conjured at the database without the approvals (the approval gate is not the proof).
	err = l.rtPlatform(l.ops.Approver2, func(ctx context.Context, tx pgx.Tx) error {
		if err := setProof(ctx, tx, good.Valid(t, l.ops.Approver2, "platform", uuid.Nil, actorproof.OpLicensingChangeExecute, r.ID.String(), r.PayloadHash)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE licensing_change_requests SET status = 'executed' WHERE id = $1`, r.ID)
		return err
	})
	if pgCode(err) == "" {
		t.Fatal("executing with one approval must be refused")
	}
	if s := l.status(r.ID); s != StatusRequested {
		t.Fatalf("status %s", s)
	}
}

// ---- Go-side shape ---------------------------------------------------------------------------------------------------------------

func TestC4_Decode_StrictShapes(t *testing.T) {
	tid, jid := uuid.NewString(), uuid.NewString()
	loosen := func(extra string) string {
		return `{"tenant_id":"` + tid + `","jurisdiction_id":"` + jid + `","lists":["pep"],"rescreen_interval_seconds":7200,"backfill_grace_seconds":0,"effective_offset_seconds":0,"legal_review_reference":"R","evidence_reference":"E"` + extra + `}`
	}
	if _, _, err := kinds[KindAMLScreeningPolicyLoosen].decode(json.RawMessage(loosen(""))); err != nil {
		t.Fatalf("a complete payload decodes: %v", err)
	}
	for name, raw := range map[string]string{
		"unknown key":     loosen(`,"x":1`),
		"missing key":     `{"tenant_id":"` + tid + `"}`,
		"lists reordered": strings.Replace(loosen(""), `["pep"]`, `["pep","pep"]`, 1),
		"empty":           ``,
		"trailing":        loosen("") + ` {}`,
		"bad evidence":    strings.Replace(loosen(""), `"E"`, `"a b"`, 1),
		"bad legal ref":   strings.Replace(loosen(""), `"R"`, `" R"`, 1),
	} {
		if _, _, err := kinds[KindAMLScreeningPolicyLoosen].decode(json.RawMessage(raw)); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Lists are canonicalised (sorted) so that equal decisions hash equally.
	v, _, err := kinds[KindAMLScreeningPolicyLoosen].decode(json.RawMessage(strings.Replace(loosen(""), `["pep"]`, `["sanctions","pep"]`, 1)))
	if err != nil || strings.Join(v.(ScreeningPolicyLoosenPayload).Lists, ",") != "pep,sanctions" {
		t.Fatalf("canonical lists: %v %v", v, err)
	}
	cfg := func(s string) json.RawMessage {
		return json.RawMessage(`{"tenant_id":"` + tid + `",` + s + `,"evidence_reference":"E"}`)
	}
	for name, raw := range map[string]json.RawMessage{
		"enabled without cooling": cfg(`"enabled":true,"cooling_seconds":null`),
		"disabled with cooling":   cfg(`"enabled":false,"cooling_seconds":5`),
		"negative cooling":        cfg(`"enabled":true,"cooling_seconds":-1`),
		"absent cooling key":      cfg(`"enabled":false`),
	} {
		if _, _, err := kinds[KindAMLScreeningLooseningConfigure].decode(raw); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err := kinds[KindAMLScreeningLooseningConfigure].decode(cfg(`"enabled":true,"cooling_seconds":0`)); err != nil {
		t.Errorf("a stated zero cooling period decodes (an explicit decision, not a default): %v", err)
	}
	for _, k := range []Kind{KindAMLScreeningPolicyLoosen, KindAMLScreeningLooseningConfigure} {
		spec := kinds[k]
		if !spec.platformOnly || spec.tenantPerm(nil) != "" {
			t.Errorf("%s must be platform-only", k)
		}
	}
}

// ---- migration ---------------------------------------------------------------------------------------------------------------

// 0190 up / down / up on a throwaway database: the down restores the 0189 schema byte for byte (the 0180 classifier under its own
// name, the 0154 guard and constraint, no capability table), the re-up equals the first up, nothing ships (zero capability rows), and
// the down is refused (LC095) while a capability row or a request of the new kinds exists.
func TestMigration0190_UpDownUp_Snapshot_NoValueShips_DownRefusedWithData(t *testing.T) {
	ctx := context.Background()
	u := scratchdb.New(t, "m0190_")
	pool, err := db.Connect(ctx, u, 8, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	full, _ := migrationsThrough(t, 190)
	base, _ := migrationsThrough(t, 189)
	if _, err := pool.MigrateUp(ctx, base); err != nil {
		t.Fatalf("migrate to 0189: %v", err)
	}
	before := schemaSnapshot(t, pool)
	if applied, err := pool.MigrateUp(ctx, full); err != nil || len(applied) != 1 || applied[0] != 190 {
		t.Fatalf("0190 up: %v %v", applied, err)
	}
	up := schemaSnapshot(t, pool)
	for _, want := range []string{"table:aml_screening_loosening_settings:rls=true:force=true", "aml_screening_loosening_settings_10_guard", "aml_screening_policies_scope_request",
		"function:licensing_change_classify_base(text,jsonb)", "function:screening_policy_loosen_apply(uuid)", "function:screening_loosening_settings_apply(uuid)",
		"column:aml_screening_policies:change_request_id", "aml_screening_policies_change_request"} {
		if !strings.Contains(strings.ReplaceAll(up, ", ", ","), want) {
			t.Fatalf("0190 up must create %q", want)
		}
	}
	var rows, kinds int
	if err := pool.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM licensing_change_kinds WHERE kind LIKE 'aml\_screening\_%'`).Scan(&kinds)
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE aml_screening_loosening_settings NO FORCE ROW LEVEL SECURITY`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM aml_screening_loosening_settings`).Scan(&rows); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE aml_screening_loosening_settings FORCE ROW LEVEL SECURITY`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || kinds != 2 {
		t.Fatalf("0190 ships zero capability rows and two kinds, found %d rows, %d kinds", rows, kinds)
	}
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("0190 down: %v", err)
	}
	if got := schemaSnapshot(t, pool); got != before {
		t.Fatalf("0190 down must restore the 0189 schema byte for byte; %s", firstDiff(before, got))
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("0190 up again: %v", err)
	}
	if got := schemaSnapshot(t, pool); got != up {
		t.Fatalf("0190 up again must equal the first up; %s", firstDiff(up, got))
	}

	// Down refusal while a request of a new kind exists (a single-actor disable of a never-enabled tenant is refused as UNCHANGED, so
	// the seed is a widening enable request that stays open).
	prooftest.InstallVia(t, pool.Raw(), u)
	launchfix.SeedOperatorsAt(t, u)
	ops := launchfix.Operators()
	var tid uuid.UUID
	if err := pool.WithPlatformAdmin(ctx, ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "m0190", "m0190-"+uuid.NewString(), "under_platform_licence")
		tid = tn.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	svc, err := New(Config{Pool: pool})
	if err != nil {
		t.Fatal(err)
	}
	amlfix.GrantOfficerAs(t, pool, tid, ops.Requester, "platform")
	amlfix.GrantOfficerAs(t, pool, tid, ops.Approver1, "platform")
	cool := int64(0)
	if _, err := svc.Request(ctx, Actor{StaffID: ops.Requester, Scope: ScopePlatform, Role: string(auth.RolePlatformAdmin)},
		RequestInput{Kind: KindAMLScreeningLooseningConfigure, Payload: payload(t, ScreeningLooseningConfigurePayload{TenantID: tid, Enabled: true, CoolingSeconds: &cool, EvidenceReference: "EVID-M0190"}),
			ReasonCode: "m0190"}); err != nil {
		t.Fatalf("seed a request: %v", err)
	}
	_, err = pool.MigrateDown(ctx, full, 1)
	if err == nil || !strings.Contains(err.Error(), "LC095") {
		t.Fatalf("0190 down must refuse LC095 while a request of the kinds exists, got %v", err)
	}
	if got := schemaSnapshot(t, pool); got != up {
		t.Fatal("a refused down must change nothing")
	}
}

// ---- the production guard ----------------------------------------------------------------------------------------------------

// scratchWorld is a world on a throwaway database migrated through 0190 with the environment anchor recorded as given ("" = unrecorded
// = production). The service runs over the owner pool there (the governed paths are the same functions and guards).
func scratchWorld(t *testing.T, prefix, anchor string) *world {
	t.Helper()
	ctx := context.Background()
	u := scratchdb.New(t, prefix)
	pool, err := db.Connect(ctx, u, 12, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	full, _ := migrationsThrough(t, 190)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if anchor != "" {
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO database_environment (environment) VALUES ($1)`, anchor)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	prooftest.InstallVia(t, pool.Raw(), u)
	launchfix.SeedOperatorsAt(t, u)
	w := &world{t: t, ownerURL: u, owner: pool, rt: pool, ops: launchfix.Operators()}
	plat := func(id uuid.UUID) Actor {
		return Actor{StaffID: id, Scope: ScopePlatform, Role: string(auth.RolePlatformAdmin), RequestID: "lc-" + id.String()[:8]}
	}
	w.req, w.ap1, w.ap2 = plat(w.ops.Requester), plat(w.ops.Approver1), plat(w.ops.Approver2)
	w.svc = w.service(Config{})
	return w
}

// A loosening can never enable the withdrawal gate, and in a database whose environment anchor is production (or unrecorded) it is refused
// outright where a gate exists. In a staging anchor the same flow works and still leaves the trigger policies untouched.
func TestC4_ProductionAnchor_LooseningNeverEnablesTheGate_AndIsRefusedWhereAGateExists(t *testing.T) {
	for _, tc := range []struct {
		anchor    string
		envOK     bool
		wantAfter string // hint the loosening is refused with while the latest trigger version gates ("" = succeeds)
	}{
		{"production", false, "gate_environment_refused"},
		{"", false, "gate_environment_refused"}, // unrecorded anchor = production
		{"staging", true, ""},
	} {
		name := tc.anchor
		if name == "" {
			name = "unrecorded"
		}
		t.Run(name, func(t *testing.T) {
			w := scratchWorld(t, "c4env_", tc.anchor)
			l := lwOn(w)
			var ok bool
			if err := w.owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT aml_screening_gate_environment_ok()`).Scan(&ok)
			}); err != nil || ok != tc.envOK {
				t.Fatalf("environment guard = %v (%v), want %v", ok, err, tc.envOK)
			}
			gating := func() int {
				return w.ownerTenantInt(l.tenant, `SELECT count(*) FROM aml_screening_trigger_policies WHERE tenant_id = $1 AND gate_withdrawal`, l.tenant)
			}
			triggers := func() int {
				return w.ownerTenantInt(l.tenant, `SELECT count(*) FROM aml_screening_trigger_policies WHERE tenant_id = $1`, l.tenant)
			}
			// Configuring the capability touches no trigger policy, whatever the anchor says.
			l.enable(l.tenant, 0)
			if triggers() != 0 {
				t.Fatalf("configuring the capability must not write a trigger policy")
			}
			// A fixture gate (the owner bypasses the function's guard; the production function itself refuses this, see the static pin).
			l.seedTrigger(true, 7200)
			if triggers() != 1 || gating() != 1 {
				t.Fatal("fixture gate")
			}
			_, err := l.request(KindAMLScreeningPolicyLoosen, l.loosenPayload(20000, fixtureGrace))
			if tc.wantAfter != "" {
				wantHint(t, err, tc.wantAfter, "a gate exists in a production-anchored database")
				if n := l.requestCount(KindAMLScreeningPolicyLoosen); n != 0 || len(l.policies()) != 1 {
					t.Fatalf("nothing is written: %d requests, %d versions", n, len(l.policies()))
				}
				// With the gate switched off by a newer (non-gating) trigger version, the same loosening is allowed - and enables nothing.
				l.seedTrigger(false, 0)
			}
			done := l.widen(KindAMLScreeningPolicyLoosen, l.loosenPayload(20000, fixtureGrace), "screening_loosening_decision")
			if !done.Executed || len(l.policies()) != 2 {
				t.Fatalf("loosening %+v", done)
			}
			wantTriggers := 1
			if tc.wantAfter != "" {
				wantTriggers = 2
			}
			if triggers() != wantTriggers {
				t.Fatalf("a loosening must never write a trigger policy: %d", triggers())
			}
			if tc.wantAfter != "" && gating() != 1 { // only the fixture row of this test gates; the loosening added none
				t.Fatalf("gating rows = %d", gating())
			}
		})
	}
}

// The static half of the guard: migration 0190 neither defines nor replaces the gate, its environment anchor helper or the trigger
// policy author, and the loosening path calls the same environment helper.
func TestC4_Migration_DoesNotTouchTheGateOrItsProductionGuard(t *testing.T) {
	b, err := readFileT(t, "../../migrations/0190_c4_screening_policy_governed_loosening.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, forbidden := range []string{"FUNCTION aml_screening_trigger_policy_set", "FUNCTION aml_screening_gate_environment_ok", "FUNCTION aml_screening_gate_denies",
		"FUNCTION screening_worker_available", "FUNCTION screening_worker_heartbeat", "INTO public.aml_screening_trigger_policies", "INSERT INTO aml_screening_trigger_policies",
		"UPDATE public.aml_screening_trigger_policies", "database_environment"} {
		if strings.Contains(src, forbidden) {
			t.Errorf("migration 0190 must not contain %q", forbidden)
		}
	}
	if !strings.Contains(src, "public.aml_screening_gate_environment_ok()") {
		t.Error("the loosening validator must apply the production environment guard")
	}
	k, err := readFileT(t, "screening_loosening_kinds.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(k), "aml_screening_trigger_policy") && strings.Contains(string(k), "INSERT") {
		t.Error("the Go kinds must not write a trigger policy")
	}
}

func readFileT(t *testing.T, path string) ([]byte, error) {
	t.Helper()
	return os.ReadFile(path)
}

// ---- review fixes (security HIGH-1, LOW-1, LOW-2, MEDIUM-1; ledger-finance H-1, M-1, M-2) ------------------------------------------

// HIGH-1: three plain platform admins with no aml_officer grant can neither enable the capability nor loosen; the controls the AML
// routes require of an officer (a live grant on the tenant, the owner-scope quorum) apply to the requester, every counted approver and the
// executor. The mechanism stays inert without them.
func TestC4_OfficerControls_PlainPlatformAdminsCannotEnableOrLoosen(t *testing.T) {
	l := newLWNoOfficers(t)
	// Nobody is an officer: enabling is refused at filing.
	_, err := l.request(KindAMLScreeningLooseningConfigure, l.cfgPayload(l.tenant, true, 0))
	wantHint(t, err, "officer_grant_required", "a plain platform admin enabling the capability")
	if len(l.settings(l.tenant)) != 0 {
		t.Fatal("nothing is configured")
	}
	// The requester alone is an officer: the quorum (two distinct Persons) does not hold.
	amlfix.GrantOfficerAs(t, l.owner, l.tenant, l.req.StaffID, "platform")
	_, err = l.request(KindAMLScreeningLooseningConfigure, l.cfgPayload(l.tenant, true, 0))
	wantHint(t, err, "officer_quorum_not_met", "one officer is not a quorum")
	// Quorum met by the requester and ap1; the approval of a plain admin (ap2) is refused when it is recorded.
	amlfix.GrantOfficerAs(t, l.owner, l.tenant, l.ap1.StaffID, "platform")
	r := l.mustRequest(l.req, KindAMLScreeningLooseningConfigure, l.cfgPayload(l.tenant, true, 0), "loosening_enable_decision").Request
	_, err = l.approve(l.ap2, r.ID)
	wantHint(t, err, "officer_grant_required", "a non-officer approver")
	if n := l.ownerInt(`SELECT count(*) FROM licensing_change_approvals WHERE request_id = $1`, r.ID); n != 0 {
		t.Fatalf("the refused approval is not recorded: %d", n)
	}
	// Approvals by two officers execute it (ap2 becomes an officer).
	amlfix.GrantOfficerAs(t, l.owner, l.tenant, l.ap2.StaffID, "platform")
	l.mustApprove(l.ap1, r.ID)
	if done := l.mustApprove(l.ap2, r.ID); !done.Executed {
		t.Fatal("two officers enable the capability")
	}
	// Loosening needs the same: ap3 (a plain admin) cannot take part; a requester without a grant cannot file.
	lr := l.mustRequest(l.req, KindAMLScreeningPolicyLoosen, l.loosenPayload(14400, 7200), "screening_loosening_decision").Request
	if _, err := l.approve(l.ap3, lr.ID); GuardCode(err) != "LC050" || hintOf(err) != "officer_grant_required" {
		t.Fatalf("a non-officer approving a loosening: %v", err)
	}
	if _, err := l.svc.Request(context.Background(), l.ap3, RequestInput{Kind: KindAMLScreeningPolicyLoosen, Payload: payload(t, l.loosenPayload(20000, fixtureGrace)), ReasonCode: "screening_loosening_decision"}); hintOf(err) != "officer_grant_required" {
		t.Fatalf("a non-officer filing a loosening: %v", err)
	}
	// Disabling (narrowing) stays open to any platform admin: the fail-closed direction.
	if res, err := l.svc.Request(context.Background(), l.ap3, RequestInput{Kind: KindAMLScreeningLooseningConfigure, Payload: payload(t, l.cfgPayload(l.tenant, false, 0)), ReasonCode: "loosening_disable_decision"}); err != nil || !res.Executed {
		t.Fatalf("any platform admin may disable: %+v %v", res, err)
	}
	if len(l.policies()) != 1 {
		t.Fatal("no policy version was written by a non-officer path")
	}
}

// HIGH-1 at execution: an approver who is an officer when approving but not when the final approval executes does not count.
func TestC4_OfficerControls_ApproverLosingStandingBeforeExecutionDoesNotCount(t *testing.T) {
	l := newLWNoOfficers(t)
	for _, a := range []Actor{l.req, l.ap2, l.ap3} {
		amlfix.GrantOfficerAs(t, l.owner, l.tenant, a.StaffID, "platform")
	}
	amlfix.GrantOfficerUntil(t, l.owner, l.tenant, l.ap1.StaffID, "platform", time.Now().Add(2500*time.Millisecond))
	l.enable(l.tenant, 0) // ap1 and ap2 approve while ap1 is still an officer
	r := l.mustRequest(l.req, KindAMLScreeningPolicyLoosen, l.loosenPayload(14400, 7200), "screening_loosening_decision").Request
	l.mustApprove(l.ap1, r.ID)
	time.Sleep(2800 * time.Millisecond) // ap1's grant has expired
	before := len(l.policies())
	_, err := l.approve(l.ap2, r.ID)
	wantHint(t, err, "officer_grant_required", "an approval by someone who is no longer an officer does not count")
	if len(l.policies()) != before {
		t.Fatal("nothing is written")
	}
	// The request cannot be completed by a third approver either (the expired approval still occupies one of the two counted places in Go's
	// count, and the database refuses at execution): it is cancelled and refiled, like a stale approval.
	if _, err := l.approve(l.ap3, r.ID); hintOf(err) != "officer_grant_required" {
		t.Fatalf("a third approval cannot rescue it: %v", err)
	}
	if len(l.policies()) != before {
		t.Fatal("nothing is written")
	}
	fresh := l.mustRequest(l.req, KindAMLScreeningPolicyLoosen, l.loosenPayload(30000, 7200), "screening_loosening_decision").Request
	l.mustApprove(l.ap2, fresh.ID)
	if done := l.mustApprove(l.ap3, fresh.ID); !done.Executed {
		t.Fatalf("two current officers execute a fresh request: %+v", done)
	}
}

// LF H-1: the sanctions list is never removed (a safe default, not a business policy); the other dimensions are tested above.
func TestC4_SanctionsListRemoval_IsRefusedEvenWithEverythingElseInOrder(t *testing.T) {
	l := newLW(t)
	l.enable(l.tenant, 0)
	_, err := l.request(KindAMLScreeningPolicyLoosen, l.loosenPayload(20000, 7200, "pep"))
	wantHint(t, err, "sanctions_list_removal_refused", "pep only")
	// The SQL applies it when Go is bypassed.
	body := `{"tenant_id":"` + l.tenant.String() + `","jurisdiction_id":"` + l.jur.String() + `","lists":["pep"],"rescreen_interval_seconds":20000,"backfill_grace_seconds":7200,"effective_offset_seconds":0,"legal_review_reference":"R","evidence_reference":"E"}`
	err = l.rt.WithPlatformAdmin(context.Background(), l.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT widening FROM licensing_change_classify('aml_screening_policy_loosen', $1::jsonb)`, body)
		return err
	})
	if pgCode(err) != "LC050" || !strings.Contains(err.Error(), "sanctions list cannot be removed") {
		t.Fatalf("direct classify: %v", err)
	}
}

// SEC LOW-1: the capability lock orders a final approval behind a disable that holds it first. Lock queue order: the disable queues first,
// the final approval second; when the lock holder lets go the disable commits and the approval then observes DISABLED.
func TestC4_CapabilityLock_AFinalApprovalWaitingBehindADisableObservesIt(t *testing.T) {
	l := newLW(t)
	l.enable(l.tenant, 0)
	before := len(l.policies())
	r := l.mustRequest(l.req, KindAMLScreeningPolicyLoosen, l.loosenPayload(14400, 7200), "screening_loosening_decision").Request
	l.mustApprove(l.ap1, r.ID)

	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	defer letGo() // never leave the lock holder blocked if an assertion fails first
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = l.owner.WithPlatformAdmin(context.Background(), l.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('screening_loosening:' || $1::text, 190))`, l.tenant.String()); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	var disableErr, approveErr error
	var disabled *Result
	wg.Add(2)
	go func() {
		defer wg.Done()
		disabled, disableErr = l.svc.Request(context.Background(), l.req, RequestInput{Kind: KindAMLScreeningLooseningConfigure,
			Payload: payload(t, l.cfgPayload(l.tenant, false, 0)), ReasonCode: "loosening_disable_decision"})
	}()
	time.Sleep(500 * time.Millisecond) // the disable is queued on the lock first
	go func() {
		defer wg.Done()
		_, approveErr = l.approve(l.ap2, r.ID)
	}()
	time.Sleep(500 * time.Millisecond)
	if len(l.policies()) != before {
		t.Fatal("the final approval must not run past the lock")
	}
	letGo()
	wg.Wait()
	if disableErr != nil || disabled == nil || !disabled.Executed {
		t.Fatalf("the disable: %+v %v", disabled, disableErr)
	}
	wantHint(t, approveErr, "loosening_disabled", "the final approval after the disable")
	if len(l.policies()) != before {
		t.Fatal("no policy version after a disable")
	}
}

// SEC MEDIUM-1: the version takes effect at least the cooling period after the decision, whatever the payload offset says.
func TestC4_EffectiveTime_IsAtLeastTheCoolingPeriodAfterExecution(t *testing.T) {
	l := newLW(t)
	l.enable(l.tenant, 5)
	r := l.mustRequest(l.req, KindAMLScreeningPolicyLoosen, l.loosenPayload(14400, 7200), "screening_loosening_decision").Request // offset 0
	l.mustApprove(l.ap1, r.ID)
	time.Sleep(5200 * time.Millisecond)
	done := l.mustApprove(l.ap2, r.ID)
	if !done.Executed {
		t.Fatalf("not executed: %+v", done)
	}
	pol := l.policies()
	if d := time.Until(pol[len(pol)-1].Effective); d < 4*time.Second || d > 6*time.Second {
		t.Fatalf("effective_from must be about the cooling period (5 s) after the execution, got %s ahead", d)
	}
	// The payload offset wins when it is longer.
	l2 := newLW(t)
	l2.enable(l2.tenant, 1)
	p := l2.loosenPayload(14400, 7200)
	p.EffectiveOffsetSeconds = 600
	r2 := l2.mustRequest(l2.req, KindAMLScreeningPolicyLoosen, p, "screening_loosening_decision").Request
	l2.mustApprove(l2.ap1, r2.ID)
	time.Sleep(1200 * time.Millisecond)
	l2.mustApprove(l2.ap2, r2.ID)
	pol = l2.policies()
	if d := time.Until(pol[len(pol)-1].Effective); d < 9*time.Minute {
		t.Fatalf("the longer offset wins: %s", d)
	}
}

// SEC LOW-2: the platform branch of the policy guard also bounds the effective time to [executed_at, executed_at + 30 d] and fixes the reason
// code. The executed request is forged by the owner (triggers off for the fixture only) so the guard is tested without the executor.
func TestC4_PolicyGuard_BoundsTheEffectiveTimeAndTheReasonCode(t *testing.T) {
	l := newLW(t)
	try := func(effective, reason string) error {
		var out error
		fixtureErr := l.owner.WithPlatformAdmin(context.Background(), l.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			for _, stmt := range []string{`ALTER TABLE licensing_change_requests DISABLE TRIGGER licensing_change_requests_guard`} {
				if _, err := tx.Exec(ctx, stmt); err != nil {
					return err
				}
			}
			var req uuid.UUID
			payloadJSON := `{"tenant_id":"` + l.tenant.String() + `","jurisdiction_id":"` + l.jur.String() + `","lists":["pep","sanctions"],"rescreen_interval_seconds":14400,"backfill_grace_seconds":7200,"effective_offset_seconds":0,"legal_review_reference":"R","evidence_reference":"E"}`
			if err := tx.QueryRow(ctx, `INSERT INTO licensing_change_requests (kind, payload, payload_hash, reason_code, widening, required_approvals, requested_by, requested_by_person_id,
					requested_scope, status, created_at, created_txid, expires_at, executed_by, executed_at, executed_txid)
				VALUES ('aml_screening_policy_loosen', $1::jsonb, repeat('0', 64), 'x', true, 2, $2, $3, 'platform', 'executed', now(), txid_current(), now() + interval '1 day', $4, now(), txid_current())
				RETURNING id`, payloadJSON, l.req.StaffID, l.ops.Persons[0], l.ap2.StaffID).Scan(&req); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, l.tenant.String()); err != nil {
				return err
			}
			_, out = tx.Exec(ctx, `INSERT INTO aml_screening_policies (tenant_id, licensing_jurisdiction_id, lists, rescreen_interval_seconds, backfill_grace_seconds, effective_from,
					legal_review_reference, reason_code, created_by, created_by_person_id, created_by_scope, change_request_id)
				VALUES ($1, $2, '{pep,sanctions}', 14400, 7200, `+effective+`, 'R', $3, $4, $5, 'platform', $6)`, l.tenant, l.jur, reason, l.ap2.StaffID, l.ops.Persons[2], req)
			return errors.New("rollback")
		})
		if out == nil && fixtureErr != nil && fixtureErr.Error() != "rollback" {
			t.Fatalf("fixture: %v", fixtureErr)
		}
		return out
	}
	// The control: a lawful row of the same shape is admitted (so the refusals below are the bounds and nothing else).
	if err := try(`now() + interval '1 hour'`, "governed_loosening"); err != nil {
		t.Fatalf("control: %v", err)
	}
	for name, c := range map[string][2]string{
		"before the execution": {`now() - interval '1 day'`, "governed_loosening"},
		"beyond 30 days":       {`now() + interval '31 days'`, "governed_loosening"},
		"another reason code":  {`now() + interval '1 hour'`, "something_else"},
	} {
		if err := try(c[0], c[1]); pgCode(err) != "LC040" {
			t.Fatalf("%s: want LC040, got %v", name, err)
		}
	}
}

// LF M-1: once the backfill grace of the gating policy has run out, a longer grace is refused (it would re-open the grace for persons
// registered before the gate). Anything else of a loosening still works. Staging anchor, so a fixture gate is admissible.
func TestC4_GraceExtensionAfterExpiry_IsRefused(t *testing.T) {
	w := scratchWorld(t, "c4grace_", "staging")
	l := lwOn(w)
	l.enable(l.tenant, 0)
	l.seedPolicy(l.tenant, l.jur, "{pep,sanctions}", fixtureInterval, 60) // the grace is 60 s ...
	l.seedTrigger(true, 7200)                                             // ... and the gate took effect an hour ago: expired
	_, err := l.request(KindAMLScreeningPolicyLoosen, l.loosenPayload(14400, 3600))
	wantHint(t, err, "grace_extension_after_expiry_refused", "a longer grace after the grace has run out")
	// Keeping the grace and lengthening the interval is allowed (the interval retro-release stays an owner decision, D10).
	if done := l.widen(KindAMLScreeningPolicyLoosen, l.loosenPayload(14400, 60), "screening_loosening_decision"); !done.Executed {
		t.Fatal("an interval-only loosening is allowed")
	}
}

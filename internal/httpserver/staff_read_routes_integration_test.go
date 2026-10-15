//go:build integration

// ADR 0114 slices S2 and S4 over real HTTP, on a private scratch database migrated to the latest
// on-disk migration (the cgAPI / frWorld harness): R1 payment attempts list and detail, R1b the
// payout attempt summary on the admin withdrawal detail, R2 reconciliation runs and mismatches for
// payment_statement, ledger_vs_projection and sportsbook_settlement. Everything is synthetic
// (MOCK provider, test-only references).
package httpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

type srWorld struct {
	*frWorld
	rmTok, pmTok, bxTok string
	depA                []uuid.UUID // disputed deposits, tenant A
	ambAtt, ambWD       uuid.UUID   // ambiguous payout (M2-admissible)
	unbAtt, unbWD       uuid.UUID   // unbound invalid_provider_reference park
	destAtt, destWD     uuid.UUID   // destination_mismatch park
	depB                uuid.UUID   // disputed deposit, tenant B
	allStaff            []uuid.UUID
	secrets             []string // reference texts that must never appear in any body
}

// parkedPayout claims a real USD MOCK payout and parks it with reason (no provider reference).
func (w *frWorld) parkedPayout(amount int64, reason string) (wd, att uuid.UUID) {
	t := w.a.t
	t.Helper()
	ctx := context.Background()
	pool := w.a.pool
	fundWallet(t, pool, w.tenant, w.brand, w.player, "USD", 100000)
	if err := pool.WithTenant(ctx, w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO withdrawal_policies (tenant_id, asset_code, approval_threshold_minor_units, required_approvals, effective_from)
			VALUES ($1, 'USD', 1000000, 2, now() - interval '1 hour') ON CONFLICT DO NOTHING`, w.tenant)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	wr := mustCreateWithdrawalRequest(t, pool, w.tenant, w.brand, w.player, walletIDFor(t, pool, w.tenant, w.player, "USD"), "USD", amount)
	do := func(fn func(ctx context.Context, tx pgx.Tx) error) {
		t.Helper()
		if err := pool.WithTenant(ctx, w.tenant, fn); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	do(func(ctx context.Context, tx pgx.Tx) error { return withdrawal.MoveToPendingReview(ctx, tx, wr.ID) })
	do(func(ctx context.Context, tx pgx.Tx) error {
		ok, err := withdrawal.Approve(ctx, tx, wr.ID, uuid.New(), true, nil, nil, auth.StepUpAssertion{})
		if err == nil && !ok {
			err = errors.New("automated approval did not approve")
		}
		return err
	})
	claim, err := w.orch.ClaimForDispatch(ctx, pool, payments.KYCEnforcementPayoutGate{}, w.tenant, wr.ID, "bank_transfer",
		payments.SubmitActor{StaffID: uuid.New(), IPAddress: "127.0.0.1", UserAgent: "t", RequestID: uuid.NewString()})
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}
	do(func(ctx context.Context, tx pgx.Tx) error {
		return payments.ApplyDisputeFromNonTerminal(ctx, tx, claim.Attempt.ID, payments.EvidenceSync, reason)
	})
	return wr.ID, claim.Attempt.ID
}

// disputedDepositInTenantB gives tenant B (w.other) a brand, player, wallet and a disputed deposit.
func (w *frWorld) disputedDepositInTenantB() uuid.UUID {
	t := w.a.t
	t.Helper()
	ctx := context.Background()
	brand, player, wallet, person := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if err := w.a.pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.a.pool.WithTenant(ctx, w.other, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, name, slug, status) VALUES ($1, $2, 'b', $3, 'active')`, brand, w.other, "srb-"+brand.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status) VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			player, w.other, brand, person, player.String()+"@sr.invalid"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, 'USD')`, wallet, w.other, brand, player)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	orch, mock := newMockOrchestrator()
	mustRegisterCapability(t, w.a.pool, w.other, mock)
	res, err := orch.InitiateDepositAttempt(ctx, w.a.pool, payments.KYCEnforcementDepositGate{}, payments.MockCredentialResolver{}, payments.InitiateDepositParams{MarketGuard: testUngatedGuard,
		Scope:     payments.DepositScope{TenantID: w.other, BrandID: brand, PlayerAccountID: player, WalletID: wallet},
		AssetCode: "USD", Amount: 4000, PaymentMethod: "card", IdempotencyKey: uuid.NewString(),
	})
	if err != nil || res.Attempt.ProviderReference == nil {
		t.Fatalf("tenant B deposit: %v", err)
	}
	if err := w.a.pool.WithTenant(ctx, w.other, func(ctx context.Context, tx pgx.Tx) error {
		_, err := payments.ApplyReceiptEvidence(ctx, tx, orch, w.other, "mock", payments.ReceiptEvidence{
			EventType: "deposit", ProviderReference: *res.Attempt.ProviderReference,
			Outcome: payments.OutcomeSucceeded, Amount: 3999, AssetCode: "USD",
		})
		return err
	}); err != nil {
		t.Fatalf("tenant B mismatching receipt: %v", err)
	}
	return res.Attempt.ID
}

func newSRWorld(t *testing.T) *srWorld {
	t.Helper()
	reg := &payments.StatementSourceRegistry{}
	reg.Register("mock")
	f := newFRWorldWithSources(t, reg)
	w := &srWorld{frWorld: f}
	w.rmTok = f.a.token(f.a.staff(f.tenant, "risk_manager"), f.tenant, auth.RoleRiskManager)
	w.pmTok = f.a.token(f.a.staff(f.tenant, "promotions_manager"), f.tenant, auth.RolePromotionsManager)
	w.bxTok = f.a.token(f.a.staff(f.tenant, "bonus_operations"), f.tenant, auth.RoleBonusOperations)
	for i := 0; i < 3; i++ {
		w.depA = append(w.depA, f.disputedDeposit())
	}
	w.ambAtt = f.ambiguousPayout()
	w.unbWD, w.unbAtt = f.parkedPayout(3000, "invalid_provider_reference")
	w.destWD, w.destAtt = f.parkedPayout(2000, "destination_mismatch")
	w.depB = f.disputedDepositInTenantB()
	w.allStaff = []uuid.UUID{f.f1, f.f2, f.fNoGrant, f.ta, f.supp, f.comp, f.otherFin, f.pa, f.pb, f.grantApprover, f.acting, f.actingNoGrant}
	if err := f.a.pool.WithTenant(context.Background(), f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT withdrawal_request_id FROM payment_attempts WHERE id = $1`, w.ambAtt).Scan(&w.ambWD); err != nil {
			return err
		}
		// The MOCK provider reuses ids as references; a reference that equals an id the response
		// legitimately carries (an attempt, withdrawal or deposit-intent id) cannot be grepped for.
		rows, err := tx.Query(ctx, `SELECT r FROM (
			SELECT provider_reference AS r FROM payment_attempts WHERE provider_reference IS NOT NULL
			UNION SELECT merchant_reference FROM payment_attempts UNION SELECT external_idempotency_key FROM payment_attempts) x
			WHERE r NOT IN (SELECT id::text FROM payment_attempts UNION SELECT withdrawal_request_id::text FROM payment_attempts WHERE withdrawal_request_id IS NOT NULL
			                UNION SELECT deposit_intent_id::text FROM payment_attempts WHERE deposit_intent_id IS NOT NULL)`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			w.secrets = append(w.secrets, s)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(w.secrets) < 6 {
		t.Fatalf("fixture produced only %d reference strings; the exclusion greps would be vacuous", len(w.secrets))
	}
	return w
}

func (w *srWorld) attemptsURL(tenant uuid.UUID, q string) string {
	u := "/v1/admin/tenants/" + tenant.String() + "/payment-attempts"
	if q != "" {
		u += "?" + q
	}
	return u
}

// noLeak asserts the body carries none of the forbidden strings: reference texts, player, wallet,
// brand ids, any staff id of the world.
func (w *srWorld) noLeak(t *testing.T, label string, body []byte) {
	t.Helper()
	s := string(body)
	for _, sec := range w.secrets {
		if strings.Contains(s, sec) {
			t.Errorf("%s: body leaks reference text %q: %s", label, sec, s)
		}
	}
	for _, id := range append([]uuid.UUID{w.player, w.wallet, w.brand}, w.allStaff...) {
		if strings.Contains(s, id.String()) {
			t.Errorf("%s: body contains forbidden id %s", label, id)
		}
	}
	if regexp.MustCompile(`"destination[a-z_]*"\s*:`).MatchString(s) {
		t.Errorf("%s: body carries a destination key: %s", label, s)
	}
	for _, bad := range []string{`"provider_reference"`, `"merchant_reference"`, `"player_account_id"`, `"external_idempotency_key"`, "SQLSTATE", "violates"} {
		if strings.Contains(s, bad) {
			t.Errorf("%s: body contains %s", label, bad)
		}
	}
}

func decodePage(t *testing.T, res cgResp) paymentAttemptListResponse {
	t.Helper()
	var p paymentAttemptListResponse
	res.decode(t, &p)
	return p
}

func (w *srWorld) platformReadAudits(t *testing.T, tenant uuid.UUID, action string) int {
	t.Helper()
	var n int
	if err := w.a.pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = $1 AND subject_tenant_id = $2 AND tenant_id IS NULL`, action, tenant).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// --- R1: authorization matrix ---

func TestStaffReadR1_AuthzMatrix(t *testing.T) {
	w := newSRWorld(t)
	list := w.attemptsURL(w.tenant, "")
	detail := w.attemptsURL(w.tenant, "") + "/" + w.depA[0].String()
	paTok := w.tok(w.pa)
	for _, c := range []struct {
		name string
		tok  string
		want int
	}{
		{"platform_admin", paTok, http.StatusOK},
		{"compliance", w.tok(w.comp), http.StatusOK},
		{"finance", w.tok(w.f1), http.StatusOK},
		{"tenant_admin", w.tok(w.ta), http.StatusForbidden},
		{"support", w.tok(w.supp), http.StatusForbidden},
		{"risk_manager", w.rmTok, http.StatusForbidden},
		{"promotions_manager", w.pmTok, http.StatusForbidden},
		{"bonus_operations", w.bxTok, http.StatusForbidden},
		{"no token", "", http.StatusUnauthorized},
	} {
		for _, p := range []string{list, detail} {
			res := w.a.do("GET", p, c.tok, nil)
			if res.status != c.want {
				t.Errorf("%s %s: got %d, want %d: %s", c.name, p, res.status, c.want, res.body)
			}
			w.noLeak(t, c.name, res.body)
		}
	}
	// A player-principal token is refused by the staff gate.
	pl, err := w.a.issuer.Issue(w.player.String(), w.tenant, auth.RolePlayer, auth.PrincipalPlayer, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if res := w.a.do("GET", list, pl, nil); res.status != http.StatusForbidden && res.status != http.StatusUnauthorized {
		t.Errorf("player token: got %d", res.status)
	}
	// withdrawal:review is NOT accepted: finance holds it and reads; a role holding only
	// withdrawal:review would not (no such role exists; the route permission is pinned in the
	// auth role-matrix test and by the tenant_admin / support rows above).
}

func TestStaffReadR1_CrossTenant(t *testing.T) {
	w := newSRWorld(t)
	foreignFin := w.tok(w.otherFin) // finance of tenant B
	// naming tenant A with tenant B's token: 404, no data, both routes.
	for _, p := range []string{w.attemptsURL(w.tenant, ""), w.attemptsURL(w.tenant, "") + "/" + w.depA[0].String()} {
		res := w.a.do("GET", p, foreignFin, nil)
		if res.status != http.StatusNotFound {
			t.Errorf("cross-tenant %s: got %d, want 404: %s", p, res.status, res.body)
		}
		w.noLeak(t, "cross-tenant", res.body)
		if strings.Contains(string(res.body), w.depA[0].String()) {
			t.Errorf("cross-tenant body names an attempt of the other tenant")
		}
	}
	// tenant B's own listing holds only tenant B's attempt.
	res := w.a.do("GET", w.attemptsURL(w.other, ""), foreignFin, nil)
	if res.status != http.StatusOK {
		t.Fatalf("tenant B list: %d %s", res.status, res.body)
	}
	pg := decodePage(t, res)
	if len(pg.Items) != 1 || pg.Items[0].ID != w.depB.String() {
		t.Fatalf("tenant B sees %+v, want exactly its own disputed deposit %s", pg.Items, w.depB)
	}
	// tenant A's attempt id through tenant B's own route is 404 (indistinguishable from unknown).
	for _, id := range []string{w.depA[0].String(), uuid.NewString()} {
		res := w.a.do("GET", w.attemptsURL(w.other, "")+"/"+id, foreignFin, nil)
		if res.status != http.StatusNotFound {
			t.Errorf("foreign/unknown attempt %s through own tenant: %d", id, res.status)
		}
	}
	// A client-supplied tenant id is refused, not honoured.
	if res := w.a.do("GET", w.attemptsURL(w.other, "tenant_id="+w.tenant.String()), foreignFin, nil); res.status != http.StatusBadRequest {
		t.Errorf("tenant_id query parameter: got %d, want 400", res.status)
	}
	// A platform caller can read either tenant, each with its own rows only.
	paTok := w.tok(w.pa)
	a := decodePage(t, w.a.do("GET", w.attemptsURL(w.tenant, "limit=200"), paTok, nil))
	b := decodePage(t, w.a.do("GET", w.attemptsURL(w.other, "limit=200"), paTok, nil))
	if len(a.Items) != 6 || len(b.Items) != 1 {
		t.Fatalf("platform view: tenant A %d (want 6), tenant B %d (want 1)", len(a.Items), len(b.Items))
	}
	// Unknown tenant for a platform caller: 404 and no audit row.
	ghost := uuid.New()
	if res := w.a.do("GET", w.attemptsURL(ghost, ""), paTok, nil); res.status != http.StatusNotFound {
		t.Errorf("unknown tenant: %d", res.status)
	}
	if n := w.platformReadAudits(t, ghost, "payment_attempts.platform_read"); n != 0 {
		t.Errorf("an unknown tenant produced %d audit rows", n)
	}
	// Malformed tenant id.
	if res := w.a.do("GET", "/v1/admin/tenants/not-a-uuid/payment-attempts", paTok, nil); res.status != http.StatusBadRequest {
		t.Errorf("malformed tenant id: %d", res.status)
	}
}

// --- R1: shape, exclusions, admissible kinds, filters ---

func TestStaffReadR1_ShapeExclusionsAndAdmissibleKinds(t *testing.T) {
	w := newSRWorld(t)
	res := w.a.do("GET", w.attemptsURL(w.tenant, "limit=200"), w.tok(w.f1), nil)
	if res.status != http.StatusOK {
		t.Fatalf("list: %d %s", res.status, res.body)
	}
	w.noLeak(t, "finance list", res.body)
	pg := decodePage(t, res)
	byID := map[string]paymentAttemptDTO{}
	for _, it := range pg.Items {
		byID[it.ID] = it
		if it.Exponent != 2 || it.AssetCode != "USD" {
			t.Errorf("%s: asset/exponent = %s/%d, want USD/2", it.ID, it.AssetCode, it.Exponent)
		}
		if it.AdmissibleKinds == nil {
			t.Errorf("%s: admissible_kinds is null", it.ID)
		}
		if (it.SubjectRef.WithdrawalRequestID == nil) == (it.SubjectRef.DepositIntentID == nil) {
			t.Errorf("%s: subject_ref must name exactly one subject: %+v", it.ID, it.SubjectRef)
		}
	}
	kindsOf := func(id uuid.UUID) []string { return byID[id.String()].AdmissibleKinds }
	eq := func(got []string, want ...string) bool { return strings.Join(got, ",") == strings.Join(want, ",") }
	if k := kindsOf(w.depA[0]); !eq(k, "m1_deposit_evidence") {
		t.Errorf("disputed deposit kinds = %v", k)
	}
	amb := byID[w.ambAtt.String()]
	if amb.State != "ambiguous" || amb.Operation != "payout" || amb.AmountMinorUnits != "5000" {
		t.Errorf("ambiguous payout row = %+v", amb)
	}
	if amb.HasProviderReference {
		if !eq(amb.AdmissibleKinds, "m2_declare_paid", "m2_declare_not_paid") {
			t.Errorf("ambiguous payout with reference kinds = %v", amb.AdmissibleKinds)
		}
	} else if !eq(amb.AdmissibleKinds, "m2_declare_not_paid") {
		t.Errorf("ambiguous payout without reference kinds = %v", amb.AdmissibleKinds)
	}
	unb := byID[w.unbAtt.String()]
	if unb.State != "disputed" || unb.TerminalReason == nil || *unb.TerminalReason != "invalid_provider_reference" || unb.HasProviderReference {
		t.Errorf("unbound park row = %+v", unb)
	}
	if !eq(unb.AdmissibleKinds, "m4_evidence_paid", "m4_evidence_not_paid") {
		t.Errorf("unbound park kinds (database predicate payment_m4_in_scope) = %v", unb.AdmissibleKinds)
	}
	dest := byID[w.destAtt.String()]
	if dest.TerminalReason == nil || *dest.TerminalReason != "destination_mismatch" || !eq(dest.AdmissibleKinds, "m4_evidence_not_paid") {
		t.Errorf("destination_mismatch row = %+v", dest)
	}
	if unb.SubjectRef.WithdrawalRequestID == nil || *unb.SubjectRef.WithdrawalRequestID != w.unbWD.String() {
		t.Errorf("unbound subject_ref = %+v, want withdrawal %s", unb.SubjectRef, w.unbWD)
	}
	// Detail equals the list row for the same attempt.
	for _, id := range []uuid.UUID{w.depA[1], w.ambAtt, w.unbAtt, w.destAtt} {
		dres := w.a.do("GET", w.attemptsURL(w.tenant, "")+"/"+id.String(), w.tok(w.comp), nil)
		if dres.status != http.StatusOK {
			t.Fatalf("detail %s: %d %s", id, dres.status, dres.body)
		}
		w.noLeak(t, "detail", dres.body)
		var d paymentAttemptDTO
		dres.decode(t, &d)
		dj, _ := json.Marshal(d)
		lj, _ := json.Marshal(byID[id.String()])
		if string(dj) != string(lj) {
			t.Errorf("detail %s differs from its list row:\n%s\n%s", id, dj, lj)
		}
	}
}

// An open K3 resolution is reported and empties admissible_kinds (M1 deposit; M2 payout for R1b).
func TestStaffReadR1_OpenResolutionAndR1b(t *testing.T) {
	w := newSRWorld(t)
	// M1 on a disputed deposit.
	code, body, dto := w.request(w.f1, w.depA[0], nil)
	if code != http.StatusCreated {
		t.Fatalf("M1 request: %d %s", code, body)
	}
	one := decodeAttempt(t, w.a.do("GET", w.attemptsURL(w.tenant, "")+"/"+w.depA[0].String(), w.tok(w.comp), nil))
	if one.OpenResolutionID == nil || *one.OpenResolutionID != dto.ID || len(one.AdmissibleKinds) != 0 {
		t.Errorf("deposit with open M1: open_resolution_id=%v kinds=%v, want %s and none", one.OpenResolutionID, one.AdmissibleKinds, dto.ID)
	}
	// M2 on the ambiguous payout; the withdrawal detail (R1b) names it.
	m2 := w.a.do("POST", w.base(), w.tok(w.f1), w.m2Body(w.ambAtt, "m2_declare_not_paid"))
	if m2.status != http.StatusCreated {
		t.Fatalf("M2 request: %d %s", m2.status, m2.body)
	}
	var m2dto frDTO
	m2.decode(t, &m2dto)
	det := w.a.do("GET", "/v1/admin/withdrawals/"+w.ambWD.String(), w.tok(w.f1), nil)
	if det.status != http.StatusOK {
		t.Fatalf("withdrawal detail: %d %s", det.status, det.body)
	}
	// The detail is the existing finance view (it carries the withdrawal's own player_account_id by
	// design); R1b adds only payout_attempt, which must carry none of the forbidden strings.
	for _, sec := range w.secrets {
		if strings.Contains(string(det.body), sec) {
			t.Errorf("withdrawal detail carries reference text %q", sec)
		}
	}
	var d struct {
		PayoutAttempt *payoutAttemptSummaryDTO `json:"payout_attempt"`
	}
	det.decode(t, &d)
	if d.PayoutAttempt == nil || d.PayoutAttempt.AttemptID != w.ambAtt.String() || d.PayoutAttempt.State != "ambiguous" ||
		d.PayoutAttempt.OpenResolutionID == nil || *d.PayoutAttempt.OpenResolutionID != m2dto.ID || d.PayoutAttempt.AttemptCount != 1 {
		t.Fatalf("R1b summary = %+v, want the ambiguous attempt with open resolution %s", d.PayoutAttempt, m2dto.ID)
	}
	pj, _ := json.Marshal(d.PayoutAttempt)
	w.noLeak(t, "payout_attempt summary", pj)
	// A parked payout shows its reason from the closed catalogue.
	det = w.a.do("GET", "/v1/admin/withdrawals/"+w.destWD.String(), w.tok(w.f1), nil)
	det.decode(t, &d)
	if d.PayoutAttempt == nil || d.PayoutAttempt.State != "disputed" || d.PayoutAttempt.TerminalReason == nil || *d.PayoutAttempt.TerminalReason != "destination_mismatch" {
		t.Errorf("R1b destination park = %+v", d.PayoutAttempt)
	}
	// A withdrawal with no attempt: the key is present and null.
	fundWallet(t, w.a.pool, w.tenant, w.brand, w.player, "USD", 1000)
	wr := mustCreateWithdrawalRequest(t, w.a.pool, w.tenant, w.brand, w.player, walletIDFor(t, w.a.pool, w.tenant, w.player, "USD"), "USD", 100)
	det = w.a.do("GET", "/v1/admin/withdrawals/"+wr.ID.String(), w.tok(w.f1), nil)
	if det.status != http.StatusOK || !strings.Contains(string(det.body), `"payout_attempt":null`) {
		t.Errorf("withdrawal without attempt: %d %s", det.status, det.body)
	}
	// R1b stays on withdrawal:review: compliance and platform admin cannot open the detail at all.
	for _, tok := range []string{w.tok(w.comp), w.tok(w.pa)} {
		if res := w.a.do("GET", "/v1/admin/withdrawals/"+w.ambWD.String(), tok, nil); res.status != http.StatusForbidden {
			t.Errorf("withdrawal detail for a non-finance role: %d", res.status)
		}
	}
}

func decodeAttempt(t *testing.T, res cgResp) paymentAttemptDTO {
	t.Helper()
	if res.status != http.StatusOK {
		t.Fatalf("attempt: %d %s", res.status, res.body)
	}
	var d paymentAttemptDTO
	res.decode(t, &d)
	return d
}

func TestStaffReadR1_FiltersAndBadParams(t *testing.T) {
	w := newSRWorld(t)
	tok := w.tok(w.f1)
	count := func(q string) int {
		res := w.a.do("GET", w.attemptsURL(w.tenant, q), tok, nil)
		if res.status != http.StatusOK {
			t.Fatalf("%s: %d %s", q, res.status, res.body)
		}
		return len(decodePage(t, res).Items)
	}
	for q, want := range map[string]int{
		"": 6, "operation=payout": 3, "operation=deposit": 3, "state=disputed": 5, "state=ambiguous": 1, "state=succeeded": 0,
		"terminal_reason=destination_mismatch": 1, "terminal_reason=invalid_provider_reference": 1, "operation=deposit&state=disputed": 3,
		"withdrawal_request_id=" + w.destWD.String(): 1, "withdrawal_request_id=" + uuid.NewString(): 0,
		"operation=deposit&withdrawal_request_id=" + w.destWD.String(): 0,
	} {
		if got := count(q); got != want {
			t.Errorf("filter %q: %d rows, want %d", q, got, want)
		}
	}
	for _, q := range []string{
		"operation=withdrawal", "operation=", "state=bogus", "state=OTHER", "terminal_reason=other", "terminal_reason=made_up", "terminal_reason=",
		"withdrawal_request_id=nope", "withdrawal_request_id=" + strings.ToUpper(uuid.NewString()), "limit=0", "limit=201", "limit=-1", "limit=abc", "limit=",
		"limit=1&limit=2", "cursor=garbage", "cursor=", "foo=bar", "offset=1", "tenant_id=" + w.tenant.String(), "state=disputed&state=pending",
		"cursor=" + rawCursorForTest("v2.1000000."+uuid.NewString()), "provider_reference=abc",
	} {
		res := w.a.do("GET", w.attemptsURL(w.tenant, q), tok, nil)
		if res.status != http.StatusBadRequest {
			t.Errorf("query %q: got %d, want 400: %s", q, res.status, res.body)
		}
		w.noLeak(t, q, res.body)
	}
	for _, p := range []string{"not-a-uuid", uuid.Nil.String()} {
		if res := w.a.do("GET", w.attemptsURL(w.tenant, "")+"/"+p, tok, nil); res.status != http.StatusBadRequest {
			t.Errorf("detail id %q: %d", p, res.status)
		}
	}
	if res := w.a.do("GET", w.attemptsURL(w.tenant, "")+"/"+w.depA[0].String()+"?x=1", tok, nil); res.status != http.StatusBadRequest {
		t.Errorf("detail with a query parameter: %d", res.status)
	}
	// Non-GET methods are not routed (read-only).
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if res := w.a.do(m, w.attemptsURL(w.tenant, ""), tok, map[string]any{}); res.status != http.StatusMethodNotAllowed && res.status != http.StatusNotFound {
			t.Errorf("%s on the list route: %d", m, res.status)
		}
	}
}

func rawCursorForTest(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// --- R1: pagination ---

func TestStaffReadR1_PaginationStabilityAndCursorIsolation(t *testing.T) {
	w := newSRWorld(t)
	tok := w.tok(w.f1)
	collect := func(tenant uuid.UUID, tok string, limit int, between func(page int)) []string {
		var ids []string
		cur := ""
		for page := 0; page < 20; page++ {
			q := fmt.Sprintf("limit=%d", limit)
			if cur != "" {
				q += "&cursor=" + url.QueryEscape(cur)
			}
			res := w.a.do("GET", w.attemptsURL(tenant, q), tok, nil)
			if res.status != http.StatusOK {
				t.Fatalf("page %d: %d %s", page, res.status, res.body)
			}
			pg := decodePage(t, res)
			if len(pg.Items) > limit {
				t.Fatalf("page %d holds %d > limit %d", page, len(pg.Items), limit)
			}
			for _, it := range pg.Items {
				ids = append(ids, it.ID)
			}
			if pg.NextCursor == nil {
				return ids
			}
			if len(pg.Items) != limit {
				t.Fatalf("a non-final page holds %d rows, want %d", len(pg.Items), limit)
			}
			cur = *pg.NextCursor
			if between != nil {
				between(page)
			}
		}
		t.Fatal("pagination did not terminate")
		return nil
	}
	full := collect(w.tenant, tok, 200, nil)
	if len(full) != 6 {
		t.Fatalf("full listing = %d, want 6", len(full))
	}
	// Order: newest first by (created_at, id) descending, verified against the database.
	var dbOrder []string
	if err := w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text FROM payment_attempts WHERE tenant_id = $1 ORDER BY created_at DESC, id DESC`, w.tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			dbOrder = append(dbOrder, s)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(full, ",") != strings.Join(dbOrder, ",") {
		t.Fatalf("listing order %v != (created_at DESC, id DESC) %v", full, dbOrder)
	}
	// limit=2 pages concatenate to exactly the full listing; limits 1 and 4 too.
	for _, lim := range []int{1, 2, 4, 6} {
		if got := collect(w.tenant, tok, lim, nil); strings.Join(got, ",") != strings.Join(full, ",") {
			t.Errorf("limit %d pages = %v, want %v", lim, got, full)
		}
	}
	// A row created between pages never duplicates or skips an existing row (it is newer than the
	// cursor, so it sits before the first page).
	var added uuid.UUID
	got := collect(w.tenant, tok, 2, func(page int) {
		if page == 0 {
			added = w.disputedDeposit()
		}
	})
	if strings.Join(got, ",") != strings.Join(full, ",") {
		t.Errorf("pagination with a concurrent insert = %v, want the original six %v", got, full)
	}
	if now := collect(w.tenant, tok, 200, nil); len(now) != 7 || now[0] != added.String() {
		t.Errorf("after the insert: %v (new row %s must head the listing)", now, added)
	}
	// Cursor isolation: a cursor minted for tenant A used against tenant B positions inside B only.
	resA := w.a.do("GET", w.attemptsURL(w.tenant, "limit=1"), tok, nil)
	curA := *decodePage(t, resA).NextCursor
	resB := w.a.do("GET", w.attemptsURL(w.other, "cursor="+url.QueryEscape(curA)), w.tok(w.otherFin), nil)
	if resB.status != http.StatusOK {
		t.Fatalf("tenant A cursor on tenant B: %d %s", resB.status, resB.body)
	}
	for _, it := range decodePage(t, resB).Items {
		if it.ID != w.depB.String() {
			t.Errorf("tenant B page holds foreign row %s", it.ID)
		}
	}
	for _, id := range full {
		if strings.Contains(string(resB.body), id) && id != w.depB.String() {
			t.Errorf("tenant A attempt %s appears in tenant B's page", id)
		}
	}
	// A cursor older than every row yields an empty last page.
	old := encodePaymentAttemptCursor(paymentAttemptCursor{CreatedAt: time.UnixMicro(1), ID: uuid.New()})
	if pg := decodePage(t, w.a.do("GET", w.attemptsURL(w.tenant, "cursor="+url.QueryEscape(old)), tok, nil)); len(pg.Items) != 0 || pg.NextCursor != nil {
		t.Errorf("cursor older than every row: %+v", pg)
	}
}

// --- R1: platform reads are audited, tenant reads are not; staff ids never leak ---

func TestStaffReadR1_PlatformReadAuditAndNoStaffLeak(t *testing.T) {
	w := newSRWorld(t)
	action := "payment_attempts.platform_read"
	before := w.platformReadAudits(t, w.tenant, action)
	w.a.do("GET", w.attemptsURL(w.tenant, ""), w.tok(w.f1), nil)
	w.a.do("GET", w.attemptsURL(w.tenant, ""), w.tok(w.comp), nil)
	if n := w.platformReadAudits(t, w.tenant, action); n != before {
		t.Fatalf("tenant reads wrote %d platform audit rows", n-before)
	}
	if res := w.a.do("GET", w.attemptsURL(w.tenant, ""), w.tok(w.pa), nil); res.status != http.StatusOK {
		t.Fatalf("platform list: %d %s", res.status, res.body)
	}
	if res := w.a.do("GET", w.attemptsURL(w.tenant, "")+"/"+w.depA[0].String(), w.tok(w.pa), nil); res.status != http.StatusOK {
		t.Fatalf("platform detail: %d", res.status)
	}
	if n := w.platformReadAudits(t, w.tenant, action); n != before+2 {
		t.Fatalf("platform reads wrote %d audit rows, want 2", n-before)
	}
	// The row is tenant-visible (ADR 0104) and names no platform staff id.
	res := w.a.do("GET", "/v1/admin/audit-log/platform-actions", w.tok(w.ta), nil)
	if res.status != http.StatusOK || !strings.Contains(string(res.body), action) {
		t.Fatalf("tenant platform-actions view: %d %s", res.status, res.body)
	}
	for _, id := range w.allStaff {
		if strings.Contains(string(res.body), id.String()) {
			t.Errorf("platform-actions view leaks staff id %s", id)
		}
	}
	// A refused platform caller (a role without the permission) writes nothing: build a platform
	// token with a tenant role is impossible, so use the failing path of a tenant-scoped refusal.
	if res := w.a.do("GET", w.attemptsURL(w.tenant, ""), w.tok(w.ta), nil); res.status != http.StatusForbidden {
		t.Fatalf("tenant_admin: %d", res.status)
	}
	if n := w.platformReadAudits(t, w.tenant, action); n != before+2 {
		t.Fatalf("a refused read wrote an audit row")
	}
}

// An out-of-catalogue terminal reason is reported as `other`, never passed through (the stored
// value is aged onto the row on the scratch database, with the guard trigger disabled there only).
func TestStaffReadR1_UnknownTerminalReasonIsOther(t *testing.T) {
	w := newSRWorld(t)
	const odd = "weird_provider_text_SECRET123"
	if err := w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE payment_attempts DISABLE TRIGGER USER`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_attempts SET terminal_reason = $2 WHERE id = ANY($1)`, []uuid.UUID{w.depA[2], w.destAtt}, odd); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE payment_attempts ENABLE TRIGGER USER`)
		return err
	}); err != nil {
		t.Skipf("cannot age a row on this scratch database: %v", err)
	}
	res := w.a.do("GET", w.attemptsURL(w.tenant, "")+"/"+w.depA[2].String(), w.tok(w.f1), nil)
	d := decodeAttempt(t, res)
	if d.TerminalReason == nil || *d.TerminalReason != "other" || strings.Contains(string(res.body), "SECRET123") {
		t.Errorf("unknown reason rendered as %v: %s", d.TerminalReason, res.body)
	}
	// R1b maps through the same catalogue.
	det := w.a.do("GET", "/v1/admin/withdrawals/"+w.destWD.String(), w.tok(w.f1), nil)
	var wd struct {
		PayoutAttempt *payoutAttemptSummaryDTO `json:"payout_attempt"`
	}
	det.decode(t, &wd)
	if wd.PayoutAttempt == nil || wd.PayoutAttempt.TerminalReason == nil || *wd.PayoutAttempt.TerminalReason != "other" || strings.Contains(string(det.body), "SECRET123") {
		t.Errorf("R1b rendered an unknown reason as %+v: %s", wd.PayoutAttempt, det.body)
	}
}

// --- R2 ---

type srRecon struct {
	runPay, runLedger, runSB uuid.UUID
	attemptKeyed             string
}

// seedRecon inserts one run per stream and mismatches in the exact formats the emitters write.
func (w *srWorld) seedRecon(t *testing.T, tenantID uuid.UUID) srRecon {
	t.Helper()
	var s srRecon
	s.runPay, s.runLedger, s.runSB = uuid.New(), uuid.New(), uuid.New()
	acct, tx1, bet, wal := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	att := w.depA[0].String()
	s.attemptKeyed = att
	secretRef := "SECRET-PSP-REF-9f2"
	err := w.a.pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, r := range []struct {
			id     uuid.UUID
			stream string
		}{{s.runPay, "payment_statement"}, {s.runLedger, "ledger_vs_projection"}, {s.runSB, "sportsbook_settlement"}} {
			if _, err := tx.Exec(ctx, `INSERT INTO reconciliation_runs (id, tenant_id, stream, period_start, period_end, status)
				VALUES ($1, $2, $3, now() - interval '1 day', now(), 'mismatches_found')`, r.id, tenantID, r.stream); err != nil {
				return err
			}
		}
		ins := func(run uuid.UUID, key, exp, act, kind, status string) error {
			_, err := tx.Exec(ctx, `INSERT INTO reconciliation_mismatches (id, tenant_id, reconciliation_run_id, reconciliation_key, expected_value, actual_value, mismatch_kind, investigation_status, resolved_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, CASE WHEN $8 = 'resolved' THEN now() END)`, uuid.New(), tenantID, run, key, exp, act, kind, status)
			return err
		}
		rows := []struct {
			run                     uuid.UUID
			key, exp, act, kind, st string
		}{
			{s.runPay, "provider=mock provider_reference=" + secretRef + " kind=deposit attempt=" + att + " check=amount",
				"platform: attempt=" + att + " op=deposit state=pending amount=5000 asset=USD provider_reference=" + secretRef,
				"statement: line=1 kind=deposit status=ok amount=4999 asset=USD merchant_reference=MREF-SECRET-77", "pay_amount_mismatch", "open"},
			{s.runPay, "provider=mock provider_tx_id=" + secretRef + " type=deposit check=ledger_join",
				"statement: a line for this reference", "ledger: no posting", "pay_missing_platform_record", "investigating"},
			{s.runPay, "provider=mock attempt=" + att + " resolution=" + uuid.NewString() + " check=m4_not_paid_but_paid",
				"declared not paid", "statement shows paid amount=77 asset=USD", "pay_declared_not_paid_but_paid", "resolved"},
			{s.runLedger, acct, "asset=USD type=player_cash debit=5 credit=7", "asset=USD type=player_cash debit=5 credit=6", "balance_mismatch", "open"},
			{s.runLedger, tx1, "an executed ledger_adjustment_requests row linked to this manual_adjustment transaction",
				"no linked executed request (reason_code=x created_at=2026-01-01T00:00:00Z)", "ledger_unlinked_manual_adjustment", "open"},
			{s.runSB, "wallet=" + wal + " asset=USD account_type=player_locked_cash", "open_bet_stakes=500", "ledger_locked_net=300", "sb_locked_mismatch", "open"},
			{s.runSB, "bet=" + bet + " check=end_state status=won", "cash=1 locked=0 house=0", "cash=2 locked=0 house=0", "sb_bet_net_mismatch", "open"},
		}
		for _, r := range rows {
			if err := ins(r.run, r.key, r.exp, r.act, r.kind, r.st); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed reconciliation: %v", err)
	}
	w.secrets = append(w.secrets, secretRef, "MREF-SECRET-77")
	return s
}

func (w *srWorld) reconURL(tenant uuid.UUID, kind, q string) string {
	u := "/v1/admin/tenants/" + tenant.String() + "/reconciliation/" + kind
	if q != "" {
		u += "?" + q
	}
	return u
}

func TestStaffReadR2_PermissionMatrixPerStream(t *testing.T) {
	w := newSRWorld(t)
	w.seedRecon(t, w.tenant)
	toks := []struct {
		name string
		tok  string
	}{
		{"platform_admin", w.tok(w.pa)}, {"tenant_admin", w.tok(w.ta)}, {"compliance", w.tok(w.comp)}, {"finance", w.tok(w.f1)},
		{"risk_manager", w.rmTok}, {"support", w.tok(w.supp)}, {"promotions_manager", w.pmTok}, {"bonus_operations", w.bxTok},
	}
	// ADR 0114 section 3 / D-OPS-2.
	want := map[string]map[string]bool{
		"payment_statement":     {"platform_admin": true, "compliance": true, "finance": true},
		"ledger_vs_projection":  {"platform_admin": true, "compliance": true, "finance": true},
		"sportsbook_settlement": {"tenant_admin": true, "compliance": true, "finance": true},
	}
	for stream, allowed := range want {
		for _, c := range toks {
			for _, kind := range []string{"runs", "mismatches"} {
				res := w.a.do("GET", w.reconURL(w.tenant, kind, "stream="+stream), c.tok, nil)
				exp := http.StatusForbidden
				if allowed[c.name] {
					exp = http.StatusOK
				}
				if res.status != exp {
					t.Errorf("%s %s as %s: got %d, want %d: %s", kind, stream, c.name, res.status, exp, res.body)
				}
				w.noLeak(t, c.name+" "+stream, res.body)
			}
		}
	}
	// The route gate runs before the stream is even parsed: a role holding none of the three
	// permissions is 403 whatever the stream parameter says.
	for _, c := range []struct{ name, tok string }{
		{"support", w.tok(w.supp)}, {"risk_manager", w.rmTok}, {"promotions_manager", w.pmTok}, {"bonus_operations", w.bxTok},
	} {
		for _, q := range []string{"", "stream=bogus", "stream=casino_statement", "stream=payment_statement&limit=0"} {
			for _, kind := range []string{"runs", "mismatches"} {
				if res := w.a.do("GET", w.reconURL(w.tenant, kind, q), c.tok, nil); res.status != http.StatusForbidden {
					t.Errorf("%s %s %q: got %d, want 403", c.name, kind, q, res.status)
				}
			}
		}
	}
	// Unauthenticated, and the casino streams are not served here.
	if res := w.a.do("GET", w.reconURL(w.tenant, "runs", "stream=payment_statement"), "", nil); res.status != http.StatusUnauthorized {
		t.Errorf("unauthenticated: %d", res.status)
	}
	for _, s := range []string{"casino_consistency", "casino_statement", "", "nope", "PAYMENT_STATEMENT"} {
		q := "stream=" + s
		if s == "" {
			q = ""
		}
		for _, kind := range []string{"runs", "mismatches"} {
			if res := w.a.do("GET", w.reconURL(w.tenant, kind, q), w.tok(w.f1), nil); res.status != http.StatusBadRequest {
				t.Errorf("%s stream=%q: got %d, want 400", kind, s, res.status)
			}
		}
	}
	if res := w.a.do("GET", w.reconURL(w.tenant, "runs", "stream=payment_statement&stream=ledger_vs_projection"), w.tok(w.f1), nil); res.status != http.StatusBadRequest {
		t.Errorf("duplicate stream: %d", res.status)
	}
	// Permission is decided before the vocabularies: an unauthorised caller gets 403 even with bad filters.
	if res := w.a.do("GET", w.reconURL(w.tenant, "mismatches", "stream=sportsbook_settlement&status=bogus&kind=bogus"), w.tok(w.pa), nil); res.status != http.StatusForbidden {
		t.Errorf("platform_admin with bad filters on a stream it cannot read: %d, want 403", res.status)
	}
	if res := w.a.do("GET", w.reconURL(w.tenant, "mismatches", "stream=payment_statement&status=bogus"), w.tok(w.ta), nil); res.status != http.StatusForbidden {
		t.Errorf("tenant_admin with bad filters on payment_statement: %d, want 403", res.status)
	}
}

func TestStaffReadR2_CrossTenantAndPlatformAudit(t *testing.T) {
	w := newSRWorld(t)
	w.seedRecon(t, w.tenant)
	w.seedRecon(t, w.other)
	foreign := w.tok(w.otherFin)
	for _, kind := range []string{"runs", "mismatches"} {
		res := w.a.do("GET", w.reconURL(w.tenant, kind, "stream=payment_statement"), foreign, nil)
		if res.status != http.StatusNotFound {
			t.Errorf("cross-tenant %s: %d, want 404", kind, res.status)
		}
	}
	// Each tenant sees only its own rows: ids differ and counts are per tenant.
	idsOf := func(tenant uuid.UUID, tok string) map[string]bool {
		out := map[string]bool{}
		res := w.a.do("GET", w.reconURL(tenant, "mismatches", "stream=payment_statement&limit=200"), tok, nil)
		if res.status != http.StatusOK {
			t.Fatalf("mismatches: %d %s", res.status, res.body)
		}
		var pg pagedResponse[staffReconciliationMismatchDTO]
		res.decode(t, &pg)
		if pg.Total != 3 || len(pg.Items) != 3 {
			t.Fatalf("payment_statement total=%d items=%d, want 3", pg.Total, len(pg.Items))
		}
		for _, it := range pg.Items {
			out[it.ID] = true
		}
		return out
	}
	a, b := idsOf(w.tenant, w.tok(w.f1)), idsOf(w.other, foreign)
	for id := range a {
		if b[id] {
			t.Errorf("mismatch %s visible in both tenants", id)
		}
	}
	// A platform read is audited per call; a tenant read is not.
	action := "reconciliation.platform_read"
	before := w.platformReadAudits(t, w.tenant, action)
	w.a.do("GET", w.reconURL(w.tenant, "runs", "stream=payment_statement"), w.tok(w.f1), nil)
	if w.platformReadAudits(t, w.tenant, action) != before {
		t.Error("a tenant read wrote a platform audit row")
	}
	for _, kind := range []string{"runs", "mismatches"} {
		if res := w.a.do("GET", w.reconURL(w.tenant, kind, "stream=ledger_vs_projection"), w.tok(w.pa), nil); res.status != http.StatusOK {
			t.Fatalf("platform %s: %d", kind, res.status)
		}
	}
	if n := w.platformReadAudits(t, w.tenant, action); n != before+2 {
		t.Errorf("platform reads wrote %d audit rows, want 2", n-before)
	}
	if res := w.a.do("GET", w.reconURL(uuid.New(), "runs", "stream=ledger_vs_projection"), w.tok(w.pa), nil); res.status != http.StatusNotFound {
		t.Errorf("unknown tenant: %d", res.status)
	}
}

func TestStaffReadR2_ShapeSubjectsAndExclusions(t *testing.T) {
	w := newSRWorld(t)
	s := w.seedRecon(t, w.tenant)
	tok := w.tok(w.comp)
	get := func(stream, q string) (pagedResponse[staffReconciliationMismatchDTO], []byte) {
		res := w.a.do("GET", w.reconURL(w.tenant, "mismatches", "stream="+stream+q), tok, nil)
		if res.status != http.StatusOK {
			t.Fatalf("%s: %d %s", stream, res.status, res.body)
		}
		w.noLeak(t, stream, res.body)
		for _, bad := range []string{"reconciliation_key", "expected_value", "actual_value", "MREF-SECRET", "SECRET-PSP"} {
			if strings.Contains(string(res.body), bad) {
				t.Errorf("%s: body contains %q", stream, bad)
			}
		}
		var pg pagedResponse[staffReconciliationMismatchDTO]
		res.decode(t, &pg)
		return pg, res.body
	}
	pay, _ := get("payment_statement", "")
	if pay.Total != 3 {
		t.Fatalf("payment total %d", pay.Total)
	}
	byKind := map[string]staffReconciliationMismatchDTO{}
	for _, it := range pay.Items {
		byKind[it.Kind] = it
		if it.Stream != "payment_statement" {
			t.Errorf("stream = %s", it.Stream)
		}
	}
	am := byKind["pay_amount_mismatch"]
	if am.SubjectRef == nil || am.SubjectRef.Type != "payment_attempt" || am.SubjectRef.ID != s.attemptKeyed {
		t.Errorf("pay_amount_mismatch subject_ref = %+v", am.SubjectRef)
	}
	if am.ExpectedMinorUOM == nil || *am.ExpectedMinorUOM != "5000" || am.ActualMinorUOM == nil || *am.ActualMinorUOM != "4999" || am.AssetCode == nil || *am.AssetCode != "USD" {
		t.Errorf("pay_amount_mismatch amounts = %v/%v asset %v", am.ExpectedMinorUOM, am.ActualMinorUOM, am.AssetCode)
	}
	if mp := byKind["pay_missing_platform_record"]; mp.SubjectRef != nil || mp.ExpectedMinorUOM != nil || mp.Status != "investigating" {
		t.Errorf("provider_tx_id-keyed mismatch must carry no subject or amount: %+v", mp)
	}
	led, _ := get("ledger_vs_projection", "")
	for _, it := range led.Items {
		switch it.Kind {
		case "balance_mismatch":
			if it.SubjectRef == nil || it.SubjectRef.Type != "ledger_account" || it.ExpectedMinorUOM != nil || it.AssetCode == nil || *it.AssetCode != "USD" || it.Exponent == nil || *it.Exponent != 2 {
				t.Errorf("balance_mismatch = %+v", it)
			}
		case "ledger_unlinked_manual_adjustment":
			if it.SubjectRef == nil || it.SubjectRef.Type != "ledger_transaction" {
				t.Errorf("unlinked = %+v", it)
			}
		}
	}
	sb, _ := get("sportsbook_settlement", "")
	for _, it := range sb.Items {
		switch it.Kind {
		case "sb_locked_mismatch":
			if it.SubjectRef == nil || it.SubjectRef.Type != "wallet" || it.ExpectedMinorUOM != nil || it.ActualMinorUOM != nil || it.AssetCode != nil {
				t.Errorf("sb_locked_mismatch = %+v", it)
			}
		case "sb_bet_net_mismatch":
			if it.SubjectRef == nil || it.SubjectRef.Type != "bet" || it.ExpectedMinorUOM != nil {
				t.Errorf("sb_bet_net_mismatch = %+v", it)
			}
		}
	}
	// Filters: status, kind (per stream), subject_id, and their combination.
	if pg, _ := get("payment_statement", "&status=open"); pg.Total != 1 {
		t.Errorf("status=open: %d", pg.Total)
	}
	if pg, _ := get("payment_statement", "&kind=pay_amount_mismatch"); pg.Total != 1 {
		t.Errorf("kind filter: %d", pg.Total)
	}
	if pg, _ := get("payment_statement", "&subject_id="+s.attemptKeyed); pg.Total != 2 {
		t.Errorf("subject_id filter (attempt named by two mismatches): %d", pg.Total)
	}
	if pg, _ := get("payment_statement", "&subject_id="+s.attemptKeyed+"&kind=pay_amount_mismatch&status=open"); pg.Total != 1 {
		t.Errorf("combined filter: %d", pg.Total)
	}
	if pg, _ := get("payment_statement", "&subject_id="+uuid.NewString()); pg.Total != 0 || len(pg.Items) != 0 {
		t.Errorf("unknown subject: %d", pg.Total)
	}
	// Runs.
	res := w.a.do("GET", w.reconURL(w.tenant, "runs", "stream=ledger_vs_projection"), tok, nil)
	var runs pagedResponse[staffReconciliationRunDTO]
	res.decode(t, &runs)
	if runs.Total != 1 || runs.Items[0].ID != s.runLedger.String() || runs.Items[0].Stream != "ledger_vs_projection" || runs.Items[0].Status != "mismatches_found" {
		t.Errorf("runs = %+v", runs)
	}
	// A stream's kind is refused on another stream (the vocabulary is per stream).
	if res := w.a.do("GET", w.reconURL(w.tenant, "mismatches", "stream=ledger_vs_projection&kind=pay_amount_mismatch"), tok, nil); res.status != http.StatusBadRequest {
		t.Errorf("cross-stream kind: %d", res.status)
	}
}

func TestStaffReadR2_BadParams(t *testing.T) {
	w := newSRWorld(t)
	w.seedRecon(t, w.tenant)
	tok := w.tok(w.f1)
	for _, q := range []string{
		"status=bogus", "status=OPEN", "status=other", "kind=bogus", "kind=other", "kind=sb_bet_net_mismatch", "subject_id=nope",
		"subject_id=" + strings.ToUpper(uuid.NewString()), "subject_id=" + uuid.Nil.String(), "limit=0", "limit=201", "limit=x", "offset=-1", "offset=x",
		"limit=1&limit=2", "status=open&status=resolved", "foo=bar", "tenant_id=" + w.tenant.String(), "cursor=abc",
	} {
		res := w.a.do("GET", w.reconURL(w.tenant, "mismatches", "stream=payment_statement&"+q), tok, nil)
		if res.status != http.StatusBadRequest {
			t.Errorf("mismatches %q: %d, want 400: %s", q, res.status, res.body)
		}
	}
	for _, q := range []string{"limit=0", "limit=201", "offset=-1", "status=open", "kind=pay_duplicate", "foo=1"} {
		res := w.a.do("GET", w.reconURL(w.tenant, "runs", "stream=payment_statement&"+q), tok, nil)
		if res.status != http.StatusBadRequest {
			t.Errorf("runs %q: %d, want 400", q, res.status)
		}
	}
	// Pagination: limit/offset walk the list without gaps.
	seen := map[string]bool{}
	for off := 0; off < 3; off++ {
		res := w.a.do("GET", w.reconURL(w.tenant, "mismatches", fmt.Sprintf("stream=payment_statement&limit=1&offset=%d", off)), tok, nil)
		var pg pagedResponse[staffReconciliationMismatchDTO]
		res.decode(t, &pg)
		if pg.Total != 3 || len(pg.Items) != 1 || pg.Limit != 1 || pg.Offset != off {
			t.Fatalf("offset %d page: %+v", off, pg)
		}
		seen[pg.Items[0].ID] = true
	}
	if len(seen) != 3 {
		t.Errorf("offset paging produced %d distinct rows, want 3", len(seen))
	}
	// Read-only: no write verb is routed.
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if res := w.a.do(m, w.reconURL(w.tenant, "mismatches", "stream=payment_statement"), tok, map[string]any{}); res.status != http.StatusMethodNotAllowed && res.status != http.StatusNotFound {
			t.Errorf("%s: %d", m, res.status)
		}
	}
}

// Invariant 7.1: tenant isolation holds in the database even if the handler's own check were
// removed. The same statements the handlers run, issued in tenant B's scope WITHOUT the explicit
// tenant predicate and naming tenant A's rows, return nothing.
func TestStaffReadRoutes_RLSBackstopWithoutHandlerChecks(t *testing.T) {
	w := newSRWorld(t)
	w.seedRecon(t, w.tenant)
	ctx := context.Background()
	subj := uuid.New()
	if code, body, _ := w.request(w.f1, w.depA[0], nil); code != http.StatusCreated {
		t.Fatalf("M1 request: %d %s", code, body)
	}
	for _, c := range []struct{ name, sql string }{
		{"payment_attempts", `SELECT count(*) FROM payment_attempts WHERE id = $1`},
		{"payment_attempts joined to its withdrawal", `SELECT count(*) FROM payment_attempts a LEFT JOIN withdrawal_requests w ON w.id = a.withdrawal_request_id WHERE a.id = $1`},
		{"payment_manual_resolutions", `SELECT count(*) FROM payment_manual_resolutions WHERE attempt_id = $1`},
	} {
		count := func(tenantID uuid.UUID) int {
			var n int
			if err := w.a.pool.WithPrincipalScope(ctx, tenantID, subj, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, c.sql, w.depA[0]).Scan(&n)
			}); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			return n
		}
		if own := count(w.tenant); own != 1 {
			t.Errorf("%s: positive control failed, tenant A sees %d of its own row", c.name, own)
		}
		if foreign := count(w.other); foreign != 0 {
			t.Errorf("%s: tenant B scope sees %d of tenant A's rows without any handler check", c.name, foreign)
		}
	}
	for _, q := range []string{`SELECT count(*) FROM reconciliation_runs`, `SELECT count(*) FROM reconciliation_mismatches`} {
		var nA, nB int
		if err := w.a.pool.WithTenantReadOnly(ctx, w.tenant, func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, q).Scan(&nA) }); err != nil {
			t.Fatal(err)
		}
		if err := w.a.pool.WithTenantReadOnly(ctx, w.other, func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, q).Scan(&nB) }); err != nil {
			t.Fatal(err)
		}
		if nA == 0 || nB != 0 {
			t.Errorf("%s: tenant A sees %d (want > 0), tenant B sees %d (want 0)", q, nA, nB)
		}
	}
}

// A platform read that cannot be audited returns nothing (fail closed): the audit write is
// committed before the tenant data is read. The failure is injected on the scratch database only.
func TestStaffReadRoutes_PlatformReadFailsClosedWhenAuditCannotBeWritten(t *testing.T) {
	w := newSRWorld(t)
	w.seedRecon(t, w.tenant)
	if err := w.a.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE FUNCTION sr_block_platform_reads() RETURNS trigger AS $$
			BEGIN
				IF NEW.action IN ('payment_attempts.platform_read', 'reconciliation.platform_read') THEN
					RAISE EXCEPTION 'injected audit failure';
				END IF;
				RETURN NEW;
			END $$ LANGUAGE plpgsql`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `CREATE TRIGGER sr_block_platform_reads BEFORE INSERT ON audit_log FOR EACH ROW EXECUTE FUNCTION sr_block_platform_reads()`)
		return err
	}); err != nil {
		t.Skipf("cannot inject an audit failure on this scratch database: %v", err)
	}
	pa := w.tok(w.pa)
	for _, p := range []string{
		w.attemptsURL(w.tenant, ""), w.attemptsURL(w.tenant, "") + "/" + w.depA[0].String(),
		w.reconURL(w.tenant, "runs", "stream=ledger_vs_projection"), w.reconURL(w.tenant, "mismatches", "stream=payment_statement"),
	} {
		res := w.a.do("GET", p, pa, nil)
		if res.status != http.StatusInternalServerError {
			t.Errorf("%s: got %d, want 500 when the audit row cannot be written", p, res.status)
		}
		if strings.Contains(string(res.body), w.depA[0].String()) || strings.Contains(string(res.body), "injected") {
			t.Errorf("%s: the failed read returned data or database text: %s", p, res.body)
		}
	}
	// Tenant callers are unaffected (their reads are not platform actions).
	if res := w.a.do("GET", w.attemptsURL(w.tenant, ""), w.tok(w.f1), nil); res.status != http.StatusOK {
		t.Errorf("tenant read: %d", res.status)
	}
}

//go:build integration

package httpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

// B13-B HTTP surface (ADR 0111 2.4): the player binds a payout instrument at POST /v1/me/withdrawals (a
// distinct 409 when missing or unusable, nothing written), and the staff submit body can never influence the
// destination or the rail.

func apiCode(t *testing.T, resp *http.Response) apierror.Code {
	t.Helper()
	defer resp.Body.Close()
	var e apierror.Error
	decodeBody(t, resp, &e)
	return e.Code
}

func TestRequestWithdrawalHandler_B13B_BindingErrors(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newFinancialTestServer(t, pool, issuer, nil)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	other := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	mustActivatePlayer(t, pool, tenant.ID, other.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100_000)
	mustApproveKYCForWithdrawal(t, pool, tenant.ID, brand.ID, player.ID)
	mine := pitest.Bind(t, pool, tenant.ID, player.ID, "EUR")
	theirs := pitest.Bind(t, pool, tenant.ID, other.ID, "EUR")

	rows := func() int {
		return countRows(t, pool, tenant.ID, `SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1`, tenant.ID)
	}
	ledger := func() int {
		return countRows(t, pool, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenant.ID)
	}
	base := func(key string) map[string]any {
		return map[string]any{"asset_code": "EUR", "amount": 500, "idempotency_key": key}
	}
	cases := []struct {
		name   string
		body   map[string]any
		status int
		code   apierror.Code
	}{
		{"missing instrument", base("k-missing"), http.StatusConflict, apierror.CodePayoutInstrumentRequired},
		{"malformed instrument id", withInst(base("k-bad"), "not-a-uuid"), http.StatusBadRequest, apierror.CodeValidation},
		{"unknown instrument", withInst(base("k-unknown"), uuid.NewString()), http.StatusConflict, apierror.CodePayoutInstrumentNotUsable},
		{"another player's instrument", withInst(base("k-theirs"), theirs.String()), http.StatusConflict, apierror.CodePayoutInstrumentNotUsable},
	}
	for _, c := range cases {
		r0, l0 := rows(), ledger()
		resp := postJSON(t, srv, "/v1/me/withdrawals", player.Tokens.AccessToken, c.body)
		if resp.StatusCode != c.status {
			t.Fatalf("%s: status %d, want %d", c.name, resp.StatusCode, c.status)
		}
		if got := apiCode(t, resp); got != c.code {
			t.Fatalf("%s: code %s, want %s", c.name, got, c.code)
		}
		if rows() != r0 || ledger() != l0 {
			t.Fatalf("%s: a refusal must write nothing", c.name)
		}
	}
	// The refusal did not consume the key; the same key now succeeds and the body carries no destination data.
	resp := postJSON(t, srv, "/v1/me/withdrawals", player.Tokens.AccessToken, withInst(base("k-theirs"), mine.String()))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("bind own instrument: status %d", resp.StatusCode)
	}
	var raw strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		raw.Write(buf[:n])
		if err != nil {
			break
		}
	}
	_ = resp.Body.Close()
	for _, leak := range []string{"fingerprint", "label", "payout_instrument", mine.String()} {
		if strings.Contains(strings.ToLower(raw.String()), strings.ToLower(leak)) {
			t.Fatalf("the player response leaks %q: %s", leak, raw.String())
		}
	}
	var fp *string
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT payout_instrument_fingerprint FROM withdrawal_requests WHERE tenant_id = $1`, tenant.ID).Scan(&fp)
	}); err != nil || fp == nil {
		t.Fatalf("the request must be bound: %v %v", fp, err)
	}
}

func withInst(m map[string]any, id string) map[string]any {
	m["payout_instrument_id"] = id
	return m
}

// Without the payout-instrument service configured (no keys) a withdrawal request is a 503 and writes nothing.
func TestRequestWithdrawalHandler_B13B_NoServiceFailsClosed(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newFinancialTestServer(t, pool, issuer, nil)
	// A second server over the same DB with the service removed.
	noSvc := newFinancialTestServerWith(t, pool, issuer, nil, func(d *Deps) { d.PayoutInstruments = nil })
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100_000)
	mustApproveKYCForWithdrawal(t, pool, tenant.ID, brand.ID, player.ID)
	inst := pitest.Bind(t, pool, tenant.ID, player.ID, "EUR")
	resp := postJSON(t, noSvc, "/v1/me/withdrawals", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": 500, "idempotency_key": "k-nosvc", "payout_instrument_id": inst.String(),
	})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1`, tenant.ID); n != 0 {
		t.Fatalf("rows = %d", n)
	}
}

// Staff submit: the body cannot influence the rail or the destination.
func TestSubmitWithdrawalHandler_B13B_BodyCannotInfluenceTheDestination(t *testing.T) {
	f := setupStage3CResolutionFixture(t, 5000)
	count := func(q string, a ...any) int { return countRows(t, f.pool, f.tenant.ID, q, a...) }

	// A differing payment_method: 400 PAYMENT_METHOD_MISMATCH, nothing changed.
	resp := postJSON(t, f.srv, "/v1/admin/withdrawals/"+f.withdrawalID+"/submit", f.financeToken.AccessToken, map[string]string{"payment_method": "card"})
	if resp.StatusCode != http.StatusBadRequest || apiCode(t, resp) != apierror.CodePaymentMethodMismatch {
		t.Fatalf("a body naming another method must be 400 PAYMENT_METHOD_MISMATCH")
	}
	if count(`SELECT count(*) FROM withdrawal_requests WHERE id = $1 AND state = 'approved'`, f.withdrawalID) != 1 ||
		count(`SELECT count(*) FROM payment_attempts WHERE withdrawal_request_id = $1`, f.withdrawalID) != 0 {
		t.Fatal("a mismatching body must change nothing")
	}
	// Unknown destination-ish fields are ignored by the decoder's contract? They are not read: an extra field
	// cannot carry a destination into the route (the struct has one field).
	// An EMPTY body works: the rail is the instrument's.
	resp = postJSON(t, f.srv, "/v1/admin/withdrawals/"+f.withdrawalID+"/submit", f.financeToken.AccessToken, map[string]string{})
	if resp.StatusCode != http.StatusOK {
		var e apierror.Error
		decodeBody(t, resp, &e)
		t.Fatalf("empty body: status %d %+v", resp.StatusCode, e)
	}
	_ = resp.Body.Close()
	if count(`SELECT count(*) FROM payout_attempt_destination_snapshots s JOIN payment_attempts a ON a.id = s.attempt_id WHERE a.withdrawal_request_id = $1`, f.withdrawalID) != 1 {
		t.Fatal("the submit must have written the destination snapshot")
	}
	if count(`SELECT count(*) FROM payment_attempts WHERE withdrawal_request_id = $1 AND payment_method = 'bank_transfer'`, f.withdrawalID) != 1 {
		t.Fatal("the attempt's method must be the instrument's rail")
	}
}

// Staff submit: an unusable destination is a 409 PAYOUT_DESTINATION_NOT_USABLE, the request stays approved, and
// nothing is dispatched.
func TestSubmitWithdrawalHandler_B13B_UnusableDestination(t *testing.T) {
	f := setupStage3CResolutionFixture(t, 5000)
	var instrumentID uuid.UUID
	if err := f.pool.WithTenant(context.Background(), f.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT payout_instrument_id FROM withdrawal_requests WHERE id = $1`, f.withdrawalID).Scan(&instrumentID)
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.WithTenant(context.Background(), f.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := pitest.Shared().Suspend(ctx, tx, payoutinstrument.BlockParams{TenantID: f.tenant.ID, InstrumentID: instrumentID,
			Actor: payoutinstrument.Actor{Type: payoutinstrument.ActorStaff, ID: uuid.NewString()}, ReasonCode: "aml_review"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ledger := countRows(t, f.pool, f.tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, f.tenant.ID)
	resp := postJSON(t, f.srv, "/v1/admin/withdrawals/"+f.withdrawalID+"/submit", f.financeToken.AccessToken, map[string]string{"payment_method": "bank_transfer"})
	if resp.StatusCode != http.StatusConflict || apiCode(t, resp) != apierror.CodePayoutDestinationNotUsable {
		t.Fatalf("want 409 PAYOUT_DESTINATION_NOT_USABLE")
	}
	if countRows(t, f.pool, f.tenant.ID, `SELECT count(*) FROM withdrawal_requests WHERE id = $1 AND state = 'approved'`, f.withdrawalID) != 1 ||
		countRows(t, f.pool, f.tenant.ID, `SELECT count(*) FROM payment_attempts WHERE withdrawal_request_id = $1`, f.withdrawalID) != 0 ||
		countRows(t, f.pool, f.tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, f.tenant.ID) != ledger {
		t.Fatal("a refused destination must leave the request approved with no attempt and no posting")
	}
	if f.mock.AttemptCount() != 0 {
		t.Fatal("the provider must not be called")
	}
}

// Security L-2: a seal/integrity refusal at request time (a tampered instrument) raises a detached P1; the request
// still writes nothing and answers the same generic 409.
func TestRequestWithdrawalHandler_B13B_IntegrityRefusalRaisesAP1(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newFinancialTestServer(t, pool, issuer, nil)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100_000)
	mustApproveKYCForWithdrawal(t, pool, tenant.ID, brand.ID, player.ID)
	inst := pitest.Bind(t, pool, tenant.ID, player.ID, "EUR")
	post := func(key string) *http.Response {
		return postJSON(t, srv, "/v1/me/withdrawals", player.Tokens.AccessToken, map[string]any{
			"asset_code": "EUR", "amount": 500, "idempotency_key": key, "payout_instrument_id": inst.String(),
		})
	}
	// An ordinary refusal (suspended) raises nothing.
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := pitest.Shared().Suspend(ctx, tx, payoutinstrument.BlockParams{TenantID: tenant.ID, InstrumentID: inst,
			Actor: payoutinstrument.Actor{Type: payoutinstrument.ActorStaff, ID: uuid.NewString()}, ReasonCode: "aml_review"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if resp := post("k-l2-a"); resp.StatusCode != http.StatusConflict || apiCode(t, resp) != apierror.CodePayoutInstrumentNotUsable {
		t.Fatal("want 409 NOT_USABLE")
	}
	if rows := alertinject.ForSubject(t, pool, tenant.ID); len(rows) != 0 {
		t.Fatalf("an ordinary refusal must raise nothing: %+v", rows)
	}
	// Tamper a sealed column (owner, triggers off for one statement): integrity refusal.
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		for _, q := range []string{`ALTER TABLE payout_instruments DISABLE TRIGGER USER`, `ALTER TABLE payout_instruments NO FORCE ROW LEVEL SECURITY`,
			`UPDATE payout_instruments SET display_mask = 'XX****0000' WHERE id = '` + inst.String() + `'`,
			`ALTER TABLE payout_instruments FORCE ROW LEVEL SECURITY`, `ALTER TABLE payout_instruments ENABLE TRIGGER USER`} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if resp := post("k-l2-b"); resp.StatusCode != http.StatusConflict || apiCode(t, resp) != apierror.CodePayoutInstrumentNotUsable {
		t.Fatal("want the same generic 409")
	}
	want := "payout_instrument:" + inst.String() + ":reason:destination_integrity:" + payoutinstrument.ReasonSealInvalid
	found := false
	for _, r := range alertinject.ForSubject(t, pool, tenant.ID) {
		if r.Discriminator == want && r.Severity == "p1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no P1 %s", want)
	}
	if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1`, tenant.ID); n != 0 {
		t.Fatalf("rows = %d", n)
	}
}

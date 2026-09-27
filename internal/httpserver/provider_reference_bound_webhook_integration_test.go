//go:build integration

// PROVIDER-REF-BOUND-1 (PRH-REF): HTTP-level contract of the provider-
// reference bound on the casino and payments webhooks.
//   - A VERIFIED callback whose provider reference exceeds the platform
//     bound gets a deterministic 400 validation_error "callback rejected"
//     (never a retryable 5xx, never the uniform pre-verification 401).
//   - Exactly one allow-listed WARN line records it with the byte length
//     and a SHA-256 prefix - and NO captured log line, from any logger
//     call in the request, contains any part of the oversize value (no
//     log amplification).
//   - Nothing is written: no ledger transaction, no rejection row.
package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/providerref"
)

// oversizeRef is 4 KiB: well past the ~2.7 KB btree limit that made this
// a generic retryable 500 before PROVIDER-REF-BOUND-1. The marker makes
// any leak greppable.
func oversizeRef() string {
	return "LEAKMARKER" + strings.Repeat("Q", 4096)
}

func assertRefRejectionLogged(t *testing.T, lines []capturedLogLine, event, value, tenantID string) {
	t.Helper()
	var found []capturedLogLine
	for _, l := range lines {
		if l.msg == event {
			found = append(found, l)
		}
		for k, v := range l.attrs {
			s := fmt.Sprint(v)
			if strings.Contains(s, "LEAKMARKER") || strings.Contains(s, "QQQQQQQQ") {
				t.Fatalf("log line %q field %q leaks the oversize reference (%d bytes)", l.msg, k, len(s))
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one %s line, got %d", event, len(found))
	}
	line := found[0]
	allowed := map[string]bool{"provider_id": true, "tenant_id": true, "request_id": true,
		"ref_field": true, "ref_reason": true, "ref_len": true, "ref_sha256_prefix": true}
	for k := range line.attrs {
		if !allowed[k] {
			t.Errorf("%s carries a non-allow-listed field %q", event, k)
		}
	}
	if got := fmt.Sprint(line.attrs["ref_len"]); got != fmt.Sprint(len(value)) {
		t.Errorf("ref_len: got %s want %d", got, len(value))
	}
	if got, _ := line.attrs["ref_sha256_prefix"].(string); got != providerref.Fingerprint(value) {
		t.Errorf("ref_sha256_prefix: got %q want %q", got, providerref.Fingerprint(value))
	}
	if got, _ := line.attrs["ref_reason"].(string); got != string(providerref.ReasonTooLong) {
		t.Errorf("ref_reason: got %q", got)
	}
	if got, _ := line.attrs["tenant_id"].(string); got != tenantID {
		t.Errorf("tenant_id: got %q want %q", got, tenantID)
	}
}

func TestCasinoWebhook_OversizeProviderReference_400_LogBounded_NothingWritten(t *testing.T) {
	pool, issuer := testEnv(t)
	logger, captured := newCapturingLogger()
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServerWithLogger(t, pool, issuer, orchestrator, logger)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	mustEnableCasinoCapability(t, srv, pool, tenant)
	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	sessionID := uuid.MustParse(launched.SessionID)

	countRows := func() (ledgerTx, rejections int) {
		if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions`).Scan(&ledgerTx); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM casino_callback_rejections`).Scan(&rejections)
		}); err != nil {
			t.Fatalf("count: %v", err)
		}
		return
	}
	ledgerBefore, rejBefore := countRows()

	ref := oversizeRef()
	payload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, ref, "", "round-prhref", game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payload)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected a deterministic 400, got %d", resp.StatusCode)
	}
	var body apierror.Error
	decodeBody(t, resp, &body)
	if body.Code != apierror.CodeValidation || body.Message != "callback rejected" {
		t.Fatalf("expected validation_error/\"callback rejected\", got %+v", body)
	}
	assertRefRejectionLogged(t, captured(), "casino_webhook_provider_reference_rejected", ref, tenant.ID.String())
	if l, r := countRows(); l != ledgerBefore || r != rejBefore {
		t.Fatalf("rows written: ledger %d->%d, rejections %d->%d", ledgerBefore, l, rejBefore, r)
	}
}

// TestCasinoPlayRollback_OversizeOriginalReference_400: the test-support
// play rollback takes original_provider_tx_id from the caller; it is
// bounded before any database work with a fixed message.
func TestCasinoPlayRollback_OversizeOriginalReference_400(t *testing.T) {
	pool, issuer := testEnv(t)
	logger, captured := newCapturingLogger()
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServerWithLogger(t, pool, issuer, orchestrator, logger)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	mustEnableCasinoCapability(t, srv, pool, tenant)
	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")

	ref := oversizeRef()
	resp := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/rollback", player.Tokens.AccessToken,
		map[string]string{"original_provider_tx_id": ref})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	var body apierror.Error
	decodeBody(t, resp, &body)
	if body.Message != "original_provider_tx_id is not a valid provider reference" {
		t.Fatalf("expected the fixed provider-reference message, got %+v", body)
	}
	for _, l := range captured() {
		for k, v := range l.attrs {
			if strings.Contains(fmt.Sprint(v), "LEAKMARKER") {
				t.Fatalf("log line %q field %q leaks the oversize reference", l.msg, k)
			}
		}
	}
}

func TestPaymentWebhook_OversizeProviderReference_400_LogBounded_NothingWritten(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	logger, captured := newCapturingLogger()
	srv := newFinancialTestServerWithLogger(t, pool, issuer, orchestrator, logger)

	tenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	ref := oversizeRef()
	payload := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, ref, "", payments.OutcomeSucceeded, 5000, "EUR", "", false)
	resp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", payload)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected a deterministic 400, got %d", resp.StatusCode)
	}
	var body apierror.Error
	decodeBody(t, resp, &body)
	if body.Code != apierror.CodeValidation || body.Message != "callback rejected" {
		t.Fatalf("expected validation_error/\"callback rejected\", got %+v", body)
	}
	assertRefRejectionLogged(t, captured(), "payment_webhook_provider_reference_rejected", ref, tenant.ID.String())
	var ledgerTx int
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions`).Scan(&ledgerTx)
	}); err != nil || ledgerTx != 0 {
		t.Fatalf("no ledger transaction may exist: %d (%v)", ledgerTx, err)
	}
}

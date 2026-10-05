//go:build integration

package httpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/alerting/alertingtest"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

// ALERT-DELIVERY-1 post-commit guarantee (ledger-finance): delivery runs
// strictly after commit, in its own short transactions, on its own goroutine
// path. A failing, hanging or panicking channel can neither roll back nor
// alter a committed ledger transaction, and the ledger invariant
// SUM(debits) = SUM(credits) is unchanged.
func TestAlertDelivery_ChannelFailureHangOrPanicCannotAffectACommittedLedgerTransaction(t *testing.T) {
	w := alertingtest.NewWorld(t, "arpc_")
	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": testJWTSecret})
	if err != nil {
		t.Fatal(err)
	}
	issuer := auth.NewIssuer(keys, "platform-api-test", "platform-api-test")
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServer(t, w.Pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, w.Pool)
	brand := mustCreateBrand(t, w.Pool, tenant)
	mustRegisterCapability(t, w.Pool, tenant.ID, mockProvider)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, w.Pool, tenant.ID, player.ID)

	const amount int64 = 15000
	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": amount, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("initiate deposit: %d", resp.StatusCode)
	}
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	ref := strings.TrimPrefix(intent.RedirectURL, "https://mock-psp.invalid/pay/")
	cb := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
		mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, ref, "", payments.OutcomeSucceeded, amount, "EUR", "", false))
	if cb.StatusCode != http.StatusOK {
		t.Fatalf("deposit callback: %d", cb.StatusCode)
	}
	_ = cb.Body.Close()

	type ledger struct {
		txs, entries  int64
		debit, credit string
	}
	snapshot := func() ledger {
		var l ledger
		if err := w.Pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				SELECT (SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1),
				       (SELECT count(*) FROM ledger_entries WHERE tenant_id = $1),
				       COALESCE((SELECT SUM(amount) FILTER (WHERE direction = 'debit') FROM ledger_entries WHERE tenant_id = $1), 0)::text,
				       COALESCE((SELECT SUM(amount) FILTER (WHERE direction = 'credit') FROM ledger_entries WHERE tenant_id = $1), 0)::text`,
				tenant.ID).Scan(&l.txs, &l.entries, &l.debit, &l.credit)
		}); err != nil {
			t.Fatalf("ledger snapshot: %v", err)
		}
		return l
	}
	before := snapshot()
	if before.txs == 0 || before.entries == 0 || before.debit != before.credit {
		t.Fatalf("precondition: a committed, balanced deposit posting is required: %+v", before)
	}

	admin := w.SeedAdmin()
	w.AddRoute(admin, alerting.SeverityP2, 0, alerting.ChannelMock, "mock-target", true)
	for _, mode := range []string{"fails", "panics", "hangs"} {
		t.Run(mode, func(t *testing.T) {
			id := w.SeedAlert(tenant.ID, alerting.KindPaymentKillSwitchEngaged, "d:"+uuid.NewString())
			ch := alertingtest.NewRecordingChannel()
			switch mode {
			case "fails":
				ch.FailWith(alerting.ErrorClassUnavailable)
			case "panics":
				ch.PanicOnce("channel exploded")
			case "hangs":
				ch.BlockUntilCtxDone()
			}
			d := alerting.NewDispatcher(w.Pool, alerting.DispatcherConfig{Backoff: func(int) time.Duration { return 0 }, ClaimLease: 300 * time.Millisecond}, ch)
			if err := d.RunOnce(context.Background()); err != nil {
				t.Fatalf("a bad channel must not fail the pass: %v", err)
			}
			// Whatever happened to the delivery, nothing is stranded mid-claim
			// (an earlier subtest's retrying alert may consume a one-shot panic).
			if ev, _ := w.LatestEvent(admin, id); ev == "claimed" || ev == "" {
				t.Fatalf("delivery left in state %q", ev)
			}
			if len(ch.Calls()) == 0 {
				t.Fatal("the channel was never exercised")
			}
			if after := snapshot(); after != before {
				t.Fatalf("the committed ledger changed: before %+v after %+v", before, after)
			}
		})
	}
	// The wallet still shows the deposit.
	r := getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
	var wallet walletSummaryResponse
	decodeBody(t, r, &wallet)
	if wallet.CashBalance != amount {
		t.Fatalf("wallet cash balance %d, want %d", wallet.CashBalance, amount)
	}
}

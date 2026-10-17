//go:build integration

// PAY-DEPOSIT-MISMATCH-ALERT-1 through the REAL webhook handler path: a verified provider success whose
// amount contradicts an already-DECLINED / already-SUCCEEDED deposit returns the same uniform 200,
// changes no state, posts nothing, and raises ONE P1 carrying codes/ids only.
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
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

func TestWebhook_DepositMismatchedSuccess_DeclinedAndSucceeded_RaiseOneP1(t *testing.T) {
	for _, c := range []struct {
		name    string
		amount  int64
		settle  bool
		state   string
		reason  string
		postsTx int
	}{
		{"declined", payments.MockAmountPlayerDeclineNoCascade, false, "declined", "mismatched_success_on_declined_deposit", 0},
		{"succeeded", 6000, true, "succeeded", "mismatched_success_on_succeeded_deposit", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool, issuer := testEnv(t)
			orchestrator, mockProvider := newMockOrchestrator()
			srv := newFinancialTestServer(t, pool, issuer, orchestrator)
			tenant := mustCreateTenant(t, pool)
			brand := mustCreateBrand(t, pool, tenant)
			mustRegisterCapability(t, pool, tenant.ID, mockProvider)
			player := mustRegisterPlayer(t, srv, brand.Slug)
			mustActivatePlayer(t, pool, tenant.ID, player.ID)

			r := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
				"asset_code": "EUR", "amount": c.amount, "payment_method": "card", "idempotency_key": "dm-wh-" + uuid.NewString(),
			})
			if r.StatusCode != http.StatusCreated {
				t.Fatalf("initiate deposit: %d", r.StatusCode)
			}
			var intent depositIntentResponse
			decodeBody(t, r, &intent)

			var ref string
			var attemptID uuid.UUID
			readAttempt := func() (state string, txs int) {
				if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
					if err := tx.QueryRow(ctx, `SELECT id, state, coalesce(provider_reference,'') FROM payment_attempts WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT 1`, tenant.ID).Scan(&attemptID, &state, &ref); err != nil {
						return err
					}
					return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id=$1 AND provider_tx_id=$2`, tenant.ID, ref).Scan(&txs)
				}); err != nil {
					t.Fatalf("read back: %v", err)
				}
				return
			}
			post := func(amount int64) {
				t.Helper()
				payload := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, ref, "", payments.OutcomeSucceeded, amount, "EUR", "", false)
				resp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", payload)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("callback status %d, want the uniform 200", resp.StatusCode)
				}
				resp.Body.Close()
			}
			readAttempt()
			if c.settle {
				post(c.amount)
			}
			state, txsBefore := readAttempt()
			if state != c.state || txsBefore != c.postsTx {
				t.Fatalf("setup: state=%s postings=%d", state, txsBefore)
			}
			if rows := alertinject.ForSubject(t, pool, tenant.ID); len(rows) != 0 {
				t.Fatalf("normal path must raise nothing: %+v", rows)
			}

			post(c.amount - 1)
			post(c.amount - 1) // provider redelivery: same alert row

			state, txs := readAttempt()
			if state != c.state || txs != c.postsTx {
				t.Fatalf("raise only: state=%s postings=%d", state, txs)
			}
			rows := alertinject.WaitUpTo(t, pool, tenant.ID, 5*time.Second, func(rs []alertinject.Row) bool { return len(rs) > 0 })
			if len(rows) != 1 {
				t.Fatalf("want ONE alert, got %+v", rows)
			}
			a := rows[0]
			if a.Kind != string(alerting.KindPaymentWebhookIntegrity) || a.Severity != "p1" ||
				a.Discriminator != "attempt:"+attemptID.String()+":reason:"+c.reason {
				t.Fatalf("alert = %+v", a)
			}
			if _, ok := a.Attributes["provider_id"]; !ok || len(a.Attributes) != 1 {
				t.Fatalf("attributes must be provider_id only: %+v", a.Attributes)
			}
			for _, v := range a.Attributes {
				if s, _ := v.(string); strings.Contains(s, "@") || strings.Contains(s, ref) {
					t.Fatalf("attribute leaks data: %v", v)
				}
			}
		})
	}
}

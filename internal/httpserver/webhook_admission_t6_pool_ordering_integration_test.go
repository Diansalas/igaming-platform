//go:build integration

// ADR 0097 T6's own explicit requirement (security review §4, "It must
// kill a 'B1/B2 moved inside WithTenant' mutation directly, not only
// through T2b"): a B1-limited verified-tier callback must never acquire a
// real database connection at all - proof that admitVerified genuinely
// runs BEFORE deps.DB.WithTenant opens the domain transaction, not merely
// that no ROW gets written (T6a/e already prove that; this proves no
// CONNECTION is even taken).
//
// Uses the real, production-sized 10-connection pool (phasecapture.Pool10)
// and pool.Raw().Stat().AcquireCount(), exactly like
// webhook_admission_dbgate_integration_test.go's T4, but driven through
// the real HTTP handler (not gatedReader directly) so it also independently
// corroborates admitVerified's call-site ordering in deposit_handlers.go.
package httpserver

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/testsupport/phasecapture"
)

func TestAdmission_T6g_B1RunsBeforeWithTenant_NoPoolAcquisitionOnLimitedAttempt(t *testing.T) {
	pool := phasecapture.Pool10(t, "TEST_DATABASE_URL")
	_, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	settings := t6Settings()
	settings.VerifiedRate["payments"] = WebhookRateBurst{Rate: 10, Burst: 1}
	srv := newAdmissionTestServer(t, pool, issuer, orchestrator, nil, settings, false)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	createDeposit := func(amount int64) string {
		resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
			"asset_code": "EUR", "amount": amount, "payment_method": "card", "idempotency_key": uuid.NewString(),
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201, got %d", resp.StatusCode)
		}
		var intent depositIntentResponse
		decodeBody(t, resp, &intent)
		return providerReferenceFromRedirectURL(intent.RedirectURL)
	}

	ref1 := createDeposit(1000)
	ref2 := createDeposit(2000)
	ref3 := createDeposit(3000)

	// Both an ADMITTED and a LIMITED callback still run the shared
	// preamble (tenant-slug lookup) and VerifyCallback's own A4b-gated
	// read - those are legitimate, A4b-gated pool acquisitions that
	// happen for EVERY request regardless of B1's outcome, so a raw
	// "AcquireCount must not grow at all" assertion would be wrong (and
	// was the bug in an earlier draft of this test). The actual property
	// under test - B1 runs strictly BEFORE deps.DB.WithTenant, never
	// inside it - shows up as a STRICTLY SMALLER acquisition delta for a
	// limited attempt than for an admitted one (the difference being
	// exactly WithTenant's own connection): if a "B1 moved inside
	// WithTenant" mutation is applied, a limited attempt would still open
	// (and roll back) the domain transaction, making the two deltas equal.
	measure := func(ref string, amount int64, wantOK bool) int64 {
		before := pool.Raw().Stat().AcquireCount()
		resp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
			mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, ref, "", payments.OutcomeSucceeded, amount, "EUR", "", false))
		resp.Body.Close()
		gotOK := resp.StatusCode == http.StatusOK
		if gotOK != wantOK {
			t.Fatalf("ref %s: expected ok=%v, got status %d", ref, wantOK, resp.StatusCode)
		}
		return pool.Raw().Stat().AcquireCount() - before
	}

	// Warmup 1 (ref1) consumes B1's single burst token - admitted, so its
	// delta includes WithTenant's own connection.
	admittedDelta := measure(ref1, 1000, true)

	// ref2 is now B1-limited - zero rows, and (this test's own point) a
	// STRICTLY SMALLER acquisition delta than the admitted case.
	limitedDelta := measure(ref2, 2000, false)
	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, ref2); got != 0 {
		t.Fatalf("a limited deposit must post ZERO rows: got %d", got)
	}
	if limitedDelta >= admittedDelta {
		t.Fatalf("a B1-limited callback's pool-acquisition delta (%d) was not strictly less than an "+
			"admitted callback's (%d) - admitVerified must run strictly BEFORE deps.DB.WithTenant, "+
			"never inside it (security review §4, T6's own pool-acquisition requirement)",
			limitedDelta, admittedDelta)
	}

	// ref3, another distinct deposit, is ALSO B1-limited (B1's token is
	// still exhausted) - confirms the smaller delta is not a one-off.
	limitedDelta2 := measure(ref3, 3000, false)
	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, ref3); got != 0 {
		t.Fatalf("a second limited deposit must also post ZERO rows: got %d", got)
	}
	if limitedDelta2 >= admittedDelta {
		t.Fatalf("second B1-limited callback's pool-acquisition delta (%d) was not strictly less than "+
			"the admitted callback's (%d)", limitedDelta2, admittedDelta)
	}
}

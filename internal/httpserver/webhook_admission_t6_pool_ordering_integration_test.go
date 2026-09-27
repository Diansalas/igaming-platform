//go:build integration

// ADR 0097 T6's own explicit requirement (security review §4, "It must
// kill a 'B1/B2 moved inside WithTenant' mutation directly, not only
// through T2b"): a B1-limited verified-tier callback must never acquire a
// real database connection at all - proof that admitVerified genuinely
// runs BEFORE deps.DB.WithTenant opens the domain transaction, not merely
// that no ROW gets written (T6a/e already prove that; this proves no
// CONNECTION is even taken).
//
// Security re-verification #2 (rv-prh-i4-security.md §7.4/§7.5, "T6:
// OPEN, narrowed"): the round-4 version of this test covered payments
// only - casino and KYC survived the identical B1-inside-WithTenant
// mutation untouched. This version is table-driven across all three
// domains (payments, casino, kyc), each with its own setup (payments
// needs a real deposit intent; casino needs a launched game/session;
// kyc needs neither), and - per security's Info I5 - pins the EXACT
// expected acquisition delta for an admitted vs. a limited attempt in
// each domain, rather than a bare "limited < admitted" (which would
// survive a future change that adds an unrelated acquisition to the
// admitted path only, silently widening the margin and hiding a real
// regression). The exact numbers were measured empirically against this
// same real 10-connection pool.
//
// Security re-verification #3 (rv-prh-i4-security.md §8.3, Info I8): the
// composition below was corrected. The pinned numbers INCLUDE the
// admitted path's deps.DB.WithTenant acquisition, and credential
// resolution makes NO separate pool acquisition in this harness, because
// it uses MOCK webhook credentials (the resolver never touches the pool
// at all) - never "tenant-slug lookup, ProviderAcceptsWebhook and
// credential resolution" as an earlier draft of this comment wrongly
// said. The correct composition:
//   - payments: 3 admitted (tenant-slug lookup, ProviderAcceptsWebhook,
//     then deps.DB.WithTenant), 2 limited (the same two gated reads,
//     minus WithTenant, since B1 rejects before it opens).
//   - casino: 2 admitted (tenant-slug lookup, then deps.DB.WithTenant -
//     casino's VerifyCallback has no ProviderAcceptsWebhook-style first
//     read), 1 limited (the slug lookup alone).
//   - kyc: 2 admitted, 1 limited (same shape as casino).
//
// Uses the real, production-sized 10-connection pool (phasecapture.Pool10)
// and pool.Raw().Stat().AcquireCount(), exactly like
// webhook_admission_dbgate_integration_test.go's T4, but driven through
// the real HTTP handler (not gatedReader directly) so it also independently
// corroborates admitVerified's call-site ordering in each of the three
// domain handlers.
package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/testsupport/phasecapture"
)

// TestAdmission_T6g_B1RunsBeforeWithTenant_ExactPoolAcquisitionCounts is
// table-driven across all three webhook domains (security review §7.5's
// explicit requirement: "extend T6g (table-driven by domain) to casino
// and KYC"). Each subtest measures pool.Raw().Stat().AcquireCount()'s
// delta for one ADMITTED (passes B1) and one LIMITED (rejected by B1)
// callback and asserts the EXACT expected values (security Info I5) -
// never a bare inequality.
func TestAdmission_T6g_B1RunsBeforeWithTenant_ExactPoolAcquisitionCounts(t *testing.T) {
	t.Run("payments", func(t *testing.T) {
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

		measure := func(ref string, amount int64, wantStatus int) int64 {
			before := pool.Raw().Stat().AcquireCount()
			resp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
				mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, ref, "", payments.OutcomeSucceeded, amount, "EUR", "", false))
			resp.Body.Close()
			if resp.StatusCode != wantStatus {
				t.Fatalf("ref %s: expected status %d, got %d", ref, wantStatus, resp.StatusCode)
			}
			return pool.Raw().Stat().AcquireCount() - before
		}

		// Warmup (ref1) consumes B1's single burst token - admitted (200).
		const wantAdmittedDelta = int64(3)
		if got := measure(ref1, 1000, http.StatusOK); got != wantAdmittedDelta {
			t.Fatalf("payments: admitted acquisition delta = %d, want exactly %d (security Info I5: pin the exact count)", got, wantAdmittedDelta)
		}

		// ref2 is now B1-limited.
		const wantLimitedDelta = int64(2)
		if got := measure(ref2, 2000, http.StatusTooManyRequests); got != wantLimitedDelta {
			t.Fatalf("payments: B1-limited acquisition delta = %d, want exactly %d - admitVerified must run "+
				"strictly BEFORE deps.DB.WithTenant, never inside it (security §4/§7.5)", got, wantLimitedDelta)
		}
		if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, ref2); got != 0 {
			t.Fatalf("a limited deposit must post ZERO rows: got %d", got)
		}
	})

	t.Run("casino", func(t *testing.T) {
		pool := phasecapture.Pool10(t, "TEST_DATABASE_URL")
		_, issuer := testEnv(t)
		orchestrator, mock := newMockCasinoOrchestrator()

		tenant := mustCreateTenant(t, pool)
		brand := mustCreateBrand(t, pool, tenant)

		settings := t6Settings()
		settings.VerifiedRate["casino"] = WebhookRateBurst{Rate: 10, Burst: 1}
		srv := newAdmissionTestServer(t, pool, issuer, nil, orchestrator, settings, false)

		player := mustRegisterPlayer(t, srv, brand.Slug)
		mustActivatePlayer(t, pool, tenant.ID, player.ID)
		fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)

		game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
		mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
		mustEnableCasinoCapability(t, srv, pool, tenant)

		launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
		session := uuid.MustParse(launched.SessionID)

		measure := func(ref, round string, wantStatus int) int64 {
			before := pool.Raw().Stat().AcquireCount()
			payload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, ref, "", round, game.ProviderGameID, 100, "EUR", casino.OutcomeSucceeded, "", player.ID, session)
			resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payload)
			resp.Body.Close()
			if resp.StatusCode != wantStatus {
				t.Fatalf("ref %s: expected status %d, got %d", ref, wantStatus, resp.StatusCode)
			}
			return pool.Raw().Stat().AcquireCount() - before
		}

		// A real, distinct bet - admitted (200, posts).
		const wantAdmittedDelta = int64(2)
		if got := measure("t6g-casino-bet-1", "t6g-casino-round-1", http.StatusOK); got != wantAdmittedDelta {
			t.Fatalf("casino: admitted acquisition delta = %d, want exactly %d (security Info I5)", got, wantAdmittedDelta)
		}

		// A second, distinct bet - now B1-limited.
		const wantLimitedDelta = int64(1)
		betRef := "t6g-casino-bet-2"
		if got := measure(betRef, "t6g-casino-round-2", http.StatusTooManyRequests); got != wantLimitedDelta {
			t.Fatalf("casino: B1-limited acquisition delta = %d, want exactly %d - admitVerified must run "+
				"strictly BEFORE deps.DB.WithTenant, never inside it (security §4/§7.5)", got, wantLimitedDelta)
		}
		if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, betRef); got != 0 {
			t.Fatalf("a limited bet must post ZERO rows: got %d", got)
		}
	})

	t.Run("kyc", func(t *testing.T) {
		pool := phasecapture.Pool10(t, "TEST_DATABASE_URL")
		_, issuer := testEnv(t)
		mock := kyc.NewMockKYCProvider()
		orchestrator := kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": mock}, kyc.NewMockWebhookCredentials(mock))

		tenant := mustCreateTenant(t, pool)
		brand := mustCreateBrand(t, pool, tenant)

		settings := t6Settings()
		settings.VerifiedRate["kyc"] = WebhookRateBurst{Rate: 10, Burst: 1}
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		handler, rt := NewWithAdmission(Deps{
			DB: pool, AuthIssuer: issuer, ServiceName: "platform-api-test", Logger: logger,
			AccessTokenTTL: 5 * time.Minute, RefreshTokenTTL: time.Hour,
			PersonResolver:         identityresolution.NewMockPersonResolver(),
			KYCOrchestrator:        orchestrator,
			KYCOutboundCredentials: kyc.NewMockOutboundResolver(),
			KYCWebhookEnabled:      true, WebhookAdmission: settings,
		})
		if err := rt.LoadDirectory(context.Background()); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)

		// Security re-verification #3 (rv-prh-i4-security.md §8.4, Info
		// I7): a REAL verification, seeded through the actual player-
		// facing endpoint (the orchestrator's single, synthetic MOCK
		// adapter auto-selects with no capability-enable step needed -
		// provider_selection.go's own "exactly one registered adapter and
		// it is synthetic" rule), so the "must not change" assertion below
		// is falsifiable: an unknown-reference callback would fail closed
		// regardless of admission ordering (ErrVerificationReferenceUnknown,
		// writing no row), which is why the earlier version of this check
		// could never fail no matter what the admission code did.
		player := mustRegisterPlayer(t, srv, brand.Slug)
		createResp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
		if createResp.StatusCode != http.StatusCreated {
			t.Fatalf("kyc: expected 201 seeding a real verification, got %d", createResp.StatusCode)
		}
		var created map[string]any
		decodeBody(t, createResp, &created)
		verificationID := uuid.MustParse(created["id"].(string))
		beforeStatus, beforeUpdatedAt, providerRef := kycVerificationSnapshot(t, pool, tenant.ID, verificationID)
		if providerRef == "" {
			t.Fatal("kyc: the seeded verification has no provider_reference - the test's own setup is broken")
		}

		// The first (admitted) attempt reaches deps.DB.WithTenant - this
		// test is only about WHETHER it got there (past B1), never about
		// what domain processing then decides, so its own HTTP status is
		// not asserted here. It uses an UNRELATED reference (not the real
		// seeded verification's own) so the property under test here
		// (pool-acquisition delta) does not depend on whether the domain
		// layer accepts or rejects this specific callback's content.
		before := pool.Raw().Stat().AcquireCount()
		admitted := mock.CallbackPayload(tenant.ID, "t6g-kyc-1", kyc.ProviderApproved, "")
		admittedResp := rawPostCallback(t, srv, "/v1/webhooks/kyc/"+tenant.Slug+"/mock", payments.InboundCallback{Header: admitted.Header, Body: admitted.Body})
		admittedResp.Body.Close()
		if admittedResp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("kyc: the first callback must not itself be B1-limited (got 429) - the test's own setup is broken")
		}
		const wantAdmittedDelta = int64(2)
		if got := pool.Raw().Stat().AcquireCount() - before; got != wantAdmittedDelta {
			t.Fatalf("kyc: admitted acquisition delta = %d, want exactly %d (security Info I5)", got, wantAdmittedDelta)
		}

		// A second, distinct callback - naming the REAL seeded
		// verification's own provider_reference this time - is now
		// unambiguously B1-limited.
		before = pool.Raw().Stat().AcquireCount()
		limited := mock.CallbackPayload(tenant.ID, providerRef, kyc.ProviderApproved, "")
		limitedResp := rawPostCallback(t, srv, "/v1/webhooks/kyc/"+tenant.Slug+"/mock", payments.InboundCallback{Header: limited.Header, Body: limited.Body})
		limitedResp.Body.Close()
		if limitedResp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("kyc: expected the second callback to be B1-limited (429), got %d", limitedResp.StatusCode)
		}
		const wantLimitedDelta = int64(1)
		if got := pool.Raw().Stat().AcquireCount() - before; got != wantLimitedDelta {
			t.Fatalf("kyc: B1-limited acquisition delta = %d, want exactly %d - admitVerified must run "+
				"strictly BEFORE deps.DB.WithTenant, never inside it (security §4/§7.5)", got, wantLimitedDelta)
		}
		// Security's Info I7 (now falsifiable): the REAL, PRE-EXISTING
		// verification named by the limited callback's own reference must
		// be completely untouched - same status, same updated_at - proof
		// the limited callback never reached domain processing.
		afterStatus, afterUpdatedAt, _ := kycVerificationSnapshot(t, pool, tenant.ID, verificationID)
		if afterStatus != beforeStatus || !afterUpdatedAt.Equal(beforeUpdatedAt) {
			t.Fatalf("a B1-limited KYC callback changed the targeted verification: status %q -> %q, "+
				"updated_at %s -> %s", beforeStatus, afterStatus, beforeUpdatedAt, afterUpdatedAt)
		}
	})
}

// kycVerificationSnapshot reads one kyc_verifications row's status,
// updated_at and provider_reference - security's Info I7 falsifiable
// unchanged-row check.
func kycVerificationSnapshot(t *testing.T, pool *db.Pool, tenantID, verificationID uuid.UUID) (status string, updatedAt time.Time, providerReference string) {
	t.Helper()
	var providerRef *string
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT status, updated_at, provider_reference FROM kyc_verifications WHERE tenant_id = $1 AND id = $2`,
			tenantID, verificationID).Scan(&status, &updatedAt, &providerRef)
	})
	if err != nil {
		t.Fatalf("kycVerificationSnapshot: %v", err)
	}
	if providerRef != nil {
		providerReference = *providerRef
	}
	return status, updatedAt, providerReference
}

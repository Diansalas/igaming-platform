//go:build integration

// CAS-WH-TENANT-1 (Stage 10.2, ADR 0091). Code-review finding (2026-09-26):
// internal/httpserver's own TestCasinoWebhook_TamperMatrix_Rejected (C5)
// necessarily asserts only the UNIFORM HTTP-layer 401 "callback rejected" -
// that uniformity is itself the point of C6's enumeration-resistance
// requirement, so the HTTP layer can never assert a per-case Reason without
// contradicting the very property it exists to prove.
//
// This file is the place a specific Reason per tamper case CAN be
// observed: package-level, calling Orchestrator.ReceiveCallback directly,
// which returns the *webhookauth.AuthError itself (never redacted). Each
// case here is the exact package-level analogue of one
// TestCasinoWebhook_TamperMatrix_Rejected HTTP subtest, sharing its name.
package casino

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/testsupport/noeffect"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

func TestCasinoWebhook_TamperMatrix_ExactReason(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	genuine := provider.CallbackPayload(f.tenantID, CallbackEventBet, "cas-c5-reason-bet-1", "", "round-c5-reason", "game-1",
		1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.New())

	flipHex := func(s string) string {
		flipped := strings.Replace(s, "0", "f", 1)
		if flipped == s {
			flipped = strings.Replace(s, "1", "e", 1)
		}
		return flipped
	}

	cases := []struct {
		name       string
		build      func() webhookauth.Inbound
		wantReason webhookauth.Reason
	}{
		{"tampered_body_amount", func() webhookauth.Inbound {
			in := genuine
			in.Body = []byte(strings.Replace(string(genuine.Body), `"amount":1000`, `"amount":9999`, 1))
			return in
		}, webhookauth.ReasonSignatureInvalid},
		{"tampered_signature_header", func() webhookauth.Inbound {
			in := genuine
			in.Header = genuine.Header.Clone()
			sig := in.Header.Get(webhookauth.CasinoSignatureHeader)
			in.Header.Set(webhookauth.CasinoSignatureHeader, flipHex(sig))
			return in
		}, webhookauth.ReasonSignatureInvalid},
		{"missing_key_id_header", func() webhookauth.Inbound {
			in := genuine
			in.Header = genuine.Header.Clone()
			in.Header.Del(webhookauth.CasinoKeyIDHeader)
			return in
		}, webhookauth.ReasonSignatureMissing},
		{"unknown_key_id", func() webhookauth.Inbound {
			in := genuine
			in.Header = genuine.Header.Clone()
			in.Header.Set(webhookauth.CasinoKeyIDHeader, "mock-v2")
			return in
		}, webhookauth.ReasonCredentialUnavailable},
		{"legacy_signature_field_in_body", func() webhookauth.Inbound {
			// Genuinely, correctly signed over a legacy-shaped body (see
			// the M1 fix in internal/httpserver's own tamper matrix) - the
			// ONLY thing wrong is the post-verification legacy-field guard.
			legacyBody := []byte(strings.TrimSuffix(string(genuine.Body), "}") + `,"signature":"deadbeef"}`)
			return provider.SignRawBody(f.tenantID, legacyBody)
		}, webhookauth.ReasonSignatureInvalid},
	}

	before := noeffect.CaptureCasino(t, pool, []uuid.UUID{f.tenantID})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", c.build())
				return err
			})
			var authErr *webhookauth.AuthError
			if !errors.As(err, &authErr) {
				t.Fatalf("expected a *webhookauth.AuthError, got %v", err)
			}
			if authErr.Reason != c.wantReason {
				t.Fatalf("expected Reason=%s, got %s (err=%v)", c.wantReason, authErr.Reason, err)
			}
		})
	}
	noeffect.AssertNoCasinoEffect(t, pool, []uuid.UUID{f.tenantID}, before)
}

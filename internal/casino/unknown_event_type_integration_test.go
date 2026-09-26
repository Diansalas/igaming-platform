//go:build integration

// Stage 10.2 final review (K10/L6): the casino Orchestrator's own
// ReceiveCallback switch on event.EventType has a `default:` branch
// (orchestrator.go, after the LoadCapability check) that is unreachable
// through MockCasinoProvider today - MockCasinoProvider.HandleCallback
// already rejects an unrecognized event_type as ErrCallbackMalformedBody
// during its own post-verification parsing, before ever returning a
// CallbackEvent. That default branch is nonetheless defense in depth for a
// FUTURE adapter whose own parsing is looser and hands the orchestrator a
// verified CallbackEvent carrying an EventType outside {bet, win,
// rollback}. Before this fix it returned a plain, unwrapped error, which
// internal/httpserver's error mapping falls through to a 500 for a
// genuinely verified caller - the same "verified caller gets a
// distinguishable, non-4xx status" defect class point 7 exists to close
// for auth failures. This test exercises that default branch directly
// with a minimal wrapper provider that returns a genuinely-verified
// CallbackEvent carrying a bogus EventType, and proves both the 400-shaped
// error and that nothing was written (no bet/win/rollback posted, no
// ledger effect, no tombstone).
package casino

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/testsupport/noeffect"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// unknownEventTypeCasinoProvider wraps a genuine *MockCasinoProvider so
// HandleCallback's own verification (raw-byte HMAC, then structural
// parsing) runs completely unmodified and genuinely succeeds - only the
// EventType of the already-verified, already-parsed CallbackEvent is then
// swapped for a value MockCasinoProvider's own parsing would never itself
// produce, standing in for a future adapter with looser event-type
// validation than the mock's closed enum check.
type unknownEventTypeCasinoProvider struct {
	*MockCasinoProvider
}

func (p *unknownEventTypeCasinoProvider) HandleCallback(ctx context.Context, in webhookauth.Inbound, cred webhookauth.Credential) (CallbackEvent, error) {
	event, err := p.MockCasinoProvider.HandleCallback(ctx, in, cred)
	if err != nil {
		return event, err
	}
	event.EventType = CallbackEventType("free-spin-award")
	return event, nil
}

func TestCasinoWebhook_UnknownVerifiedEventType_MalformedBody(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	mock := NewMockCasinoProvider("mock-casino", "EUR")
	provider := &unknownEventTypeCasinoProvider{MockCasinoProvider: mock}
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(mock))

	in := mock.CallbackPayload(f.tenantID, CallbackEventBet, "cas-k10-unknown-event-1", "", "round-k10", "game-1",
		1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.New())

	before := noeffect.CaptureCasino(t, pool, []uuid.UUID{f.tenantID})
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", in)
		return err
	})
	if !errors.Is(err, ErrCallbackMalformedBody) {
		t.Fatalf("expected ErrCallbackMalformedBody for an unknown verified event type, got %v", err)
	}
	var authErr *webhookauth.AuthError
	if errors.As(err, &authErr) {
		t.Fatalf("an unknown verified event type must NOT map to the uniform pre-verification 401 (AuthError), got %+v", authErr)
	}
	noeffect.AssertNoCasinoEffect(t, pool, []uuid.UUID{f.tenantID}, before)
}

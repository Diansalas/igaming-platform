package casino

import (
	"errors"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/providerref"
)

// PROVIDER-REF-BOUND-1: validateCallbackReferences bounds every provider-
// supplied identifier of a verified casino callback.
func TestValidateCallbackReferences(t *testing.T) {
	exact := strings.Repeat("a", providerref.MaxBytes)
	over := exact + "a"
	ok := CallbackEvent{EventType: CallbackEventRollback, ProviderTxID: exact, OriginalProviderTxID: exact,
		RoundID: exact, ProviderGameID: exact, AssetCode: "EUR"}
	if err := validateCallbackReferences(ok); err != nil {
		t.Fatalf("exact-max references must pass: %v", err)
	}
	// Absent optional fields are the domain's own decision, not the bound's.
	if err := validateCallbackReferences(CallbackEvent{EventType: CallbackEventBet, ProviderTxID: "bet-1"}); err != nil {
		t.Fatalf("absent optional fields must pass: %v", err)
	}
	for field, mutate := range map[string]func(*CallbackEvent){
		"provider_tx_id":          func(e *CallbackEvent) { e.ProviderTxID = over },
		"original_provider_tx_id": func(e *CallbackEvent) { e.OriginalProviderTxID = over },
		"round_id":                func(e *CallbackEvent) { e.RoundID = over },
		"provider_game_id":        func(e *CallbackEvent) { e.ProviderGameID = over },
		"asset_code":              func(e *CallbackEvent) { e.AssetCode = over },
	} {
		ev := ok
		mutate(&ev)
		err := validateCallbackReferences(ev)
		if !errors.Is(err, ErrProviderReferenceInvalid) || !errors.Is(err, providerref.ErrInvalid) {
			t.Fatalf("%s: expected ErrProviderReferenceInvalid, got %v", field, err)
		}
		if refErr, _ := providerref.AsError(err); refErr == nil || refErr.Field != field || refErr.Reason != providerref.ReasonTooLong {
			t.Fatalf("%s: wrong detail %+v", field, refErr)
		}
		if strings.Contains(err.Error(), over) {
			t.Fatalf("%s: error text leaks the value", field)
		}
		if _, recorded := rejectionClassFor(err); recorded {
			t.Fatalf("%s: must not be a recorded rejection class", field)
		}
	}
	// An empty provider_tx_id is rejected (required).
	if err := validateCallbackReferences(CallbackEvent{EventType: CallbackEventBet}); !errors.Is(err, ErrProviderReferenceInvalid) {
		t.Fatalf("empty provider_tx_id must be rejected, got %v", err)
	}
	// Invalid UTF-8 (reachable from an adapter that does not decode JSON).
	if err := validateCallbackReferences(CallbackEvent{ProviderTxID: "bet-\xff"}); !errors.Is(err, ErrProviderReferenceInvalid) {
		t.Fatalf("invalid UTF-8 must be rejected, got %v", err)
	}
}

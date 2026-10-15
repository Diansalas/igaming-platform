package casino

import (
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// CAS-PLAYER-REF-1 (A3): the provider-facing request/event types carry the
// opaque provider-scoped player reference and no platform account id. A
// static shape check, so a future field that re-introduces a raw account id
// on the vendor-facing surface fails here, not in review.
func TestProviderFacingTypesCarryNoRawPlayerAccountID(t *testing.T) {
	for _, tc := range []struct {
		typ     reflect.Type
		wantRef bool
	}{
		{reflect.TypeOf(LaunchRequest{}), true},
		{reflect.TypeOf(BalanceRequest{}), true},
		{reflect.TypeOf(CallbackEvent{}), true},
		{reflect.TypeOf(mockCasinoCallbackBody{}), true},
	} {
		hasRef := false
		for i := 0; i < tc.typ.NumField(); i++ {
			field := tc.typ.Field(i)
			name := strings.ToLower(field.Name)
			if strings.Contains(name, "playeraccount") || strings.Contains(name, "accountid") {
				t.Errorf("%s has a field %q that can carry a raw platform account id; use the opaque PlayerRef", tc.typ.Name(), field.Name)
			}
			if field.Name == "PlayerRef" {
				hasRef = true
			}
		}
		if tc.wantRef && !hasRef {
			t.Errorf("%s must carry the opaque PlayerRef", tc.typ.Name())
		}
	}
	// The wire name of the mock body's field is player_ref, never player_account_id.
	f, _ := reflect.TypeOf(mockCasinoCallbackBody{}).FieldByName("PlayerRef")
	if !strings.HasPrefix(f.Tag.Get("json"), "player_ref") {
		t.Fatalf("the callback body must carry the player under the json name player_ref, got %v", f.Tag)
	}
}

// A zero reference never resolves; the nil short-circuit needs no database.
func TestResolveProviderPlayerRef_NilReferenceNeverResolves(t *testing.T) {
	if _, err := ResolveProviderPlayerRef(t.Context(), nil, uuid.New(), "mock-casino", uuid.Nil); err != ErrPlayerRefUnresolved {
		t.Fatalf("expected ErrPlayerRefUnresolved for the nil reference, got %v", err)
	}
}

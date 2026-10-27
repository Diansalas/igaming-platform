package httpserver

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/licensingchange"
)

// The check code is the database hint only when it is in the closed table; anything else is dropped (never copied into a response).
func TestLicensingChange_CheckCodes_AreTheClosedTableOnly(t *testing.T) {
	guard := func(code, hint string) error {
		return &licensingchange.GuardError{Code: code, Message: "database text", Hint: hint}
	}
	if got := licensingChangeCheckCodes(guard("LC050", "loosening_cooling_period")); len(got) != 1 || got[0] != "loosening_cooling_period" {
		t.Fatalf("a closed code is carried: %v", got)
	}
	for name, err := range map[string]error{
		"unknown hint":       guard("LC050", "made_up"),
		"empty hint":         guard("LC050", ""),
		"another guard code": guard("LC020", "loosening_cooling_period"),
		"plain error":        &pgconn.PgError{Code: "LC050", Hint: "loosening_cooling_period"},
	} {
		if got := licensingChangeCheckCodes(err); len(got) != 0 {
			t.Errorf("%s: %v", name, got)
		}
	}
	code, _ := licensingChangeErrorCode(guard("LC050", "loosening_disabled"))
	if string(code) != "LICENSING_CHANGE_GUARD_REFUSED" {
		t.Fatalf("an LC050 refusal keeps the generic guard code: %s", code)
	}
}

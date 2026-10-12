package httpserver

import (
	"testing"

	"github.com/Diansalas/igaming-platform/internal/launchgov"
)

// ADR 0113 slice 2 review: a refusal carrying ONLY H6HoldBalanceNonZero (a stranded hold, no request)
// must not say "withdrawal requests still hold player funds"; with H6WithdrawalsHolding it still does.
func TestLaunchErrorCode_HoldBalanceMessageMatchesTheCause(t *testing.T) {
	_, msg, _ := launchErrorCode(&launchgov.PreconditionError{Base: launchgov.ErrWithdrawalsHolding, Codes: []string{launchgov.H6HoldBalanceNonZero}})
	if msg != "closure refused: player withdrawal hold balances are not zero" {
		t.Fatalf("hold balance only: %q", msg)
	}
	_, msg, _ = launchErrorCode(&launchgov.PreconditionError{Base: launchgov.ErrWithdrawalsHolding,
		Codes: []string{launchgov.H6WithdrawalsHolding, launchgov.H6HoldBalanceNonZero}})
	if msg != "closure refused: withdrawal requests still hold player funds" {
		t.Fatalf("requests holding: %q", msg)
	}
}

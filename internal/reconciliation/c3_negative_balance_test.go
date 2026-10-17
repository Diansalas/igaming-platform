package reconciliation

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// C3: the detector is READ-ONLY. Its statement (with and without the gate-decision term) contains no write or DDL
// keyword, and negative_balance.go issues no Exec (the run's only writes are persistRun's mismatch rows and the alert,
// both outside this file).
func TestC3_DetectorIsReadOnly(t *testing.T) {
	write := regexp.MustCompile(`(?i)\b(insert|update|delete|truncate|alter|create|drop|grant|revoke|copy|lock|set_config|nextval|pg_advisory)\b`)
	for _, q := range []string{negativeBalanceCheckSQL, negativeBalanceGateDecisionTerm} {
		if m := write.FindString(q); m != "" {
			t.Fatalf("the C3 statement must only read, found %q", m)
		}
	}
	if !strings.Contains(negativeBalanceCheckSQL, gateDecisionTermToken) {
		t.Fatal("the gate-decision term placeholder must be present exactly where negativeBalanceSQL expects it")
	}
	src, err := os.ReadFile("negative_balance.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), ".Exec(") || strings.Contains(string(src), "persistRun(") {
		t.Fatal("negative_balance.go must not write")
	}
}

// The predicate is the WD050 gate's (internal/rg walletBalanceNegative): the same two account types and the same sign
// rule on wallet_balance_projection, and no netting against the withdrawal hold account.
func TestC3_PredicateMatchesTheGate(t *testing.T) {
	for _, want := range []string{
		`la.account_type IN ('player_cash', 'player_locked_cash')`,
		`p.credit_total < p.debit_total`,
		`a.cr < a.dr`,
	} {
		if !strings.Contains(negativeBalanceCheckSQL, want) {
			t.Fatalf("predicate fragment %q missing", want)
		}
	}
	if strings.Contains(negativeBalanceCheckSQL, "player_withdrawal_hold") {
		t.Fatal("the hold account must not be netted into the balance (it would hide a chargeback with an open withdrawal)")
	}
	gate, err := os.ReadFile("../rg/rg.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gate), `account_type IN ('player_cash', 'player_locked_cash')`) || !strings.Contains(string(gate), `credit_total < debit_total`) {
		t.Fatal("the WD050 gate predicate changed: update the C3 detector (negative_balance.go) to match it")
	}
}

func TestC3_SplitRoutesEachKindToItsOwnDiscriminator(t *testing.T) {
	c := splitLedgerMismatches([]Mismatch{
		{MismatchKind: MismatchKindBalanceMismatch},
		{MismatchKind: MismatchKindLedgerUnlinkedManualAdjustment},
		{MismatchKind: MismatchKindWithdrawalHoldOpenMismatch},
		{MismatchKind: MismatchKindPlayerWalletNegativeBalance},
		{MismatchKind: MismatchKindPlayerWalletNegativeBalance},
		{MismatchKind: MismatchKindPayoutDispatchedWhileWalletNegative},
	})
	if c != (ledgerRunCounts{drift: 1, unlinked: 1, holds: 1, negative: 2, payoutWhileNegative: 1}) {
		t.Fatalf("split: %+v", c)
	}
	if discLedgerWalletNegative == discLedgerVsProjection || discLedgerPayoutWhileNegative == discLedgerWalletNegative {
		t.Fatal("distinct discriminators")
	}
}

func TestC3_StaffVocabulary(t *testing.T) {
	kinds, _ := StaffViewMismatchKinds(StreamLedgerVsProjection)
	have := map[MismatchKind]bool{}
	for _, k := range kinds {
		have[k] = true
	}
	for _, want := range []MismatchKind{MismatchKindPlayerWalletNegativeBalance, MismatchKindPayoutDispatchedWhileWalletNegative} {
		if !have[want] {
			t.Errorf("%s missing from the ledger_vs_projection staff vocabulary", want)
		}
	}
}

// The staff view names the negative account as the subject of the wallet finding; the payout finding is keyed by a
// withdrawal request, which has no subject type in the staff contract (like check D's request keys): nil.
func TestC3_StaffSubject(t *testing.T) {
	id := "0b3c2f6e-6a1f-4c55-9a4e-1d2b3c4d5e6f"
	if s := MismatchSubject(StreamLedgerVsProjection, MismatchKindPlayerWalletNegativeBalance, id, ""); s == nil || s.Type != "ledger_account" || s.ID != id {
		t.Fatalf("wallet finding subject: %+v", s)
	}
	if s := MismatchSubject(StreamLedgerVsProjection, MismatchKindPayoutDispatchedWhileWalletNegative, id, ""); s != nil {
		t.Fatalf("payout finding subject must be nil: %+v", s)
	}
	if AmountKindExposed(MismatchKindPlayerWalletNegativeBalance) || AmountKindExposed(MismatchKindPayoutDispatchedWhileWalletNegative) {
		t.Fatal("C3 kinds expose no amount through the staff view")
	}
}

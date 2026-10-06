package payments

import (
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/alerting"
)

// The discriminator's reason part is ALWAYS from the closed set: every reason a
// deposit dispute can carry maps to itself (invalid_provider_reference:<d>
// collapses to its prefix) and anything else - including hostile strings - is
// "unclassified", never echoed.
func TestAlertReasonFor_ClosedSetOnly(t *testing.T) {
	for _, r := range DepositDisputeTerminalReasons() {
		if strings.HasSuffix(r, ":") {
			if got := alertReasonFor(r + "too_long"); got != TerminalReasonInvalidProviderReference {
				t.Errorf("%q collapsed to %q", r, got)
			}
			continue
		}
		if got := alertReasonFor(r); got != r {
			t.Errorf("closed reason %q mapped to %q", r, got)
		}
	}
	for _, hostile := range []string{"", "ref\x07bell", "provider said: card 4111", strings.Repeat("a", 500), "sync_amount_mismatch; DROP"} {
		if got := alertReasonFor(hostile); got != alertReasonUnclassified {
			t.Errorf("unlisted reason %q leaked as %q", hostile, got)
		}
	}
}

// Each T10 park reason, as the alert's discriminator, satisfies the DB
// discriminator charset/length CHECK and carries only allowlisted attributes
// (Alert.validate runs before any SQL, so a bad shape would silently become a
// raise_failed fallback instead of the P1).
func TestDepositParkAlert_ShapeIsValidForEveryReason(t *testing.T) {
	disc := regexp.MustCompile(`^[A-Za-z0-9:_.-]{1,160}$`)
	attempt := PaymentAttempt{ID: uuid.New(), TenantID: uuid.New(), Operation: AttemptOperationDeposit}
	reasons := []string{}
	for _, r := range DepositDisputeTerminalReasons() {
		reasons = append(reasons, strings.TrimSuffix(r, ":"))
	}
	for sub := range pollContradictionSubReasons {
		reasons = append(reasons, alertReasonPollContradictsTermin+":"+sub)
	}
	for r := range depositEscalationReasons {
		reasons = append(reasons, r)
	}
	for _, r := range reasons {
		a := depositParkAlert(attempt, "mock", r)
		if !disc.MatchString(a.Discriminator) {
			t.Errorf("discriminator %q violates the alerts.discriminator charset/length", a.Discriminator)
		}
		if a.Kind != alerting.KindPaymentWebhookIntegrity || a.SubjectTenantID != attempt.TenantID {
			t.Errorf("kind/subject: %+v", a)
		}
		def := alerting.MustDef(a.Kind)
		for k := range a.Attributes {
			if _, ok := def.AllowedKeys[k]; !ok {
				t.Errorf("attribute %q is not on the %s allowlist", k, a.Kind)
			}
		}
	}
	// Multiple-success and backstop alerts use only their own allowlists.
	if def := alerting.MustDef(alerting.KindPaymentMultipleSuccessForIntent); def.Severity != alerting.SeverityP1 || def.Scope != alerting.ScopeKindPlatform {
		t.Errorf("multiple-success must be a platform-owned p1: %+v", def)
	}
}

// Payout operations are F-pay's surface: the deposit park raise is a no-op for
// them (never a misleading deposit alert on a payout dispute).
func TestRaiseDepositParkAlert_IsNoOpForPayoutAttempts(t *testing.T) {
	attempt := PaymentAttempt{ID: uuid.New(), TenantID: uuid.New(), Operation: AttemptOperationPayout}
	if err := raiseDepositParkAlert(nil, nil, attempt, "mock", TerminalReasonSyncAmountMismatch); err != nil { //nolint:staticcheck // nil ctx/tx are never touched on the no-op path
		t.Fatalf("a payout attempt must be a no-op, got %v", err)
	}
}

// B6/B7: the T16 escalation discriminator reasons are a closed set of exactly two values, they
// are NOT terminal dispute reasons (the attempt stays live), and the terminal-reason mapper
// never echoes them (it maps them to "unclassified" like any unlisted string).
func TestB6B7_EscalationReasons_ClosedSet(t *testing.T) {
	want := map[string]bool{"deposit_settlement_window_exceeded": true, "deposit_unreferenced_submitting": true}
	if len(depositEscalationReasons) != len(want) {
		t.Fatalf("closed escalation reason set = %v", depositEscalationReasons)
	}
	for r := range want {
		if _, ok := depositEscalationReasons[r]; !ok {
			t.Errorf("missing closed escalation reason %q", r)
		}
		if IsDepositDisputeTerminalReason(r) {
			t.Errorf("%q must not be a terminal dispute reason (an escalation never changes state)", r)
		}
		if got := alertReasonFor(r); got != alertReasonUnclassified {
			t.Errorf("alertReasonFor(%q) = %q, want unclassified", r, got)
		}
	}
	disc := regexp.MustCompile(`^[A-Za-z0-9:_.-]{1,160}$`)
	attempt := PaymentAttempt{ID: uuid.New(), TenantID: uuid.New(), Operation: AttemptOperationDeposit}
	for r := range depositEscalationReasons {
		a := depositParkAlert(attempt, "mock", r)
		if !disc.MatchString(a.Discriminator) || a.Kind != alerting.KindPaymentWebhookIntegrity {
			t.Errorf("bad escalation alert shape for %q: %+v", r, a)
		}
	}
}

// A payout attempt is a no-op for the deposit escalation raise (F-pay's surface).
func TestRaiseDepositEscalationAlert_IsNoOpForPayoutAttempts(t *testing.T) {
	attempt := PaymentAttempt{ID: uuid.New(), TenantID: uuid.New(), Operation: AttemptOperationPayout}
	if err := raiseDepositEscalationAlert(nil, nil, attempt, alertReasonDepositSettlementWindowExceeded); err != nil { //nolint:staticcheck // nil ctx/tx are never touched on the no-op path
		t.Fatalf("a payout attempt must be a no-op, got %v", err)
	}
}

package httpserver

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

// ADR 0102 8 row 8 alert shape: subject is the route-validated target, the
// discriminator is the switch id, attributes are exactly the Kind allowlist's
// members, the takeover flag is carried, and the staff-supplied reason code is
// bounded (an over-long one must never push the alert past the attribute size
// cap, which would turn it into a go_validation fallback).
func TestKillSwitchEngagedAlert_ShapeAllowlistAndBounds(t *testing.T) {
	target := uuid.New()
	scope := payments.KillSwitchScopePlatform
	ks := payments.KillSwitch{ID: uuid.New(), TenantID: target, ProviderScope: "mock", OperationScope: payments.KillSwitchOperationDeposit,
		ReasonCode: "incident-7", ChangedByScope: scope}

	a := killSwitchEngagedAlert(target, "req-1", ks, true)
	if a.Kind != alerting.KindPaymentKillSwitchEngaged || a.SubjectTenantID != target || a.Discriminator != "switch:"+ks.ID.String() {
		t.Fatalf("kind/subject/discriminator: %+v", a)
	}
	if a.Attributes["is_platform_takeover"] != true || a.Attributes["changed_by_scope"] != string(scope) ||
		a.Attributes["provider_scope"] != "mock" || a.Attributes["operation_scope"] != "deposit" || a.Attributes["reason_code"] != "incident-7" {
		t.Fatalf("attributes: %v", a.Attributes)
	}
	if b := killSwitchEngagedAlert(target, "req-1", ks, false); b.Attributes["is_platform_takeover"] != false {
		t.Fatal("is_platform_takeover must reflect the engage, not be a constant")
	}
	def := alerting.MustDef(a.Kind)
	for k := range a.Attributes {
		if _, ok := def.AllowedKeys[k]; !ok {
			t.Fatalf("attribute %q is not on the Kind allowlist", k)
		}
	}

	ks.ReasonCode = strings.Repeat("r", 5000)
	long := killSwitchEngagedAlert(target, "req-1", ks, false)
	if got := long.Attributes["reason_code"].(string); len(got) != killSwitchAlertReasonMax {
		t.Fatalf("reason_code must be bounded to %d, got %d", killSwitchAlertReasonMax, len(got))
	}
}

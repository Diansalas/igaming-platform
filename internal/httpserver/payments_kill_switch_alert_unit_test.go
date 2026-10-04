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

	a := killSwitchEngagedAlert(target, "req-1", ks, false)
	if a.Kind != alerting.KindPaymentKillSwitchEngaged || a.SubjectTenantID != target || a.Discriminator != "switch:"+ks.ID.String() {
		t.Fatalf("kind/subject/discriminator: %+v", a)
	}
	if a.Attributes["is_platform_takeover"] != false || a.Attributes["changed_by_scope"] != string(scope) ||
		a.Attributes["provider_scope"] != "mock" || a.Attributes["operation_scope"] != "deposit" || a.Attributes["reason_code"] != "incident-7" {
		t.Fatalf("attributes: %v", a.Attributes)
	}
	if b := killSwitchEngagedAlert(target, "req-1", ks, true); b.Attributes["is_platform_takeover"] != true {
		t.Fatal("is_platform_takeover must reflect the engage, not be a constant")
	}
	def := alerting.MustDef(a.Kind)
	for k := range a.Attributes {
		if _, ok := def.AllowedKeys[k]; !ok {
			t.Fatalf("attribute %q is not on the Kind allowlist", k)
		}
	}

	// Takeover: its own discriminator, so the takeover is observable.
	tk := killSwitchEngagedAlert(target, "req-1", ks, true)
	if tk.Discriminator != "switch:"+ks.ID.String()+":takeover" {
		t.Fatalf("takeover discriminator: %q", tk.Discriminator)
	}
	if a.Discriminator == tk.Discriminator {
		t.Fatal("takeover and ordinary engage must not share a key")
	}

	// S-3: the reason_code attribute is a closed token or "nonconforming";
	// free text, uppercase, spaces, an over-long value or multi-byte text never
	// reach the payload.
	for _, bad := range []string{strings.Repeat("r", 5000), strings.Repeat("r", 65), "Free Text!", "has space", "ünïcode", "-leading", ""} {
		ks.ReasonCode = bad
		got := killSwitchEngagedAlert(target, "req-1", ks, false).Attributes["reason_code"]
		if got != "nonconforming" {
			t.Fatalf("reason %q must map to nonconforming, got %v", bad, got)
		}
	}
	ks.ReasonCode = "incident_123.a-b"
	if got := killSwitchEngagedAlert(target, "req-1", ks, false).Attributes["reason_code"]; got != "incident_123.a-b" {
		t.Fatalf("a conforming reason must pass through, got %v", got)
	}
}

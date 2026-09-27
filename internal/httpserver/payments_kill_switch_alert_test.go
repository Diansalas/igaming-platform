// RV-PRH-I1 security review L7: pins the S95-C7 engage "alert" event's name
// and fields. This is a log line, not delivered alert routing - see
// logKillSwitchEngagedAlert's own doc comment and the ADR 0095 §10.6/task
// registry relabelling this fix round makes ("event IMPLEMENTED, delivery
// NOT IMPLEMENTED").
package httpserver

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/payments"
)

type capturingKillSwitchLogger struct {
	warnCalls  []string
	errorCalls []string
}

func (l *capturingKillSwitchLogger) Warn(msg string, args ...any) {
	l.warnCalls = append(l.warnCalls, msg)
}
func (l *capturingKillSwitchLogger) Error(msg string, args ...any) {
	l.errorCalls = append(l.errorCalls, renderKV(msg, args))
}

func renderKV(msg string, args []any) string {
	var b strings.Builder
	b.WriteString(msg)
	for _, a := range args {
		b.WriteString(" ")
		if s, ok := a.(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

func TestLogKillSwitchEngagedAlert_EmitsPinnedEventAndFields(t *testing.T) {
	logger := &capturingKillSwitchLogger{}
	scope := payments.KillSwitchScopeTenant
	ks := payments.KillSwitch{
		ID: uuid.New(), TenantID: uuid.New(), ProviderScope: "mock", OperationScope: payments.KillSwitchOperationDeposit,
		Engaged: true, EngagedByScope: &scope, ReasonCode: "incident-1", ChangedBy: uuid.New(), ChangedByScope: scope,
	}
	logKillSwitchEngagedAlert(logger, ks, "req-1")

	if len(logger.errorCalls) != 1 {
		t.Fatalf("expected exactly one Error-level log call, got %d: %v", len(logger.errorCalls), logger.errorCalls)
	}
	line := logger.errorCalls[0]
	if !strings.Contains(line, "payments_kill_switch_engaged_alert") {
		t.Errorf("expected the pinned event name payments_kill_switch_engaged_alert, got %q", line)
	}
	for _, field := range []string{ks.TenantID.String(), ks.ProviderScope, string(ks.OperationScope), ks.ReasonCode, ks.ChangedBy.String(), "req-1"} {
		if !strings.Contains(line, field) {
			t.Errorf("expected the alert to carry %q, got %q", field, line)
		}
	}
}

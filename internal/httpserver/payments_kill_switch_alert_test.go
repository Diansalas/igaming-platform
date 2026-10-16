// RV-PRH-I1 security review L7: pins the S95-C7 engage "alert" event's name
// and fields. This is a log line, not delivered alert routing - see
// logKillSwitchEngagedAlert's own doc comment and the ADR 0095 §10.6/task
// registry relabelling this fix round makes ("event IMPLEMENTED, delivery
// NOT IMPLEMENTED").
package httpserver

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/observability"
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

// logKeyCounts counts how often each key appears in one slog text line (key=value pairs).
func logKeyCounts(line string) map[string]int {
	out := map[string]int{}
	for _, m := range regexp.MustCompile(`(?:^|\s)([a-z_]+)=`).FindAllStringSubmatch(line, -1) {
		out[m[1]]++
	}
	return out
}

// alertLines returns the lines of buf that carry the engaged-alert event.
func alertLines(buf string) []string {
	var out []string
	for _, l := range strings.Split(buf, "\n") {
		if strings.Contains(l, "msg=payments_kill_switch_engaged_alert") {
			out = append(out, l)
		}
	}
	return out
}

// SA-4 (docs/plans/staging-acceptance-suite-record.md): the engaged-alert line carried tenant_id (and request_id) twice,
// first from the request-context logger (the nil UUID for a platform admin) and then its own value, so a log query could
// not tell which was meant. The call site now passes the BASE logger; this pins that the line written through it has every
// key exactly once, with the affected tenant under tenant_id, even for a platform caller whose context tenant is the nil UUID.
func TestLogKillSwitchEngagedAlert_NoDuplicateKeys(t *testing.T) {
	scope := payments.KillSwitchScopePlatform
	ks := payments.KillSwitch{
		ID: uuid.New(), TenantID: uuid.New(), ProviderScope: "mock", OperationScope: payments.KillSwitchOperationDeposit,
		Engaged: true, EngagedByScope: &scope, ReasonCode: "incident-1", ChangedBy: uuid.New(), ChangedByScope: scope,
	}
	var buf bytes.Buffer
	base := slog.New(slog.NewTextHandler(&buf, nil))

	logKillSwitchEngagedAlert(base, ks, "req-1")
	lines := alertLines(buf.String())
	if len(lines) != 1 {
		t.Fatalf("want one alert line, got %v", lines)
	}
	for k, n := range logKeyCounts(lines[0]) {
		if n != 1 {
			t.Errorf("key %q appears %d times in %q", k, n, lines[0])
		}
	}
	if !strings.Contains(lines[0], "tenant_id="+ks.TenantID.String()) || strings.Contains(lines[0], uuid.Nil.String()) {
		t.Errorf("tenant_id must be the affected tenant and the nil UUID must not appear: %q", lines[0])
	}

	// The premise the call site relies on: the request-context logger of a platform admin already carries both keys,
	// so passing it here would duplicate them. If this ever stops being true the call-site comment is obsolete.
	buf.Reset()
	ctx := observability.WithRequestState(context.Background(), &observability.RequestState{RequestID: "req-1", TenantID: uuid.Nil.String()})
	logKillSwitchEngagedAlert(observability.LoggerFromContext(ctx, base), ks, "req-1")
	counts := logKeyCounts(alertLines(buf.String())[0])
	if counts["tenant_id"] != 2 || counts["request_id"] != 2 {
		t.Errorf("premise changed: the context logger no longer duplicates tenant_id/request_id (%v); revisit SA-4's call-site choice", counts)
	}
}

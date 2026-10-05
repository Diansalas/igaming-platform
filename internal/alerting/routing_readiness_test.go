package alerting

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ALERT-DELIVERY-1 routing readiness (ADR 0102 section 18): unit tests.

func TestErrorClass_Retryable_Table(t *testing.T) {
	cases := map[ErrorClass]bool{
		ErrorClassTimeout:       true,
		ErrorClassUnavailable:   true,
		ErrorClassUnknown:       true,
		ErrorClassRejected:      false,
		ErrorClassMisconfigured: false,
		ErrorClassNone:          true, // unrecognised/empty is treated as unknown
		ErrorClass("garbage"):   true,
	}
	for c, want := range cases {
		if got := c.Retryable(); got != want {
			t.Errorf("ErrorClass(%q).Retryable() = %v, want %v", c, got, want)
		}
	}
}

func TestErrorClass_Normalized_NeverProducesAnUnpersistableClass(t *testing.T) {
	for _, c := range []ErrorClass{"", "garbage", "TIMEOUT"} {
		if c.normalized() != ErrorClassUnknown {
			t.Errorf("%q must normalise to unknown", c)
		}
	}
	for _, c := range []ErrorClass{ErrorClassTimeout, ErrorClassUnavailable, ErrorClassRejected, ErrorClassMisconfigured, ErrorClassUnknown} {
		if c.normalized() != c {
			t.Errorf("%q must be preserved", c)
		}
	}
}

// Human-notification flag pins: log and mock never reach a person.
func TestHumanNotification_LogAndMockAreFalse(t *testing.T) {
	if (LogSink{}).HumanNotification() {
		t.Fatal("LogSink must not claim to notify a person")
	}
	if (&MockSink{}).HumanNotification() {
		t.Fatal("MockSink must not claim to notify a person")
	}
	for k, want := range map[ChannelKind]bool{ChannelLog: false, ChannelMock: false, "pager": true, "": true, "webhook": true} {
		if got := ChannelKindIsHumanNotification(k); got != want {
			t.Errorf("ChannelKindIsHumanNotification(%q) = %v, want %v (unknown kinds are presumed human: fail closed)", k, got, want)
		}
	}
}

func TestRetryBudgets_ConstantsAndOverride(t *testing.T) {
	b := DefaultRetryBudgets()
	if b.P1 != (SeverityBudget{8, 5 * time.Minute}) || b.P2 != (SeverityBudget{5, time.Minute}) || b.P3 != (SeverityBudget{3, time.Minute}) {
		t.Fatalf("budgets drifted from p1 8/5m, p2 5/1m, p3 3/1m: %+v", b)
	}
	cfg := DefaultDispatcherConfig()
	for sev, want := range map[Severity]int{SeverityP1: 8, SeverityP2: 5, SeverityP3: 3, "bogus": 3} {
		if got := cfg.attemptBudget(sev); got != want {
			t.Errorf("attemptBudget(%s) = %d, want %d", sev, got, want)
		}
	}
	cfg.MaxAttempts = 2
	if cfg.attemptBudget(SeverityP1) != 2 {
		t.Fatal("MaxAttempts override must apply to every severity")
	}
}

func TestBackoff_CapPerSeverity(t *testing.T) {
	cfg := DefaultDispatcherConfig()
	if got := cfg.backoffFor(SeverityP1, 30); got != 5*time.Minute {
		t.Fatalf("p1 backoff cap = %v, want 5m", got)
	}
	if got := cfg.backoffFor(SeverityP2, 30); got != time.Minute {
		t.Fatalf("p2 backoff cap = %v, want 1m", got)
	}
	if got := cfg.backoffFor(SeverityP1, 0); got != time.Second {
		t.Fatalf("first backoff = %v, want 1s", got)
	}
	if got := cfg.backoffFor(SeverityP1, -5); got != time.Second {
		t.Fatalf("negative attempt must clamp: %v", got)
	}
	cfg.Backoff = func(int) time.Duration { return 7 * time.Second }
	if cfg.backoffFor(SeverityP3, 1) != 7*time.Second {
		t.Fatal("Backoff override must win")
	}
}

func fixedWired(human bool, kinds ...ChannelKind) map[ChannelKind]bool {
	m := map[ChannelKind]bool{}
	for _, k := range kinds {
		m[k] = human
	}
	return m
}

func find(rs []SeverityReadiness, s Severity) SeverityReadiness {
	for _, r := range rs {
		if r.Severity == s {
			return r
		}
	}
	return SeverityReadiness{}
}

func TestEvaluateReadiness_Table(t *testing.T) {
	now := time.Now()
	route := func(sev Severity, step int, kind ChannelKind, enabled, hasRec, dbHuman bool, age time.Duration) RouteConfig {
		return RouteConfig{Severity: sev, Step: step, ChannelKind: kind, Enabled: enabled, HasRecipient: hasRec, DBHuman: dbHuman, EffectiveFrom: now.Add(-age)}
	}
	cases := []struct {
		name   string
		routes []RouteConfig
		wired  map[ChannelKind]bool
		want   bool
		reason ReadinessReason
	}{
		{"no route", nil, fixedWired(false, ChannelLog), false, ReadinessNoRoute},
		{"only a step-1 route", []RouteConfig{route(SeverityP1, 1, ChannelLog, true, true, false, 0)}, fixedWired(false, ChannelLog), false, ReadinessNoRoute},
		{"disabled", []RouteConfig{route(SeverityP1, 0, ChannelLog, false, true, false, 0)}, fixedWired(false, ChannelLog), false, ReadinessRouteDisabled},
		{"enabled log route is not human", []RouteConfig{route(SeverityP1, 0, ChannelLog, true, true, false, 0)}, fixedWired(false, ChannelLog), false, ReadinessNotHuman},
		{"enabled mock route is not human", []RouteConfig{route(SeverityP1, 0, ChannelMock, true, true, false, 0)}, fixedWired(false, ChannelMock), false, ReadinessNotHuman},
		{"human kind not wired in this binary", []RouteConfig{route(SeverityP1, 0, "pager", true, true, true, 0)}, fixedWired(false, ChannelLog), false, ReadinessNotWired},
		{"human in DB but sink says not human", []RouteConfig{route(SeverityP1, 0, "pager", true, true, true, 0)}, fixedWired(false, "pager"), false, ReadinessNotHuman},
		{"sink says human but DB says not", []RouteConfig{route(SeverityP1, 0, "pager", true, true, false, 0)}, fixedWired(true, "pager"), false, ReadinessNotHuman},
		{"hypothetical human route fully configured", []RouteConfig{route(SeverityP1, 0, "pager", true, true, true, 0)}, fixedWired(true, "pager"), true, ReadinessReady},
		{"newer disabled version wins over an older enabled one",
			[]RouteConfig{route(SeverityP1, 0, "pager", true, true, true, time.Hour), route(SeverityP1, 0, "pager", false, true, true, 0)},
			fixedWired(true, "pager"), false, ReadinessRouteDisabled},
	}
	for _, c := range cases {
		got := find(EvaluateReadiness(c.routes, c.wired), SeverityP1)
		if got.Ready != c.want || got.Reason != c.reason {
			t.Errorf("%s: ready=%v reason=%s, want ready=%v reason=%s", c.name, got.Ready, got.Reason, c.want, c.reason)
		}
	}
}

func TestEvaluateReadiness_CountsAndRequiredSeverities(t *testing.T) {
	now := time.Now()
	routes := []RouteConfig{
		{Severity: SeverityP2, Step: 0, ChannelKind: ChannelLog, Enabled: true, HasRecipient: true, EffectiveFrom: now},
		{Severity: SeverityP2, Step: 1, ChannelKind: ChannelMock, Enabled: false, EffectiveFrom: now},
	}
	res := EvaluateReadiness(routes, fixedWired(false, ChannelLog, ChannelMock))
	p2 := find(res, SeverityP2)
	if p2.RoutesConfigured != 2 || p2.RoutesEnabled != 1 || p2.HumanRoutesEnabled != 0 {
		t.Fatalf("counts wrong: %+v", p2)
	}
	if !p2.Required || find(res, SeverityP3).Required {
		t.Fatal("p1/p2 are required for readiness, p3 is not")
	}
	// Today nothing can be ready: no human-notification kind exists.
	for _, r := range res {
		if r.Ready {
			t.Fatalf("%s must not be ready with only log/mock routes", r.Severity)
		}
	}
}

func TestReadinessSnapshot_StaleAndUnevaluatedAreNotReady(t *testing.T) {
	now := time.Now()
	ready := []SeverityReadiness{{Severity: SeverityP1, Required: true, Ready: true, Reason: ReadinessReady}}
	snap := ReadinessSnapshot{Evaluated: true, EvaluatedAt: now, Severities: ready}
	if !find(snap.Effective(now.Add(time.Second), time.Minute), SeverityP1).Ready {
		t.Fatal("a fresh ready snapshot must be ready")
	}
	if got := find(snap.Effective(now.Add(2*time.Minute), time.Minute), SeverityP1); got.Ready || got.Reason != ReadinessStale {
		t.Fatalf("a stale snapshot must be NOT ready (stale): %+v", got)
	}
	if got := find((ReadinessSnapshot{}).Effective(now, time.Minute), SeverityP1); got.Ready || got.Reason != ReadinessNotEvaluated {
		t.Fatalf("an unevaluated snapshot must be not ready: %+v", got)
	}
	for _, r := range []ReadinessReason{ReadinessNoRoute, ReadinessRouteDisabled, ReadinessNotHuman, ReadinessNotWired, ReadinessStale, ReadinessNotEvaluated} {
		if MissingOperatorInput(r) == "" {
			t.Errorf("reason %s must name the missing operator input", r)
		}
	}
	if MissingOperatorInput(ReadinessReady) != "" {
		t.Fatal("ready has no missing input")
	}
}

type captureLog struct{ buf bytes.Buffer }

func (c *captureLog) Error(msg string, args ...any) {
	l := slog.New(slog.NewTextHandler(&c.buf, nil))
	l.Error(msg, args...)
}

func TestReadinessTracker_LogsTransitionsAndRepeats(t *testing.T) {
	clk := &manualClockUnit{now: time.Now()}
	tr := &readinessTracker{clock: clk, staleAfter: time.Minute}
	lg := &captureLog{}
	notReady := []SeverityReadiness{{Severity: SeverityP1, Required: true, Reason: ReadinessNotHuman}, {Severity: SeverityP2, Required: true, Reason: ReadinessNotHuman}}
	ready := []SeverityReadiness{{Severity: SeverityP1, Required: true, Ready: true, Reason: ReadinessReady}, {Severity: SeverityP2, Required: true, Reason: ReadinessNotHuman}}

	tr.update(lg, clk.now, notReady)
	if n := strings.Count(lg.buf.String(), "alert_routing_not_ready"); n != 2 {
		t.Fatalf("first evaluation must log not-ready for p1 and p2, got %d: %s", n, lg.buf.String())
	}
	lg.buf.Reset()
	clk.now = clk.now.Add(15 * time.Second)
	tr.update(lg, clk.now, notReady)
	if lg.buf.Len() != 0 {
		t.Fatalf("unchanged not-ready within 10m must not log again: %s", lg.buf.String())
	}
	clk.now = clk.now.Add(readinessRepeatLogEvery)
	tr.update(lg, clk.now, notReady)
	if !strings.Contains(lg.buf.String(), "alert_routing_not_ready") {
		t.Fatal("not-ready must be re-logged after the repeat interval")
	}
	lg.buf.Reset()
	tr.update(lg, clk.now, ready)
	if !strings.Contains(lg.buf.String(), "alert_routing_ready") || !strings.Contains(lg.buf.String(), "severity=p1") {
		t.Fatalf("not-ready -> ready transition must log: %s", lg.buf.String())
	}
	lg.buf.Reset()
	tr.update(lg, clk.now, notReady)
	if !strings.Contains(lg.buf.String(), "alert_routing_not_ready") {
		t.Fatal("ready -> not-ready transition must log")
	}
}

type manualClockUnit struct{ now time.Time }

func (m *manualClockUnit) Now() time.Time                           { return m.now }
func (m *manualClockUnit) Sleep(_ context.Context, _ time.Duration) {}

func TestDeliveryState_NonHumanSentIsNeverDelivered(t *testing.T) {
	cases := []struct {
		event string
		kind  ChannelKind
		want  string
	}{
		{"", "", DeliveryStatePending},
		{"claimed", ChannelLog, DeliveryStatePending},
		{"failed", ChannelMock, DeliveryStateRetrying},
		{"sent", ChannelLog, DeliveryStateRecordedNonHuman},
		{"sent", ChannelMock, DeliveryStateRecordedNonHuman},
		{"sent", "pager", DeliveryStateDelivered},
		{"unrouted", "", DeliveryStateUnrouted},
		{"dead", ChannelLog, DeliveryStateDeliveryFailed},
		{"suppressed_simulation", "", DeliveryStateSuppressed},
	}
	for _, c := range cases {
		if got := DeliveryState(c.event, c.kind); got != c.want {
			t.Errorf("DeliveryState(%q,%q) = %q, want %q", c.event, c.kind, got, c.want)
		}
	}
}

func TestValidateRecipientRef_Table(t *testing.T) {
	ok := []string{"ops", "mock:p1", "team-a.rota_1", "svc:oncall-primary"}
	bad := []string{
		"", "Ops", "ops@example.com", "+15551234567", "5551234567",
		"169.254.169.254:80", "10.0.0.5", "host.internal:8080", "https://evil.example/hook", "a/b",
		"0123456789abcdef0123456789abcdef", // 32-hex routing-key shape
		"key-0123456789abcdef0123456789abcdef-x",
	}
	for _, r := range ok {
		if err := ValidateRecipientRef(r); err != nil {
			t.Errorf("%q should be accepted: %v", r, err)
		}
	}
	for _, r := range bad {
		if err := ValidateRecipientRef(r); err == nil {
			t.Errorf("%q should be refused", r)
		}
	}
	if err := ValidateRecipientRef("ops@example.com"); err != nil && strings.Contains(err.Error(), "example.com") {
		t.Fatal("the error must never echo the value")
	}
}

// The test channel is importable from tests only (design section 6).
func TestStatic_AlertingtestImportedOnlyByTests(t *testing.T) {
	root := staticRepoRoot(t)
	needle := regexp.MustCompile(`"github.com/Diansalas/igaming-platform/internal/alerting/alertingtest"`)
	var offenders []string
	staticWalkNonTestGoFiles(t, root, func(path string, src []byte) {
		rel, _ := filepath.Rel(root, path)
		if strings.HasPrefix(rel, filepath.Join("internal", "alerting", "alertingtest")+string(filepath.Separator)) {
			return // the package itself
		}
		if needle.Match(src) {
			offenders = append(offenders, rel)
		}
	})
	if len(offenders) != 0 {
		t.Fatalf("alertingtest imported by non-test files: %v", offenders)
	}
}

// No migration and no non-test Go file may insert a route or a recipient
// (HD-PRH2-4: routes are human-authored through the audited endpoint only).
func TestStatic_NoMigrationOrProductionCodeSeedsARoute(t *testing.T) {
	root := staticRepoRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	ins := regexp.MustCompile(`(?is)INSERT\s+INTO\s+alert_routes`)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(root, "migrations", e.Name()))
		if ins.Match(b) {
			t.Errorf("migration %s inserts into alert_routes", e.Name())
		}
	}
	staticWalkNonTestGoFiles(t, root, func(path string, src []byte) {
		rel, _ := filepath.Rel(root, path)
		if strings.HasPrefix(rel, filepath.Join("internal", "alerting", "alertingtest")) {
			return
		}
		if ins.Match(src) && rel != filepath.Join("internal", "httpserver", "alert_routing_handlers.go") {
			t.Errorf("%s inserts into alert_routes outside the audited authoring endpoint", rel)
		}
	})
}

// Security H-1: every function 0117 creates pins search_path with pg_temp last.
func TestMigration0117_EveryCreatedFunctionPinsSearchPath(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(staticRepoRoot(t), "migrations", "0117_alert_routing_readiness.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	created := regexp.MustCompile(`(?m)^CREATE (?:OR REPLACE )?FUNCTION (\w+)`).FindAllStringSubmatch(src, -1)
	if len(created) != len(alertingFunctionsPinned0117Created) {
		t.Fatalf("0117 creates %d functions, test lists %d: update the lists", len(created), len(alertingFunctionsPinned0117Created))
	}
	pins := strings.Count(src, "SET search_path = pg_catalog, public, pg_temp")
	// 7 ALTER FUNCTION pins + one per created function.
	if want := 7 + len(created); pins != want {
		t.Fatalf("found %d search_path pins, want %d", pins, want)
	}
	for _, m := range created {
		if !alertingFunctionsPinned0117Created[m[1]] {
			t.Errorf("unlisted created function %s", m[1])
		}
	}
}

var alertingFunctionsPinned0117Created = map[string]bool{
	"alerting_channel_kind_is_human_notification": true,
	"alert_routes_guard":                          true,
}

var _ = io.Discard

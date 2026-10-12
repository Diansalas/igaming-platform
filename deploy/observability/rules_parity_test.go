package observability_test

// Parity test for the alert rule files in this directory (OPS-READINESS-LOCAL).
//
// Purpose: a rule file nobody evaluates rots silently (a metric gets renamed, the rule keeps
// referencing a series that no longer exists, and the day it matters nothing fires). This test makes
// that a build failure. It proves, using only the standard library plus the OpenTelemetry modules the
// platform already depends on:
//
//   - every metric name and label key used in prometheus-alert-rules.yml is emitted by the code
//     (custom instruments are found in the Go source; the otelhttp request-duration histogram is
//     proven by running the real otelhttp handler and reading what it records);
//   - every `event:` in log-event-alert-rules.yml is a string literal in non-test Go code;
//   - every entry in pending-signals.yml still names a metric that does NOT exist (so a newly added
//     instrument forces the entry to be promoted into the real rules);
//   - every runbook path referenced exists, and each file states that it is NOT wired and that
//     ALERT-DELIVERY-1 is OPEN (so none of these files can be mistaken for working alerting).
//
// It does not evaluate PromQL (no promtool in this repository). Syntax errors in an expression are
// therefore NOT caught here; that is a stated limit, checked by the CI promtool step once a backend
// exists.

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"net/http"
	"net/http/httptest"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.yaml.in/yaml/v3"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// goSources returns the concatenated text of every non-test .go file under internal/ and cmd/.
func goSources(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	var sb strings.Builder
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sb.Write(b)
			sb.WriteByte('\n')
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	return sb.String()
}

// goFiles returns every non-test .go file under internal/ and cmd/ (path -> text).
func goFiles(t *testing.T) map[string]string {
	t.Helper()
	root := repoRoot(t)
	out := map[string]string{}
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[p] = string(b)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	return out
}

var instrumentVarRe = regexp.MustCompile(`(\w+)\s*,\s*_\s*:?=\s*\w+\.(?:Int64|Float64)\w+\(\s*"([a-z][a-z0-9_.]*)"`)
var spanEndRe = regexp.MustCompile(`\n}\n`)

// metricAttrKeys maps each custom metric (Prometheus name) to the attribute keys recorded ON THAT
// INSTRUMENT: for a synchronous instrument held in a variable, the keys inside the functions that call
// <var>.Add/Record; for observable gauges (no variable), the keys used anywhere in the instrument's own package
// directory. A label that is recorded only on some OTHER instrument does not count.
func metricAttrKeys(files map[string]string) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	dirKeys := map[string]map[string]bool{}
	for path, text := range files {
		d := filepath.Dir(path)
		if dirKeys[d] == nil {
			dirKeys[d] = map[string]bool{}
		}
		for k := range emittedAttrKeys(text) {
			dirKeys[d][k] = true
		}
	}
	for path, text := range files {
		fileKeys := dirKeys[filepath.Dir(path)] // same package: helpers such as severityAttr live in sibling files
		for _, m := range instrumentRe.FindAllStringSubmatch(text, -1) {
			out[strings.ReplaceAll(m[1], ".", "_")] = fileKeys
		}
		for _, m := range instrumentVarRe.FindAllStringSubmatch(text, -1) {
			name := strings.ReplaceAll(m[2], ".", "_")
			keys := map[string]bool{}
			for _, call := range regexp.MustCompile(regexp.QuoteMeta(m[1])+`\.(?:Add|Record)\(`).FindAllStringIndex(text, -1) {
				span := text[call[0]:]
				if end := spanEndRe.FindStringIndex(span); end != nil {
					span = span[:end[0]]
				}
				for k := range emittedAttrKeys(span) {
					keys[k] = true
				}
				// one level of same-file helpers (e.g. severityAttr) used to build the attributes
				for _, h := range regexp.MustCompile(`\b([a-zA-Z_]\w*)\(`).FindAllStringSubmatch(span, -1) {
					if loc := regexp.MustCompile(`(?m)^func ` + regexp.QuoteMeta(h[1]) + `\(`).FindStringIndex(text); loc != nil {
						body := text[loc[0]:]
						if end := spanEndRe.FindStringIndex(body); end != nil {
							body = body[:end[0]]
						}
						for k := range emittedAttrKeys(body) {
							keys[k] = true
						}
					}
				}
			}
			if len(keys) > 0 {
				out[name] = keys
			}
		}
	}
	return out
}

var instrumentRe = regexp.MustCompile(`\.(?:Int64|Float64)(?:Counter|UpDownCounter|Histogram|Gauge|ObservableCounter|ObservableUpDownCounter|ObservableGauge)\(\s*"([a-z][a-z0-9_.]*)"`)
var attrKeyRe = regexp.MustCompile(`attribute\.(?:String|Int|Int64|Bool|Float64)\(\s*"([a-z][a-z0-9_.]*)"`)

// emittedCustomMetrics: instrument names created in the platform's own code.
func emittedCustomMetrics(src string) map[string]bool {
	out := map[string]bool{}
	for _, m := range instrumentRe.FindAllStringSubmatch(src, -1) {
		out[strings.ReplaceAll(m[1], ".", "_")] = true
	}
	return out
}

func emittedAttrKeys(src string) map[string]bool {
	out := map[string]bool{}
	for _, m := range attrKeyRe.FindAllStringSubmatch(src, -1) {
		out[strings.ReplaceAll(m[1], ".", "_")] = true
	}
	return out
}

// otelhttpSeries runs the real otelhttp handler (same wrapper internal/httpserver/server.go uses)
// and returns the Prometheus-translated series names and label keys it records.
func otelhttpSeries(t *testing.T) (series map[string]bool, labels map[string]bool, routeValues map[string]bool) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	h := otelhttp.NewHandler(mux, "parity-test", otelhttp.WithMeterProvider(mp))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("probe handler: got %d", rec.Code)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	series, labels, routeValues = map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "http.server.request.duration" {
				continue
			}
			base := strings.ReplaceAll(m.Name, ".", "_")
			if m.Unit == "s" {
				base += "_seconds"
			}
			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("http.server.request.duration is %T, expected a float64 histogram", m.Data)
			}
			for _, suffix := range []string{"_bucket", "_count", "_sum"} {
				series[base+suffix] = true
			}
			for _, dp := range hist.DataPoints {
				for _, kv := range dp.Attributes.ToSlice() {
					k := strings.ReplaceAll(string(kv.Key), ".", "_")
					labels[k] = true
					if k == "http_route" {
						routeValues[kv.Value.Emit()] = true
					}
				}
			}
		}
	}
	if len(series) == 0 {
		t.Fatal("otelhttp recorded no http.server.request.duration series: the 5xx and readiness rules would reference nothing")
	}
	return series, labels, routeValues
}

// ---- expression extraction ----------------------------------------------------------------------

type rule struct {
	name string
	expr string
	line string
}

// parseRules extracts `- alert: Name` and its `expr:` (single line or `|` block) from a Prometheus
// rule file. A deliberately small scanner: no YAML dependency exists in this module.
func parseRules(text string) []rule {
	lines := strings.Split(text, "\n")
	var out []rule
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		trim := strings.TrimSpace(l)
		if !strings.HasPrefix(trim, "- alert:") {
			continue
		}
		r := rule{name: strings.TrimSpace(strings.TrimPrefix(trim, "- alert:"))}
		indent := len(l) - len(strings.TrimLeft(l, " "))
		for j := i + 1; j < len(lines); j++ {
			lj := lines[j]
			tj := strings.TrimSpace(lj)
			ij := len(lj) - len(strings.TrimLeft(lj, " "))
			if tj != "" && ij <= indent && strings.HasPrefix(tj, "- ") {
				break
			}
			if strings.HasPrefix(tj, "expr:") {
				rest := strings.TrimSpace(strings.TrimPrefix(tj, "expr:"))
				if rest == "|" || rest == ">" {
					var parts []string
					for k := j + 1; k < len(lines); k++ {
						lk := lines[k]
						if strings.TrimSpace(lk) == "" {
							continue
						}
						if len(lk)-len(strings.TrimLeft(lk, " ")) <= ij {
							break
						}
						parts = append(parts, strings.TrimSpace(lk))
					}
					r.expr = strings.Join(parts, " ")
				} else {
					r.expr = rest
				}
				break
			}
		}
		out = append(out, r)
	}
	return out
}

var (
	stringLitRe = regexp.MustCompile(`"[^"]*"`)
	rangeRe     = regexp.MustCompile(`\[[^\]]*\]`)
	braceRe     = regexp.MustCompile(`\{([^}]*)\}`)
	matcherRe   = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*(?:=~|!~|!=|=)`)
	byRe        = regexp.MustCompile(`\b(?:by|without|on|ignoring)\s*\(([^)]*)\)`)
	identRe     = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)
)

var promKeywords = map[string]bool{
	"by": true, "without": true, "on": true, "ignoring": true, "and": true, "or": true, "unless": true,
	"offset": true, "bool": true, "group_left": true, "group_right": true,
	"sum": true, "min": true, "max": true, "avg": true, "count": true, "stddev": true, "topk": true, "bottomk": true,
}

// analyse returns the metric names and label keys an expression references.
func analyse(expr string) (metrics []string, labels []string) {
	e := stringLitRe.ReplaceAllString(expr, `""`)
	e = rangeRe.ReplaceAllString(e, "")
	for _, m := range braceRe.FindAllStringSubmatch(e, -1) {
		for _, mm := range matcherRe.FindAllStringSubmatch(m[1], -1) {
			labels = append(labels, mm[1])
		}
	}
	e = braceRe.ReplaceAllString(e, "")
	for _, m := range byRe.FindAllStringSubmatch(e, -1) {
		for _, l := range strings.Split(m[1], ",") {
			if l = strings.TrimSpace(l); l != "" {
				labels = append(labels, l)
			}
		}
	}
	e = byRe.ReplaceAllString(e, " ")
	for _, loc := range identRe.FindAllStringIndex(e, -1) {
		id := e[loc[0]:loc[1]]
		rest := strings.TrimLeft(e[loc[1]:], " ")
		if strings.HasPrefix(rest, "(") || promKeywords[id] {
			continue
		}
		// a bare number suffix such as the "m" in 5m is removed with the range; anything left is a metric
		metrics = append(metrics, id)
	}
	return metrics, labels
}

// ---- tests ---------------------------------------------------------------------------------------

func TestPrometheusRulesReferenceOnlyEmittedSeries(t *testing.T) {
	root := repoRoot(t)
	src := goSources(t)
	custom := emittedCustomMetrics(src)
	if len(custom) < 10 {
		t.Fatalf("found only %d custom instruments in source; the scanner is broken", len(custom))
	}
	httpSeries, httpLabels, routes := otelhttpSeries(t)
	perMetric := metricAttrKeys(goFiles(t))

	rules := parseRules(readFile(t, filepath.Join(root, "deploy/observability/prometheus-alert-rules.yml")))
	if len(rules) < 15 {
		t.Fatalf("parsed only %d rules; the parser or the file is broken", len(rules))
	}
	seen := map[string]bool{}
	for _, r := range rules {
		if seen[r.name] {
			t.Errorf("duplicate alert name %s", r.name)
		}
		seen[r.name] = true
		if strings.TrimSpace(r.expr) == "" {
			t.Errorf("%s: no expr found", r.name)
			continue
		}
		ms, ls := analyse(r.expr)
		allowed := map[string]bool{}
		for _, m := range ms {
			if httpSeries[m] {
				for k := range httpLabels {
					allowed[k] = true
				}
			}
			for k := range perMetric[m] {
				allowed[k] = true
			}
		}
		if len(ms) == 0 {
			t.Errorf("%s: expression references no metric: %q", r.name, r.expr)
		}
		for _, m := range ms {
			if !custom[m] && !httpSeries[m] {
				t.Errorf("%s: metric %q is not emitted by the code (custom instruments: found in source; http: proven by running otelhttp)", r.name, m)
			}
		}
		for _, l := range ls {
			if !allowed[l] {
				t.Errorf("%s: label %q is not an attribute key recorded on the metric(s) this expression uses (%v)", r.name, l, ms)
			}
		}
	}

	// The readiness rule pins an http_route VALUE; prove the real handler records exactly that value.
	if !strings.Contains(readFile(t, filepath.Join(root, "deploy/observability/prometheus-alert-rules.yml")), `http_route="/readyz"`) {
		t.Error("readiness rule no longer pins http_route=\"/readyz\"; update this test with it")
	}
	if !routes["/readyz"] {
		var got []string
		for r := range routes {
			got = append(got, r)
		}
		sort.Strings(got)
		t.Errorf("otelhttp does not record http.route=\"/readyz\" for the mux pattern; recorded %v: fix the ReadinessFailing rule", got)
	}
}

func TestReadyzRouteIsRegisteredWithTheAssumedPattern(t *testing.T) {
	src := readFile(t, filepath.Join(repoRoot(t), "internal/httpserver/health.go")) +
		readFile(t, filepath.Join(repoRoot(t), "internal/httpserver/server.go"))
	if !strings.Contains(src, `"GET /readyz"`) {
		t.Error(`internal/httpserver no longer registers "GET /readyz"; the ReadinessFailing rule's http_route value is stale`)
	}
}

var eventRe = regexp.MustCompile(`(?m)^\s+event:\s*"([^"]+)"`)

func TestLogEventRulesReferenceRealLogEvents(t *testing.T) {
	root := repoRoot(t)
	src := goSources(t)
	text := readFile(t, filepath.Join(root, "deploy/observability/log-event-alert-rules.yml"))
	events := eventRe.FindAllStringSubmatch(text, -1)
	if len(events) < 8 {
		t.Fatalf("parsed only %d log events; parser or file broken", len(events))
	}
	for _, e := range events {
		if !strings.Contains(src, `"`+e[1]+`"`) {
			t.Errorf("log event %q is not a string literal in any non-test Go file: the rule matches nothing", e[1])
		}
	}
	for _, p := range regexp.MustCompile(`(?m)^\s+source:\s*(\S+)`).FindAllStringSubmatch(text, -1) {
		if _, err := os.Stat(filepath.Join(root, p[1])); err != nil {
			t.Errorf("source file %s does not exist", p[1])
		}
	}
}

func TestPendingSignalsAreStillAbsent(t *testing.T) {
	root := repoRoot(t)
	src := goSources(t)
	custom := emittedCustomMetrics(src)
	text := readFile(t, filepath.Join(root, "deploy/observability/pending-signals.yml"))
	names := regexp.MustCompile(`(?m)^\s+proposed_metric:\s*(\S+)`).FindAllStringSubmatch(text, -1)
	if len(names) < 3 {
		t.Fatalf("parsed %d pending signals; expected the db pool, reconciliation staleness and drift entries", len(names))
	}
	for _, n := range names {
		if custom[n[1]] || strings.Contains(src, `"`+n[1]+`"`) {
			t.Errorf("pending signal %q now exists in the code: promote it into prometheus-alert-rules.yml and delete the pending entry", n[1])
		}
	}
}

func TestRuleFilesAreHonestAboutBeingUnwired(t *testing.T) {
	root := repoRoot(t)
	for _, f := range []string{"prometheus-alert-rules.yml", "log-event-alert-rules.yml", "pending-signals.yml"} {
		text := readFile(t, filepath.Join(root, "deploy/observability", f))
		head := text
		if len(head) > 1800 {
			head = head[:1800]
		}
		if !strings.Contains(head, "NOT WIRED") {
			t.Errorf("%s must state NOT WIRED in its header", f)
		}
		if !strings.Contains(head, "ALERT-DELIVERY-1 is OPEN") {
			t.Errorf("%s must state ALERT-DELIVERY-1 is OPEN in its header", f)
		}
	}
}

func TestRunbookReferencesResolve(t *testing.T) {
	root := repoRoot(t)
	re := regexp.MustCompile(`(?m)^\s+runbook:\s*(docs/\S+?\.md)`)
	for _, f := range []string{"prometheus-alert-rules.yml", "log-event-alert-rules.yml"} {
		text := readFile(t, filepath.Join(root, "deploy/observability", f))
		refs := re.FindAllStringSubmatch(text, -1)
		if len(refs) == 0 {
			t.Errorf("%s references no runbook", f)
		}
		for _, r := range refs {
			if _, err := os.Stat(filepath.Join(root, r[1])); err != nil {
				t.Errorf("%s: runbook %s does not exist", f, r[1])
			}
		}
	}
}

func TestEveryPrometheusRuleHasSeverityAndRunbook(t *testing.T) {
	root := repoRoot(t)
	text := readFile(t, filepath.Join(root, "deploy/observability/prometheus-alert-rules.yml"))
	n := strings.Count(text, "- alert:")
	if got := len(regexp.MustCompile(`(?m)^\s+severity:\s*(page|ticket|info)\s*$`).FindAllString(text, -1)); got != n {
		t.Errorf("%d rules but %d valid severity labels (page|ticket|info)", n, got)
	}
	if got := len(regexp.MustCompile(`(?m)^\s+runbook:\s*docs/`).FindAllString(text, -1)); got != n {
		t.Errorf("%d rules but %d runbook annotations", n, got)
	}
	// No receiver or recipient may be invented in a rule file.
	for _, bad := range []string{"receiver:", "@", "pagerduty", "slack", "webhook_url", "email:", "routing_key"} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(bad)) {
			t.Errorf("rule file mentions %q: no receiver or recipient may be invented here (ALERT-DELIVERY-1)", bad)
		}
	}
}

// ---- structural YAML checks (go.yaml.in/yaml/v3, already in the module graph) -------------------------

type promFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Alert       string            `yaml:"alert"`
			Expr        string            `yaml:"expr"`
			For         string            `yaml:"for"`
			Labels      map[string]string `yaml:"labels"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

func TestPrometheusRulesAreStructurallyValidYAML(t *testing.T) {
	text := readFile(t, filepath.Join(repoRoot(t), "deploy/observability/prometheus-alert-rules.yml"))
	var f promFile
	if err := yaml.Unmarshal([]byte(text), &f); err != nil {
		t.Fatalf("prometheus-alert-rules.yml is not valid YAML: %v", err)
	}
	n := 0
	durRe := regexp.MustCompile(`^[0-9]+(ms|s|m|h|d|w|y)$`)
	for _, g := range f.Groups {
		if g.Name == "" || len(g.Rules) == 0 {
			t.Errorf("group %q is empty or unnamed", g.Name)
		}
		for _, r := range g.Rules {
			n++
			if r.Alert == "" || strings.TrimSpace(r.Expr) == "" {
				t.Errorf("rule %q: alert and expr are required", r.Alert)
			}
			if r.For != "" && !durRe.MatchString(r.For) {
				t.Errorf("%s: bad for: %q", r.Alert, r.For)
			}
			if sev := r.Labels["severity"]; sev != "page" && sev != "ticket" && sev != "info" {
				t.Errorf("%s: severity %q is not page|ticket|info", r.Alert, sev)
			}
			if r.Annotations["summary"] == "" || r.Annotations["runbook"] == "" {
				t.Errorf("%s: summary and runbook annotations are required", r.Alert)
			}
			if strings.Count(r.Expr, "(") != strings.Count(r.Expr, ")") || strings.Count(r.Expr, "{") != strings.Count(r.Expr, "}") || strings.Count(r.Expr, "[") != strings.Count(r.Expr, "]") {
				t.Errorf("%s: unbalanced brackets in expr (PromQL syntax is otherwise NOT validated here)", r.Alert)
			}
		}
	}
	if n != strings.Count(text, "- alert:") {
		t.Errorf("YAML parse found %d rules but the text has a different number of '- alert:' entries", n)
	}
}

func TestOtherRuleFilesAreValidYAML(t *testing.T) {
	root := repoRoot(t)
	var logf struct {
		Rules []map[string]string `yaml:"rules"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(root, "deploy/observability/log-event-alert-rules.yml"))), &logf); err != nil {
		t.Fatalf("log-event-alert-rules.yml: %v", err)
	}
	for _, r := range logf.Rules {
		for _, k := range []string{"id", "severity", "event", "match", "threshold", "source", "runbook"} {
			if r[k] == "" {
				t.Errorf("log rule %q missing %s", r["id"], k)
			}
		}
	}
	var pend struct {
		Pending []map[string]any `yaml:"pending"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(root, "deploy/observability/pending-signals.yml"))), &pend); err != nil {
		t.Fatalf("pending-signals.yml: %v", err)
	}
	if len(pend.Pending) < 3 {
		t.Errorf("expected at least 3 pending signals, got %d", len(pend.Pending))
	}
}

// PromQL syntax is NOT validated by this suite: that needs promtool or the Prometheus PromQL parser,
// a new dependency this repository has not adopted. Recommended once a backend exists: run
// `promtool check rules deploy/observability/prometheus-alert-rules.yml` in CI.

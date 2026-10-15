package observability_test

// A9: the alert rules are only worth anything if the names they use are the names a scraper actually sees. This
// file scrapes the REAL Prometheus exposition (internal/observability.InitMetricsExposed, the same handler
// cmd/platform-api serves on METRICS_ADDR) and asserts the series and labels the rules depend on, and it checks
// the structure of the scrape-config and compose examples shipped next to the rules.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.yaml.in/yaml/v3"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/observability"
)

func scrapeText(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape = %d", rec.Code)
	}
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

type noopPurger struct{}

func (noopPurger) PurgeResolved(context.Context, time.Duration, int) (alerting.PurgeResult, error) {
	return alerting.PurgeResult{}, nil
}

func TestExposition_RuleSeriesAreScrapedWithTheNamesAndLabelsTheRulesUse(t *testing.T) {
	_, handler, shutdown, err := observability.InitMetricsExposed(context.Background(), "platform-api", "none")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	// The real otelhttp wrapper on a /readyz that answers 503, as internal/httpserver does.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	api := otelhttp.NewHandler(mux, "platform-api")
	api.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/readyz", nil))

	// A real retention tick records alert_retention_runs_total and alert_retention_last_run_unix_seconds.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		alerting.RunRetentionLoop(ctx, noopPurger{}, alerting.RetentionConfig{
			OlderThan: 8 * 24 * time.Hour,
			OnTick:    func(string) { cancel() },
		})
		close(done)
	}()
	<-done

	body := scrapeText(t, handler)
	for _, want := range []string{
		// otelhttp: the series and the two labels ReadinessFailing selects on
		`http_server_request_duration_seconds_count{`,
		`http_route="/readyz"`,
		`http_response_status_code="503"`,
		// alerting
		`alert_retention_runs_total{`,
		`result="ok"`,
		`alert_retention_last_run_unix_seconds`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the scraped exposition lacks %q", want)
		}
	}
	// A counter whose name already ends in _total is not suffixed twice, and no series is renamed with a
	// namespace prefix: the rules would silently match nothing.
	for _, bad := range []string{"_total_total", "platform_api_alert_", "platform_api_http_"} {
		if strings.Contains(body, bad) {
			t.Errorf("the exposition contains %q", bad)
		}
	}
	// Go runtime collectors are present (a basic sanity signal for a scraper) and no secret-looking text.
	if !strings.Contains(body, "go_goroutines") {
		t.Error("go_goroutines missing from the exposition")
	}
	for _, bad := range []string{"password", "secret", "DATABASE_URL", "Bearer "} {
		if strings.Contains(body, bad) {
			t.Errorf("the exposition mentions %q", bad)
		}
	}
}

// SR9-A9-1: otelhttp records server.address / server.port from the client-controlled Host header. Thousands of
// distinct Host values must not grow the exposition, and must not push the 5xx and /readyz series (which the
// rules select on) into the SDK overflow series that carries no status or route label.
func TestExposition_HostHeaderFloodIsBoundedAndRuleSeriesSurvive(t *testing.T) {
	_, handler, shutdown, err := observability.InitMetricsExposed(context.Background(), "platform-api", "none")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	mux.HandleFunc("GET /boom", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	mux.HandleFunc("GET /ok", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	api := otelhttp.NewHandler(mux, "platform-api")

	paths := []string{"/ok", "/readyz", "/boom"}
	for i := 0; i < 3000; i++ {
		req := httptest.NewRequest(http.MethodGet, paths[i%len(paths)], nil)
		req.Host = "h" + strings.Repeat("x", i%7) + "-" + strconv.Itoa(i) + ".attacker.example:" + strconv.Itoa(1024+i%60000)
		api.ServeHTTP(httptest.NewRecorder(), req)
	}
	// One normal request after the flood, as the rules would see it.
	api.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/readyz", nil))

	body := scrapeText(t, handler)
	for _, bad := range []string{"server_address", "server_port", "attacker.example", "otel_metric_overflow", "network_protocol", "user_agent"} {
		if strings.Contains(body, bad) {
			t.Errorf("the exposition carries %q after a Host flood", bad)
		}
	}
	httpLines := 0
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "http_server") {
			httpLines++
		}
	}
	// method x status x route x scheme is a handful of series, each with ~15 histogram lines.
	if httpLines > 200 {
		t.Errorf("%d http_server lines after 3000 distinct Hosts: the label set is not bounded", httpLines)
	}
	if len(body) > 200_000 {
		t.Errorf("exposition is %d bytes after a Host flood", len(body))
	}
	for _, want := range []string{
		`http_server_request_duration_seconds_count{`,
		`http_route="/readyz"`,
		`http_response_status_code="503"`,
		`http_route="/boom"`,
		`http_response_status_code="500"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the exposition lacks %q after a Host flood", want)
		}
	}
	// The exact series the rules select on, with the full count (1000 flood + 1 normal readyz 503).
	re := regexp.MustCompile(`(?m)^http_server_request_duration_seconds_count\{[^}]*http_response_status_code="503"[^}]*http_route="/readyz"[^}]*\} (\d+)`)
	re2 := regexp.MustCompile(`(?m)^http_server_request_duration_seconds_count\{[^}]*http_route="/readyz"[^}]*http_response_status_code="503"[^}]*\} (\d+)`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		m = re2.FindStringSubmatch(body)
	}
	if m == nil || m[1] != "1001" {
		t.Errorf("the /readyz 503 series count = %v, want 1001 (all requests land in ONE series)", m)
	}
}

func TestScrapeConfigExampleIsConsistentWithTheRulesAndMetricsListener(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "deploy/observability")

	var prom struct {
		Global struct {
			ScrapeInterval string `yaml:"scrape_interval"`
		} `yaml:"global"`
		RuleFiles     []string `yaml:"rule_files"`
		ScrapeConfigs []struct {
			JobName     string `yaml:"job_name"`
			MetricsPath string `yaml:"metrics_path"`
			Scheme      string `yaml:"scheme"`
			Static      []struct {
				Targets []string          `yaml:"targets"`
				Labels  map[string]string `yaml:"labels"`
			} `yaml:"static_configs"`
		} `yaml:"scrape_configs"`
		Alerting map[string]any `yaml:"alerting"`
	}
	raw := readFile(t, filepath.Join(dir, "prometheus.yml"))
	if err := yaml.Unmarshal([]byte(raw), &prom); err != nil {
		t.Fatalf("prometheus.yml: %v", err)
	}
	if len(prom.ScrapeConfigs) != 1 || prom.ScrapeConfigs[0].JobName != "platform-api" {
		t.Fatalf("scrape configs = %+v", prom.ScrapeConfigs)
	}
	sc := prom.ScrapeConfigs[0]
	if sc.MetricsPath != "/metrics" {
		t.Errorf("metrics_path = %q, want /metrics (the only route of the metrics listener)", sc.MetricsPath)
	}
	if len(sc.Static) == 0 || len(sc.Static[0].Targets) == 0 {
		t.Fatal("no scrape target")
	}
	for _, tgt := range sc.Static[0].Targets {
		if !regexp.MustCompile(`^[A-Za-z0-9.\-]+:[0-9]{2,5}$`).MatchString(tgt) {
			t.Errorf("target %q is not host:port", tgt)
		}
		// 9464 is the port documented for METRICS_ADDR; it must differ from the public API port.
		if strings.HasSuffix(tgt, ":8080") {
			t.Errorf("target %q is the public API port: metrics are on a separate listener", tgt)
		}
	}
	// No credential of any kind in the example, and no Alertmanager receiver.
	lower := strings.ToLower(raw)
	for _, bad := range []string{"password", "bearer_token", "authorization", "basic_auth", "secret", "api_key"} {
		if strings.Contains(lower, bad+":") {
			t.Errorf("prometheus.yml configures %q", bad)
		}
	}
	if prom.Alerting != nil {
		t.Error("prometheus.yml configures an Alertmanager: no receiver or recipient may be invented (ALERT-DELIVERY-1)")
	}
	// Every rule file is the shipped rules file, mounted at that path by the compose example.
	for _, rf := range prom.RuleFiles {
		if filepath.Base(rf) != "prometheus-alert-rules.yml" {
			t.Errorf("rule_files entry %q is not the shipped rules file", rf)
		}
	}
	if len(prom.RuleFiles) == 0 {
		t.Error("no rule_files")
	}

	var compose struct {
		Services map[string]struct {
			Image   string   `yaml:"image"`
			Ports   []string `yaml:"ports"`
			Volumes []string `yaml:"volumes"`
			Command []string `yaml:"command"`
		} `yaml:"services"`
	}
	craw := readFile(t, filepath.Join(dir, "docker-compose.observability.yml"))
	if err := yaml.Unmarshal([]byte(craw), &compose); err != nil {
		t.Fatalf("docker-compose.observability.yml: %v", err)
	}
	p, ok := compose.Services["prometheus"]
	if !ok || len(compose.Services) != 1 {
		t.Fatalf("compose services = %v, want exactly prometheus", compose.Services)
	}
	if !strings.Contains(p.Image, ":") || strings.HasSuffix(p.Image, ":latest") {
		t.Errorf("image %q must carry an explicit version tag", p.Image)
	}
	for _, port := range p.Ports {
		host := strings.SplitN(port, ":", 2)[0]
		if ip, err := netip.ParseAddr(host); err != nil || !ip.IsLoopback() {
			t.Errorf("port mapping %q publishes the unauthenticated Prometheus UI beyond loopback", port)
		}
	}
	mounted := false
	for _, v := range p.Volumes {
		if strings.Contains(v, "prometheus-alert-rules.yml") {
			mounted = true
			if !strings.HasSuffix(v, ":ro") {
				t.Errorf("volume %q must be read-only", v)
			}
		}
		if strings.HasPrefix(v, "./") && !strings.HasSuffix(v, ":ro") && !strings.Contains(v, "prometheus-data") {
			t.Errorf("host bind mount %q must be read-only", v)
		}
		for _, hostPath := range []string{"/var/run/docker.sock", "/etc", "~/"} {
			if strings.HasPrefix(v, hostPath) {
				t.Errorf("volume %q mounts a sensitive host path", v)
			}
		}
	}
	if !mounted {
		t.Error("the compose example does not mount the rules file")
	}
	for _, f := range []string{"prometheus.yml", "prometheus-alert-rules.yml"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("compose mounts %s which does not exist", f)
		}
	}
	for _, bad := range []string{"password", "secret", "token", "api_key"} {
		if strings.Contains(strings.ToLower(craw), bad+"=") || strings.Contains(strings.ToLower(craw), bad+":") {
			t.Errorf("the compose example sets %q", bad)
		}
	}
}

package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/email"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener")
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func devCfg() config.Config {
	return config.Config{Environment: "development", EnvironmentExplicit: true, HTTPAddr: ":8080"}
}

func TestStartMetricsServer_DisabledByDefaultServesOnlyMetricsWhenEnabled(t *testing.T) {
	// off: a no-op that binds nothing
	stop, err := startMetricsServer(devCfg(), nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, handler, shutdown, err := observability.InitMetricsExposed(context.Background(), "wiring-test", "none")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	cfg := devCfg()
	cfg.MetricsAddr = freeAddr(t)
	stop, err = startMetricsServer(cfg, handler, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) (int, string) {
		var resp *http.Response
		for i := 0; i < 50; i++ {
			resp, err = http.Get("http://" + cfg.MetricsAddr + path)
			if err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/metrics"); code != 200 || !strings.Contains(body, "target_info") {
		t.Fatalf("/metrics = %d (target_info missing)\n%.300s", code, body)
	}
	for _, p := range []string{"/", "/healthz", "/readyz", "/v1/me", "/debug/pprof/"} {
		if code, _ := get(p); code != http.StatusNotFound {
			t.Errorf("%s on the metrics listener = %d, want 404", p, code)
		}
	}
	// the port is released after shutdown
	if err := stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ln, err := net.Listen("tcp", cfg.MetricsAddr); err != nil {
		t.Errorf("the metrics port was not released: %v", err)
	} else {
		_ = ln.Close()
	}
}

func TestStartMetricsServer_BindFailureFailsStartupAndAMissingHandlerIsRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener")
	}
	defer func() { _ = ln.Close() }()
	cfg := devCfg()
	cfg.MetricsAddr = ln.Addr().String()
	if _, err := startMetricsServer(cfg, http.NotFoundHandler(), quietLogger()); err == nil {
		t.Error("binding an address in use must fail startup, not be swallowed")
	}
	cfg.MetricsAddr = freeAddr(t)
	if _, err := startMetricsServer(cfg, nil, quietLogger()); err == nil {
		t.Error("METRICS_ADDR set with no handler must be refused")
	}
}

// ---- alert channel wiring -------------------------------------------------------------------------

type fakeStore struct {
	scheme string
	val    []byte
	err    error
}

func (f fakeStore) Scheme() string { return f.scheme }
func (f fakeStore) Get(context.Context, secretstore.Ref) (secretstore.Secret, error) {
	if f.err != nil {
		return secretstore.Secret{}, f.err
	}
	return secretstore.NewSecret(f.val), nil
}

func bundleWith(backends ...secretstore.Store) providerBundle {
	return providerBundle{Email: email.NewMockProvider(), SecretBackends: backends}
}

func TestBuildAlertSinks_NothingConfiguredBuildsNothing(t *testing.T) {
	sinks, err := buildAlertSinks(devCfg(), bundleWith(), quietLogger())
	if err != nil || len(sinks) != 0 {
		t.Fatalf("sinks=%v err=%v, want none", sinks, err)
	}
}

func TestBuildAlertSinks_EmailOverTheMockProviderIsNotAHumanNotification_AndProductionRefusesIt(t *testing.T) {
	cfg := devCfg()
	cfg.AlertEmailRecipients = []string{"ops@example.com"}
	sinks, err := buildAlertSinks(cfg, bundleWith(), quietLogger())
	if err != nil || len(sinks) != 1 || sinks[0].ChannelKind() != alerting.ChannelEmail || sinks[0].HumanNotification() {
		t.Fatalf("dev email sink: %v err=%v", sinks, err)
	}
	prod := cfg
	prod.Environment = "production"
	if _, err := buildAlertSinks(prod, bundleWith(), quietLogger()); err == nil {
		t.Fatal("production built an email channel over the MOCK provider")
	}
	unset := cfg
	unset.EnvironmentExplicit = false // GuardEnvironment() == production
	if _, err := buildAlertSinks(unset, bundleWith(), quietLogger()); err == nil {
		t.Fatal("an unset APP_ENV (production posture) built an email channel over the MOCK provider")
	}
}

func TestBuildAlertSinks_WebhookNeedsAConfiguredSecretBackendAndMapsStoreErrors(t *testing.T) {
	cfg := devCfg()
	cfg.AlertWebhookURL = "http://127.0.0.1:8099/hook"
	cfg.AlertWebhookSecretRef = "devfile://alert-webhook?version=v1"

	// the backend named by the reference is not configured: startup error
	if _, err := buildAlertSinks(cfg, bundleWith(), quietLogger()); err == nil {
		t.Fatal("a webhook whose secret backend is not configured must fail startup")
	}
	// an unparsable reference
	bad := cfg
	bad.AlertWebhookSecretRef = "a-literal-secret-value"
	if _, err := buildAlertSinks(bad, bundleWith(fakeStore{scheme: "devfile"}), quietLogger()); err == nil {
		t.Fatal("a literal secret in place of a reference was accepted")
	}
	// and the error message never contains the reference or the value
	if _, err := buildAlertSinks(bad, bundleWith(fakeStore{scheme: "devfile"}), quietLogger()); err != nil && strings.Contains(err.Error(), "a-literal-secret-value") {
		t.Errorf("the error message echoes the configured secret: %v", err)
	}

	secret := []byte("0123456789abcdef0123456789abcdef-secret")
	sinks, err := buildAlertSinks(cfg, bundleWith(fakeStore{scheme: "devfile", val: secret}), quietLogger())
	if err != nil || len(sinks) != 1 || sinks[0].ChannelKind() != alerting.ChannelWebhook {
		t.Fatalf("sinks=%v err=%v", sinks, err)
	}
	if sinks[0].HumanNotification() {
		t.Error("a development-posture (loopback) webhook must not claim to notify a person")
	}

	// store error classes
	for _, c := range []struct {
		err  error
		want alerting.ErrorClass
	}{
		{secretstore.NewError(secretstore.ClassUnavailable), alerting.ErrorClassUnavailable},
		{secretstore.NewError(secretstore.ClassNotFound), alerting.ErrorClassMisconfigured},
		{secretstore.NewError(secretstore.ClassAccessDenied), alerting.ErrorClassMisconfigured},
		{secretstore.NewError(secretstore.ClassStoreConfig), alerting.ErrorClassMisconfigured},
	} {
		fn, err := alertWebhookSecretFunc(cfg, []secretstore.Store{fakeStore{scheme: "devfile", err: c.err}})
		if err != nil {
			t.Fatal(err)
		}
		if _, got := fn(context.Background()); got != c.want {
			t.Errorf("store error %v => %q, want %q", c.err, got, c.want)
		}
	}
	fn, _ := alertWebhookSecretFunc(cfg, []secretstore.Store{fakeStore{scheme: "devfile", val: secret}})
	if got, class := fn(context.Background()); class != alerting.ErrorClassNone || string(got) != string(secret) {
		t.Errorf("secret fetch = %q/%q", got, class)
	}
}

// ---- static: the metrics exposition is wired in exactly one place and never on the public mux ------------

func TestStatic_MetricsExpositionIsNeverOnThePublicMux(t *testing.T) {
	root := filepath.Join("..", "..")
	bad := map[string]string{
		"InitMetricsExposed": "internal/observability/ and cmd/platform-api/main.go only",
		"NewMetricsServer":   "cmd/platform-api/observability_wiring.go only",
		"promhttp":           "internal/observability/prometheus.go only",
	}
	allowed := map[string][]string{
		"InitMetricsExposed": {"internal/observability/prometheus.go", "internal/observability/metrics.go", "cmd/platform-api/main.go"},
		"NewMetricsServer":   {"internal/observability/prometheus.go", "cmd/platform-api/observability_wiring.go"},
		"promhttp":           {"internal/observability/prometheus.go"},
	}
	count := map[string]int{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "vendor", "worktrees":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		text := string(b)
		for needle, where := range bad {
			if !strings.Contains(text, needle) {
				continue
			}
			ok := false
			for _, a := range allowed[needle] {
				if rel == a {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s references %s: allowed in %s", rel, needle, where)
			}
			count[needle]++
		}
		if strings.HasPrefix(rel, "internal/httpserver/") && (strings.Contains(text, `"/metrics"`) || strings.Contains(text, " /metrics")) {
			t.Errorf("%s mounts or mentions /metrics on the public API mux", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for needle := range bad {
		if count[needle] == 0 {
			t.Errorf("static scan found no use of %s: the pin is vacuous", needle)
		}
	}
}

// The alert channels are constructed in exactly one file and registered through AddSinks exactly once, so the
// dispatcher's sink set is the log sink plus what buildAlertSinks returns (the older
// TestMain_AlertDispatcherWiring_LogSinkOnlyNeverMock keeps pinning the CONSTRUCTOR sink to the log sink, and that
// the MOCK sink is never referenced).
func TestStatic_AlertChannelsAreBuiltOnlyInObservabilityWiring(t *testing.T) {
	files := readCmdFiles(t)
	var addSinks int
	for name, src := range files {
		for _, ctor := range []string{"channels.NewEmailSink(", "channels.NewWebhookSink("} {
			if strings.Contains(src, ctor) && name != "observability_wiring.go" {
				t.Errorf("%s constructs an alert channel (%s): only observability_wiring.go may", name, ctor)
			}
		}
		addSinks += strings.Count(src, ".AddSinks(")
		if strings.Contains(src, "alerting.MockSink") || strings.Contains(src, "alertingtest.") {
			t.Errorf("%s references a mock/test alert channel", name)
		}
	}
	if !strings.Contains(files["observability_wiring.go"], "channels.NewEmailSink(") ||
		!strings.Contains(files["observability_wiring.go"], "channels.NewWebhookSink(") {
		t.Error("observability_wiring.go no longer builds both channels: the pin is vacuous")
	}
	if addSinks != 1 {
		t.Errorf("AddSinks is called %d times, want exactly once (in main.go, before the dispatcher loop starts)", addSinks)
	}
	if !strings.Contains(files["main.go"], "alertDispatcher.AddSinks(") {
		t.Error("main.go does not register the configured channels on the dispatcher")
	}
	mainSrc := files["main.go"]
	if strings.Index(mainSrc, "alertDispatcher.AddSinks(") > strings.Index(mainSrc, "alerting.RunDispatcherLoop(") {
		t.Error("AddSinks must run before the dispatcher loop is started")
	}
}

// Under the production posture (explicit production, or an UNSET APP_ENV which is treated as production) a loopback or
// plain-http webhook is refused at startup, and a valid public https URL is a human-notification channel.
func TestBuildAlertSinks_ProductionPostureRefusesLoopbackAndTreatsPublicHTTPSAsHuman(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef-secret")
	store := fakeStore{scheme: "awssm", val: secret}
	ref := "awssm://arn:aws:secretsmanager:eu-west-1:123456789012:secret:alert-webhook?versionId=11111111111111111111111111111111"
	for _, c := range []struct {
		name     string
		env      string
		explicit bool
		url      string
		wantErr  bool
	}{
		{"production, public https", "production", true, "https://hooks.example.com/alerts", false},
		{"production, loopback http", "production", true, "http://127.0.0.1:8099/hook", true},
		{"production, loopback https", "production", true, "https://127.0.0.1:8443/hook", true},
		{"unset APP_ENV, loopback http", "development", false, "http://127.0.0.1:8099/hook", true},
		{"staging, plain http public", "staging", true, "http://hooks.example.com/alerts", true},
		{"staging, loopback", "staging", true, "https://localhost/alerts", true},
	} {
		cfg := config.Config{Environment: c.env, EnvironmentExplicit: c.explicit, HTTPAddr: ":8080",
			AlertWebhookURL: c.url, AlertWebhookSecretRef: ref}
		sinks, err := buildAlertSinks(cfg, bundleWith(store), quietLogger())
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", c.name, err, c.wantErr)
			continue
		}
		if err == nil && (len(sinks) != 1 || !sinks[0].HumanNotification()) {
			t.Errorf("%s: a production-posture webhook must be a human-notification channel: %v", c.name, sinks)
		}
	}
}

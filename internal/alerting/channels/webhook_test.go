package channels_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/alerting/alertingtest"
	"github.com/Diansalas/igaming-platform/internal/alerting/channels"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef-test-secret")

type receivedReq struct {
	header http.Header
	body   []byte
}

// webhookReceiver is the test double of the receiving end: it verifies the signature like a real receiver must.
type webhookReceiver struct {
	srv   *httptest.Server
	mu    sync.Mutex
	got   []receivedReq
	mode  atomic.Value // alerting.ErrorClass scripted failure ("" = 200)
	hangs atomic.Bool
	sigOK atomic.Int32
	sigNG atomic.Int32
}

func newReceiver(t *testing.T) *webhookReceiver {
	t.Helper()
	r := &webhookReceiver{}
	r.mode.Store(alerting.ErrorClassNone)
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		if r.hangs.Load() {
			<-req.Context().Done()
			return
		}
		if VerifyForTest(req, b) {
			r.sigOK.Add(1)
		} else {
			r.sigNG.Add(1)
		}
		r.mu.Lock()
		r.got = append(r.got, receivedReq{header: req.Header.Clone(), body: b})
		r.mu.Unlock()
		switch r.mode.Load().(alerting.ErrorClass) {
		case alerting.ErrorClassTimeout:
			select {
			case <-req.Context().Done():
			case <-time.After(3 * time.Second):
			}
		case alerting.ErrorClassUnavailable:
			w.WriteHeader(http.StatusServiceUnavailable)
		case alerting.ErrorClassRejected:
			w.WriteHeader(http.StatusForbidden)
		case alerting.ErrorClassMisconfigured:
			http.Redirect(w, req, "/elsewhere", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func VerifyForTest(req *http.Request, body []byte) bool {
	return channels.VerifyWebhookSignature(testSecret, req.Header.Get("X-Alert-Timestamp"), body,
		req.Header.Get("X-Alert-Signature"), time.Now(), 5*time.Minute)
}

func (r *webhookReceiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func (r *webhookReceiver) last() receivedReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.got[len(r.got)-1]
}

func okSecret(context.Context) ([]byte, alerting.ErrorClass) {
	return testSecret, alerting.ErrorClassNone
}

func devSink(t *testing.T, url string, logger *slog.Logger, secret channels.SecretFunc) *channels.WebhookSink {
	t.Helper()
	s, err := channels.NewWebhookSink(channels.WebhookConfig{
		URL: url, Secret: secret, AllowLoopbackDev: true, Logger: logger, ResponseHeaderTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sampleDelivery() alerting.Delivery {
	return alerting.Delivery{
		IdempotencyKey: "a:0:0", DedupKey: uuid.NewString() + ":0", Discriminator: "attempt:abc:reason:x",
		AlertID: uuid.New(), Kind: alerting.KindPaymentWebhookIntegrity, Severity: alerting.SeverityP1,
		SubjectTenantID: uuid.New(),
		Attributes:      map[string]alerting.AttrValue{"operator": "alice@example.com", "reason": "CANARY-PII-7731"},
		RecipientRef:    "interim-owner",
	}
}

func TestWebhookSink_Conformance(t *testing.T) {
	alertingtest.RunChannelConformance(t, func(t *testing.T, logger *slog.Logger) alertingtest.Subject {
		r := newReceiver(t)
		return alertingtest.Subject{
			// A dev-posture sink is never presented as a human notification, so it needs no declaration.
			Sink:          devSink(t, r.srv.URL, logger, okSecret),
			Notifications: r.count,
			Hang:          func() { r.hangs.Store(true) },
			Fail:          func(c alerting.ErrorClass) { r.mode.Store(c) },
			FailClasses: []alerting.ErrorClass{alerting.ErrorClassTimeout, alerting.ErrorClassUnavailable,
				alerting.ErrorClassRejected, alerting.ErrorClassMisconfigured},
		}
	})
}

func TestWebhookSink_RequestIsSignedStableAndCarriesNoAttributeValues(t *testing.T) {
	r := newReceiver(t)
	s := devSink(t, r.srv.URL, nil, okSecret)
	d := sampleDelivery()
	if out, c := s.Deliver(context.Background(), d); out != alerting.OutcomeSent || c != alerting.ErrorClassNone {
		t.Fatalf("deliver = %q/%q", out, c)
	}
	got := r.last()
	if r.sigOK.Load() != 1 || r.sigNG.Load() != 0 {
		t.Fatalf("receiver verified %d good / %d bad signatures", r.sigOK.Load(), r.sigNG.Load())
	}
	if got.header.Get("Idempotency-Key") != d.DedupKey || got.header.Get("X-Alert-Id") != d.AlertID.String() {
		t.Errorf("headers: Idempotency-Key=%q X-Alert-Id=%q", got.header.Get("Idempotency-Key"), got.header.Get("X-Alert-Id"))
	}
	if !strings.HasPrefix(got.header.Get("X-Alert-Signature"), "v1=") || got.header.Get("Content-Type") != "application/json" {
		t.Errorf("signature/content-type headers: %v", got.header)
	}
	// The secret is never sent, in any header or the body.
	for k, vs := range got.header {
		for _, v := range vs {
			if strings.Contains(v, string(testSecret)) {
				t.Errorf("secret appears in header %s", k)
			}
		}
	}
	if strings.Contains(string(got.body), string(testSecret)) {
		t.Error("secret appears in the body")
	}
	// No attribute value, no PII.
	for _, bad := range []string{"CANARY-PII-7731", "alice@example.com", "operator"} {
		if strings.Contains(string(got.body), bad) {
			t.Errorf("body leaks %q: %s", bad, got.body)
		}
	}
	var m map[string]any
	if err := json.Unmarshal(got.body, &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"schema": true, "alert_id": true, "dedup_key": true, "kind": true, "severity": true,
		"discriminator": true, "subject_tenant_id": true, "sent_at": true}
	for k := range m {
		if !want[k] {
			t.Errorf("unexpected body field %q", k)
		}
	}
	if m["kind"] != "payment.webhook_integrity" || m["severity"] != "p1" || m["subject_tenant_id"] != d.SubjectTenantID.String() {
		t.Errorf("body = %v", m)
	}
}

func TestWebhookSink_TamperedBodyOrStaleTimestampOrWrongSecretFailsVerification(t *testing.T) {
	body := []byte(`{"kind":"x"}`)
	ts := "1700000000"
	now := time.Unix(1700000000, 0)
	good := "v1=" + hexHMAC(testSecret, ts, body)
	if !channels.VerifyWebhookSignature(testSecret, ts, body, good, now, time.Minute) {
		t.Fatal("a correct signature must verify")
	}
	if channels.VerifyWebhookSignature(testSecret, ts, []byte(`{"kind":"y"}`), good, now, time.Minute) {
		t.Error("tampered body verified")
	}
	if channels.VerifyWebhookSignature(testSecret, "1700000001", body, good, now, time.Minute) {
		t.Error("tampered timestamp verified")
	}
	if channels.VerifyWebhookSignature([]byte("another-secret-another-secret-123456"), ts, body, good, now, time.Minute) {
		t.Error("wrong secret verified")
	}
	if channels.VerifyWebhookSignature(testSecret, ts, body, good, now.Add(10*time.Minute), time.Minute) {
		t.Error("stale timestamp (replay) verified")
	}
	if channels.VerifyWebhookSignature(testSecret, ts, body, good, now.Add(-10*time.Minute), time.Minute) {
		t.Error("future timestamp verified")
	}
	if channels.VerifyWebhookSignature(testSecret, "not-a-number", body, good, now, time.Minute) {
		t.Error("garbage timestamp verified")
	}
	if channels.VerifyWebhookSignature(testSecret, ts, body, strings.TrimPrefix(good, "v1="), now, time.Minute) {
		t.Error("signature without the version prefix verified")
	}
}

func TestWebhookSink_RetryReusesTheSameIdempotencyKeyAndDedupsAfterSuccess(t *testing.T) {
	r := newReceiver(t)
	s := devSink(t, r.srv.URL, nil, okSecret)
	d := sampleDelivery()
	r.mode.Store(alerting.ErrorClassUnavailable)
	for attempt := 0; attempt < 2; attempt++ {
		d.IdempotencyKey = d.DedupKey + ":" + string(rune('0'+attempt))
		if out, c := s.Deliver(context.Background(), d); out != alerting.OutcomeFailed || c != alerting.ErrorClassUnavailable {
			t.Fatalf("attempt %d = %q/%q", attempt, out, c)
		}
	}
	r.mode.Store(alerting.ErrorClassNone)
	if out, _ := s.Deliver(context.Background(), d); out != alerting.OutcomeSent {
		t.Fatal("recovery attempt did not send")
	}
	// after success the same key is not sent again by this process
	if out, _ := s.Deliver(context.Background(), d); out != alerting.OutcomeSent {
		t.Fatal("repeat after success must report sent")
	}
	if r.count() != 3 {
		t.Fatalf("requests = %d, want 3 (2 failed + 1 success, no duplicate send)", r.count())
	}
	keys := map[string]bool{}
	for _, g := range r.got {
		keys[g.header.Get("Idempotency-Key")] = true
	}
	if len(keys) != 1 || !keys[d.DedupKey] {
		t.Fatalf("idempotency keys across retries = %v, want exactly {%s}", keys, d.DedupKey)
	}
}

func TestWebhookSink_StatusClassification(t *testing.T) {
	for _, c := range []struct {
		code int
		out  alerting.Outcome
		cls  alerting.ErrorClass
	}{
		{200, alerting.OutcomeSent, ""}, {201, alerting.OutcomeSent, ""}, {204, alerting.OutcomeSent, ""},
		{301, alerting.OutcomeFailed, alerting.ErrorClassMisconfigured}, {302, alerting.OutcomeFailed, alerting.ErrorClassMisconfigured},
		{307, alerting.OutcomeFailed, alerting.ErrorClassMisconfigured},
		{400, alerting.OutcomeFailed, alerting.ErrorClassRejected}, {401, alerting.OutcomeFailed, alerting.ErrorClassRejected},
		{403, alerting.OutcomeFailed, alerting.ErrorClassRejected}, {404, alerting.OutcomeFailed, alerting.ErrorClassRejected},
		{408, alerting.OutcomeFailed, alerting.ErrorClassUnavailable}, {425, alerting.OutcomeFailed, alerting.ErrorClassUnavailable},
		{429, alerting.OutcomeFailed, alerting.ErrorClassUnavailable},
		{500, alerting.OutcomeFailed, alerting.ErrorClassUnavailable}, {502, alerting.OutcomeFailed, alerting.ErrorClassUnavailable},
		{503, alerting.OutcomeFailed, alerting.ErrorClassUnavailable},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if c.code >= 300 && c.code < 400 {
				w.Header().Set("Location", "http://127.0.0.1:1/never")
			}
			w.WriteHeader(c.code)
		}))
		s := devSink(t, srv.URL, nil, okSecret)
		out, cls := s.Deliver(context.Background(), sampleDelivery())
		srv.Close()
		if out != c.out || cls != c.cls {
			t.Errorf("status %d => %q/%q, want %q/%q", c.code, out, cls, c.out, c.cls)
		}
	}
}

func TestWebhookSink_NeverFollowsARedirect(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { targetHits.Add(1) }))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, target.URL+"/metadata", http.StatusTemporaryRedirect) // 307 would re-POST the body
	}))
	defer redir.Close()
	s := devSink(t, redir.URL, nil, okSecret)
	out, c := s.Deliver(context.Background(), sampleDelivery())
	if out != alerting.OutcomeFailed || c != alerting.ErrorClassMisconfigured {
		t.Fatalf("redirect => %q/%q, want failed/misconfigured", out, c)
	}
	if targetHits.Load() != 0 {
		t.Fatalf("the redirect target was contacted %d time(s)", targetHits.Load())
	}
}

func TestWebhookSink_ProductionPostureRefusesNonPublicDestinationsAtConstruction(t *testing.T) {
	for _, u := range []string{
		"http://hooks.example.com/a", "https://127.0.0.1/a", "https://localhost/a", "https://10.0.0.5/a",
		"https://169.254.169.254/latest", "https://[::1]/a", "https://user:pw@hooks.example.com/a", "",
		"https://hooks.example.com/a#x", "https://2130706433/a", "https://hooks.internal/a",
	} {
		if _, err := channels.NewWebhookSink(channels.WebhookConfig{URL: u, Secret: okSecret}); err == nil {
			t.Errorf("production posture accepted %q", u)
		}
	}
	if _, err := channels.NewWebhookSink(channels.WebhookConfig{URL: "https://hooks.example.com/alerts", Secret: okSecret}); err != nil {
		t.Errorf("a public https URL was refused: %v", err)
	}
	if _, err := channels.NewWebhookSink(channels.WebhookConfig{URL: "https://hooks.example.com/alerts"}); err == nil {
		t.Error("a sink without a secret source was built")
	}
	// Development posture admits loopback only.
	if _, err := channels.NewWebhookSink(channels.WebhookConfig{URL: "http://10.0.0.5/a", Secret: okSecret, AllowLoopbackDev: true}); err == nil {
		t.Error("development posture accepted a private address")
	}
}

func TestWebhookSink_SecretHandling(t *testing.T) {
	r := newReceiver(t)

	t.Run("short secret fails closed with no request", func(t *testing.T) {
		s := devSink(t, r.srv.URL, nil, func(context.Context) ([]byte, alerting.ErrorClass) { return []byte("short"), "" })
		before := r.count()
		out, c := s.Deliver(context.Background(), sampleDelivery())
		if out != alerting.OutcomeFailed || c != alerting.ErrorClassMisconfigured || r.count() != before {
			t.Fatalf("short secret => %q/%q, requests %d->%d", out, c, before, r.count())
		}
	})
	t.Run("empty secret fails closed", func(t *testing.T) {
		s := devSink(t, r.srv.URL, nil, func(context.Context) ([]byte, alerting.ErrorClass) { return nil, "" })
		if out, c := s.Deliver(context.Background(), sampleDelivery()); out != alerting.OutcomeFailed || c != alerting.ErrorClassMisconfigured {
			t.Fatalf("empty secret => %q/%q", out, c)
		}
	})
	t.Run("store errors keep their class", func(t *testing.T) {
		for _, cls := range []alerting.ErrorClass{alerting.ErrorClassUnavailable, alerting.ErrorClassMisconfigured} {
			s := devSink(t, r.srv.URL, nil, func(context.Context) ([]byte, alerting.ErrorClass) { return nil, cls })
			if out, c := s.Deliver(context.Background(), sampleDelivery()); out != alerting.OutcomeFailed || c != cls {
				t.Fatalf("secret class %q => %q/%q", cls, out, c)
			}
		}
	})
	t.Run("secret cached for the TTL then refetched (rotation)", func(t *testing.T) {
		var calls atomic.Int32
		now := time.Now()
		var mu sync.Mutex
		s, err := channels.NewWebhookSink(channels.WebhookConfig{
			URL: r.srv.URL, AllowLoopbackDev: true, ResponseHeaderTimeout: 300 * time.Millisecond,
			Secret: func(context.Context) ([]byte, alerting.ErrorClass) { calls.Add(1); return testSecret, "" },
			Now:    func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
		})
		if err != nil {
			t.Fatal(err)
		}
		s.Deliver(context.Background(), sampleDelivery())
		s.Deliver(context.Background(), sampleDelivery())
		if calls.Load() != 1 {
			t.Fatalf("secret fetched %d times within the TTL, want 1", calls.Load())
		}
		mu.Lock()
		now = now.Add(6 * time.Minute)
		mu.Unlock()
		s.Deliver(context.Background(), sampleDelivery())
		if calls.Load() != 2 {
			t.Fatalf("secret fetched %d times after the TTL, want 2 (rotation must take effect)", calls.Load())
		}
	})
	t.Run("secret never appears in logs", func(t *testing.T) {
		var sb strings.Builder
		var mu sync.Mutex
		w := logWriter(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return sb.Write(p) })
		s := devSink(t, r.srv.URL, slog.New(slog.NewTextHandler(w, nil)), okSecret)
		r.mode.Store(alerting.ErrorClassUnavailable)
		defer r.mode.Store(alerting.ErrorClassNone)
		s.Deliver(context.Background(), sampleDelivery())
		mu.Lock()
		defer mu.Unlock()
		for _, bad := range []string{string(testSecret), r.srv.URL, "127.0.0.1", "CANARY-PII-7731"} {
			if strings.Contains(sb.String(), bad) {
				t.Errorf("log leaks %q: %s", bad, sb.String())
			}
		}
	})
}

func TestWebhookSink_HumanNotificationIsOnlyClaimedForTheProductionPosture(t *testing.T) {
	dev := devSink(t, "http://127.0.0.1:9/x", nil, okSecret)
	if dev.HumanNotification() {
		t.Error("a development-posture (loopback-allowed) sink must not claim to notify a person")
	}
	prod, err := channels.NewWebhookSink(channels.WebhookConfig{URL: "https://hooks.example.com/alerts", Secret: okSecret})
	if err != nil {
		t.Fatal(err)
	}
	if !prod.HumanNotification() || prod.ChannelKind() != alerting.ChannelWebhook {
		t.Error("a production-posture webhook sink is a human-notification channel")
	}
}

func TestWebhookSink_SanitisesTemplateValues(t *testing.T) {
	r := newReceiver(t)
	s := devSink(t, r.srv.URL, nil, okSecret)
	d := sampleDelivery()
	d.Discriminator = "ok\"\r\n,\"injected\":true"
	s.Deliver(context.Background(), d)
	var m map[string]any
	if err := json.Unmarshal(r.last().body, &m); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if _, bad := m["injected"]; bad {
		t.Fatal("a discriminator injected a JSON field")
	}
	if strings.ContainsAny(m["discriminator"].(string), "\r\n\",") {
		t.Fatalf("discriminator not sanitised: %q", m["discriminator"])
	}
}

// A destination refused at CONNECT time (the validated name resolved to a non-public address) is permanent: retrying
// cannot help and the dispatcher must not burn its retry budget on it.
func TestWebhookSink_RefusedDestinationIsAPermanentMisconfiguration(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	defer srv.Close()
	s, err := channels.NewWebhookSink(channels.WebhookConfig{URL: "https://hooks.example.com/alerts", Secret: okSecret})
	if err != nil {
		t.Fatal(err)
	}
	channels.SetTargetForTest(s, srv.URL) // a loopback peer, under the production posture
	out, class := s.Deliver(context.Background(), sampleDelivery())
	if out != alerting.OutcomeFailed || class != alerting.ErrorClassMisconfigured {
		t.Fatalf("refused destination => %q/%q, want failed/misconfigured", out, class)
	}
	if class.Retryable() {
		t.Error("a refused destination must not be retryable")
	}
	if hits.Load() != 0 {
		t.Fatalf("the loopback server received %d request(s)", hits.Load())
	}
}

func TestWebhookSink_ConnectionToAPrivatePeerIsRefusedByTheTransportItself(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener")
	}
	defer func() { _ = ln.Close() }()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = c.Close()
		}
	}()
	// A production-posture sink whose configured name is public. DNS is simulated by dialing the listener's
	// address through the sink's OWN transport: the dial control must refuse the loopback peer.
	s, err := channels.NewWebhookSink(channels.WebhookConfig{URL: "https://hooks.example.com/alerts", Secret: okSecret})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := channels.DialForTest(s, context.Background(), "tcp", ln.Addr().String())
	if err == nil {
		_ = conn.Close()
		t.Fatal("the production-posture transport connected to a loopback peer")
	}
	time.Sleep(50 * time.Millisecond)
	if accepted.Load() != 0 {
		t.Fatalf("the loopback listener accepted %d connection(s)", accepted.Load())
	}
}

func hexHMAC(secret []byte, ts string, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(ts + "."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// logWriter adapts a func to io.Writer (named to avoid clashing with the integration-tagged test helpers).
type logWriter func([]byte) (int, error)

func (f logWriter) Write(p []byte) (int, error) { return f(p) }

package channels

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/egressguard"
)

// WebhookSink delivers an alert as one signed HTTPS POST to the single configured endpoint (A9,
// ALERT_WEBHOOK_URL). It is provider-neutral: the receiver may be a pager integration, a chat bridge or an
// incident tool, and nothing here knows which.
//
// Request contract (documented for receivers in docs/runbooks/observability-and-alerting.md):
//
//	POST <configured URL>   Content-Type: application/json
//	X-Alert-Timestamp:  unix seconds when the request was signed
//	Idempotency-Key:    "<alert_id>:<escalation_step>", STABLE across retries: the receiver dedupes on it
//	X-Alert-Id:         the alert id
//	X-Alert-Signature:  v1=<hex HMAC-SHA256(secret, timestamp + "." + body)>
//
// A receiver must verify the signature in constant time, reject a timestamp more than 5 minutes from its
// clock, and treat a repeated Idempotency-Key as already handled. Delivery is at-least-once.
//
// The body is {"schema","alert_id","dedup_key","kind","severity","discriminator","subject_tenant_id"?,
// "sent_at"}: no attribute values (the Kinds' attributes include operator-supplied text for some Kinds, so
// none is forwarded). The discriminator is a PSEUDONYMOUS identifier (for example staff:<uuid>,
// withdrawal:<id>), which is personal data once linkable to a person (SR9-A9-6): the receiver's vendor needs a
// DPA. Receivers must dedupe on the SIGNED dedup_key in the body; the Idempotency-Key and X-Alert-Id headers
// are NOT covered by the signature (SR9-A9-7) and equal the body's dedup_key and alert_id.
//
// Hardening (each independently tested; security review to probe):
//
//   - the URL is the only destination: it comes from configuration, never from the alert, the route or the
//     recipient_ref; it must pass egressguard.ValidateURL (https only, no userinfo, no private/loopback/
//     link-local literal, no internal host name);
//   - egressguard.DialControl refuses a non-public peer address at CONNECT time, after DNS (defeats a name that
//     resolves to a private address and DNS rebinding);
//   - no redirect is ever followed, and the transport ignores HTTP(S)_PROXY;
//   - bounded dial, TLS handshake, response-header and overall timeouts; the response body is capped and
//     discarded, never read into logs;
//   - the signing secret is fetched from the secret store by REFERENCE (SecretFunc), cached for a short TTL
//     so rotation takes effect, never logged, never stored in the sink beyond the cache, minimum 32 bytes;
//   - failures are classified (4xx other than 408/425/429 = rejected, permanent; 3xx or a refused
//     destination = misconfigured, permanent; 5xx, 408/425/429, transport errors = retryable) and only the
//     CLASS is logged, never an error string (a transport error string contains the URL).
type WebhookSink struct {
	target        *url.URL
	client        *http.Client
	secret        SecretFunc
	logger        *slog.Logger
	now           func() time.Time
	allowDevLocal bool
	sent          *sentSet

	mu          sync.Mutex
	cached      []byte
	cachedUntil time.Time
}

// SecretFunc fetches the webhook signing secret from the secret store. A non-empty class reports a failure
// (alerting.ErrorClassMisconfigured for a missing/denied/invalid reference, alerting.ErrorClassUnavailable for a store outage).
type SecretFunc func(ctx context.Context) (secret []byte, class alerting.ErrorClass)

// WebhookConfig configures NewWebhookSink.
type WebhookConfig struct {
	URL    string
	Secret SecretFunc
	// AllowLoopbackDev admits a loopback receiver and plain http to it (development only; the caller derives it
	// from config.GuardEnvironment() == "development"). A sink built with it never reports HumanNotification.
	AllowLoopbackDev bool
	// AllowNAT64 and AllowSingleLabelHosts forward the egressguard options of the same name (default: refused).
	AllowNAT64            bool
	AllowSingleLabelHosts []string
	Logger                *slog.Logger
	Now                   func() time.Time // tests only
	// ResponseHeaderTimeout may only SHORTEN the 5s default (tests); 0 keeps the default.
	ResponseHeaderTimeout time.Duration
}

const (
	webhookMinSecretBytes  = 32
	webhookSecretCacheTTL  = 5 * time.Minute
	webhookRequestTimeout  = 10 * time.Second
	webhookMaxBodyDrain    = 4 << 10
	webhookSchema          = "igaming.alert.v1"
	webhookSignatureHeader = "X-Alert-Signature"
)

// NewWebhookSink validates the configured URL and builds the hardened client.
func NewWebhookSink(cfg WebhookConfig) (*WebhookSink, error) {
	u, err := egressguard.ValidateURL(cfg.URL, egressguard.URLOptions{
		AllowLoopbackDev: cfg.AllowLoopbackDev, AllowNAT64: cfg.AllowNAT64, AllowSingleLabelHosts: cfg.AllowSingleLabelHosts,
	})
	if err != nil {
		return nil, err
	}
	if cfg.Secret == nil {
		return nil, errors.New("alerting: webhook sink: no secret source")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	headerTimeout := 5 * time.Second
	if cfg.ResponseHeaderTimeout > 0 && cfg.ResponseHeaderTimeout < headerTimeout {
		headerTimeout = cfg.ResponseHeaderTimeout
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second, Control: egressguard.DialControlWith(egressguard.DialOptions{AllowLoopback: cfg.AllowLoopbackDev, AllowNAT64: cfg.AllowNAT64})}
	transport := &http.Transport{
		Proxy:                  nil, // never an environment proxy: the dial control must judge the real peer
		DialContext:            dialer.DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    3 * time.Second,
		ResponseHeaderTimeout:  headerTimeout,
		ExpectContinueTimeout:  time.Second,
		IdleConnTimeout:        30 * time.Second,
		MaxIdleConns:           2,
		MaxIdleConnsPerHost:    2,
		MaxResponseHeaderBytes: 16 << 10,
		ForceAttemptHTTP2:      true,
	}
	return &WebhookSink{
		target: u,
		client: &http.Client{
			Transport:     transport,
			Timeout:       webhookRequestTimeout,
			CheckRedirect: egressguard.NoRedirects,
		},
		secret: cfg.Secret, logger: logger, now: now, allowDevLocal: cfg.AllowLoopbackDev, sent: newSentSet(4096),
	}, nil
}

func (*WebhookSink) ChannelKind() alerting.ChannelKind { return alerting.ChannelWebhook }

// HumanNotification is true for a production-posture sink (the endpoint is a person's pager/chat/incident
// integration); a development-posture sink (loopback allowed) is never presented as notifying a person.
func (s *WebhookSink) HumanNotification() bool { return !s.allowDevLocal }

type webhookPayload struct {
	Schema          string `json:"schema"`
	AlertID         string `json:"alert_id"`
	DedupKey        string `json:"dedup_key"`
	Kind            string `json:"kind"`
	Severity        string `json:"severity"`
	Discriminator   string `json:"discriminator"`
	SubjectTenantID string `json:"subject_tenant_id,omitempty"`
	SentAt          string `json:"sent_at"`
}

// Deliver implements alerting.Sink.
func (s *WebhookSink) Deliver(ctx context.Context, d alerting.Delivery) (alerting.Outcome, alerting.ErrorClass) {
	if s.sent.has(d.DedupKey) {
		return alerting.OutcomeSent, alerting.ErrorClassNone
	}
	if err := ctx.Err(); err != nil {
		return alerting.OutcomeFailed, classifyContextErr(err)
	}
	secret, class := s.signingSecret(ctx)
	if class != alerting.ErrorClassNone {
		return alerting.OutcomeFailed, class
	}
	ts := s.now().UTC()
	p := webhookPayload{
		Schema: webhookSchema, AlertID: d.AlertID.String(), DedupKey: safeToken(d.DedupKey),
		Kind: safeToken(string(d.Kind)), Severity: safeToken(string(d.Severity)),
		Discriminator: safeToken(d.Discriminator), SentAt: ts.Format(time.RFC3339),
	}
	if d.SubjectTenantID != uuid.Nil {
		p.SubjectTenantID = d.SubjectTenantID.String()
	}
	body, err := json.Marshal(p)
	if err != nil {
		return alerting.OutcomeFailed, alerting.ErrorClassUnknown
	}
	stamp := strconv.FormatInt(ts.Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.target.String(), bytes.NewReader(body))
	if err != nil {
		return alerting.OutcomeFailed, alerting.ErrorClassMisconfigured
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "igaming-platform-alerting/1")
	req.Header.Set("X-Alert-Timestamp", stamp)
	req.Header.Set("X-Alert-Id", p.AlertID)
	req.Header.Set("Idempotency-Key", p.DedupKey)
	req.Header.Set(webhookSignatureHeader, "v1="+signWebhook(secret, stamp, body))

	resp, err := s.client.Do(req)
	if err != nil {
		// Only a CLASS is logged: a transport error string contains the URL, which may carry a token.
		c := classifyTransportErr(err)
		s.logger.Warn("alert_webhook_delivery_failed", "alert_id", d.AlertID, "error_class", string(c))
		return alerting.OutcomeFailed, c
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.CopyN(io.Discard, resp.Body, webhookMaxBodyDrain)

	if c := classifyStatus(resp.StatusCode); c != alerting.ErrorClassNone {
		s.logger.Warn("alert_webhook_delivery_failed", "alert_id", d.AlertID, "error_class", string(c),
			"status_class", resp.StatusCode/100)
		return alerting.OutcomeFailed, c
	}
	s.sent.add(d.DedupKey)
	s.logger.Info("alert_webhook_delivered", "alert_id", d.AlertID, "kind", d.Kind, "severity", d.Severity)
	return alerting.OutcomeSent, alerting.ErrorClassNone
}

// signWebhook is the v1 signature: hex(HMAC-SHA256(secret, timestamp + "." + body)).
func signWebhook(secret []byte, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(timestamp))
	m.Write([]byte{'.'})
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// VerifyWebhookSignature is the receiver-side check, exported for the test double and for documentation: constant
// time, and it rejects a timestamp more than tolerance from now.
func VerifyWebhookSignature(secret []byte, timestamp string, body []byte, header string, now time.Time, tolerance time.Duration) bool {
	sec, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	age := now.Sub(time.Unix(sec, 0))
	if age < -tolerance || age > tolerance {
		return false
	}
	want := "v1=" + signWebhook(secret, timestamp, body)
	return hmac.Equal([]byte(want), []byte(header))
}

func (s *WebhookSink) signingSecret(ctx context.Context) ([]byte, alerting.ErrorClass) {
	s.mu.Lock()
	if s.cached != nil && s.now().Before(s.cachedUntil) {
		sec := s.cached
		s.mu.Unlock()
		return sec, alerting.ErrorClassNone
	}
	s.mu.Unlock()
	sec, class := s.secret(ctx)
	if class != alerting.ErrorClassNone {
		return nil, normalizeClass(class)
	}
	if len(sec) < webhookMinSecretBytes {
		// Fail closed: a short/empty secret would sign with a guessable key.
		return nil, alerting.ErrorClassMisconfigured
	}
	s.mu.Lock()
	s.cached, s.cachedUntil = sec, s.now().Add(webhookSecretCacheTTL)
	s.mu.Unlock()
	return sec, alerting.ErrorClassNone
}

func classifyStatus(code int) alerting.ErrorClass {
	switch {
	case code >= 200 && code < 300:
		return alerting.ErrorClassNone
	case code >= 300 && code < 400:
		return alerting.ErrorClassMisconfigured // a redirect is never followed; the configured URL is wrong
	case code == http.StatusRequestTimeout, code == http.StatusTooEarly, code == http.StatusTooManyRequests:
		return alerting.ErrorClassUnavailable
	case code >= 400 && code < 500:
		return alerting.ErrorClassRejected
	default:
		return alerting.ErrorClassUnavailable
	}
}

func classifyTransportErr(err error) alerting.ErrorClass {
	switch {
	case errors.Is(err, egressguard.ErrRefused):
		return alerting.ErrorClassMisconfigured // the destination resolved to a non-public address: retrying cannot help
	case errors.Is(err, context.DeadlineExceeded):
		return alerting.ErrorClassTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return alerting.ErrorClassTimeout
	}
	return alerting.ErrorClassUnavailable
}

// normalizeClass maps any class outside the persisted vocabulary to unknown, so a buggy secret source can never
// make the dispatcher's last_error_class CHECK refuse the outcome row (alerting does the same for sinks).
func normalizeClass(c alerting.ErrorClass) alerting.ErrorClass {
	switch c {
	case alerting.ErrorClassTimeout, alerting.ErrorClassUnavailable, alerting.ErrorClassRejected,
		alerting.ErrorClassMisconfigured, alerting.ErrorClassUnknown:
		return c
	default:
		return alerting.ErrorClassUnknown
	}
}

package channels_test

import (
	"crypto/tls"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/alerting/channels"
)

// The properties that make the webhook client an SSRF-resistant sender, pinned on the constructed client itself.
func TestWebhookSink_ClientHardeningIsPinned(t *testing.T) {
	s, err := channels.NewWebhookSink(channels.WebhookConfig{URL: "https://hooks.example.com/alerts", Secret: okSecret})
	if err != nil {
		t.Fatal(err)
	}
	c := channels.ClientForTest(s)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T", c.Transport)
	}
	if tr.Proxy != nil {
		t.Error("the transport honours an environment proxy: the dial-time address check would judge the proxy, not the destination")
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Error("TLS 1.2 floor missing")
	}
	if tr.TLSClientConfig != nil && tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("certificate verification is disabled")
	}
	if tr.DialContext == nil {
		t.Error("no dial control installed")
	}
	if tr.TLSHandshakeTimeout <= 0 || tr.TLSHandshakeTimeout > 5*time.Second {
		t.Errorf("TLS handshake timeout = %v", tr.TLSHandshakeTimeout)
	}
	if tr.ResponseHeaderTimeout <= 0 || tr.ResponseHeaderTimeout > 5*time.Second {
		t.Errorf("response header timeout = %v", tr.ResponseHeaderTimeout)
	}
	if c.Timeout <= 0 || c.Timeout > 10*time.Second {
		t.Errorf("overall timeout = %v", c.Timeout)
	}
	if tr.MaxResponseHeaderBytes <= 0 || tr.MaxResponseHeaderBytes > 64<<10 {
		t.Errorf("MaxResponseHeaderBytes = %d", tr.MaxResponseHeaderBytes)
	}
	if c.CheckRedirect == nil {
		t.Fatal("redirect policy missing: Go would follow up to 10 redirects")
	}
	req, _ := http.NewRequest(http.MethodPost, "https://hooks.example.com/next", nil)
	if err := c.CheckRedirect(req, []*http.Request{req}); err != http.ErrUseLastResponse {
		t.Errorf("CheckRedirect = %v, want http.ErrUseLastResponse", err)
	}
}

// Source-level pin: no environment-proxy lookup and no insecure TLS in the sink, and the only egress client is
// the one built in NewWebhookSink.
func TestWebhookSink_SourceHasNoProxyOrInsecureTLS(t *testing.T) {
	b, err := os.ReadFile("webhook.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, bad := range []string{"ProxyFromEnvironment", "InsecureSkipVerify: true", "http.DefaultClient", "http.DefaultTransport", "http.Get(", "http.Post("} {
		if strings.Contains(src, bad) {
			t.Errorf("webhook.go contains %q", bad)
		}
	}
	if !strings.Contains(src, "Proxy:") || !strings.Contains(src, "nil, // never an environment proxy") {
		t.Error("the explicit `Proxy: nil` is gone")
	}
}

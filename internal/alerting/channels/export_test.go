package channels

import (
	"context"
	"net"
	"net/http"
	"net/url"
)

// DialForTest dials through the webhook sink's OWN transport dialer (and therefore its egress dial control).
func DialForTest(s *WebhookSink, ctx context.Context, network, addr string) (net.Conn, error) {
	return s.client.Transport.(*http.Transport).DialContext(ctx, network, addr)
}

// SetTargetForTest points the sink at an arbitrary URL WITHOUT the construction-time validation, so a test can
// prove the dial-time check on its own (a public name that resolves to a private address is simulated this way).
func SetTargetForTest(s *WebhookSink, raw string) {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	s.target = u
}

// ClientForTest exposes the sink's http.Client so tests can pin its hardening (no proxy, TLS floor, bounded
// timeouts, no redirects).
func ClientForTest(s *WebhookSink) *http.Client { return s.client }

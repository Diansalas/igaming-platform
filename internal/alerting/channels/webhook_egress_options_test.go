package channels_test

import (
	"testing"

	"github.com/Diansalas/igaming-platform/internal/alerting/channels"
)

// SR9-A9-2 / SR9-A9-9: the egress options reach the sink. A single-label host and a NAT64 literal are refused
// by default and admitted only through the explicit options.
func TestNewWebhookSink_SingleLabelHostAndNAT64NeedExplicitOptions(t *testing.T) {
	if _, err := channels.NewWebhookSink(channels.WebhookConfig{URL: "https://metadata/a", Secret: okSecret}); err == nil {
		t.Error("a single-label host was accepted by default")
	}
	if _, err := channels.NewWebhookSink(channels.WebhookConfig{URL: "https://hooks/a", Secret: okSecret, AllowSingleLabelHosts: []string{"hooks"}}); err != nil {
		t.Errorf("an allow-listed single-label host was refused: %v", err)
	}
	if _, err := channels.NewWebhookSink(channels.WebhookConfig{URL: "https://metadata/a", Secret: okSecret, AllowSingleLabelHosts: []string{"hooks"}}); err == nil {
		t.Error("a single-label host outside the allow-list was accepted")
	}
	if _, err := channels.NewWebhookSink(channels.WebhookConfig{URL: "https://[64:ff9b::808:808]/a", Secret: okSecret}); err == nil {
		t.Error("a NAT64 literal was accepted by default")
	}
	if _, err := channels.NewWebhookSink(channels.WebhookConfig{URL: "https://[64:ff9b::808:808]/a", Secret: okSecret, AllowNAT64: true}); err != nil {
		t.Errorf("a NAT64 literal embedding a public IPv4 was refused when allowed: %v", err)
	}
	if _, err := channels.NewWebhookSink(channels.WebhookConfig{URL: "https://[64:ff9b::7f00:1]/a", Secret: okSecret, AllowNAT64: true}); err == nil {
		t.Error("a NAT64 literal embedding loopback was accepted")
	}
}

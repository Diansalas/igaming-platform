package egressguard

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestPublicAddr_Table(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"93.184.216.34", true},
		{"8.8.8.8", true},
		{"2606:4700:4700::1111", true},
		{"127.0.0.1", false},
		{"127.255.255.254", false},
		{"::1", false},
		{"0.0.0.0", false},
		{"::", false},
		{"10.0.0.1", false},
		{"172.16.0.1", false},
		{"172.31.255.255", false},
		{"172.32.0.1", true},
		{"192.168.1.1", false},
		{"169.254.169.254", false}, // cloud metadata
		{"169.254.0.1", false},
		{"fd00::1", false},
		{"fe80::1", false},
		{"fec0::1", false},
		{"100.64.0.1", false}, // CGNAT
		{"100.127.255.255", false},
		{"100.128.0.1", true},
		{"192.0.0.1", false},
		{"192.0.2.1", false},
		{"198.51.100.1", false},
		{"203.0.113.1", false},
		{"198.18.0.1", false},
		{"224.0.0.1", false},
		{"ff02::1", false},
		{"240.0.0.1", false},
		{"255.255.255.255", false},
		{"::ffff:127.0.0.1", false}, // v4-mapped loopback must not slip through
		{"::ffff:10.0.0.1", false},
		{"::ffff:93.184.216.34", true},
		{"::ffff:100.64.0.1", false}, // v4-mapped CGNAT: only the prefix list, after Unmap, catches it
		{"::ffff:192.0.2.1", false},  // v4-mapped TEST-NET-1
		{"::ffff:198.18.0.1", false},
		{"64:ff9b::7f00:1", false}, // NAT64 embedding loopback
		{"2002:7f00:1::1", false},  // 6to4 embedding loopback
		{"2001:0:4136:e378:8000:63bf:3fff:fdd2", false},
	}
	for _, tc := range cases {
		ip := netip.MustParseAddr(tc.ip)
		if got := PublicAddr(ip); got != tc.want {
			t.Errorf("PublicAddr(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
	if PublicAddr(netip.Addr{}) {
		t.Error("the zero Addr must not be public")
	}
}

func TestValidateURL(t *testing.T) {
	good := []string{
		"https://hooks.example.com/alerts",
		"https://hooks.example.com:8443/a/b?x=1",
		"https://93.184.216.34/alerts",
	}
	for _, u := range good {
		if _, err := ValidateURL(u, URLOptions{}); err != nil {
			t.Errorf("ValidateURL(%q): %v", u, err)
		}
	}
	bad := []string{
		"",
		"http://hooks.example.com/alerts", // https only
		"ftp://hooks.example.com/a",
		"https://",
		"https:///path",
		"https://user:pw@hooks.example.com/a",    // userinfo
		"https://user@hooks.example.com/a",       // userinfo
		"https://hooks.example.com/a#frag",       // fragment
		"https://127.0.0.1/a",                    // loopback literal
		"https://[::1]/a",                        // loopback literal
		"https://10.1.2.3/a",                     // private literal
		"https://169.254.169.254/latest/meta",    // metadata literal
		"https://[fd00::1]/a",                    // ULA literal
		"https://localhost/a",                    // loopback name
		"https://LOCALHOST./a",                   // trailing dot and case
		"https://foo.localhost/a",                // .localhost
		"https://hooks.internal/a",               // .internal
		"https://hooks.example.com:0/a",          // port 0
		"https://hooks.example.com:70000/a",      // port range
		"https://hooks.example.com/a\r\nX: y",    // control characters
		"https://hooks.example.com/ a",           // space
		"https://0x7f.0.0.1/a",                   // numeric obfuscation
		"https://2130706433/a",                   // decimal obfuscation
		"https://hooks.example.com\\@evil.com/a", // backslash confusion
		"//hooks.example.com/a",
		strings.Repeat("https://a.example.com/", 200),
	}
	for _, u := range bad {
		if _, err := ValidateURL(u, URLOptions{}); err == nil {
			t.Errorf("ValidateURL(%q) accepted a bad URL", u)
		}
	}
}

func TestValidateURL_DevLoopbackOnlyWhenAllowed(t *testing.T) {
	for _, u := range []string{"http://127.0.0.1:8099/hook", "http://localhost:8099/hook", "https://127.0.0.1:8443/hook"} {
		if _, err := ValidateURL(u, URLOptions{}); err == nil {
			t.Errorf("%q accepted without AllowLoopbackDev", u)
		}
		if _, err := ValidateURL(u, URLOptions{AllowLoopbackDev: true}); err != nil {
			t.Errorf("%q refused with AllowLoopbackDev: %v", u, err)
		}
	}
	// AllowLoopbackDev admits loopback ONLY: never a private or metadata address, never plain http to a public host.
	for _, u := range []string{"http://10.0.0.1/a", "http://169.254.169.254/a", "http://hooks.example.com/a", "https://10.0.0.1/a",
		"http://foo.localhost/a", "https://foo.localhost/a", "http://localhost.evil.example/a"} {
		if _, err := ValidateURL(u, URLOptions{AllowLoopbackDev: true}); err == nil {
			t.Errorf("%q accepted with AllowLoopbackDev", u)
		}
	}
}

// The dial-time control is what defeats DNS rebinding: it judges the address actually connected to, not the
// name in the URL.
func TestDialControl_RefusesNonPublicAtConnectTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()

	strict := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{
		DialContext: (&net.Dialer{Control: DialControl(false)}).DialContext, Proxy: nil,
	}}
	if resp, err := strict.Get(srv.URL); err == nil {
		_ = resp.Body.Close()
		t.Fatal("dial to a loopback test server succeeded under the strict control")
	}
	lax := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{
		DialContext: (&net.Dialer{Control: DialControl(true)}).DialContext, Proxy: nil,
	}}
	resp, err := lax.Get(srv.URL)
	if err != nil {
		t.Fatalf("AllowLoopback control refused loopback: %v", err)
	}
	_ = resp.Body.Close()

	// AllowLoopback must not open the other private ranges.
	for _, addr := range []string{"10.0.0.1:443", "169.254.169.254:80", "[fd00::1]:443", "192.168.0.1:80", "100.64.0.1:80"} {
		if err := DialControl(true)("tcp", addr, nil); err == nil {
			t.Errorf("DialControl(true) allowed %s", addr)
		}
	}
	if err := DialControl(false)("tcp", "93.184.216.34:443", nil); err != nil {
		t.Errorf("public address refused: %v", err)
	}
	if err := DialControl(false)("tcp", "not-an-address", nil); err == nil {
		t.Error("an unparsable address must be refused (fail closed)")
	}
}

// SR9-A9-2: IPv6 is an ALLOW-list (2000::/3, global unicast) minus the special-purpose ranges inside it; one
// representative address per special range, at the edges where the range is a sub-range.
func TestPublicAddr_SpecialPurposeRanges(t *testing.T) {
	deny := []struct{ name, ip string }{
		{"::/96 deprecated IPv4-compatible", "::1.2.3.4"},
		{"::/96 low", "::2"},
		{"::ffff:0:0/96 mapped, non-public v4 inside", "::ffff:0a00:1"},
		{"2001:10::/28 ORCHID", "2001:10::1"},
		{"2001:10::/28 ORCHID high edge", "2001:1f:ffff::1"},
		{"2001:20::/28 ORCHIDv2", "2001:20::1"},
		{"2001:20::/28 ORCHIDv2 high edge", "2001:2f:ffff::1"},
		{"2001:2::/48 benchmarking", "2001:2::1"},
		{"2001:2::/48 benchmarking high edge", "2001:2:0:ffff::1"},
		{"2001::/23 IETF protocol assignments", "2001:1::1"},
		{"2001:30::/28 drone remote id (inside 2001::/23)", "2001:30::1"},
		{"2001::/32 Teredo", "2001:0:1::1"},
		{"2001:db8::/32 documentation", "2001:db8::1"},
		{"3fff::/20 documentation", "3fff::1"},
		{"3fff::/20 documentation high edge", "3fff:0fff:ffff::1"},
		{"5f00::/16 SRv6 SIDs", "5f00::1"},
		{"2002::/16 6to4", "2002:0808:0808::1"},
		{"64:ff9b::/96 NAT64", "64:ff9b::808:808"},
		{"64:ff9b:1::/48 local NAT64", "64:ff9b:1::1"},
		{"100::/64 discard", "100::1"},
		{"outside 2000::/3: 1fff", "1fff:ffff::1"},
		{"outside 2000::/3: 4000", "4000::"},
		{"outside 2000::/3: 8000", "8000::1"},
		{"outside 2000::/3: e000", "e000::1"},
		{"v4 192.88.99.0/24 6to4 relay anycast", "192.88.99.1"},
		{"v4 192.88.99.0/24 high edge", "192.88.99.255"},
		{"v4-mapped 192.88.99.0/24", "::ffff:192.88.99.1"},
	}
	for _, tc := range deny {
		if PublicAddr(netip.MustParseAddr(tc.ip)) {
			t.Errorf("%s: PublicAddr(%s) = true, want false", tc.name, tc.ip)
		}
	}
	allow := []string{
		"2606:4700:4700::1111", "2001:4860:4860::8888", "2a00:1450:4009:81f::200e", "3ffe::1", "2001:200::1", "2001:db9::1",
		"192.88.98.255", "192.88.100.0", // neighbours of 192.88.99.0/24
		"3fff:1000::1", // just above 3fff::/20
	}
	for _, ip := range allow {
		if !PublicAddr(netip.MustParseAddr(ip)) {
			t.Errorf("PublicAddr(%s) = false, want true", ip)
		}
	}
	// A v4-mapped IPv6 URL literal is refused outright (non-canonical, never a deliberate public destination).
	if _, err := ValidateURL("https://[::ffff:93.184.216.34]/a", URLOptions{}); err == nil {
		t.Error("a v4-mapped IPv6 URL literal was accepted")
	}
}

func TestNAT64_DeniedByDefaultAndEmbeddedV4StillJudgedWhenAllowed(t *testing.T) {
	pub := netip.MustParseAddr("64:ff9b::808:808") // embeds 8.8.8.8
	if PublicAddr(pub) {
		t.Fatal("NAT64 must be denied by default")
	}
	if !PublicAddrWith(pub, AddrOptions{AllowNAT64: true}) {
		t.Error("NAT64 embedding a public v4 must be admitted when explicitly allowed")
	}
	for _, ip := range []string{"64:ff9b::7f00:1", "64:ff9b::a00:1", "64:ff9b::a9fe:a9fe", "64:ff9b::c058:6301", "64:ff9b::6440:1"} {
		if PublicAddrWith(netip.MustParseAddr(ip), AddrOptions{AllowNAT64: true}) {
			t.Errorf("NAT64 %s embeds a non-public v4 and must stay refused even when NAT64 is allowed", ip)
		}
	}
	if PublicAddrWith(netip.MustParseAddr("64:ff9b:1::808:808"), AddrOptions{AllowNAT64: true}) {
		t.Error("64:ff9b:1::/48 must stay refused")
	}
	if err := DialControlWith(DialOptions{})("tcp", "[64:ff9b::808:808]:443", nil); err == nil {
		t.Error("dial control admitted NAT64 by default")
	}
	if err := DialControlWith(DialOptions{AllowNAT64: true})("tcp", "[64:ff9b::808:808]:443", nil); err != nil {
		t.Errorf("dial control refused allowed NAT64: %v", err)
	}
	if err := DialControlWith(DialOptions{AllowNAT64: true})("tcp", "[64:ff9b::7f00:1]:443", nil); err == nil {
		t.Error("dial control admitted NAT64 embedding loopback")
	}
	if _, err := ValidateURL("https://[64:ff9b::808:808]/a", URLOptions{}); err == nil {
		t.Error("NAT64 literal accepted by default")
	}
	if _, err := ValidateURL("https://[64:ff9b::808:808]/a", URLOptions{AllowNAT64: true}); err != nil {
		t.Errorf("NAT64 literal refused when allowed: %v", err)
	}
}

func TestValidateURL_SingleLabelHostsNeedAnExplicitAllowList(t *testing.T) {
	for _, u := range []string{"https://metadata/a", "https://metadata:8443/a", "https://METADATA./a", "https://kubernetes/a", "https://consul/a"} {
		if _, err := ValidateURL(u, URLOptions{}); err == nil {
			t.Errorf("%q accepted: a single-label host resolves through the resolver search path", u)
		}
	}
	if _, err := ValidateURL("https://hooks/a", URLOptions{AllowSingleLabelHosts: []string{"hooks"}}); err != nil {
		t.Errorf("allow-listed single-label host refused: %v", err)
	}
	if _, err := ValidateURL("https://Hooks./a", URLOptions{AllowSingleLabelHosts: []string{"HOOKS"}}); err != nil {
		t.Errorf("allow-list is case and trailing-dot insensitive: %v", err)
	}
	if _, err := ValidateURL("https://metadata/a", URLOptions{AllowSingleLabelHosts: []string{"hooks"}}); err == nil {
		t.Error("a host not on the allow-list was accepted")
	}
	for _, h := range []string{"localhost", "2130706433", "0x7f"} {
		if _, err := ValidateURL("https://"+h+"/a", URLOptions{AllowSingleLabelHosts: []string{h}}); err == nil {
			t.Errorf("allow-listing %q admitted it", h)
		}
	}
	if _, err := ValidateURL("https://hooks.example.com/a", URLOptions{}); err != nil {
		t.Errorf("dotted host refused: %v", err)
	}
}

func TestNoRedirectClientPolicy(t *testing.T) {
	var hits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++; w.WriteHeader(200) }))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redir.Close()
	c := &http.Client{CheckRedirect: NoRedirects}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, redir.URL, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || hits != 0 {
		t.Fatalf("redirect followed: status %d, target hits %d", resp.StatusCode, hits)
	}
}

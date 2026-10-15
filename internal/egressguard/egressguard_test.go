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

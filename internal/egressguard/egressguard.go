// Package egressguard is the platform's outbound-request SSRF guard for destinations that an operator
// configures as a URL (today: the alert webhook channel, ALERT_WEBHOOK_URL). It has three independent
// layers, none of which trusts another:
//
//  1. ValidateURL: a static check of the configured URL (https only, no userinfo, no fragment, no
//     numeric-obfuscated or private IP literal, no loopback or internal host name).
//  2. DialControl: a net.Dialer.Control hook that judges the address actually being connected to AFTER
//     DNS resolution, at connect time, for every connection (so DNS rebinding and a name that resolves
//     to a private address are refused even though the URL passed layer 1).
//  3. NoRedirects: an http.Client.CheckRedirect that never follows a redirect (a redirect is the classic
//     way to walk from an allowed host to an internal one).
//
// A caller must also build its http.Transport with Proxy == nil (an environment HTTP(S)_PROXY would
// otherwise move the connection, and with it layer 2, to the proxy).
package egressguard

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// ErrRefused is wrapped by every refusal. The message never contains the offending URL or address.
var ErrRefused = errors.New("egressguard: destination refused")

func refuse(reason string) error { return fmt.Errorf("%w: %s", ErrRefused, reason) }

// Special-purpose IPv4 ranges that are not publicly routable unicast destinations beyond what the net/netip
// predicates already cover (loopback, RFC 1918, link-local, multicast, unspecified).
var nonPublicV4 = mustPrefixes(
	"0.0.0.0/8",       // "this network"
	"100.64.0.0/10",   // carrier-grade NAT
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // TEST-NET-1
	"192.88.99.0/24",  // deprecated 6to4 relay anycast
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"240.0.0.0/4",     // reserved (includes broadcast)
)

// IPv6 is an ALLOW-list: only global unicast 2000::/3 is considered at all (everything else, including ::/96,
// ::ffff:0:0/96, 64:ff9b::/96, 100::/64, 5f00::/16, fc00::/7, fe80::/10 and ff00::/8, is outside it and
// therefore refused), minus the special-purpose ranges that sit INSIDE 2000::/3.
var (
	globalUnicastV6 = netip.MustParsePrefix("2000::/3")
	nat64Prefix     = netip.MustParsePrefix("64:ff9b::/96")
	nonPublicV6     = mustPrefixes(
		"::/96",          // deprecated IPv4-compatible (outside 2000::/3; listed so the table documents it)
		"::ffff:0:0/96",  // IPv4-mapped (judged as its IPv4; listed for the same reason)
		"2001::/23",      // IETF protocol assignments: Teredo, PCP/AMT anycast, benchmarking, ORCHID, ORCHIDv2, drone remote id
		"2001:2::/48",    // benchmarking
		"2001:10::/28",   // ORCHID (deprecated)
		"2001:20::/28",   // ORCHIDv2
		"2001:db8::/32",  // documentation
		"2002::/16",      // 6to4 (embeds an IPv4 destination)
		"3fff::/20",      // documentation
		"5f00::/16",      // SRv6 SIDs
		"64:ff9b:1::/48", // local-use NAT64
	)
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

// AddrOptions tunes PublicAddrWith.
type AddrOptions struct {
	// AllowNAT64 admits 64:ff9b::/96 (RFC 6052 well-known NAT64) for IPv6-only networks, but only when the
	// embedded IPv4 destination is itself public. The default (false) refuses every NAT64 address.
	AllowNAT64 bool
}

// PublicAddr reports whether ip is a globally routable unicast address that is safe to connect to from the
// platform. It is fail-closed: an invalid address is not public; IPv6 is accepted only inside 2000::/3 minus
// the special-purpose ranges; IPv4 is refused in every reserved, private, loopback, link-local (including the
// cloud metadata address), multicast, carrier-grade-NAT, documentation or anycast-relay range. An
// IPv4-mapped IPv6 address is judged as its IPv4. NAT64 is refused (see PublicAddrWith).
func PublicAddr(ip netip.Addr) bool { return PublicAddrWith(ip, AddrOptions{}) }

// PublicAddrWith is PublicAddr with options.
func PublicAddrWith(ip netip.Addr, opts AddrOptions) bool {
	if !ip.IsValid() {
		return false
	}
	ip = ip.WithZone("").Unmap()
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	if ip.Is4() {
		return !inAny(nonPublicV4, ip)
	}
	if nat64Prefix.Contains(ip) {
		if !opts.AllowNAT64 {
			return false
		}
		b := ip.As16()
		return PublicAddrWith(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), AddrOptions{})
	}
	if !globalUnicastV6.Contains(ip) {
		return false
	}
	return !inAny(nonPublicV6, ip)
}

func inAny(ps []netip.Prefix, ip netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// DialControl returns a net.Dialer.Control function that refuses to connect to any non-public address. With
// allowLoopback (development only) it additionally admits loopback addresses and NOTHING else: the other
// private, link-local and metadata ranges stay refused. An address it cannot parse is refused.
func DialControl(allowLoopback bool) func(network, address string, c syscall.RawConn) error {
	return DialControlWith(DialOptions{AllowLoopback: allowLoopback})
}

// DialOptions tunes DialControlWith.
type DialOptions struct {
	AllowLoopback bool // development only
	AllowNAT64    bool // see AddrOptions.AllowNAT64
}

// DialControlWith is DialControl with options.
func DialControlWith(opts DialOptions) func(network, address string, c syscall.RawConn) error {
	return func(_ string, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return refuse("unparsable dial address")
		}
		ip := ap.Addr().Unmap()
		if opts.AllowLoopback && ip.IsLoopback() {
			return nil
		}
		if !PublicAddrWith(ip, AddrOptions{AllowNAT64: opts.AllowNAT64}) {
			return refuse("non-public dial address")
		}
		return nil
	}
}

// NoRedirects is an http.Client.CheckRedirect that never follows a redirect: the 3xx response is returned
// to the caller, which treats it as a rejection.
func NoRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// URLOptions tunes ValidateURL.
type URLOptions struct {
	// AllowLoopbackDev admits a loopback destination (an IP literal in 127.0.0.0/8 or ::1, or the host name
	// "localhost") and, for that destination only, plain http. It exists so development can target a local
	// receiver; the caller sets it only for an explicit development environment. It never admits a private,
	// link-local or metadata destination.
	AllowLoopbackDev bool
	// AllowNAT64 admits a 64:ff9b::/96 IP literal whose embedded IPv4 is public (default: refused).
	AllowNAT64 bool
	// AllowSingleLabelHosts lists the ONLY dot-less host names (for example an in-cluster service name) that
	// are accepted. A single-label name resolves through the resolver's search path, so a name such as
	// "metadata" can reach an internal service; the connect-time check still applies to whatever it resolves
	// to, but the static check refuses it unless it is explicitly listed. Comparison is case-insensitive.
	AllowSingleLabelHosts []string
}

const maxURLBytes = 2048

var (
	hostLabelRe  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	numericishRe = regexp.MustCompile(`^(0x[0-9a-f]+|[0-9]+)(\.(0x[0-9a-f]+|[0-9]+))*$`)
	badURLCharRe = regexp.MustCompile(`[\x00-\x20\x7f\\]`)
)

// ValidateURL statically validates a configured destination URL and returns it parsed. It performs no DNS
// and no I/O (layer 2, DialControl, judges the resolved address at connect time).
func ValidateURL(raw string, opts URLOptions) (*url.URL, error) {
	if raw == "" || len(raw) > maxURLBytes {
		return nil, refuse("empty or oversized url")
	}
	if badURLCharRe.MatchString(raw) {
		return nil, refuse("control character, space or backslash in url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, refuse("unparsable url")
	}
	if u.Opaque != "" || u.User != nil || u.Fragment != "" || u.RawFragment != "" {
		return nil, refuse("userinfo, fragment or opaque url form")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return nil, refuse("no host")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return nil, refuse("port out of range")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, refuse("empty port")
	}

	loopbackHost := false
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Is4In6() {
			return nil, refuse("v4-mapped ipv6 literal")
		}
		switch {
		case opts.AllowLoopbackDev && ip.IsLoopback():
			loopbackHost = true
		case !PublicAddrWith(ip, AddrOptions{AllowNAT64: opts.AllowNAT64}):
			return nil, refuse("non-public ip literal")
		}
	} else {
		if numericishRe.MatchString(host) {
			return nil, refuse("numeric host that is not a plain ip literal")
		}
		if host == "localhost" || strings.HasSuffix(host, ".localhost") {
			if !opts.AllowLoopbackDev || host != "localhost" {
				return nil, refuse("loopback host name")
			}
			loopbackHost = true
		} else {
			for _, suffix := range []string{".local", ".localdomain", ".internal", ".intranet", ".lan", ".home.arpa"} {
				if strings.HasSuffix(host, suffix) {
					return nil, refuse("internal host name")
				}
			}
			if !strings.Contains(host, ".") && !singleLabelAllowed(host, opts.AllowSingleLabelHosts) {
				return nil, refuse("single-label host name that is not allow-listed")
			}
			if len(host) > 253 {
				return nil, refuse("host too long")
			}
			for _, label := range strings.Split(host, ".") {
				if !hostLabelRe.MatchString(label) {
					return nil, refuse("host is not an ASCII (punycode) DNS name")
				}
			}
		}
	}

	switch u.Scheme {
	case "https":
	case "http":
		if !opts.AllowLoopbackDev || !loopbackHost {
			return nil, refuse("https required")
		}
	default:
		return nil, refuse("https required")
	}
	return u, nil
}

func singleLabelAllowed(host string, allow []string) bool {
	for _, a := range allow {
		if strings.ToLower(strings.TrimSuffix(strings.TrimSpace(a), ".")) == host {
			return true
		}
	}
	return false
}

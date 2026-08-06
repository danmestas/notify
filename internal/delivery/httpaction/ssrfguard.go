package httpaction

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// blockedNets is the package-level set of CIDRs not covered by the net.IP
// classifier methods (IsLoopback, IsPrivate, etc.).  Parsed once at init time;
// panics on malformed literals (programming error, not a runtime condition).
var blockedNets = func() []*net.IPNet {
	cidrs := []string{
		"100.64.0.0/10",      // CGNAT / Tailscale mesh (RFC 6598) — routes to internal hosts on this infra
		"0.0.0.0/8",          // "this" network — 0.x.x.x routes to loopback on Linux; IsUnspecified only catches 0.0.0.0
		"64:ff9b::/96",       // NAT64 well-known prefix (RFC 6052) — embeds IPv4 (e.g. 64:ff9b::7f00:1 = 127.0.0.1)
		"255.255.255.255/32", // limited broadcast
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(fmt.Sprintf("ssrfguard: invalid blocklist CIDR %q: %v", cidr, err))
		}
		nets = append(nets, n)
	}
	return nets
}()

// IsBlockedIP reports whether ip is in a range that the SSRF guard must reject:
// loopback, link-local unicast/multicast, RFC1918 private (10/8, 172.16/12,
// 192.168/16), unique-local IPv6 (fc00::/7 — covered by IsPrivate in Go 1.17+),
// unspecified (0.0.0.0/::), multicast, CGNAT/Tailscale mesh (100.64.0.0/10),
// the "this" network (0.0.0.0/8), NAT64 prefix (64:ff9b::/96), and broadcast
// (255.255.255.255/32).
//
// Checking at dial time (post-DNS-resolution) defeats DNS-rebinding attacks that
// a URL-string check at request-build time would miss.
//
// IPv4-mapped IPv6 addresses (e.g. ::ffff:127.0.0.1) are normalised by the
// net.IP classifier methods via To4() so they are caught correctly without
// special handling here.
//
// These ranges are blocked unconditionally. The only exemption is an operator
// naming a specific host in the allowlist (see allowlist.go), which is applied
// by hostname before dialling rather than by relaxing this classifier.
func IsBlockedIP(ip net.IP) bool {
	if ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() || // covers 10/8, 172.16/12, 192.168/16, and fc00::/7
		ip.IsUnspecified() ||
		ip.IsMulticast() {
		return true
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ssrfDialControl is a net.Dialer.Control function — called after DNS resolution
// but before the actual TCP connect syscall, so `address` is already
// "resolvedIP:port". This is the primary SSRF defence: it is immune to
// DNS-rebinding attacks that a URL-string check at request-build time would miss.
func ssrfDialControl(_ string, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("ssrf guard: parse address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Should not happen after DNS resolution; explicit rejection is safer than permit.
		return fmt.Errorf("ssrf guard: non-IP address %q after DNS resolution", host)
	}
	if IsBlockedIP(ip) {
		return fmt.Errorf("ssrf guard: request to blocked address %s", ip)
	}
	return nil
}

// NewSSRFSafeClientWithTimeout returns an SSRF-safe client (see
// NewSSRFSafeClient) whose overall per-request Timeout is set to d. Callers
// outside this package that need a bounded request (rather than relying solely
// on a context deadline) should use this instead of constructing their own
// wrapper around NewSSRFSafeClient, so the dialer/Control guard logic stays in
// exactly one place.
func NewSSRFSafeClientWithTimeout(d time.Duration) *http.Client {
	c := NewSSRFSafeClient()
	c.Timeout = d
	return c
}

// NewSSRFSafeClient returns an http.Client that enforces the SSRF guard at two
// independent layers:
//
//  1. Transport.DialContext uses a net.Dialer whose Control hook checks the
//     resolved IP at dial time — the primary defence, immune to DNS-rebinding.
//  2. CheckRedirect re-applies the same guard for literal-IP redirect targets,
//     surfacing a clear error before any connection attempt is made.
//
// A blocked dial surfaces as a transport error and flows through the step's
// existing failure policy (fail_closed → NonRetryableError; continue → zero-value
// output) without any special casing here.
//
// Hosts named in the operator allowlist (see allowlist.go) are exempt from both
// layers. The allowlist is read at dial time, not captured here, so a client
// built in a package-level var still honours configuration loaded later.
func NewSSRFSafeClient() *http.Client {
	guarded := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   ssrfDialControl,
	}
	// Same dialer minus the guard, used only for explicitly allowlisted hosts.
	allowed := &net.Dialer{
		Timeout:   guarded.Timeout,
		KeepAlive: guarded.KeepAlive,
	}
	dialContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
		// addr here is the host:port from the request URL, before DNS
		// resolution -- so this compares the name the operator listed, which is
		// what they can actually vouch for. The resolved-IP check still runs for
		// every host that is not listed.
		if host, _, err := net.SplitHostPort(addr); err == nil && hostAllowed(host) {
			return allowed.DialContext(ctx, network, addr)
		}
		return guarded.DialContext(ctx, network, addr)
	}
	transport := &http.Transport{
		DialContext:           dialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("ssrf guard: too many redirects")
			}
			// Belt-and-suspenders early rejection for literal-IP redirect targets.
			// Hostname-based redirects to private ranges are caught by the
			// dial-time Control hook, which remains the definitive enforcement point.
			host := req.URL.Hostname()
			if hostAllowed(host) {
				return nil
			}
			if ip := net.ParseIP(host); ip != nil && IsBlockedIP(ip) {
				return fmt.Errorf("ssrf guard: redirect to blocked address %s", ip)
			}
			return nil
		},
	}
}

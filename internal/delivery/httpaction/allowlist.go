package httpaction

import (
	"fmt"
	"net"
	"strings"
	"sync/atomic"
)

// The SSRF guard blocks every internal range unconditionally. Some operators
// genuinely need one or two internal hosts reachable from a workflow HTTP step
// (an on-prem CRM, an internal webhook receiver). This is the opt-in escape
// hatch for exactly that, and nothing more:
//
//   - Deny-by-default is unchanged. An empty allowlist -- the default, and the
//     state of every deployment that never sets the variable -- means the guard
//     behaves exactly as it did before this file existed.
//   - Entries are EXACT hosts. No wildcards, no suffix matching, no CIDRs.
//     "corp.example" does not admit "evil.corp.example". Matching an operator's
//     intent loosely is how allowlists become the vulnerability they were meant
//     to contain, so a pattern that looks like it should match a family of hosts
//     is rejected at parse time rather than silently never matching.
//   - An allowlisted host bypasses the IP-range check entirely. That is the
//     point: the host resolves to an internal address, which is why it needed
//     listing. The operator is asserting they trust that name.
//
// The allowlist is process-wide rather than a constructor argument because one
// client (internal/providers/webhook) is built in a package-level var, before
// main can read any configuration. Consulting it at dial time instead of
// capturing it at construction keeps every existing call site unchanged.
var allowedHosts atomic.Pointer[map[string]struct{}]

// ConfigureAllowlist parses raw -- a comma-separated list of exact hostnames or
// IP literals -- and installs it as the process-wide SSRF allowlist, replacing
// any previous one. An empty or whitespace-only raw value installs an empty
// allowlist, which is the deny-everything default.
//
// It returns an error on any entry that cannot mean what it appears to mean, so
// a mistyped allowlist fails at startup rather than quietly admitting nothing.
func ConfigureAllowlist(raw string) error {
	hosts := make(map[string]struct{})
	for _, field := range strings.Split(raw, ",") {
		entry := strings.TrimSpace(field)
		if entry == "" {
			continue
		}
		key, err := allowlistKey(entry)
		if err != nil {
			return fmt.Errorf("ssrf allowlist: %w", err)
		}
		hosts[key] = struct{}{}
	}
	allowedHosts.Store(&hosts)
	return nil
}

// allowlistKey validates one entry and reduces it to the form hostAllowed
// compares against.
func allowlistKey(entry string) (string, error) {
	switch {
	case strings.Contains(entry, "*"):
		return "", fmt.Errorf("entry %q contains a wildcard; only exact hosts are supported, list each host separately", entry)
	case strings.Contains(entry, "://"):
		return "", fmt.Errorf("entry %q looks like a URL; list the host alone, without a scheme", entry)
	case strings.Contains(entry, "/"):
		return "", fmt.Errorf("entry %q contains %q; CIDR ranges and paths are not supported, list each host separately", entry, "/")
	}

	host := normalizeHost(entry)
	if host == "" {
		return "", fmt.Errorf("entry %q is not a usable host", entry)
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	// A bare colon in a non-IP entry is a port, which the guard never sees:
	// dialing splits it off before the host is compared.
	if strings.Contains(host, ":") {
		return "", fmt.Errorf("entry %q contains a port; list the host alone", entry)
	}
	for _, r := range host {
		if r <= ' ' || r == '?' || r == '#' || r == '@' {
			return "", fmt.Errorf("entry %q contains an invalid character in a hostname", entry)
		}
		if r > 127 {
			return "", fmt.Errorf("entry %q is not ASCII; list the punycode (xn--) form of an internationalised host", entry)
		}
	}
	return host, nil
}

// normalizeHost puts a host into the canonical form used on both sides of an
// allowlist comparison: lowercase (DNS is case-insensitive) with the optional
// FQDN root dot removed, so "Host.Example." and "host.example" are one host.
//
// Case folding is deliberately ASCII-only rather than strings.ToLower. Unicode
// folding maps distinct code points onto ASCII letters -- U+212A KELVIN SIGN
// folds to "k" -- so a Unicode host could fold onto an allowlisted ASCII name
// and inherit its exemption. Entries are restricted to ASCII at parse time, and
// with ASCII-only folding here a non-ASCII request host simply never matches.
func normalizeHost(host string) string {
	b := []byte(strings.TrimSpace(host))
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	out := string(b)
	if len(out) > 1 {
		out = strings.TrimSuffix(out, ".")
	}
	return out
}

// hostAllowed reports whether host was explicitly listed by the operator and is
// therefore exempt from the IP-range check. host is the name as it appeared in
// the request URL, before DNS resolution.
func hostAllowed(host string) bool {
	hosts := allowedHosts.Load()
	if hosts == nil || len(*hosts) == 0 {
		return false
	}
	key := normalizeHost(host)
	if ip := net.ParseIP(key); ip != nil {
		key = ip.String()
	}
	_, ok := (*hosts)[key]
	return ok
}

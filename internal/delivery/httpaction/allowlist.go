package httpaction

import (
	"fmt"
	"net"
	"strconv"
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
//   - An entry is a bare host (any port) or host:port (that port only; IPv6 as
//     "[::1]:3738"). Prefer the port form: allowing 127.0.0.1 on every port
//     exposes every local service, not just the one the operator meant.
//   - An allowlisted host bypasses the IP-range check entirely. That is the
//     point: the host resolves to an internal address, which is why it needed
//     listing. The operator is asserting they trust that name.
//
// The allowlist is process-wide rather than a constructor argument because one
// client (internal/providers/webhook) is built in a package-level var, before
// main can read any configuration. Consulting it at dial time instead of
// capturing it at construction keeps every existing call site unchanged.
var allowedHosts atomic.Pointer[allowlist]

// allowlist holds bare-host entries (any port) and host:port entries apart, so
// a port-scoped entry never widens into a host-wide one.
type allowlist struct {
	hosts     map[string]struct{}
	hostPorts map[string]struct{}
}

// ConfigureAllowlist parses raw -- a comma-separated list of exact hostnames or
// IP literals -- and installs it as the process-wide SSRF allowlist, replacing
// any previous one. An empty or whitespace-only raw value installs an empty
// allowlist, which is the deny-everything default.
//
// It returns an error on any entry that cannot mean what it appears to mean, so
// a mistyped allowlist fails at startup rather than quietly admitting nothing.
func ConfigureAllowlist(raw string) error {
	list := &allowlist{hosts: map[string]struct{}{}, hostPorts: map[string]struct{}{}}
	for _, field := range strings.Split(raw, ",") {
		entry := strings.TrimSpace(field)
		if entry == "" {
			continue
		}
		host, port, err := parseEntry(entry)
		if err != nil {
			return fmt.Errorf("ssrf allowlist: %w", err)
		}
		if port == "" {
			list.hosts[host] = struct{}{}
		} else {
			list.hostPorts[net.JoinHostPort(host, port)] = struct{}{}
		}
	}
	allowedHosts.Store(list)
	return nil
}

// parseEntry splits one entry into a validated host and an optional port
// ("" means any port).
func parseEntry(entry string) (host, port string, err error) {
	if net.ParseIP(entry) == nil && strings.Contains(entry, ":") && !strings.Contains(entry, "://") {
		h, p, serr := net.SplitHostPort(entry)
		if serr != nil {
			return "", "", fmt.Errorf("entry %q is not a valid host:port (IPv6 needs brackets, e.g. [::1]:3738): %v", entry, serr)
		}
		n, perr := strconv.Atoi(p)
		if perr != nil || n < 1 || n > 65535 {
			return "", "", fmt.Errorf("entry %q has invalid port %q; the port must be a number from 1 to 65535", entry, p)
		}
		host, err = allowlistKey(h)
		return host, strconv.Itoa(n), err
	}
	host, err = allowlistKey(entry)
	return host, "", err
}

// allowlistKey validates one host (no port) and reduces it to the form
// hostAllowed compares against.
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
	if strings.Contains(host, ":") {
		return "", fmt.Errorf("entry %q is not a usable host", entry)
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

// hostAllowed reports whether host:port was explicitly listed by the operator
// and is therefore exempt from the IP-range check. host is the name as it
// appeared in the request URL, before DNS resolution; for a literal IP that is
// the very address dialed, so a port-scoped IP entry matches the real
// destination. A bare-host entry matches any port; a host:port entry only that
// port.
func hostAllowed(host, port string) bool {
	list := allowedHosts.Load()
	if list == nil || (len(list.hosts) == 0 && len(list.hostPorts) == 0) {
		return false
	}
	key := normalizeHost(host)
	if ip := net.ParseIP(key); ip != nil {
		key = ip.String()
	}
	if _, ok := list.hosts[key]; ok {
		return true
	}
	if port == "" {
		return false
	}
	_, ok := list.hostPorts[net.JoinHostPort(key, port)]
	return ok
}

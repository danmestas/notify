package providers

import (
	"context"
	"crypto/tls"
	"net"
)

// smtpsPort is SMTP submission over implicit TLS (RFC 8314): the server
// expects a TLS handshake as its first bytes and sends no greeting until then,
// so a plain dial on it waits for a banner that never comes.
const smtpsPort = "465"

// DialSMTP connects to the SMTP server at addr ("host:port"). On port 465 it
// opens TLS straight away, verified for host. On any other port it dials plain
// TCP, and the caller upgrades with STARTTLS when the server offers it.
// net/smtp sees a *tls.Conn as already encrypted, so PLAIN auth works on both.
func DialSMTP(ctx context.Context, addr, host string) (net.Conn, error) {
	return dialSMTP(ctx, addr, host, nil)
}

// dialSMTP is DialSMTP with an optional TLS config, so tests can trust their
// own certificate.
func dialSMTP(ctx context.Context, addr, host string, cfg *tls.Config) (net.Conn, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if port != smtpsPort {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
	return dialImplicitTLS(ctx, addr, host, cfg)
}

func dialImplicitTLS(ctx context.Context, addr, host string, cfg *tls.Config) (net.Conn, error) {
	if cfg == nil {
		cfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	cfg = cfg.Clone()
	cfg.ServerName = host
	d := tls.Dialer{Config: cfg}
	return d.DialContext(ctx, "tcp", addr)
}

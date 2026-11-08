package providers

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"testing"
	"time"
)

// smtpsServer listens with TLS from the first byte, as port 465 does, and
// greets only after the handshake. It answers EHLO and QUIT.
func smtpsServer(t *testing.T) (addr string, roots *x509.CertPool) {
	t.Helper()
	// httptest's certificate is issued for 127.0.0.1 and example.com.
	hs := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(hs.Close)
	roots = x509.NewCertPool()
	roots.AddCert(hs.Certificate())
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: hs.TLS.Certificates})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				_, _ = c.Write([]byte("220 smtps.test ESMTP\r\n"))
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if len(line) >= 4 && line[:4] == "QUIT" {
						_, _ = c.Write([]byte("221 bye\r\n"))
						return
					}
					_, _ = c.Write([]byte("250 smtps.test\r\n"))
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), roots
}

func TestDialImplicitTLS_GetsTheGreeting(t *testing.T) {
	addr, roots := smtpsServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialImplicitTLS(ctx, addr, "127.0.0.1", &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c, err := smtp.NewClient(conn, "127.0.0.1")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = c.Close() }()
	if _, ok := c.TLSConnectionState(); !ok {
		t.Fatal("client does not see the connection as TLS, so PLAIN auth would be refused")
	}
	if err := c.Quit(); err != nil {
		t.Fatalf("quit: %v", err)
	}
}

func TestDialImplicitTLS_VerifiesTheHost(t *testing.T) {
	addr, roots := smtpsServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := dialImplicitTLS(ctx, addr, "smtp.wrong.test", &tls.Config{RootCAs: roots}); err == nil {
		t.Fatal("dial succeeded for a host the certificate does not name")
	}
}

func TestDialSMTP_PlainOnOtherPorts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_, _ = c.Write([]byte("220 plain.test ESMTP\r\n"))
			_ = c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := DialSMTP(ctx, ln.Addr().String(), "127.0.0.1")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, isTLS := conn.(*tls.Conn); isTLS {
		t.Fatal("a non-465 port was dialled with TLS")
	}
}

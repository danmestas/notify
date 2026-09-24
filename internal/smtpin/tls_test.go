package smtpin

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCert writes a self-signed certificate for mx.notify.test with the
// given serial to dir, returning the cert and key paths.
func writeCert(t *testing.T, dir string, serial int64) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "mx.notify.test"},
		DNSNames:     []string{"mx.notify.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

// bumpMTime moves both files' modification time forward, so a reload is
// detected even on filesystems with coarse timestamps.
func bumpMTime(t *testing.T, paths ...string) {
	t.Helper()
	at := time.Now().Add(time.Minute)
	for _, p := range paths {
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
}

// servedSerial dials, upgrades, and returns the serial number the server presented.
func servedSerial(t *testing.T, f *fixture) int64 {
	t.Helper()
	c := f.dial(t)
	if err := c.StartTLS(&tls.Config{InsecureSkipVerify: true, ServerName: "mx.notify.test"}); err != nil { //nolint:gosec // self-signed test cert
		t.Fatalf("STARTTLS: %v", err)
	}
	st, ok := c.TLSConnectionState()
	if !ok || len(st.PeerCertificates) == 0 {
		t.Fatal("no TLS state after STARTTLS")
	}
	return st.PeerCertificates[0].SerialNumber.Int64()
}

// TestSTARTTLS_OfferedAndDeliversOverTLS is #362: with a certificate the
// EHLO lists STARTTLS, a sender upgrades, and the reply is delivered over
// the encrypted connection.
func TestSTARTTLS_OfferedAndDeliversOverTLS(t *testing.T) {
	certPath, keyPath := writeCert(t, t.TempDir(), 1)
	certs, err := LoadCertificate(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("LoadCertificate: %v", err)
	}
	f := newFixtureWith(t, certs)

	c := f.dial(t)
	if ok, _ := c.Extension("STARTTLS"); !ok {
		t.Fatal("EHLO does not offer STARTTLS with a certificate loaded")
	}
	if err := c.StartTLS(&tls.Config{InsecureSkipVerify: true, ServerName: "mx.notify.test"}); err != nil { //nolint:gosec // self-signed test cert
		t.Fatalf("STARTTLS: %v", err)
	}
	if _, ok := c.TLSConnectionState(); !ok {
		t.Fatal("connection is not TLS after STARTTLS")
	}
	if err := c.Mail("dev@example.com"); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	if err := c.Rcpt(f.mailbox); err != nil {
		t.Fatalf("RCPT: %v", err)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatalf("DATA: %v", err)
	}
	if _, err := w.Write([]byte("From: dev@example.com\r\nTo: " + f.mailbox + "\r\nSubject: re\r\n\r\nover tls\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("end DATA: %v", err)
	}
	if f.responder.count() != 1 {
		t.Fatalf("responder dispatched %d times, want 1", f.responder.count())
	}
}

// TestSTARTTLS_NotOfferedWithoutCertificate pins today's behaviour for a
// receiver started with no certificate.
func TestSTARTTLS_NotOfferedWithoutCertificate(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	if ok, _ := c.Extension("STARTTLS"); ok {
		t.Fatal("STARTTLS offered with no certificate")
	}
}

// TestSTARTTLS_RenewalIsPickedUpAndABadOneIsNot: a replaced certificate is
// served on the next connection with no restart, and a renewal that cannot
// be loaded keeps the previous certificate rather than dropping TLS.
func TestSTARTTLS_RenewalIsPickedUpAndABadOneIsNot(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeCert(t, dir, 1)
	certs, err := LoadCertificate(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("LoadCertificate: %v", err)
	}
	f := newFixtureWith(t, certs)
	if got := servedSerial(t, f); got != 1 {
		t.Fatalf("serial = %d, want 1", got)
	}

	writeCert(t, dir, 2)
	bumpMTime(t, certPath, keyPath)
	if got := servedSerial(t, f); got != 2 {
		t.Fatalf("after renewal serial = %d, want 2", got)
	}

	if err := os.WriteFile(certPath, []byte("half-written"), 0o600); err != nil {
		t.Fatalf("corrupt cert: %v", err)
	}
	bumpMTime(t, certPath, keyPath)
	if got := servedSerial(t, f); got != 2 {
		t.Fatalf("after a bad renewal serial = %d, want the previous 2", got)
	}
}

// TestLoadCertificate_UnusableFilesRefuse: asked-for TLS that cannot load is
// an error at start, which main turns into a refusal to boot.
func TestLoadCertificate_UnusableFilesRefuse(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadCertificate(filepath.Join(dir, "missing.pem"), filepath.Join(dir, "missing.key"), nil); err == nil {
		t.Fatal("missing files loaded")
	}
	certPath, keyPath := writeCert(t, dir, 1)
	_, otherKey := writeCert(t, t.TempDir(), 2)
	if _, err := LoadCertificate(certPath, otherKey, nil); err == nil {
		t.Fatal("a certificate with the wrong key loaded")
	}
	if _, err := LoadCertificate(certPath, keyPath, nil); err != nil {
		t.Fatalf("a good pair refused: %v", err)
	}
}

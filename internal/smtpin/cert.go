package smtpin

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// CertSource serves the receiver's STARTTLS certificate from a certificate
// and key file on disk, re-reading them when either file changes so a
// renewal (certbot, Caddy) takes effect without a restart (#362).
//
// A renewal that cannot be loaded -- a half-written file, a key that does not
// match -- keeps the last good certificate and logs, rather than dropping
// TLS for every sender until someone notices.
type CertSource struct {
	certPath, keyPath string
	log               *slog.Logger

	mu       sync.Mutex
	cert     *tls.Certificate
	certMod  time.Time
	keyMod   time.Time
	lastFail time.Time
}

// LoadCertificate loads certPath and keyPath and returns a source that
// reloads them on change. An error here means the files are unusable now,
// which the caller treats as a refusal to start: an operator who asked for
// TLS should not get a receiver that silently offers none.
func LoadCertificate(certPath, keyPath string, log *slog.Logger) (*CertSource, error) {
	if log == nil {
		log = slog.Default()
	}
	cs := &CertSource{certPath: certPath, keyPath: keyPath, log: log}
	cm, km, err := cs.modTimes()
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("smtpin: load certificate: %w", err)
	}
	cs.cert, cs.certMod, cs.keyMod = &cert, cm, km
	return cs, nil
}

// TLSConfig returns the config the SMTP server offers STARTTLS with.
func (cs *CertSource) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: cs.getCertificate,
	}
}

// getCertificate serves the current certificate, reloading first if either
// file has changed since it was last read. Inbound mail handshakes are rare
// enough that two stats per handshake cost nothing.
func (cs *CertSource) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cm, km, err := cs.modTimes()
	if err != nil {
		cs.reportFailure(err)
		return cs.cert, nil
	}
	if cm.Equal(cs.certMod) && km.Equal(cs.keyMod) {
		return cs.cert, nil
	}
	cert, err := tls.LoadX509KeyPair(cs.certPath, cs.keyPath)
	if err != nil {
		cs.reportFailure(err)
		return cs.cert, nil
	}
	cs.cert, cs.certMod, cs.keyMod = &cert, cm, km
	cs.log.Info("smtpin: certificate reloaded", "cert", cs.certPath)
	return cs.cert, nil
}

// reportFailure logs a failed reload, at most once a minute so a broken
// renewal does not flood the log with one line per inbound connection.
func (cs *CertSource) reportFailure(err error) {
	if time.Since(cs.lastFail) < time.Minute {
		return
	}
	cs.lastFail = time.Now()
	cs.log.Error("smtpin: certificate reload failed, serving the previous one", "cert", cs.certPath, "err", err)
}

func (cs *CertSource) modTimes() (cert, key time.Time, err error) {
	ci, err := os.Stat(cs.certPath)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("smtpin: certificate: %w", err)
	}
	ki, err := os.Stat(cs.keyPath)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("smtpin: key: %w", err)
	}
	return ci.ModTime(), ki.ModTime(), nil
}

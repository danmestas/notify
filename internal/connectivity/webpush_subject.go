package connectivity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"strings"

	"github.com/danmestas/notify/internal/domain"
)

// ErrInvalidVAPIDSubject is returned when a Web Push VAPID subject is not a
// contact a push service will accept (#358): Apple rejects the JWT outright
// when the "sub" claim is empty, localhost, or otherwise unroutable.
var ErrInvalidVAPIDSubject = errors.New("connectivity: invalid VAPID subject")

// ErrNoVAPIDSubject is returned when a Web Push send has no subject to sign
// with: the environment has none set and the server has no public base URL
// to derive one from.
var ErrNoVAPIDSubject = errors.New("connectivity: no VAPID subject: set one for this environment, or set NOTIFY_PUBLIC_BASE_URL")

// WithPublicBaseURL gives the service the server's public URL, from which the
// default Web Push VAPID subject is derived as https://<host>. A base URL
// whose host would be refused as a subject (localhost, a bare IP) yields no
// default rather than a bad one.
func WithPublicBaseURL(base string) Option {
	return func(svc *Service) {
		u, err := url.Parse(strings.TrimSpace(base))
		if err != nil || u.Host == "" {
			return
		}
		candidate := "https://" + u.Host
		if ValidateVAPIDSubject(candidate) == nil {
			svc.defaultVAPIDSubject = candidate
		}
	}
}

// ValidateVAPIDSubject checks that s is a contact RFC 8292 allows and a push
// service will accept: mailto:<address> or https://<host>, where the domain
// is a real, dotted name rather than localhost or an IP literal.
func ValidateVAPIDSubject(s string) error {
	s = strings.TrimSpace(s)
	var host string
	switch {
	case strings.HasPrefix(s, "mailto:"):
		addr, err := mail.ParseAddress(strings.TrimPrefix(s, "mailto:"))
		if err != nil {
			return fmt.Errorf("%w: %q is not a mailto: address", ErrInvalidVAPIDSubject, s)
		}
		at := strings.LastIndexByte(addr.Address, '@')
		host = addr.Address[at+1:]
	case strings.HasPrefix(s, "https://"):
		u, err := url.Parse(s)
		if err != nil || u.Hostname() == "" {
			return fmt.Errorf("%w: %q is not an https URL", ErrInvalidVAPIDSubject, s)
		}
		host = u.Hostname()
	default:
		return fmt.Errorf("%w: %q must start with mailto: or https://", ErrInvalidVAPIDSubject, s)
	}
	// A dotted name rules out "localhost" and single-label intranet hosts;
	// the suffix and IP checks catch the rest.
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") ||
		net.ParseIP(strings.Trim(h, "[]")) != nil || !strings.Contains(h, ".") {
		return fmt.Errorf("%w: %q does not name a reachable contact", ErrInvalidVAPIDSubject, s)
	}
	return nil
}

// WebPushSubject reports the scope's VAPID subject and whether it is the
// server default rather than one set for this environment. An empty subject
// means Web Push cannot send (ErrNoVAPIDSubject).
func (svc *Service) WebPushSubject(ctx context.Context, scope domain.Scope) (subject string, isDefault bool, err error) {
	cfg, err := svc.webPushConfig(ctx, scope)
	if err != nil {
		return "", false, err
	}
	if cfg.Subject != "" {
		return cfg.Subject, false, nil
	}
	return svc.defaultVAPIDSubject, true, nil
}

// SetWebPushSubject stores the scope's VAPID subject, minting the keypair
// first if the scope has none. An empty subject clears the override so the
// server default applies again. An invalid subject is refused here, at setup,
// rather than discovered later as a 403 from the push service.
func (svc *Service) SetWebPushSubject(ctx context.Context, scope domain.Scope, subject string) error {
	subject = strings.TrimSpace(subject)
	if subject != "" {
		if err := ValidateVAPIDSubject(subject); err != nil {
			return err
		}
	}
	if _, _, err := svc.EnsureWebPushKeys(ctx, scope); err != nil {
		return err
	}
	in, err := svc.store.Integrations().GetByProviderID(ctx, scope, webPushProviderID)
	if err != nil {
		return fmt.Errorf("connectivity: read webpush integration: %w", err)
	}
	cfg, err := svc.webPushConfig(ctx, scope)
	if err != nil {
		return err
	}
	cfg.Subject = subject
	plaintext, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("connectivity: marshal webpush config: %w", err)
	}
	enc, err := svc.encrypt(plaintext)
	if err != nil {
		return fmt.Errorf("connectivity: encrypt webpush config: %w", err)
	}
	in.EncCredentials = enc
	if err := svc.store.Integrations().Update(ctx, scope, in); err != nil {
		return fmt.Errorf("connectivity: store webpush subject: %w", err)
	}
	return nil
}

// webPushConfig reads and opens the scope's webpush credentials.
func (svc *Service) webPushConfig(ctx context.Context, scope domain.Scope) (WebPushConfig, error) {
	in, err := svc.store.Integrations().GetByProviderID(ctx, scope, webPushProviderID)
	if err != nil {
		return WebPushConfig{}, err
	}
	plaintext, err := svc.decrypt(in.EncCredentials)
	if err != nil {
		return WebPushConfig{}, fmt.Errorf("connectivity: decrypt webpush config: %w", err)
	}
	var cfg WebPushConfig
	if err := json.Unmarshal(plaintext, &cfg); err != nil {
		return WebPushConfig{}, fmt.Errorf("connectivity: unmarshal webpush config: %w", err)
	}
	return cfg, nil
}

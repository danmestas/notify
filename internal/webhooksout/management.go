package webhooksout

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	neturl "net/url"
	"strings"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/secretbox"
	"github.com/danmestas/notify/internal/store"
)

// ManagementService is the CRUD surface for an environment's webhook
// endpoints. It also owns the signing secret's lifecycle: minted or
// caller-supplied on create, sealed at rest, shown once, rotatable.
type ManagementService struct {
	st  *store.Store
	box *secretbox.Box
}

// ErrSigningUnavailable reports a secret operation on a ManagementService built
// without a secretbox -- a wiring fault, not a tenant condition.
var ErrSigningUnavailable = errors.New("webhooksout: signing secrets are not configured")

// NewManagement builds the service. box seals signing secrets; nil disables
// them, which only tests should want -- an endpoint created without a box
// receives unsigned events.
func NewManagement(st *store.Store, box *secretbox.Box) *ManagementService {
	return &ManagementService{st: st, box: box}
}

// Created is the result of Create and RotateSecret: the endpoint plus its
// signing secret in the clear. The secret is not stored in the clear and is
// not shown again; a caller that loses it rotates. Secret is empty when Create
// found an existing endpoint and no secret was supplied.
type Created struct {
	Endpoint domain.WebhookEndpoint
	Secret   string
	// Existed reports that Create matched an endpoint already registered for
	// the URL instead of making a new one.
	Existed bool
}

// Secret bounds. A supplied secret is HMAC key material: long enough to resist
// guessing, and not an obvious placeholder.
const (
	MinSecretLen = 32
	MaxSecretLen = 256
	minDistinct  = 8
)

// ErrWeakSecret reports a caller-supplied signing secret that is too short,
// too long or obviously guessable.
var ErrWeakSecret = errors.New("webhooksout: signing secret is too weak")

func validateSecret(s string) error {
	if len(s) < MinSecretLen {
		return fmt.Errorf("%w: need at least %d characters", ErrWeakSecret, MinSecretLen)
	}
	if len(s) > MaxSecretLen {
		return fmt.Errorf("%w: at most %d characters", ErrWeakSecret, MaxSecretLen)
	}
	distinct := map[rune]struct{}{}
	for _, r := range s {
		distinct[r] = struct{}{}
	}
	if len(distinct) < minDistinct {
		return fmt.Errorf("%w: too few distinct characters", ErrWeakSecret)
	}
	return nil
}

// NormalizeURL trims the URL and lowercases its scheme and host, the parts a
// URL is case-insensitive in. An unparseable URL is returned trimmed only.
func NormalizeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := neturl.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return raw
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return u.String()
}

// Create registers an endpoint for url, idempotently: when the environment
// already has one for the (normalised) URL it is returned with Existed set,
// unchanged and with its secret untouched. A new endpoint, or an existing one
// when secret is supplied, gets its signing secret set to secret, or to 32
// random bytes as hex when secret is empty. Secrets are sealed under the
// master key before they touch the store, and returned in the clear only when
// set by this call.
func (svc *ManagementService) Create(ctx context.Context, scope domain.Scope, url string, active bool, secret string) (Created, error) {
	if secret != "" {
		if err := validateSecret(secret); err != nil {
			return Created{}, err
		}
		if svc.box == nil {
			return Created{}, ErrSigningUnavailable
		}
	}
	url = NormalizeURL(url)
	existing, found, err := svc.findByURL(ctx, scope, url)
	if err != nil {
		return Created{}, err
	}
	if !found {
		plain, sealed, err := svc.seal(secret)
		if err != nil {
			return Created{}, err
		}
		ep, err := svc.st.WebhookEndpoints().Create(ctx, scope, domain.WebhookEndpoint{URL: url, Active: active, Secret: sealed})
		if err == nil {
			return Created{Endpoint: ep, Secret: plain}, nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return Created{}, err
		}
		// Lost a race with a concurrent create of the same URL.
		if existing, found, err = svc.findByURL(ctx, scope, url); err != nil || !found {
			return Created{}, errors.Join(err, store.ErrConflict)
		}
	}
	if secret == "" {
		return Created{Endpoint: existing, Existed: true}, nil
	}
	c, err := svc.RotateSecret(ctx, scope, existing.ID, secret)
	c.Existed = true
	return c, err
}

// findByURL matches url against the environment's endpoints under
// NormalizeURL, so rows stored before normalisation still match. It prefers
// the active row, then the newest.
func (svc *ManagementService) findByURL(ctx context.Context, scope domain.Scope, url string) (domain.WebhookEndpoint, bool, error) {
	eps, err := svc.st.WebhookEndpoints().List(ctx, scope)
	if err != nil {
		return domain.WebhookEndpoint{}, false, err
	}
	var best domain.WebhookEndpoint
	var found bool
	for _, ep := range eps {
		if NormalizeURL(ep.URL) != url {
			continue
		}
		// An active row is the live one; among equals the newest (highest
		// ULID) wins. Deactivated duplicates are left alone.
		if !found || (ep.Active && !best.Active) || (ep.Active == best.Active && ep.ID > best.ID) {
			best, found = ep, true
		}
	}
	return best, found, nil
}

// RotateSecret replaces an existing endpoint's signing secret with secret, or
// with a freshly minted one when secret is empty, and returns it once. Events
// sent after this call are signed with the new secret; there is no overlap
// window, so a consumer rotates by updating its verifier first and then
// calling this.
func (svc *ManagementService) RotateSecret(ctx context.Context, scope domain.Scope, id, secret string) (Created, error) {
	if svc.box == nil {
		return Created{}, ErrSigningUnavailable
	}
	if secret != "" {
		if err := validateSecret(secret); err != nil {
			return Created{}, err
		}
	}
	plain, sealed, err := svc.seal(secret)
	if err != nil {
		return Created{}, err
	}
	ep, err := svc.st.WebhookEndpoints().SetSecret(ctx, scope, id, sealed)
	if err != nil {
		return Created{}, err
	}
	return Created{Endpoint: ep, Secret: plain}, nil
}

// seal returns the secret in the clear and sealed, generating one when plain
// is empty. With no box the sealed form is empty, which the consumer reads as
// "unsigned endpoint".
func (svc *ManagementService) seal(plain string) (string, string, error) {
	if svc.box == nil {
		return "", "", nil
	}
	if plain == "" {
		var raw [32]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", "", fmt.Errorf("webhooksout: mint secret: %w", err)
		}
		plain = hex.EncodeToString(raw[:])
	}
	sealed, err := svc.box.Seal(plain)
	if err != nil {
		return "", "", fmt.Errorf("webhooksout: seal secret: %w", err)
	}
	return plain, sealed, nil
}

func (svc *ManagementService) List(ctx context.Context, scope domain.Scope) ([]domain.WebhookEndpoint, error) {
	return svc.st.WebhookEndpoints().List(ctx, scope)
}

func (svc *ManagementService) GetByID(ctx context.Context, scope domain.Scope, id string) (domain.WebhookEndpoint, error) {
	return svc.st.WebhookEndpoints().GetByID(ctx, scope, id)
}

func (svc *ManagementService) Update(ctx context.Context, scope domain.Scope, id, url string, active *bool) (domain.WebhookEndpoint, error) {
	ep, err := svc.st.WebhookEndpoints().UpdateURL(ctx, scope, id, NormalizeURL(url))
	if err != nil {
		return domain.WebhookEndpoint{}, err
	}
	if active != nil {
		ep, err = svc.st.WebhookEndpoints().SetActive(ctx, scope, id, *active)
		if err != nil {
			return domain.WebhookEndpoint{}, err
		}
	}
	return ep, nil
}

func (svc *ManagementService) Delete(ctx context.Context, scope domain.Scope, id string) error {
	return svc.st.WebhookEndpoints().Delete(ctx, scope, id)
}

// Package streamsource manages inbound webhook-push stream sources: per-source
// push-only secrets (SHA-256 hash lookup + AES-256-GCM at rest), topic scoping,
// and ingest limits. It is the inbound mirror of connectivity's outbound
// catalog, with identity's secret lifecycle.
//
// Request-shaped ingest only: a stream source holds no upstream connection and
// no upstream credentials — there is nothing to dial or probe, so (unlike
// connectivity) this service has no SSRF client and no verify path.
package streamsource

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/timestamp"
)

// secretLen is the number of random bytes in a freshly minted source secret,
// matching identity's key length (32 bytes → 64 hex chars on the wire).
const secretLen = 32

// Ingest-limit defaults (applied at Create; kept in Go, not SQL, so they are
// unit-testable). See the plan's §0.7/§3 retention arithmetic.
const (
	defaultRateLimitPerMin = 600
	defaultMaxEventBytes   = 32 << 10 // 32 KiB
	defaultMaxBatchEvents  = 100
	defaultByteBudget      = 2 << 20 // 2 MiB per budget window
)

// Ingest-limit guardrails: a tenant editing limits in the console cannot
// self-serve past these ceilings (the retention guard depends on them).
const (
	maxRateLimitPerMin = 6000
	maxByteBudget      = 16 << 20 // 16 MiB
)

// maxNameLen bounds a source name.
const maxNameLen = 64

// secretMaskPrefix masks a source secret in console views: it is followed by
// the last 4 hex chars of the secret HASH (never the secret itself), so the
// masked form identifies a source without ever reconstructing its credential.
const secretMaskPrefix = "••••••••••••••••••••••••••••••••••••••••••••••••••••••••••••"

// namePattern is the URL-safe slug allowlist for a source name, matching
// connectivity's identifierPattern: a name is used verbatim as a console URL
// path segment, so it must be lowercase letters/digits/hyphens/underscores
// starting with a letter or digit.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ErrInvalidName is returned by Create when the name fails namePattern or the
// length bound.
var ErrInvalidName = fmt.Errorf("streamsource: name may only contain lowercase letters, numbers, hyphens, and underscores (max 64)")

// ErrInvalidTopic is returned by Create when the topic fails
// domain.ValidateFeedTopic. Callers match on this sentinel via errors.Is
// rather than a string.Contains substring check on the wrapped error text.
var ErrInvalidTopic = fmt.Errorf("streamsource: invalid topic")

// Service mints and authenticates per-source push secrets and manages source
// lifecycle over the backing store.
type Service struct {
	store *store.Store
	gcm   cipher.AEAD
}

// New builds a Service. masterKey must be exactly 32 bytes (AES-256-GCM); New
// panics otherwise — a misconfigured key is a deploy-time programmer error, not
// a runtime condition (identity.New semantics).
func New(s *store.Store, masterKey []byte) *Service {
	if len(masterKey) != 32 {
		panic(fmt.Sprintf("streamsource: masterKey must be 32 bytes for AES-256-GCM, got %d", len(masterKey)))
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		panic(fmt.Sprintf("streamsource: aes.NewCipher: %v", err))
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic(fmt.Sprintf("streamsource: cipher.NewGCM: %v", err))
	}
	return &Service{store: s, gcm: gcm}
}

// Create validates name and topic, applies the limit defaults, mints a fresh
// secret, and persists the source. It returns the stored row and the raw hex
// secret — shown to the caller exactly once (reveal-once).
func (svc *Service) Create(ctx context.Context, scope domain.Scope, name, topic string) (domain.StreamSource, string, error) {
	if !namePattern.MatchString(name) || len(name) > maxNameLen {
		return domain.StreamSource{}, "", ErrInvalidName
	}
	if err := domain.ValidateFeedTopic(topic); err != nil {
		return domain.StreamSource{}, "", fmt.Errorf("%w: %v", ErrInvalidTopic, err)
	}

	raw, enc, hash, err := svc.mint()
	if err != nil {
		return domain.StreamSource{}, "", err
	}

	src := domain.StreamSource{
		Name:            name,
		Topic:           topic,
		SecretHash:      hash,
		EncSecret:       enc,
		RateLimitPerMin: defaultRateLimitPerMin,
		MaxEventBytes:   defaultMaxEventBytes,
		MaxBatchEvents:  defaultMaxBatchEvents,
		ByteBudget:      defaultByteBudget,
		Active:          true,
		CreatedAt:       timestamp.Now(),
	}
	created, err := svc.store.StreamSources().Create(ctx, scope, src)
	if err != nil {
		return domain.StreamSource{}, "", fmt.Errorf("streamsource: create: %w", err)
	}
	return created, raw, nil
}

// List returns the scope's sources (enc_secret omitted by the repo).
func (svc *Service) List(ctx context.Context, scope domain.Scope) ([]domain.StreamSource, error) {
	return svc.store.StreamSources().List(ctx, scope)
}

// Get returns one source by id, scope-checked.
func (svc *Service) Get(ctx context.Context, scope domain.Scope, id string) (domain.StreamSource, error) {
	return svc.store.StreamSources().GetByID(ctx, scope, id)
}

// Authenticate resolves a presented hex secret to its source. It does NOT
// filter on Active: an inactive source still resolves so the caller can reject
// with an attributable 403. Any unknown, malformed, or non-hex-garbage secret
// returns the same store.ErrNotFound (which the edge maps to 401) — uniformly,
// so a caller cannot distinguish "not valid hex" from "valid hex, no match" by
// error type or response shape (no oracle for probing secret format).
func (svc *Service) Authenticate(ctx context.Context, presentedSecret string) (domain.StreamSource, error) {
	raw, err := hex.DecodeString(presentedSecret)
	if err != nil {
		return domain.StreamSource{}, store.ErrNotFound
	}
	return svc.store.StreamSources().GetBySecretHash(ctx, hashSecret(raw))
}

// RotateSecret mints a new secret and swaps the source's hash+ciphertext in a
// single scope-checked UPDATE (atomic on its own — there is never a window with
// no valid secret). Returns the new raw hex, shown once. ErrNotFound when the
// id is absent in the scope.
func (svc *Service) RotateSecret(ctx context.Context, scope domain.Scope, id string) (string, error) {
	raw, enc, hash, err := svc.mint()
	if err != nil {
		return "", err
	}
	writer, _ := svc.store.Pools()
	res, err := writer.ExecContext(
		ctx,
		`UPDATE stream_sources SET secret_hash = ?, enc_secret = ?
		 WHERE id = ? AND org_id = ? AND environment_id = ?`,
		hash, enc, id, scope.OrgID, scope.EnvID,
	)
	if err != nil {
		return "", fmt.Errorf("streamsource: rotate: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("streamsource: rotate rows affected: %w", err)
	}
	if n == 0 {
		return "", store.ErrNotFound
	}
	return raw, nil
}

// SetActive flips the source's active flag, scope-checked.
func (svc *Service) SetActive(ctx context.Context, scope domain.Scope, id string, active bool) error {
	return svc.store.StreamSources().SetActive(ctx, scope, id, active)
}

// Delete removes the source permanently, scope-checked.
func (svc *Service) Delete(ctx context.Context, scope domain.Scope, id string) error {
	return svc.store.StreamSources().Delete(ctx, scope, id)
}

// TouchActivity records best-effort ingest activity (last event time and last
// rejection reason) on a source by id. It is the fire-and-forget hot-path write
// the ingest handler makes after publishing (or after rejecting) a batch: the
// id already came from an authenticated source row, so the write is unscoped,
// and the underlying repo error is intentionally swallowed — this method has no
// return because a failed activity write must never block or fail an ingest
// response (feedingest.SourceToucher relies on that contract).
func (svc *Service) TouchActivity(ctx context.Context, id, lastEventAt, lastError string) {
	_ = svc.store.StreamSources().TouchActivity(ctx, id, lastEventAt, lastError)
}

// UpdateLimits clamps the four ingest limits into their guardrail range and
// persists them, scope-checked. Non-positive values fall back to the default
// for that field; rate and byte budget are ceilinged so a tenant cannot edit
// past the retention guard.
func (svc *Service) UpdateLimits(ctx context.Context, scope domain.Scope, id string, rateLimitPerMin, maxEventBytes, maxBatchEvents int, byteBudget int64) error {
	src, err := svc.store.StreamSources().GetByID(ctx, scope, id)
	if err != nil {
		return err
	}
	src.RateLimitPerMin = clampInt(rateLimitPerMin, defaultRateLimitPerMin, maxRateLimitPerMin)
	src.MaxEventBytes = clampInt(maxEventBytes, defaultMaxEventBytes, 0)
	src.MaxBatchEvents = clampInt(maxBatchEvents, defaultMaxBatchEvents, 0)
	src.ByteBudget = clampInt64(byteBudget, defaultByteBudget, maxByteBudget)
	return svc.store.StreamSources().Update(ctx, scope, src)
}

// MaskedSecret returns a display-safe identifier for a source's secret: the
// mask sentinel plus the last 4 hex chars of the secret HASH. The raw secret is
// revealed only at mint/rotate and is not reconstructable from this value.
func (svc *Service) MaskedSecret(src domain.StreamSource) string {
	h := src.SecretHash
	if len(h) < 4 {
		return secretMaskPrefix
	}
	return secretMaskPrefix + h[len(h)-4:]
}

// mint generates a fresh secret and returns (rawHex, ciphertext, hexHash).
func (svc *Service) mint() (raw string, enc []byte, hash string, err error) {
	b := make([]byte, secretLen)
	if _, err := rand.Read(b); err != nil {
		return "", nil, "", fmt.Errorf("streamsource: mint secret: %w", err)
	}
	enc, err = svc.encrypt(b)
	if err != nil {
		return "", nil, "", fmt.Errorf("streamsource: encrypt secret: %w", err)
	}
	return hex.EncodeToString(b), enc, hashSecret(b), nil
}

// encrypt seals plaintext with AES-256-GCM, prepending the random nonce.
func (svc *Service) encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, svc.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return svc.gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// hashSecret returns the hex SHA-256 of the raw secret bytes — the stream
// source lookup key (identity.hashKey semantics).
func hashSecret(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// clampInt returns v floored to def when non-positive and ceilinged to max when
// max > 0.
func clampInt(v, def, max int) int {
	if v <= 0 {
		v = def
	}
	if max > 0 && v > max {
		v = max
	}
	return v
}

// clampInt64 is clampInt for int64 limits.
func clampInt64(v, def, max int64) int64 {
	if v <= 0 {
		v = def
	}
	if max > 0 && v > max {
		v = max
	}
	return v
}

// Package phoneverify lets a subscriber prove they hold a phone number (#130).
// Request texts a short code to the number through the environment's SMS
// integration; Confirm with that code marks the number verified on the
// subscriber. SMS steps send only to a verified number, or to one an operator
// attested consent for (consent.go), and nothing else can verify one: the
// sender API stores a phone unverified, and a write that changes it clears
// the verification (store.SubscriberRepo.Upsert).
//
// Only a salted hash of each code is stored. A code expires after CodeTTL and
// allows MaxAttempts guesses. Sends are limited to MaxSends per SendWindow for
// each subscriber and, across subscribers, for each number, so the flow cannot
// be used to flood a phone. Daily caps sit on top: MaxSendsPerPhoneDay and
// MaxFailedConfirmsPerPhoneDay per number, and MaxSendsPerEnvDay per
// environment, so neither texting nor guessing is unbounded over time.
package phoneverify

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/phonenum"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/timestamp"
)

const (
	// CodeTTL is how long a code can be confirmed after it is sent.
	CodeTTL = 10 * time.Minute
	// MaxAttempts is how many guesses one code allows.
	MaxAttempts = 5
	// MaxSends is how many codes a subscriber, or a number, can be sent per
	// SendWindow.
	MaxSends = 5
	// SendWindow is the period MaxSends counts over.
	SendWindow = time.Hour
	// MaxSendsPerPhoneDay caps codes sent to one number in a day.
	MaxSendsPerPhoneDay = 10
	// MaxFailedConfirmsPerPhoneDay caps wrong guesses against one number in
	// a day, across all its codes: two codes' worth of MaxAttempts.
	MaxFailedConfirmsPerPhoneDay = 10
	// MaxSendsPerEnvDay caps codes one environment sends in a day.
	MaxSendsPerEnvDay = 100
	// day is the period the daily caps count over.
	day = 24 * time.Hour

	codeDigits = 6
	// keepFor is how long sent-code rows are kept: the longest window.
	keepFor = day
)

var (
	ErrInvalidPhone      = errors.New("phoneverify: not an E.164 phone number")
	ErrUnknownSubscriber = errors.New("phoneverify: subscriber not found")
	ErrRateLimited       = errors.New("phoneverify: too many codes sent; try again later")
	ErrSendFailed        = errors.New("phoneverify: the code could not be sent")
	ErrNoPendingCode     = errors.New("phoneverify: no code is waiting to be confirmed")
	ErrCodeExpired       = errors.New("phoneverify: the code has expired")
	ErrWrongCode         = errors.New("phoneverify: the code is wrong")
	ErrTooManyAttempts   = errors.New("phoneverify: too many wrong codes; request a new one")
	// ErrDailyLimit: the number has had its codes, or its wrong guesses, for
	// the day.
	ErrDailyLimit = errors.New("phoneverify: this number has reached its daily limit; try again tomorrow")
	// ErrEnvironmentDailyLimit: the environment has sent its codes for the
	// day.
	ErrEnvironmentDailyLimit = errors.New("phoneverify: this environment has sent its verification codes for today; try again tomorrow")
)

// SMSResolver resolves the environment's SMS provider, its provider id and
// integration id. connectivity.Service satisfies it.
type SMSResolver interface {
	ResolveSMS(ctx context.Context, scope domain.Scope) (providers.SMSProvider, string, string, error)
}

// Pending is a code sent and waiting for Confirm.
type Pending struct {
	Phone     string
	ExpiresAt time.Time
}

// Verified is the phone Confirm marked verified.
type Verified struct {
	Phone      string
	VerifiedAt time.Time
}

// Service runs phone verification over the store and an SMS resolver.
type Service struct {
	store *store.Store
	sms   SMSResolver
	now   func() time.Time
}

// New builds a Service.
func New(st *store.Store, sms SMSResolver) *Service {
	return &Service{store: st, sms: sms, now: time.Now}
}

// WithClock replaces the clock (tests) and returns s.
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// Normalize returns number in E.164 form (see phonenum.Normalize), or
// ErrInvalidPhone.
func Normalize(number string) (string, error) {
	n, ok := phonenum.Normalize(number)
	if !ok {
		return "", ErrInvalidPhone
	}
	return n, nil
}

// Request sends a new code to number for the subscriber. The newest code is
// the only one Confirm accepts. A send the provider refuses is recorded like
// any other delivery, so it counts toward the integration's health, and
// returns ErrSendFailed.
func (s *Service) Request(ctx context.Context, scope domain.Scope, subscriberID, number string) (Pending, error) {
	phone, err := Normalize(number)
	if err != nil {
		return Pending{}, err
	}
	if err := s.requireSubscriber(ctx, scope, subscriberID); err != nil {
		return Pending{}, err
	}

	now := s.now().UTC()
	repo := s.store.PhoneVerifications()
	if err := repo.PruneBefore(ctx, scope, timestamp.Format(now.Add(-keepFor))); err != nil {
		return Pending{}, err
	}
	u, err := s.usage(ctx, scope, subscriberID, phone, now)
	if err != nil {
		return Pending{}, err
	}
	switch {
	case u.EnvDay >= MaxSendsPerEnvDay:
		return Pending{}, ErrEnvironmentDailyLimit
	case u.PhoneDay >= MaxSendsPerPhoneDay:
		return Pending{}, ErrDailyLimit
	case u.SubscriberRecent >= MaxSends || u.PhoneRecent >= MaxSends:
		return Pending{}, ErrRateLimited
	}

	provider, providerID, integrationID, err := s.sms.ResolveSMS(ctx, scope)
	if err != nil {
		return Pending{}, fmt.Errorf("%w: %w", ErrSendFailed, err)
	}

	code, err := newCode()
	if err != nil {
		return Pending{}, err
	}
	id := store.NewULID()
	expires := now.Add(CodeTTL)
	if err := repo.Create(ctx, scope, store.PhoneVerification{
		ID: id, SubscriberID: subscriberID, Phone: phone, CodeHash: hashCode(id, code),
		ExpiresAt: timestamp.Format(expires), CreatedAt: timestamp.Format(now),
	}); err != nil {
		return Pending{}, err
	}

	body := fmt.Sprintf("Your verification code is %s. It expires in %d minutes.", code, int(CodeTTL/time.Minute))
	_, sendErr := provider.Send(ctx, providers.SMSMessage{To: phone, Body: body})
	s.record(ctx, scope, subscriberID, providerID, integrationID, phone, now, sendErr)
	if sendErr != nil {
		// A provider may echo the recipient; the number goes no further.
		return Pending{}, fmt.Errorf("%w: %s", ErrSendFailed, redactPhone(sendErr.Error(), phone))
	}
	return Pending{Phone: phone, ExpiresAt: expires}, nil
}

// Confirm checks code against the subscriber's newest code and, when it
// matches, sets the number that code was sent to as the subscriber's verified
// phone. That number wins over any phone written since the Request: the
// subscriber has just proved they hold it.
func (s *Service) Confirm(ctx context.Context, scope domain.Scope, subscriberID, code string) (Verified, error) {
	repo := s.store.PhoneVerifications()
	v, err := repo.Latest(ctx, scope, subscriberID)
	if errors.Is(err, store.ErrNotFound) {
		return Verified{}, ErrNoPendingCode
	}
	if err != nil {
		return Verified{}, err
	}
	if v.Consumed {
		return Verified{}, ErrNoPendingCode
	}
	now := s.now().UTC()
	expires, err := timestamp.Parse(v.ExpiresAt)
	if err != nil {
		return Verified{}, fmt.Errorf("phoneverify: stored expiry: %w", err)
	}
	if !now.Before(expires) {
		return Verified{}, ErrCodeExpired
	}
	u, err := s.usage(ctx, scope, subscriberID, v.Phone, now)
	if err != nil {
		return Verified{}, err
	}
	if u.PhoneFailedDay >= MaxFailedConfirmsPerPhoneDay {
		return Verified{}, ErrDailyLimit
	}
	// The attempt is spent before the comparison, atomically, so concurrent
	// guesses cannot exceed MaxAttempts between them.
	ok, err := repo.ClaimAttempt(ctx, scope, v.ID, MaxAttempts)
	if err != nil {
		return Verified{}, err
	}
	if !ok {
		return Verified{}, ErrTooManyAttempts
	}
	if subtle.ConstantTimeCompare([]byte(hashCode(v.ID, strings.TrimSpace(code))), []byte(v.CodeHash)) != 1 {
		return Verified{}, ErrWrongCode
	}
	if ok, err := repo.Consume(ctx, scope, v.ID); err != nil {
		return Verified{}, err
	} else if !ok {
		return Verified{}, ErrNoPendingCode
	}
	if err := s.store.Subscribers().SetVerifiedPhone(ctx, scope, subscriberID, v.Phone, timestamp.Format(now)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Verified{}, ErrUnknownSubscriber
		}
		return Verified{}, err
	}
	return Verified{Phone: v.Phone, VerifiedAt: now}, nil
}

func (s *Service) usage(ctx context.Context, scope domain.Scope, subscriberID, phone string, now time.Time) (store.PhoneVerificationUsage, error) {
	return s.store.PhoneVerifications().Usage(ctx, scope, subscriberID, phone,
		timestamp.Format(now.Add(-SendWindow)), timestamp.Format(now.Add(-day)))
}

func (s *Service) requireSubscriber(ctx context.Context, scope domain.Scope, subscriberID string) error {
	sub, err := s.store.Subscribers().GetByExternalID(ctx, scope, subscriberID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && sub.Deleted) {
		return ErrUnknownSubscriber
	}
	return err
}

// record writes the send as an sms message, the path every delivery outcome
// takes to reach delivery health. The code is never part of it.
func (s *Service) record(ctx context.Context, scope domain.Scope, subscriberID, providerID, integrationID, phone string, at time.Time, sendErr error) {
	m := domain.Message{
		SubscriberID:  subscriberID,
		Channel:       "sms",
		ProviderID:    providerID,
		IntegrationID: integrationID,
		Subject:       "Phone verification",
		Content:       "Phone verification code (not stored)",
		Status:        "sent",
		CreatedAt:     at.Format(time.RFC3339),
	}
	if sendErr != nil {
		m.Status = "error"
		m.ErrorText = redactPhone(sendErr.Error(), phone)
		m.ErrorClass = string(providers.ClassifyError(sendErr))
	}
	// Best effort: the send already happened, and failing the request over
	// the record would invite a second code.
	_, _ = s.store.Messages().Create(ctx, scope, m)
}

// redactPhone removes phone (E.164, and its digits without the '+') from a
// provider's error text.
func redactPhone(text, phone string) string {
	text = strings.ReplaceAll(text, phone, "[phone]")
	return strings.ReplaceAll(text, strings.TrimPrefix(phone, "+"), "[phone]")
}

func newCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", fmt.Errorf("phoneverify: generate code: %w", err)
	}
	return fmt.Sprintf("%0*d", codeDigits, n.Int64()), nil
}

// hashCode salts the code with its row id, so equal codes never share a hash.
func hashCode(id, code string) string {
	sum := sha256.Sum256([]byte(id + ":" + code))
	return hex.EncodeToString(sum[:])
}

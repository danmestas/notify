package providers

import (
	"context"
	"errors"
	"io"
	"net"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
)

// ErrorClass names why a delivery failed, in terms an operator can act on.
// Every failed message records one next to its error text (#100), and the
// integration health tracker counts every class but ClassRecipient and ClassNotConfigured against the
// integration that sent it.
type ErrorClass string

const (
	// ClassAuth: the provider refused the integration's credentials (HTTP 401
	// or 403, SMTP 535, a revoked token, a missing credential).
	ClassAuth ErrorClass = "auth"
	// ClassRateLimited: the provider asked us to slow down (HTTP 429).
	ClassRateLimited ErrorClass = "rate_limited"
	// ClassProviderDown: the provider failed or could not be reached (5xx,
	// timeouts, connection errors, temporary SMTP replies).
	ClassProviderDown ErrorClass = "provider_down"
	// ClassRejectedMessage: the provider refused this particular message
	// (any other 4xx). Also the class of a failure with no provider signal.
	ClassRejectedMessage ErrorClass = "rejected_message"
	// ClassRecipient: the address is bad, unknown or gone. It says nothing
	// about the integration, so it never counts against its health.
	ClassRecipient ErrorClass = "recipient"
	// ClassNotConfigured: the scope has no integration for the channel, so
	// nothing was ever called. It is a setup fault, not a send failure, and
	// there is no integration to count it against.
	ClassNotConfigured ErrorClass = "not_configured"
)

// NotConfiguredError says the scope has no active integration for Channel.
// ClassifyError reads its class off the type, not its text.
type NotConfiguredError struct {
	Channel string
	Err     error
}

func (e *NotConfiguredError) Error() string {
	return "no active " + e.Channel + " integration is configured"
}

func (e *NotConfiguredError) Unwrap() error { return e.Err }

// ErrorClass implements the typed-error hook classifyTyped reads.
func (e *NotConfiguredError) ErrorClass() ErrorClass { return ClassNotConfigured }

// ClassifyError maps a send error from any provider to its ErrorClass. It is
// the one place provider failures are interpreted: every channel's delivery
// path records ClassifyError(err) beside the error text. A nil error has no
// class ("").
//
// Providers report failures in a few shapes, tried in order:
//   - a recipient hint in the provider's own words (a blocked bot, an
//     unregistered device), which beats the HTTP status it came with;
//   - a typed error: a gone Web Push subscription, an SMTP reply code;
//   - a transport failure: a timeout, a refused or dropped connection;
//   - the "status NNN" every HTTP provider in this tree puts in its error;
//   - a credential hint (a missing or rejected key), or a sendmail exit code;
//   - a TransientError with no status, which is a provider outage.
//
// Anything else is ClassRejectedMessage: the message failed for a reason the
// provider did not explain.
func ClassifyError(err error) ErrorClass {
	if err == nil {
		return ""
	}
	var nc *NotConfiguredError
	if errors.As(err, &nc) {
		return ClassNotConfigured
	}
	text := err.Error()
	if isRecipient(text) {
		return ClassRecipient
	}
	if c, ok := classifyTyped(err); ok {
		return c
	}
	if c, ok := classifyText(text); ok {
		return c
	}
	if IsTransient(err) {
		return ClassProviderDown
	}
	return ClassRejectedMessage
}

// ClassifyText classifies a failure known only by its message, such as a
// credential probe's detail line. It applies the text rules of ClassifyError.
func ClassifyText(text string) ErrorClass {
	if text == "" {
		return ""
	}
	if isRecipient(text) {
		return ClassRecipient
	}
	if c, ok := classifyText(text); ok {
		return c
	}
	return ClassRejectedMessage
}

// recipientCodeRe finds provider error codes that blame the recipient in a
// JSON body: Twilio 21211 (invalid To), 21610 (unsubscribed), 21614 (not a
// mobile number); WhatsApp 131026 (undeliverable), 131030 (not allowed).
var recipientCodeRe = regexp.MustCompile(`"code":\s*(21211|21610|21614|131026|131030)\b`)

func isRecipient(text string) bool {
	return hasHint(text, recipientHints) || recipientCodeRe.MatchString(text)
}

// classifyTyped reads the class off typed errors in err's chain.
func classifyTyped(err error) (ErrorClass, bool) {
	var classed interface{ ErrorClass() ErrorClass }
	if errors.As(err, &classed) {
		return classed.ErrorClass(), true
	}
	var smtpErr *textproto.Error
	if errors.As(err, &smtpErr) {
		return smtpClass(smtpErr.Code), true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ClassProviderDown, true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return ClassProviderDown, true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return ClassProviderDown, true
	}
	return "", false
}

// smtpClass maps an SMTP reply code (RFC 5321 section 4.2.3).
func smtpClass(code int) ErrorClass {
	switch {
	case code == 530 || code == 534 || code == 535 || code == 538:
		return ClassAuth
	case code == 550 || code == 551 || code == 553:
		return ClassRecipient
	case code >= 400 && code < 500:
		return ClassProviderDown
	default:
		return ClassRejectedMessage
	}
}

// statusRe finds the HTTP status every provider in this tree writes as
// "status NNN" (internal/providers/*, the connectivity probes).
var statusRe = regexp.MustCompile(`\bstatus (\d{3})\b`)

// sendmailExitRe finds a sendmail exit code; the values are sysexits.h.
var sendmailExitRe = regexp.MustCompile(`\bexited (\d+)\b`)

// classifyText applies the message-text rules.
func classifyText(text string) (ErrorClass, bool) {
	if hasHint(text, authHints) {
		return ClassAuth, true
	}
	if hasHint(text, rateHints) {
		return ClassRateLimited, true
	}
	if m := statusRe.FindStringSubmatch(text); m != nil {
		code, _ := strconv.Atoi(m[1])
		return httpClass(code), true
	}
	if m := sendmailExitRe.FindStringSubmatch(text); m != nil {
		code, _ := strconv.Atoi(m[1])
		return sendmailClass(code), true
	}
	if hasHint(text, downHints) {
		return ClassProviderDown, true
	}
	return "", false
}

// httpClass maps an HTTP status from a provider response.
func httpClass(code int) ErrorClass {
	switch {
	case code == 401 || code == 403:
		return ClassAuth
	case code == 429:
		return ClassRateLimited
	case code >= 500:
		return ClassProviderDown
	default:
		return ClassRejectedMessage
	}
}

// sendmailClass maps a sendmail exit status (sysexits.h).
func sendmailClass(code int) ErrorClass {
	switch code {
	case 67, 68: // EX_NOUSER, EX_NOHOST
		return ClassRecipient
	case 77: // EX_NOPERM
		return ClassAuth
	case 69, 71, 74, 75: // EX_UNAVAILABLE, EX_OSERR, EX_IOERR, EX_TEMPFAIL
		return ClassProviderDown
	default:
		return ClassRejectedMessage
	}
}

// recipientHints are provider phrases that blame the address, matched
// case-insensitively. They win over the status they arrive with: Telegram
// answers a bot the user blocked with 403, which is not a credential fault.
var recipientHints = []string{
	"empty recipient", "empty device token", "empty chat_id",
	// The subscriber has no address for the channel (internal/delivery).
	"has no phone number", "has no chat_id", "has no topic for provider", "subscriber channel for",
	"chat not found", "bot was blocked by the user", "user is deactivated", // Telegram
	"channel_not_found", "not_in_channel", "is_archived", "user_not_found", // Slack
	"devicenotregistered", "not a registered push notification recipient", // Expo
	"unregistered",                                                                 // FCM
	"is not a valid phone number", "not a mobile number", "unsubscribed recipient", // Twilio
	"recipient phone number not in allowed list", "message undeliverable", // WhatsApp
	"invalid `to`", "invalid to address", "invalid recipient", // Resend and others
	"subscription gone",
}

// authHints are phrases that mean the credential itself is missing or
// refused, matched case-insensitively.
var authHints = []string{
	"credentials rejected", "authentication rejected", "authentication error",
	"missing credentials", "missing api key", "missing bot token", "missing access token",
	"empty bot token", "empty access token", "empty app password", "empty identifier",
	"empty webhook url", "unknown webhook", "invalid_auth", "not_authed", "token_revoked",
	"token_expired", "account_inactive", "no token source configured",
	"api token not configured", "access token not configured", "app password not configured",
	"no server address configured", "no server base url configured", "no command configured",
	"vapid keys are missing", "private key does not match", "unencrypted connection",
}

var rateHints = []string{"ratelimited", "rate limited", "too many requests"}

var downHints = []string{
	"did not finish within", "connection refused", "connection reset", "no such host",
	"i/o timeout", "timeout", "unexpected eof", ": eof", "tls handshake",
}

func hasHint(text string, hints []string) bool {
	lower := strings.ToLower(text)
	for _, h := range hints {
		if strings.Contains(lower, h) {
			return true
		}
	}
	return false
}

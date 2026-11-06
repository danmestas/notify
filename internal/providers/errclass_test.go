package providers_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"

	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/providers/cloudflareemail"
	"github.com/danmestas/notify/internal/providers/resend"
	"github.com/danmestas/notify/internal/providers/telegram"
	"github.com/danmestas/notify/internal/providers/twilio"
	"github.com/danmestas/notify/internal/providers/webpush"
)

const (
	auth      = providers.ClassAuth
	rate      = providers.ClassRateLimited
	down      = providers.ClassProviderDown
	rejected  = providers.ClassRejectedMessage
	recipient = providers.ClassRecipient
)

// TestClassifyError_ProviderShapes lists each provider's failure in the exact
// shape its client returns it (internal/providers/*).
func TestClassifyError_ProviderShapes(t *testing.T) {
	transient := providers.NewTransientError
	cases := []struct {
		name string
		err  error
		want providers.ErrorClass
	}{
		{"nil", nil, ""},

		// Cloudflare Email
		{"cloudflare 401", fmt.Errorf("cloudflare email send: status 401: %s", `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`), auth},
		{"cloudflare 403", fmt.Errorf("cloudflare email send: status 403: forbidden"), auth},
		{"cloudflare 429", transient(fmt.Errorf("cloudflare email send: status 429: slow down")), rate},
		{"cloudflare 503", transient(fmt.Errorf("cloudflare email send: status 503: unavailable")), down},
		{"cloudflare 400", fmt.Errorf("cloudflare email send: status 400: invalid html"), rejected},
		{"cloudflare transport", transient(fmt.Errorf("cloudflare email send: %w", &net.OpError{Op: "dial", Err: errors.New("connection refused")})), down},
		{"cloudflare api error", fmt.Errorf("cloudflareemail: api error: bad sender"), rejected},

		// Resend
		{"resend missing key", fmt.Errorf("resend: missing api key"), auth},
		{"resend empty recipient", fmt.Errorf("resend: empty recipient"), recipient},
		{"resend 401", fmt.Errorf("resend: status 401: %s", `{"name":"missing_api_key"}`), auth},
		{"resend 422 to", fmt.Errorf("resend: status 422: %s", "{\"name\":\"validation_error\",\"message\":\"Invalid `to` field.\"}"), recipient},
		{"resend 422 other", fmt.Errorf("resend: status 422: %s", `{"name":"validation_error","message":"subject is required"}`), rejected},
		{"resend 429", fmt.Errorf("resend: status 429: rate limit"), rate},
		{"resend 500", transient(fmt.Errorf("resend: status 500: oops")), down},

		// SES
		{"ses missing credentials", fmt.Errorf("ses: missing credentials"), auth},
		{"ses 403", fmt.Errorf("ses: status 403: %s", `{"message":"The security token included in the request is invalid."}`), auth},
		{"ses 400", fmt.Errorf("ses: status 400: %s", `{"message":"Email address is not verified."}`), rejected},
		{"ses 503", transient(fmt.Errorf("ses: status 503: throttled")), down},

		// SMTP: smtp send wraps the server's reply.
		{"smtp 535", fmt.Errorf("smtp send: %w", &textproto.Error{Code: 535, Msg: "5.7.8 Username and Password not accepted"}), auth},
		{"smtp 550", fmt.Errorf("smtp send: %w", &textproto.Error{Code: 550, Msg: "5.1.1 user unknown"}), recipient},
		{"smtp 553", fmt.Errorf("smtp send: %w", &textproto.Error{Code: 553, Msg: "mailbox name not allowed"}), recipient},
		{"smtp 554", fmt.Errorf("smtp send: %w", &textproto.Error{Code: 554, Msg: "message rejected as spam"}), rejected},
		{"smtp 421", fmt.Errorf("smtp send: %w", &textproto.Error{Code: 421, Msg: "service not available"}), down},
		{"smtp 451", fmt.Errorf("smtp send: %w", &textproto.Error{Code: 451, Msg: "try again later"}), down},
		{"smtp dial", fmt.Errorf("smtp send: %w", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}), down},
		{"smtp deadline", fmt.Errorf("smtp send: %w", context.DeadlineExceeded), down},
		{"smtp plain auth", fmt.Errorf("smtp send: %w", errors.New("unencrypted connection")), auth},

		// sendmail
		{"sendmail no user", fmt.Errorf("sendmail: /usr/sbin/sendmail exited 67: user unknown"), recipient},
		{"sendmail tempfail", fmt.Errorf("sendmail: /usr/sbin/sendmail exited 75: deferred"), down},
		{"sendmail timeout", transient(fmt.Errorf("sendmail: /usr/sbin/sendmail did not finish within 30s")), down},

		// Twilio and SignalWire (SMS)
		{"twilio 401", fmt.Errorf("twilio: status 401: %s", `{"code":20003,"message":"Authenticate"}`), auth},
		{"twilio invalid to", fmt.Errorf("twilio: status 400: %s", `{"code": 21211, "message":"The 'To' number +1555 is not a valid phone number."}`), recipient},
		{"twilio unsubscribed", fmt.Errorf("twilio: status 400: %s", `{"code":21610,"message":"Attempt to send to unsubscribed recipient"}`), recipient},
		{"twilio 400 other", fmt.Errorf("twilio: status 400: %s", `{"code":21602,"message":"Message body is required."}`), rejected},
		{"twilio 429", fmt.Errorf("twilio: status 429: %s", `{"code":20429,"message":"Too Many Requests"}`), rate},
		{"twilio 500", transient(fmt.Errorf("twilio: status 500: oops")), down},
		{"signalwire 401", fmt.Errorf("signalwire: status 401: unauthorized"), auth},
		{"sns 403", fmt.Errorf("sns: status 403: %s", `<Code>InvalidClientTokenId</Code>`), auth},
		{"sms no phone", fmt.Errorf("sms: subscriber %q has no phone number", "sub-1"), recipient},

		// Push
		{"expo not registered", fmt.Errorf("expo: %s", `"ExponentPushToken[x]" is not a registered push notification recipient`), recipient},
		{"expo 401", fmt.Errorf("expo: https://exp.host/--/api/v2/push/send: status 401: unauthorized"), auth},
		{"fcm unregistered", fmt.Errorf("fcm: https://fcm.googleapis.com/v1/x: status 404: %s", `{"error":{"status":"NOT_FOUND","details":[{"errorCode":"UNREGISTERED"}]}}`), recipient},
		{"fcm 403", fmt.Errorf("fcm: https://fcm.googleapis.com/v1/x: status 403: SENDER_ID_MISMATCH"), auth},
		{"fcm token mint", transient(fmt.Errorf("fcm: mint access token: %w", errors.New("oauth2: server unreachable"))), down},
		{"ntfy 403", fmt.Errorf("ntfy: https://ntfy.sh/topic: status 403: forbidden"), auth},
		{"ntfy 507", transient(fmt.Errorf("ntfy: https://ntfy.sh/topic: status 507: full")), down},
		{"push no topic", fmt.Errorf("push: subscriber %q has no topic for provider %q", "s", "ntfy"), recipient},
		{"webpush gone", webpush.ErrSubscriptionGone, recipient},
		{"webpush 403", &webpush.StatusError{Code: 403, Reason: "BadJwtToken"}, auth},
		{"webpush 413", &webpush.StatusError{Code: 413, Reason: "PayloadTooLarge"}, rejected},
		{"webpush tally", fmt.Errorf("webpush: 0/2 subscriptions delivered: %w", &webpush.StatusError{Code: 400, Reason: "BadDeviceToken"}), rejected},

		// Chat
		{"slack invalid auth", fmt.Errorf("slack: api error: invalid_auth"), auth},
		{"slack revoked", fmt.Errorf("slack: api error: token_revoked"), auth},
		{"slack channel", fmt.Errorf("slack: api error: channel_not_found"), recipient},
		{"slack ratelimited", transient(fmt.Errorf("slack: api error: ratelimited")), rate},
		{"slack 500", transient(fmt.Errorf("slack: status 500: oops")), down},
		{"telegram 401", fmt.Errorf("telegram: status 401: %s", `{"ok":false,"error_code":401,"description":"Unauthorized"}`), auth},
		{"telegram blocked", fmt.Errorf("telegram: status 403: %s", `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`), recipient},
		{"telegram chat", fmt.Errorf("telegram: status 400: %s", `{"ok":false,"description":"Bad Request: chat not found"}`), recipient},
		{"telegram 429", fmt.Errorf("telegram: status 429: %s", `{"ok":false,"description":"Too Many Requests: retry after 5"}`), rate},
		{"discord unknown webhook", fmt.Errorf("discord: status 404: %s", `{"message": "Unknown Webhook", "code": 10015}`), auth},
		{"discord 400", fmt.Errorf("discord: status 400: %s", `{"content":["Must be 2000 or fewer in length."]}`), rejected},
		{"discord transport", transient(fmt.Errorf("discord: post webhook: %w", context.DeadlineExceeded)), down},
		{"whatsapp token", fmt.Errorf("whatsapp: status 401: %s", `{"error":{"code":190,"message":"Error validating access token"}}`), auth},
		{"whatsapp undeliverable", fmt.Errorf("whatsapp: status 400: %s", `{"error":{"code":131026,"message":"Message undeliverable"}}`), recipient},
		{"mastodon 422", fmt.Errorf("mastodon: status 422: %s", `{"error":"Validation failed: Text character limit of 500 exceeded"}`), rejected},
		{"bluesky 401", fmt.Errorf("bluesky: com.atproto.server.createSession: status 401: %s", `{"error":"AuthenticationRequired"}`), auth},
		{"chat webhook 502", transient(fmt.Errorf("chat-webhook: status 502: bad gateway")), down},
		{"chat no chat id", fmt.Errorf("chat: subscriber %q has no chat_id for provider %q", "s", "telegram"), recipient},

		// Anything unexplained is the message's own failure.
		{"unexplained", errors.New("template: render failed"), rejected},
		{"transient without status", transient(errors.New("upstream reset")), down},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := providers.ClassifyError(tc.err); got != tc.want {
				t.Fatalf("ClassifyError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyText_ProbeDetails covers the credential probes' detail lines
// (internal/connectivity/verify.go), which is how a check is classified.
func TestClassifyText_ProbeDetails(t *testing.T) {
	cases := []struct {
		detail string
		want   providers.ErrorClass
	}{
		{"", ""},
		{"cloudflare-email: credentials rejected (status 401)", auth},
		{"cloudflare-email: account id or api token not configured", auth},
		{"smtp: authentication rejected by mx.example.com: 535 5.7.8 bad credentials", auth},
		{"mastodon: credentials rejected (status 403)", auth},
		{"bluesky: identifier or app password not configured", auth},
		{"webpush: private key does not match public key", auth},
		{"smtp: dial mx:587 failed: dial tcp 1.2.3.4:587: connect: connection refused", down},
		{"cloudflare-email: probe https://api.cloudflare.com/client/v4/accounts/x failed: context deadline exceeded (Client.Timeout exceeded while awaiting headers)", down},
		{"ntfy: unexpected status 502: bad gateway", down},
		{"mastodon: unexpected status 404: not found", rejected},
	}
	for _, tc := range cases {
		if got := providers.ClassifyText(tc.detail); got != tc.want {
			t.Errorf("ClassifyText(%q) = %q, want %q", tc.detail, got, tc.want)
		}
	}
}

// TestClassifyError_RealClients drives provider clients against a stub
// server, so the classes follow what the clients actually return.
func TestClassifyError_RealClients(t *testing.T) {
	respond := func(status int, body string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	ctx := context.Background()
	email := providers.EmailMessage{From: "a@example.com", To: "b@example.com", Subject: "s", Text: "t"}
	cases := []struct {
		name string
		send func() error
		want providers.ErrorClass
	}{
		{"cloudflare 401", func() error {
			srv := respond(401, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`)
			_, err := (&cloudflareemail.Client{AccountID: "acct", APIToken: "tok", BaseURL: srv.URL}).Send(ctx, email)
			return err
		}, auth},
		{"cloudflare 429", func() error {
			srv := respond(429, `{}`)
			_, err := (&cloudflareemail.Client{AccountID: "acct", APIToken: "tok", BaseURL: srv.URL}).Send(ctx, email)
			return err
		}, rate},
		{"cloudflare 500", func() error {
			srv := respond(500, `{}`)
			_, err := (&cloudflareemail.Client{AccountID: "acct", APIToken: "tok", BaseURL: srv.URL}).Send(ctx, email)
			return err
		}, down},
		{"resend 403", func() error {
			srv := respond(403, `{"name":"invalid_api_key"}`)
			_, err := (&resend.Client{APIKey: "k", BaseURL: srv.URL}).Send(ctx, email)
			return err
		}, auth},
		{"twilio invalid number", func() error {
			srv := respond(400, `{"code":21211,"message":"The 'To' number is not a valid phone number."}`)
			_, err := (&twilio.Client{AccountSID: "AC1", AuthToken: "t", From: "+1", BaseURL: srv.URL}).Send(ctx, providers.SMSMessage{To: "+2", Body: "hi"})
			return err
		}, recipient},
		{"telegram blocked", func() error {
			srv := respond(403, `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`)
			_, err := (&telegram.Client{BotToken: "123:abc", BaseURL: srv.URL}).Send(ctx, providers.ChatMessage{Address: "42", Content: "hi"})
			return err
		}, recipient},
		{"telegram 401", func() error {
			srv := respond(401, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
			_, err := (&telegram.Client{BotToken: "123:abc", BaseURL: srv.URL}).Send(ctx, providers.ChatMessage{Address: "42", Content: "hi"})
			return err
		}, auth},
		{"connection refused", func() error {
			srv := respond(200, `{}`)
			url := srv.URL
			srv.Close()
			_, err := (&resend.Client{APIKey: "k", BaseURL: url}).Send(ctx, email)
			return err
		}, down},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.send()
			if err == nil {
				t.Fatal("send succeeded")
			}
			if got := providers.ClassifyError(err); got != tc.want {
				t.Fatalf("ClassifyError(%v) = %q, want %q", err, got, tc.want)
			}
		})
	}
}

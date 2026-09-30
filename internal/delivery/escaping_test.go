package delivery

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/runenv"
)

// rawText is #364's payload: every character Handlebars HTML-escapes.
const rawText = `don't "quote" <tag> & more`

// rawPayload carries rawText as payload.text.
func rawPayload(t *testing.T) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{"text": rawText})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

func run(t *testing.T, env runenv.InAppEnvelope, handle func(context.Context, engine.TaskPayload) (json.RawMessage, error)) {
	t.Helper()
	input, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := handle(context.Background(), engine.TaskPayload{RunID: "run_esc", StepID: env.Step.StepID, Input: input}); err != nil {
		t.Fatalf("handle: %v", err)
	}
}

// TestEscaping_TextEmailSubjectAndBodyAreVerbatim is #364's headline: a text
// email's subject and body carry the value exactly as sent.
func TestEscaping_TextEmailSubjectAndBodyAreVerbatim(t *testing.T) {
	s := newStore(t)
	capt := providers.NewCapture()
	h := NewEmail(s, stubResolver{provider: capt, from: "noreply@example.org"})
	env := emailEnvelope()
	env.Payload = rawPayload(t)
	env.Step.SubjectTemplate = "Re: {{payload.text}}"
	env.Step.BodyTemplate = "said: {{payload.text}}"
	run(t, env, h.HandleEmail)

	sent := capt.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d, want 1", len(sent))
	}
	if sent[0].Subject != "Re: "+rawText {
		t.Errorf("Subject = %q", sent[0].Subject)
	}
	if sent[0].Text != "said: "+rawText || sent[0].HTML != "" {
		t.Errorf("Text = %q HTML = %q", sent[0].Text, sent[0].HTML)
	}
}

// TestEscaping_HTMLEmailBodyEscapesSubjectDoesNot: an HTML body escapes the
// value and keeps the author's tags; the subject is still plain.
func TestEscaping_HTMLEmailBodyEscapesSubjectDoesNot(t *testing.T) {
	s := newStore(t)
	capt := providers.NewCapture()
	h := NewEmail(s, stubResolver{provider: capt, from: "noreply@example.org"})
	env := emailEnvelope()
	env.Payload = rawPayload(t)
	env.Step.EditorType = "html"
	env.Step.SubjectTemplate = "{{payload.text}}"
	env.Step.BodyTemplate = "<p>{{payload.text}}</p>"
	run(t, env, h.HandleEmail)

	sent := capt.Sent()[0]
	if sent.Subject != rawText {
		t.Errorf("Subject = %q, want it verbatim", sent.Subject)
	}
	if want := "<p>don&#39;t &#34;quote&#34; &lt;tag&gt; &amp; more</p>"; sent.HTML != want {
		t.Errorf("HTML = %q, want %q", sent.HTML, want)
	}
}

// TestEscaping_LayoutFollowsTheBody: a layout wraps the rendered body
// without escaping it again, and escapes its own values like the body.
func TestEscaping_LayoutFollowsTheBody(t *testing.T) {
	s := newStore(t)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	if err := s.Layouts().Upsert(context.Background(), scope, "frame", "Frame", "<main>{{content}}</main><footer>{{payload.text}}</footer>"); err != nil {
		t.Fatalf("layout: %v", err)
	}
	capt := providers.NewCapture()
	h := NewEmail(s, stubResolver{provider: capt, from: "noreply@example.org"})
	env := emailEnvelope()
	env.Payload = rawPayload(t)
	env.Step.EditorType = "html"
	env.Step.LayoutKey = "frame"
	env.Step.BodyTemplate = "<p>{{payload.text}}</p>"
	run(t, env, h.HandleEmail)

	esc := "don&#39;t &#34;quote&#34; &lt;tag&gt; &amp; more"
	if want := "<main><p>" + esc + "</p></main><footer>" + esc + "</footer>"; capt.Sent()[0].HTML != want {
		t.Errorf("HTML = %q, want %q", capt.Sent()[0].HTML, want)
	}
}

func TestEscaping_PushIsVerbatim(t *testing.T) {
	s := newStore(t)
	seedPushAddress(t, s, "ntfy", "agent-alerts")
	capt := providers.NewCapturePush()
	h := NewPush(s, stubPushResolver{p: capt, id: "ntfy"})
	env := pushEnvelope()
	env.Payload = rawPayload(t)
	env.Step.SubjectTemplate = "{{payload.text}}"
	env.Step.BodyTemplate = "{{payload.text}}"
	run(t, env, h.HandlePush)

	sent := capt.Sent()
	if len(sent) != 1 || sent[0].Title != rawText || sent[0].Body != rawText {
		t.Fatalf("push = %+v, want title and body verbatim", sent)
	}
}

func TestEscaping_SMSIsVerbatim(t *testing.T) {
	s := newStore(t)
	capt := providers.NewCaptureSMS()
	h := NewSMS(s, stubSMSResolver{p: capt, id: "twilio"})
	env := smsEnvelope()
	env.Payload = rawPayload(t)
	env.Step.BodyTemplate = "{{payload.text}}"
	run(t, env, h.HandleSMS)

	if sent := capt.Sent(); len(sent) != 1 || sent[0].Body != rawText {
		t.Fatalf("sms = %+v, want body verbatim", sent)
	}
}

func TestEscaping_InAppIsVerbatim(t *testing.T) {
	s := newStore(t)
	h := New(s)
	env := inAppEnvelope()
	env.Payload = rawPayload(t)
	env.Step.SubjectTemplate = "{{payload.text}}"
	env.Step.BodyTemplate = "{{payload.text}}"
	run(t, env, h.HandleInApp)

	msgs, err := s.Messages().List(context.Background(), domain.Scope{OrgID: "org_1", EnvID: "env_1"}, env.SubscriberID, "in_app")
	if err != nil || len(msgs) != 1 {
		t.Fatalf("messages = %v, %v", msgs, err)
	}
	if msgs[0].Subject != rawText || msgs[0].Content != rawText {
		t.Fatalf("in-app subject %q content %q, want both verbatim", msgs[0].Subject, msgs[0].Content)
	}
}

// TestEscaping_ChatByProvider: Telegram is sent with parse_mode HTML, so its
// wire text escapes the value (numeric quotes, which Telegram accepts) while
// the author's tags stay; Slack escapes only & < >; Discord is verbatim. The
// stored message is plain text in every case.
func TestEscaping_ChatByProvider(t *testing.T) {
	cases := []struct {
		provider, wire string
	}{
		{"telegram", "<b>don&#39;t &#34;quote&#34; &lt;tag&gt; &amp; more</b>"},
		{"slack", "<b>don't \"quote\" &lt;tag&gt; &amp; more</b>"},
		{"discord", "<b>" + rawText + "</b>"},
	}
	for _, c := range cases {
		t.Run(c.provider, func(t *testing.T) {
			s := newStore(t)
			seedChatAddress(t, s, c.provider, "987654321")
			capt := providers.NewCaptureChat()
			h := NewChat(s, stubChatResolver{p: capt, id: c.provider})
			env := chatEnvelope()
			env.Payload = rawPayload(t)
			env.Step.BodyTemplate = "<b>{{payload.text}}</b>"
			run(t, env, h.HandleChat)

			sent := capt.Sent()
			if len(sent) != 1 || sent[0].Content != c.wire {
				t.Fatalf("wire = %+v, want %q", sent, c.wire)
			}
			msgs, err := s.Messages().List(context.Background(), domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "chat")
			if err != nil || len(msgs) != 1 || msgs[0].Content != "<b>"+rawText+"</b>" {
				t.Fatalf("stored = %v, %v; want the plain text", msgs, err)
			}
		})
	}
}

// TestEmail_TextAndHTMLBodiesBothSent is #369 at the handler: a step with a
// text body and an HTML body fills both fields. The text part carries values
// as sent, the HTML part escapes them except where {{raw}} asks otherwise,
// and a layout wraps only the HTML part.
func TestEmail_TextAndHTMLBodiesBothSent(t *testing.T) {
	s := newStore(t)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	if err := s.Layouts().Upsert(context.Background(), scope, "frame", "Frame", "<main>{{content}}</main>"); err != nil {
		t.Fatalf("layout: %v", err)
	}
	capt := providers.NewCapture()
	h := NewEmail(s, stubResolver{provider: capt, from: "noreply@example.org"})
	env := emailEnvelope()
	b, _ := json.Marshal(map[string]any{"text": rawText, "html": "<ul><li>it's</li></ul>"})
	env.Payload = b
	env.Step.LayoutKey = "frame"
	env.Step.BodyTemplate = "said: {{payload.text}}"
	env.Step.HTMLBodyTemplate = `<p>{{payload.text}}</p>{{raw "payload.html"}}`
	run(t, env, h.HandleEmail)

	sent := capt.Sent()[0]
	if sent.Text != "said: "+rawText {
		t.Errorf("Text = %q, want the text part verbatim and unwrapped", sent.Text)
	}
	want := "<main><p>don&#39;t &#34;quote&#34; &lt;tag&gt; &amp; more</p><ul><li>it's</li></ul></main>"
	if sent.HTML != want {
		t.Errorf("HTML = %q, want %q", sent.HTML, want)
	}
}

// TestEmail_EditorTypeHTMLWithoutHTMLBodyIsUnchanged pins today's single
// HTML body for a step that has not opted into a second part.
func TestEmail_EditorTypeHTMLWithoutHTMLBodyIsUnchanged(t *testing.T) {
	s := newStore(t)
	capt := providers.NewCapture()
	h := NewEmail(s, stubResolver{provider: capt, from: "noreply@example.org"})
	env := emailEnvelope()
	env.Step.EditorType = "html"
	env.Step.BodyTemplate = "<p>hi</p>"
	run(t, env, h.HandleEmail)
	if sent := capt.Sent()[0]; sent.HTML != "<p>hi</p>" || sent.Text != "" {
		t.Fatalf("sent = %+v, want HTML only", sent)
	}
}

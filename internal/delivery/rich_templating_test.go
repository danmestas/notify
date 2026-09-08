package delivery

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/providers"

	"github.com/danmestas/notify/internal/delivery/template"
	"github.com/danmestas/notify/internal/runenv"
)

// payloadRender builds the same render closure the handlers pass to
// inAppExtras: the authored string evaluated against the run payload.
func payloadRender(payload map[string]any) func(string) string {
	data := map[string]any{"payload": payload}
	return func(tmpl string) string { return template.Render(tmpl, data) }
}

// TestInAppExtras_RendersRichFields is the regression guard for #328: every
// authorable string in the extras blob must be rendered against the payload,
// exactly as subject and body already are. A verbatim redirect reaches the
// inbox API as "{{payload.link}}" and every row linking to it is dead.
func TestInAppExtras_RendersRichFields(t *testing.T) {
	step := runenv.Step{
		Avatar:    "{{payload.avatarUrl}}",
		Redirect:  &runenv.InAppRedirect{URL: "{{payload.link}}", Target: "_blank"},
		InAppData: json.RawMessage(`{"url":"{{payload.link}}","nested":{"deep":"{{payload.repo}}"},"list":["{{payload.repo}}",7],"n":42}`),
		PrimaryAction: &runenv.InAppAction{
			Label:    "Open {{payload.repo}}",
			Redirect: &runenv.InAppRedirect{URL: "{{payload.link}}"},
		},
		SecondaryAction: &runenv.InAppAction{Label: "Mute {{payload.repo}}"},
	}
	payload := map[string]any{
		"link":      "https://quarry.example/pr/9",
		"repo":      "quarry",
		"avatarUrl": "https://cdn.example/a.png",
	}

	var got struct {
		Avatar          string                `json:"avatar"`
		PrimaryAction   *runenv.InAppAction   `json:"primaryAction"`
		SecondaryAction *runenv.InAppAction   `json:"secondaryAction"`
		Redirect        *runenv.InAppRedirect `json:"redirect"`
		Data            map[string]any        `json:"data"`
	}
	raw := inAppExtras(step, payloadRender(payload))
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal extras: %v\nextras=%s", err, raw)
	}

	if strings.Contains(string(raw), "{{") {
		t.Fatalf("extras still contain an unrendered template: %s", raw)
	}
	if got.Redirect == nil || got.Redirect.URL != "https://quarry.example/pr/9" {
		t.Errorf("redirect.url = %+v, want the rendered link", got.Redirect)
	}
	if got.Redirect != nil && got.Redirect.Target != "_blank" {
		t.Errorf("redirect.target = %q, want _blank preserved", got.Redirect.Target)
	}
	if got.Avatar != "https://cdn.example/a.png" {
		t.Errorf("avatar = %q, want the rendered url", got.Avatar)
	}
	if got.PrimaryAction == nil || got.PrimaryAction.Label != "Open quarry" {
		t.Errorf("primaryAction.label = %+v, want %q", got.PrimaryAction, "Open quarry")
	}
	if got.PrimaryAction == nil || got.PrimaryAction.Redirect == nil ||
		got.PrimaryAction.Redirect.URL != "https://quarry.example/pr/9" {
		t.Errorf("primaryAction.redirect.url not rendered: %+v", got.PrimaryAction)
	}
	if got.SecondaryAction == nil || got.SecondaryAction.Label != "Mute quarry" {
		t.Errorf("secondaryAction.label = %+v", got.SecondaryAction)
	}
	if got.Data["url"] != "https://quarry.example/pr/9" {
		t.Errorf("data.url = %v, want the rendered link", got.Data["url"])
	}
	if nested, ok := got.Data["nested"].(map[string]any); !ok || nested["deep"] != "quarry" {
		t.Errorf("data.nested.deep not rendered: %v", got.Data["nested"])
	}
	if list, ok := got.Data["list"].([]any); !ok || list[0] != "quarry" {
		t.Errorf("data.list[0] not rendered: %v", got.Data["list"])
	}
	if got.Data["n"] != float64(42) {
		t.Errorf("data.n = %v, want the non-string leaf preserved", got.Data["n"])
	}
}

// TestInAppExtras_DropsEmptyRenders pins the omission rules on the rendered
// values, mirroring steptype.inAppRedirectFromForm / inAppActionFromForm: a
// payload that is missing the key must leave no redirect:{url:""} behind.
func TestInAppExtras_DropsEmptyRenders(t *testing.T) {
	tests := []struct {
		name    string
		step    runenv.Step
		absent  []string
		present []string
	}{
		{
			name: "redirect url renders empty drops the redirect even with a target",
			step: runenv.Step{
				Redirect: &runenv.InAppRedirect{URL: "{{payload.missing}}", Target: "_blank"},
			},
			absent: []string{"redirect", "_blank"},
		},
		{
			name: "action label renders empty drops the whole action",
			step: runenv.Step{
				PrimaryAction: &runenv.InAppAction{
					Label:    "{{payload.missing}}",
					Redirect: &runenv.InAppRedirect{URL: "{{payload.link}}"},
				},
			},
			absent: []string{"primaryAction"},
		},
		{
			name: "action keeps its label when only the redirect renders empty",
			step: runenv.Step{
				PrimaryAction: &runenv.InAppAction{
					Label:    "Open {{payload.repo}}",
					Redirect: &runenv.InAppRedirect{URL: "{{payload.missing}}"},
				},
			},
			absent:  []string{"redirect"},
			present: []string{"Open quarry"},
		},
		{
			name:   "avatar renders empty is omitted",
			step:   runenv.Step{Avatar: "{{payload.missing}}"},
			absent: []string{"avatar"},
		},
	}
	payload := map[string]any{"link": "https://quarry.example/pr/9", "repo": "quarry"}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := string(inAppExtras(tc.step, payloadRender(payload)))
			for _, s := range tc.absent {
				if strings.Contains(got, s) {
					t.Errorf("extras contain %q, want it dropped: %s", s, got)
				}
			}
			for _, s := range tc.present {
				if !strings.Contains(got, s) {
					t.Errorf("extras missing %q: %s", s, got)
				}
			}
		})
	}
}

// TestInAppExtras_DoesNotMutateStep guards the pointer trap: Redirect and the
// actions are pointers into the run envelope, so rendering in place would
// overwrite the authored template with one subscriber's payload.
func TestInAppExtras_DoesNotMutateStep(t *testing.T) {
	step := runenv.Step{
		Redirect:      &runenv.InAppRedirect{URL: "{{payload.link}}"},
		PrimaryAction: &runenv.InAppAction{Label: "Open {{payload.repo}}"},
		Avatar:        "{{payload.avatarUrl}}",
	}
	_ = inAppExtras(step, payloadRender(map[string]any{
		"link": "https://quarry.example/pr/9", "repo": "quarry", "avatarUrl": "https://cdn/a.png",
	}))

	if step.Redirect.URL != "{{payload.link}}" {
		t.Errorf("step.Redirect.URL was mutated to %q", step.Redirect.URL)
	}
	if step.PrimaryAction.Label != "Open {{payload.repo}}" {
		t.Errorf("step.PrimaryAction.Label was mutated to %q", step.PrimaryAction.Label)
	}
	if step.Avatar != "{{payload.avatarUrl}}" {
		t.Errorf("step.Avatar was mutated to %q", step.Avatar)
	}
}

// TestInAppExtras_MalformedDataUnchanged pins that rendering did not alter the
// pre-existing handling of an unparseable data blob: it stays unparseable, the
// marshal below still fails, and the handler still logs and sends without rich
// fields. Rendering must not quietly turn that into something else.
func TestInAppExtras_MalformedDataUnchanged(t *testing.T) {
	step := runenv.Step{InAppData: json.RawMessage(`{"broken":`)}
	if got := string(inAppExtras(step, payloadRender(nil))); got != "{}" {
		t.Errorf("extras = %s, want {} from the pre-existing drop path", got)
	}
}

// TestRenderPushData covers the second half of #328: push authors its deep link
// the same way and handed it to the provider verbatim.
func TestRenderPushData(t *testing.T) {
	render := payloadRender(map[string]any{"link": "https://quarry.example/pr/9"})

	got := renderPushData(map[string]string{"url": "{{payload.link}}", "kind": "mention"}, render)
	if got["url"] != "https://quarry.example/pr/9" {
		t.Errorf("url = %q, want the rendered link", got["url"])
	}
	if got["kind"] != "mention" {
		t.Errorf("kind = %q, want the constant preserved", got["kind"])
	}
	if _, ok := got["{{payload.link}}"]; ok {
		t.Error("keys must not be rendered: they are the receiving app's contract")
	}
	if renderPushData(nil, render) != nil {
		t.Error("nil data must render to nil so no data payload is sent")
	}
	if renderPushData(map[string]string{}, render) != nil {
		t.Error("empty data must render to nil")
	}
}

// TestHandlePush_RendersDataAgainstPayload is the wiring guard for the push
// half of #328. renderPushData being correct is not enough — the bug was that
// nothing called it — so this drives the real handler and asserts the value
// that reaches the provider is rendered, not the authored template.
func TestHandlePush_RendersDataAgainstPayload(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seedTopic(t, s, "agent-alerts")
	capt := providers.NewCapturePush()
	h := NewPush(s, stubPushResolver{p: capt, id: "ntfy"})

	env := pushEnvelope()
	env.Payload = json.RawMessage(`{"itemCount":3,"link":"https://quarry.example/pr/9"}`)
	env.Step.Data = map[string]string{"url": "{{payload.link}}", "kind": "mention"}
	input, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := h.HandlePush(ctx, engine.TaskPayload{RunID: "run_tmpl1", StepID: "P1", Input: input}); err != nil {
		t.Fatalf("HandlePush: %v", err)
	}

	sent := capt.Sent()
	if len(sent) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(sent))
	}
	if got := sent[0].Data["url"]; got != "https://quarry.example/pr/9" {
		t.Errorf("Data[url] = %q, want the rendered link; an unrendered deep link is dead on the device", got)
	}
	if got := sent[0].Data["kind"]; got != "mention" {
		t.Errorf("Data[kind] = %q, want the constant preserved", got)
	}
}

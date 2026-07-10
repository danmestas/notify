package console

// H3/M8 regression coverage: the copilot's concurrent-send guard must never
// corrupt or orphan the in-flight first turn, the first turn must always
// terminate within a bounded time (never an infinite "Working…"), and a
// rejected send must never wipe the composer's text.

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/store"
)

// gatedBuildStream is a controllable stubBuildStream substitute. Unlike
// stubBuildStream (which fires its whole scripted event slice immediately),
// it signals subscribed once SubscribeBuildStream is called — so a test can
// block until the first turn's SSE loop is actually listening — and only
// relays events the test pushes onto its own channel afterward.
type gatedBuildStream struct {
	subscribed chan struct{}
	events     chan []byte
}

func newGatedBuildStream() *gatedBuildStream {
	return &gatedBuildStream{subscribed: make(chan struct{}, 1), events: make(chan []byte, 8)}
}

func (g *gatedBuildStream) SubscribeBuildStream(_ string, handler func([]byte)) (func() error, error) {
	select {
	case g.subscribed <- struct{}{}:
	default:
	}
	go func() {
		for e := range g.events {
			handler(e)
		}
	}()
	return func() error { return nil }, nil
}

// TestAICopilotMessage_SecondMessageRejectionDoesNotAffectFirstTurn is the H3
// regression test. It drives a REAL concurrent second /message POST (in its
// own goroutine racing the first) while the first turn is genuinely in
// flight (blocked inside the SSE loop waiting on the build stream), and
// proves: (1) the second send is rejected inline with the "Still
// generating…" note, (2) that rejection appends no message and pushes no
// checkpoint — it has zero effect on the first turn's session state, and (3)
// once the first turn's run emits its done event, the first turn still
// completes normally (session flips to Ready with the persisted draft).
func TestAICopilotMessage_SecondMessageRejectionDoesNotAffectFirstTurn(t *testing.T) {
	tc := newTestConsole(t)
	stream := newGatedBuildStream()
	tc.c.aiBuilder = &stubAIBuilder{runID: "run-h3", repo: tc.st.BuilderSessions()}
	tc.c.buildStream = stream
	cookie, csrf := tc.login(t)

	sessID := tc.seedBuilderDraft(t, tc.scope, aibuilder.WorkflowDraft{})

	var wg sync.WaitGroup
	var firstBody string
	wg.Add(1)
	go func() {
		defer wg.Done()
		rr := tc.postForm(t, "/console/api/workflows/ai/"+sessID+"/message", url.Values{
			"prompt":     {"add an email step"},
			"csrf_token": {csrf},
		}, cookie)
		firstBody = rr.Body.String()
	}()

	// Wait for the first turn to actually be in flight (subscribed to its
	// run's stream) before racing the second send against it.
	select {
	case <-stream.subscribed:
	case <-time.After(5 * time.Second):
		t.Fatal("first turn never subscribed to the build stream")
	}

	msgsBefore, err := tc.st.BuilderSessions().Messages(context.Background(), tc.scope, sessID)
	if err != nil {
		t.Fatalf("Messages before second send: %v", err)
	}

	// The concurrent second send: must be rejected inline.
	rr2 := tc.postForm(t, "/console/api/workflows/ai/"+sessID+"/message", url.Values{
		"prompt":     {"actually make it sms instead"},
		"csrf_token": {csrf},
	}, cookie)
	if !strings.Contains(strings.ToLower(rr2.Body.String()), "still generating") {
		t.Fatalf("second send body = %q, want an inline \"still generating\" rejection", rr2.Body.String())
	}

	// The rejection must be a pure no-op: no new message, no new checkpoint.
	msgsAfter, err := tc.st.BuilderSessions().Messages(context.Background(), tc.scope, sessID)
	if err != nil {
		t.Fatalf("Messages after second send: %v", err)
	}
	if len(msgsAfter) != len(msgsBefore) {
		t.Fatalf("rejected second send changed message history: before=%d after=%d", len(msgsBefore), len(msgsAfter))
	}

	// Let the first turn's run finish normally: persist the final draft as the
	// durable run's finish step would (BEFORE its done stream event, exactly
	// as agentloop.AgentLoopBuilder.finish does), then emit the done event.
	draft := &aibuilder.WorkflowDraft{Name: "Flow", TriggerIdentifier: "flow"}
	draftJSON, err := json.Marshal(draft)
	if err != nil {
		t.Fatalf("marshal draft: %v", err)
	}
	if err := tc.st.BuilderSessions().UpdateDraft(context.Background(), tc.scope, sessID, draftJSON, store.BuilderSessionReady); err != nil {
		t.Fatalf("simulate durable finish persist: %v", err)
	}
	stream.events <- aiStreamMsg(t, "done", "Added an email step.", draft, "")

	wg.Wait()

	// The first turn's stream must have actually delivered the completion —
	// not been silently dropped because of the concurrent rejection. The done
	// event patches in the Keep/Discard/Try-again action bar; a still-"Working"
	// turn would never render it.
	if !strings.Contains(firstBody, "ai-action-keep") {
		t.Errorf("first turn's stream body missing its Keep/Discard/Try-again action bar; the rejected second send may have orphaned it. body: %s", firstBody)
	}

	sess, err := tc.st.BuilderSessions().Get(context.Background(), tc.scope, sessID)
	if err != nil {
		t.Fatalf("Get session after first turn finished: %v", err)
	}
	if sess.Status != store.BuilderSessionReady {
		t.Errorf("session status after first turn = %q, want %q — the rejected second send must not have orphaned the first turn", sess.Status, store.BuilderSessionReady)
	}

	finalMsgs, err := tc.st.BuilderSessions().Messages(context.Background(), tc.scope, sessID)
	if err != nil {
		t.Fatalf("Messages after first turn: %v", err)
	}
	// msgsBefore already includes the first turn's user message (captured after
	// it subscribed to the stream); only its assistant summary is still to come.
	if len(finalMsgs) != len(msgsBefore)+1 {
		t.Errorf("final message count = %d, want %d (msgsBefore + the first turn's assistant summary, and nothing from the rejected second send)", len(finalMsgs), len(msgsBefore)+1)
	}
}

// TestAICopilotMessage_TimeoutSurfacesErrorInsteadOfHanging is the H3 bounded-
// timeout regression test: a run whose stream never emits a terminal event
// must resolve within the configured deadline with an inline error — never
// an infinite "Working…" — and must leave the session in a non-Generating
// state so a subsequent send is not permanently rejected.
func TestAICopilotMessage_TimeoutSurfacesErrorInsteadOfHanging(t *testing.T) {
	tc := newTestConsole(t)
	stream := newGatedBuildStream() // never fed any events — the run "hangs"
	tc.c.aiBuilder = &stubAIBuilder{runID: "run-hang", repo: tc.st.BuilderSessions()}
	tc.c.buildStream = stream
	tc.c.aiGenerateTimeoutOverride = 100 * time.Millisecond
	tc.c.aiBuildPollOverride = 20 * time.Millisecond
	cookie, csrf := tc.login(t)

	sessID := tc.seedBuilderDraft(t, tc.scope, aibuilder.WorkflowDraft{})

	done := make(chan *httptestResponse, 1)
	go func() {
		rr := tc.postForm(t, "/console/api/workflows/ai/"+sessID+"/message", url.Values{
			"prompt":     {"add an sms step"},
			"csrf_token": {csrf},
		}, cookie)
		done <- &httptestResponse{code: rr.Code, body: rr.Body.String()}
	}()

	select {
	case resp := <-done:
		if !strings.Contains(resp.body, "did not finish") {
			t.Errorf("stalled turn body = %q, want the bounded-timeout error note", resp.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled turn never resolved — the bounded timeout did not fire (infinite \"Working…\")")
	}

	sess, err := tc.st.BuilderSessions().Get(context.Background(), tc.scope, sessID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if sess.Status == store.BuilderSessionGenerating {
		t.Errorf("session status after timeout = %q, want the in-flight marker cleared so a future send is not permanently rejected", sess.Status)
	}
}

// httptestResponse carries a recorded response's status + body across a
// goroutine boundary without depending on httptest.ResponseRecorder's
// internal (non-)concurrency-safety.
type httptestResponse struct {
	code int
	body string
}

// TestAICopilotComposer_DisablesRequestCancellation is the H3 client-side
// root-cause fix: Datastar's @post default (requestCancellation: "auto")
// aborts any in-flight request to the SAME (method, URL) pair the instant a
// new one is dispatched — and the composer's follow-up sends and the
// ai-generate/first-turn send both repeatedly hit the identical URL. That
// means sending a second message while the first is still streaming silently
// killed the FIRST request's connection client-side (independent of, and
// before, the server's "Still generating…" guard ever mattered) — orphaning
// its "Working…" UI forever, since nothing was left listening for the run's
// terminal event even though the durable run itself completed correctly (see
// TestAICopilotMessage_SecondMessageRejectionDoesNotAffectFirstTurn). The fix
// disables Datastar's auto-cancellation on every composer @post call so a
// second send never touches the first request's connection; the server's
// existing "Still generating…" guard is what actually rejects it.
func TestAICopilotComposer_DisablesRequestCancellation(t *testing.T) {
	tc := newTestConsole(t)
	tc.injectStreamingBuilder("run-x", nil, false, nil)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/ai", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := html.UnescapeString(rr.Body.String())

	keydown := extractAttr(t, body, "ai-copilot-input", "data-on:keydown")
	sendClick := extractAttr(t, body, "ai-copilot-send", "data-on:click")
	starterClick := extractAttr(t, body, "ai-copilot-starter", "data-on:click")

	for name, attr := range map[string]string{
		"textarea Enter-to-send": keydown,
		"Send button click":      sendClick,
		"starter card click":     starterClick,
	} {
		if strings.Count(attr, "requestCancellation: 'disabled'") != 2 {
			t.Errorf("%s handler must disable requestCancellation on BOTH the follow-up (/message) and first-turn (/ai-generate) @post calls; got: %s", name, attr)
		}
	}
}

// TestAICopilotComposer_NeverClearsPromptClientSideOnSend is the M8
// client-side half of the fix: the composer used to clear $aiPrompt (and the
// textarea's DOM value) unconditionally right after firing @post, BEFORE the
// server had any chance to accept or reject the send — so a rejection (rate
// limit, over-long, still-generating) silently discarded whatever the user
// had typed. Clearing now happens server-side (via a signal patch) only on a
// successful dispatch, so the client-side handlers must never unconditionally
// clear the signal or the DOM value themselves.
func TestAICopilotComposer_NeverClearsPromptClientSideOnSend(t *testing.T) {
	tc := newTestConsole(t)
	tc.injectStreamingBuilder("run-x", nil, false, nil)
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/ai", cookie)
	body := html.UnescapeString(rr.Body.String())

	keydown := extractAttr(t, body, "ai-copilot-input", "data-on:keydown")
	sendClick := extractAttr(t, body, "ai-copilot-send", "data-on:click")

	for name, attr := range map[string]string{
		"textarea Enter-to-send": keydown,
		"Send button click":      sendClick,
	} {
		if strings.Contains(attr, "$aiPrompt = ''") || strings.Contains(attr, `$aiPrompt=''`) {
			t.Errorf("%s handler still clears $aiPrompt unconditionally client-side (must preserve on rejection); got: %s", name, attr)
		}
	}
}

// extractAttr returns the value of attrName on the element identified by id
// in html, failing the test if either is not found. A minimal hand-rolled
// scan (not a full HTML parser) is enough for the single-line Datastar
// attributes under test.
func extractAttr(t *testing.T, pageHTML, id, attrName string) string {
	t.Helper()
	// id or class token — matched as a raw substring rather than a strict
	// attribute boundary, since callers pass either an element id
	// ("ai-copilot-input") or a class token that may sit alongside others
	// in the class list ("btn btn-primary ai-copilot-send").
	idIdx := strings.Index(pageHTML, id)
	if idIdx == -1 {
		t.Fatalf("element %q not found in rendered page", id)
	}
	attrMarker := attrName + `="`
	attrIdx := strings.Index(pageHTML[idIdx:], attrMarker)
	if attrIdx == -1 {
		t.Fatalf("attribute %q not found on element %q", attrName, id)
	}
	start := idIdx + attrIdx + len(attrMarker)
	end := strings.Index(pageHTML[start:], `"`)
	if end == -1 {
		t.Fatalf("unterminated attribute %q on element %q", attrName, id)
	}
	return pageHTML[start : start+end]
}

// TestAICopilotMessage_ClearsPromptSignalOnSuccessfulDispatch proves a
// successfully-dispatched turn (first turn or follow-up) still clears the
// composer via a server-driven $aiPrompt signal patch — the M8 fix moves
// clearing server-side but must not regress the #182 "clear on send" UX.
func TestAICopilotMessage_ClearsPromptSignalOnSuccessfulDispatch(t *testing.T) {
	tc := newTestConsole(t)
	doneEvent := aiStreamMsg(t, "done", "", &aibuilder.WorkflowDraft{Name: "Flow", TriggerIdentifier: "flow"}, "")
	tc.injectStreamingBuilder("run-clear", nil, false, [][]byte{doneEvent})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/api/workflows/ai-generate", url.Values{
		"prompt":     {"build me a flow"},
		"csrf_token": {csrf},
	}, cookie)
	if !strings.Contains(rr.Body.String(), `"aiPrompt":""`) {
		t.Errorf("successful dispatch must patch $aiPrompt to \"\" so the composer clears; body: %s", rr.Body.String())
	}
}

// TestAICopilotMessage_RejectionDoesNotClearPromptSignal proves a REJECTED
// send (session already Generating) never instructs the client to clear
// $aiPrompt — paired with the client-side removal of the unconditional clear,
// this is what preserves the user's typed text on rejection (M8).
func TestAICopilotMessage_RejectionDoesNotClearPromptSignal(t *testing.T) {
	tc := newTestConsole(t)
	tc.injectStreamingBuilder("run-reject", nil, false, nil)
	cookie, csrf := tc.login(t)

	sessID := tc.seedBuilderDraft(t, tc.scope, aibuilder.WorkflowDraft{})
	if err := tc.st.BuilderSessions().SetStatus(context.Background(), tc.scope, sessID, store.BuilderSessionGenerating); err != nil {
		t.Fatalf("SetStatus(Generating): %v", err)
	}

	rr := tc.postForm(t, "/console/api/workflows/ai/"+sessID+"/message", url.Values{
		"prompt":     {"add an sms step"},
		"csrf_token": {csrf},
	}, cookie)
	if !strings.Contains(strings.ToLower(rr.Body.String()), "still generating") {
		t.Fatalf("expected an inline \"still generating\" rejection; body: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `"aiPrompt":""`) {
		t.Errorf("rejected send must NOT clear $aiPrompt (would wipe the user's typed text); body: %s", rr.Body.String())
	}
}

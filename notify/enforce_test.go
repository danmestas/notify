package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/dagnats/program"
	"github.com/nats-io/nats.go"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/runtime/mesh"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/delivery/httpaction"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/identity"
)

// violations collects the permission violations NATS reports on a connection.
// They arrive asynchronously, through the connection's error handler, never as
// a call's return value: a denied publish or subscribe just goes nowhere.
type violations struct {
	mu   sync.Mutex
	errs []string
}

func (v *violations) handler(_ *nats.Conn, _ *nats.Subscription, err error) {
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "permissions violation") {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.errs = append(v.errs, err.Error())
}

// expect waits for a violation naming subject and removes it, so a probe the
// test makes on purpose does not count against notify.
func (v *violations) expect(t *testing.T, subject string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		v.mu.Lock()
		i := slices.IndexFunc(v.errs, func(e string) bool { return strings.Contains(e, `"`+subject+`"`) })
		if i >= 0 {
			v.errs = slices.Delete(v.errs, i, i+1)
			v.mu.Unlock()
			return
		}
		v.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no permissions violation for %q: the program connection is not restricted", subject)
}

// none fails the test on any violation collected so far.
func (v *violations) none(t *testing.T) {
	t.Helper()
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, e := range v.errs {
		t.Errorf("notify's program user was denied: %s", e)
	}
	v.errs = nil
}

// capturing is notify's Program, keeping the connection program.Run attached
// it with: the one carrying notify's NKey user and its permissions.
type capturing struct {
	*Program
	conn *nats.Conn
}

func (c *capturing) Start(ctx context.Context, h program.Host) error {
	c.conn = h.Conn
	return c.Program.Start(ctx, h)
}

// runEnforced runs notify under program.Run against a real dagnats, collecting
// every permission violation its connections report.
func runEnforced(t *testing.T) (*capturing, *violations, string) {
	t.Helper()
	url := dagtest.StartSidecar(t)
	v := &violations{}
	p := &capturing{Program: &Program{Config: programConfig(t)}}
	running := runProgram(t, p, program.Options{
		URL:         url,
		SeedPath:    filepath.Join(t.TempDir(), "notify.seed"),
		NATSOptions: []nats.Option{nats.ErrorHandler(v.handler)},
	})
	t.Cleanup(func() {
		stopProgram(t, running)
		v.none(t)
	})
	if p.conn.Opts.Nkey == "" {
		t.Fatal("program connection has no NKey: notify is not logged in as its program user")
	}
	// Anonymous root may publish anywhere; notify's user may not.
	if err := p.conn.Publish("enforcetest.denied", nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := p.conn.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	v.expect(t, "enforcetest.denied")
	return p, v, url
}

// TestProgram_EnforcedJourney triggers workflows through every step notify's
// NATS use hangs off, in program mode with dagnats enforcing notify's
// manifest-derived permissions: a throttle (notify_throttle), a digest window
// (notify_digest, notify_digest_claims), a delay (parked in dagnats), in_app
// sends (NOTIFY_DELIVERY, the outbound webhook's durable consumer) and a
// stream send (NOTIFY_FEED, read back through the feed's ordered consumer).
// Any denial fails the test.
func TestProgram_EnforcedJourney(t *testing.T) {
	if err := httpaction.ConfigureAllowlist("127.0.0.1"); err != nil {
		t.Fatalf("ConfigureAllowlist: %v", err)
	}
	t.Cleanup(func() { _ = httpaction.ConfigureAllowlist("") })
	p, v, _ := runEnforced(t)
	n := p.notify()
	ctx := context.Background()

	apiKey, scope, err := identity.New(n.st, p.Config.MasterKey).Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	hooks := make(chan []byte, 8)
	customer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hooks <- body
	}))
	t.Cleanup(customer.Close)
	if _, err := n.st.WebhookEndpoints().Create(ctx, scope, domain.WebhookEndpoint{
		URL: customer.URL, Active: true,
	}); err != nil {
		t.Fatalf("WebhookEndpoints.Create: %v", err)
	}
	workflows := map[string][]domain.Step{
		"enforced": {
			{StepID: "T1", Type: "throttle", ControlValues: json.RawMessage(`{"limit":5,"windowSec":60}`)},
			{StepID: "W1", Type: "delay", ControlValues: json.RawMessage(`{"amount":1,"unit":"seconds"}`)},
			{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"subject":"Hi","body":"enforced"}`)},
			{StepID: "F1", Type: "stream", ControlValues: json.RawMessage(`{"topic":"orders","body":"enforced"}`)},
		},
		"digested": {
			{StepID: "D1", Type: "digest", ControlValues: json.RawMessage(`{"amount":1,"unit":"seconds"}`)},
			{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"subject":"Digest","body":"digested {{digest.count}}"}`)},
		},
	}
	for trigger, steps := range workflows {
		if _, err := n.au.Create(ctx, scope, authoring.WorkflowInput{
			Name: trigger, TriggerIdentifier: trigger, Steps: steps,
		}); err != nil {
			t.Fatalf("authoring.Create %s: %v", trigger, err)
		}
		if err := n.au.Publish(ctx, scope, trigger); err != nil {
			t.Fatalf("authoring.Publish %s: %v", trigger, err)
		}
	}

	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	auth := "ApiKey " + apiKey
	if st, body := call(t, http.MethodPost, srv.URL+"/v1/subscribers", auth, `{"subscriberId":"u1"}`); st != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers = %d %s", st, body)
	}
	for trigger := range workflows {
		if st, body := call(t, http.MethodPost, srv.URL+"/v1/events/trigger", auth,
			`{"name":"`+trigger+`","to":{"subscriberId":"u1"},"payload":{}}`); st != http.StatusCreated {
			t.Fatalf("POST /v1/events/trigger %s = %d %s", trigger, st, body)
		}
	}

	waitFor(t, "both in_app messages", func() bool {
		_, body := call(t, http.MethodGet, srv.URL+"/v1/messages?subscriberId=u1&channel=in_app&format=array", auth, "")
		return strings.Contains(body, "enforced") && strings.Contains(body, "digested 1")
	})
	select {
	case <-hooks:
	case <-time.After(30 * time.Second):
		t.Fatal("no outbound webhook for the in_app delivery")
	}
	if frame := firstFeedFrame(t, srv.URL, auth, "topic=orders&from=0"); !strings.Contains(frame, "enforced") {
		t.Fatalf("feed frame = %q, want the stream step's message", frame)
	}
	v.none(t)
}

// TestProgram_EnforcedMeshTurn dispatches a mesh agent turn over notify's
// program connection, as notify does when no separate mesh hub is set: the
// prompt goes out on agents.prompt, and notify listens on agents.task.stream
// and its private a2a.reply inbox.
func TestProgram_EnforcedMeshTurn(t *testing.T) {
	p, v, url := runEnforced(t)
	agent, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(agent.Close)
	const subject = "agents.prompt.enforce"
	sub, err := agent.Subscribe(subject, func(m *nats.Msg) {
		_ = m.Respond([]byte(`{}`))
		_ = agent.Publish("agents.task.stream.enforce",
			[]byte(`{"kind":"status-update","status":{"state":"working"}}`))
		_ = agent.Publish(m.Header.Get("reply_to"),
			[]byte(`{"messageId":"a1","role":"agent","parts":[{"text":"PONG"}]}`))
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := agent.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reply, err := mesh.FromConn(p.conn).Dispatch(ctx, agentdomain.Turn{
		Agent:   agentdomain.Agent{Identifier: "stub", Runtime: agentdomain.RuntimeMesh, MeshSubject: subject},
		Message: agentdomain.Activity{Role: "user", Content: json.RawMessage(`{"parts":[{"text":"PING"}]}`)},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(reply.Parts) == 0 || !strings.Contains(reply.Parts[0].Text, "PONG") {
		t.Fatalf("reply = %+v, want PONG", reply)
	}
	v.none(t)
}

// TestProgram_EnforcedBuildStream: the AI builder's console subscribes to the
// builder step's live output, which a program's worker publishes under
// stream.p.notify.<run>.build.
//
// dagnats v0.0.43 grants a program publish on stream.p.<name>.> but not
// subscribe, and stream. is an engine root no manifest field can grant, so
// the subscription is denied: the console's live builder events are lost
// until dagnats#912 is fixed.
func TestProgram_EnforcedBuildStream(t *testing.T) {
	t.Skip("dagnats v0.0.43 denies a program subscribe on its own stream.p.<name>.>: danmestas/dagnats#912")
	p, v, _ := runEnforced(t)
	bus, err := eventbus.FromConn(context.Background(), p.conn,
		eventbus.WithHostResources(), eventbus.WithProgramOutput(Manifest().Name))
	if err != nil {
		t.Fatalf("eventbus.FromConn: %v", err)
	}
	got := make(chan []byte, 1)
	unsubscribe, err := bus.SubscribeBuildStream("run1", func(b []byte) { got <- b })
	if err != nil {
		t.Fatalf("SubscribeBuildStream: %v", err)
	}
	t.Cleanup(func() { _ = unsubscribe() })
	if err := p.conn.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := p.conn.Publish("stream.p.notify.run1.build", []byte("event")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		v.none(t)
		t.Fatal("no build event on stream.p.notify.run1.build")
	}
	v.none(t)
}

func call(t *testing.T, method, url, auth, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", auth)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// firstFeedFrame opens the feed's SSE stream and returns its first data line.
func firstFeedFrame(t *testing.T, base, auth, query string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/feed/stream?"+query, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/feed/stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /v1/feed/stream = %d %s", resp.StatusCode, b)
	}
	lines := bufio.NewScanner(resp.Body)
	for lines.Scan() {
		if data, ok := strings.CutPrefix(lines.Text(), "data:"); ok {
			return data
		}
	}
	t.Fatalf("feed stream ended without a frame: %v", lines.Err())
	return ""
}

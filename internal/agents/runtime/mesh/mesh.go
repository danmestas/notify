// Package mesh implements the A2A v2 (Synadia agent protocol) dispatch client
// for agents with runtime=mesh. It is a Go port of a TypeScript conformance
// client proven against live agents: publish the v2 envelope
// {messageId, role:"user", parts:[{text}]} to the agent's prompt subject with
// a reply_to header naming a private inbox, then aggregate the answer from
// both delivery paths seen in the wild — a single answer envelope on the
// inbox (pi-style) and role:"agent" message frames on agents.task.stream.>
// terminated by a TASK_STATE_* terminal (claude-style). Service errors
// surface via the Nats-Service-Error-Code reply header; v0.3 agents (acked
// but never speak v2) are detected by heuristic and reported distinguishably.
//
// The whole A2A mess — dual reply paths, version skew, terminal states —
// stays behind Dispatch; nothing above this package sees NATS subjects or
// headers.
//
// Known limitation (faithful to the TS reference): the task-stream
// subscription is broad (agents.task.stream.>), so concurrent dispatches can
// cross-aggregate each other's stream-path frames; per-frame correlation is
// ACI-4 (streaming) scope.
package mesh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/eventbus"
)

const (
	// DefaultAllowedSubjects is the default dispatch allowlist (RFC 0002 §8):
	// mesh turns may only be published to prompt subjects on the local hub.
	DefaultAllowedSubjects = "agents.prompt.>"
	// DefaultTimeout bounds one dispatch end to end.
	DefaultTimeout = 90 * time.Second
	// ackGrace is the v0.3 detection window: a request that was acked but has
	// produced no reply envelope and no task-stream state within this window
	// belongs to an agent that will never speak v2 (TS heuristic port).
	ackGrace = 3 * time.Second
	// settle drains trailing stream frames after the terminal signal.
	settle = 300 * time.Millisecond
	// streamSubject carries task lifecycle and streamed answer frames.
	streamSubject = "agents.task.stream.>"
)

// ErrAckedNoV2Answer reports the v0.3 case: the agent acked the prompt request
// but never opened a v2 task stream or published a reply envelope. It is
// permanent — redelivering the same prompt to the same agent cannot help.
var ErrAckedNoV2Answer = errors.New("agent acked prompt but sent no v2 answer (v0.3-style agent)")

// ServiceError is a non-200 Nats-Service-Error-Code reply from the agent's
// service endpoint. 4xx-class codes are permanent; others stay transient.
type ServiceError struct {
	Code   string
	Detail string
}

func (e *ServiceError) Error() string {
	return strings.TrimSpace("a2a service error " + e.Code + " " + e.Detail)
}

// terminalState matches the task states that end a turn (TS TERMINAL port).
var terminalState = regexp.MustCompile(`TASK_STATE_(COMPLETED|FAILED|CANCELED|REJECTED)`)

// Client dispatches mesh turns over an owned NATS connection.
type Client struct {
	nc      *nats.Conn
	allow   string
	timeout time.Duration
}

// Option customises a Client at Dial time.
type Option func(*Client)

// WithAllowedSubjects replaces the dispatch allowlist pattern (NATS wildcard
// syntax: "*" one token, ">" the rest).
func WithAllowedSubjects(pattern string) Option {
	return func(c *Client) { c.allow = pattern }
}

// WithTimeout replaces the per-dispatch deadline.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.timeout = d }
}

// Dial connects to the hub and returns a ready Client.
func Dial(natsURL string, opts ...Option) (*Client, error) {
	nc, err := eventbus.DialNATS(natsURL, "notify-mesh")
	if err != nil {
		return nil, err
	}
	c := &Client{nc: nc, allow: DefaultAllowedSubjects, timeout: DefaultTimeout}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Close releases the NATS connection.
func (c *Client) Close() { c.nc.Close() }

// envelope is the v2 wire message, both directions.
type envelope struct {
	MessageID string        `json:"messageId"`
	Role      string        `json:"role"`
	Parts     []domain.Part `json:"parts"`
}

// frame is one agents.task.stream.> lifecycle/answer message. Both the
// task.status.state and statusUpdate.state shapes appear in the wild.
type frame struct {
	Kind    string `json:"kind"`
	Message *struct {
		Role  string        `json:"role"`
		Parts []domain.Part `json:"parts"`
	} `json:"message"`
	Task *struct {
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	} `json:"task"`
	StatusUpdate *struct {
		State string `json:"state"`
	} `json:"statusUpdate"`
}

// state extracts the task state carried by the frame, if any.
func (f *frame) state() string {
	if f.Task != nil && f.Task.Status.State != "" {
		return f.Task.Status.State
	}
	if f.StatusUpdate != nil {
		return f.StatusUpdate.State
	}
	return ""
}

// collector aggregates the answer from both delivery paths concurrently.
type collector struct {
	mu     sync.Mutex
	states []string
	stream strings.Builder
	reply  *string
	closed bool
	done   chan struct{}
}

// finish signals completion exactly once. Callers must hold mu.
func (col *collector) finish() {
	if !col.closed {
		col.closed = true
		close(col.done)
	}
}

// onStreamFrame records lifecycle states, accumulates role:"agent" message
// text, and finishes on a terminal state (claude-style path).
func (col *collector) onStreamFrame(m *nats.Msg) {
	var f frame
	if err := json.Unmarshal(m.Data, &f); err != nil {
		return
	}
	st := f.state()
	col.mu.Lock()
	defer col.mu.Unlock()
	if st != "" && (len(col.states) == 0 || col.states[len(col.states)-1] != st) {
		col.states = append(col.states, st)
	}
	if f.Kind == "message" && f.Message != nil && f.Message.Role == "agent" {
		col.stream.WriteString(joinParts(f.Message.Parts))
	}
	if terminalState.MatchString(st) {
		col.finish()
	}
}

// onReply captures the single answer envelope on the private inbox
// (pi-style path). An undecodable body degrades to its raw bytes, as in
// the TS reference.
func (col *collector) onReply(m *nats.Msg) {
	text := string(m.Data)
	var env envelope
	if err := json.Unmarshal(m.Data, &env); err == nil {
		text = joinParts(env.Parts)
	}
	col.mu.Lock()
	defer col.mu.Unlock()
	if col.reply == nil {
		col.reply = &text
	}
	col.finish()
}

// quiet reports whether nothing v2 has been heard yet (no reply envelope, no
// task states) — the precondition of the v0.3 heuristic.
func (col *collector) quiet() bool {
	col.mu.Lock()
	defer col.mu.Unlock()
	return col.reply == nil && len(col.states) == 0
}

// answer returns the aggregated text (reply envelope wins over stream text,
// as in the TS reference) and the recorded states.
func (col *collector) answer() (string, []string) {
	col.mu.Lock()
	defer col.mu.Unlock()
	text := col.stream.String()
	if col.reply != nil {
		text = *col.reply
	}
	return strings.TrimSpace(text), col.states
}

// Dispatch publishes the turn to the agent's mesh subject and aggregates the
// answer from both v2 delivery paths. History is prepended into the prompt
// text (the v1 wire has no history field). Errors are classified: permanent
// failures are wrapped with domain.NewPermanentError; everything else is
// transient and safe to retry.
func (c *Client) Dispatch(ctx context.Context, t domain.Turn) (domain.Reply, error) {
	subj := t.Agent.MeshSubject
	if !SubjectAllowed(c.allow, subj) {
		return domain.Reply{}, domain.NewPermanentError(
			fmt.Errorf("mesh subject %q outside allowlist %q", subj, c.allow),
		)
	}

	body, err := json.Marshal(envelope{
		MessageID: randToken(),
		Role:      "user",
		Parts:     []domain.Part{{Text: Prompt(t)}},
	})
	if err != nil {
		return domain.Reply{}, domain.NewPermanentError(fmt.Errorf("marshal envelope: %w", err))
	}

	col := &collector{done: make(chan struct{})}
	streamSub, err := c.nc.Subscribe(streamSubject, col.onStreamFrame)
	if err != nil {
		return domain.Reply{}, fmt.Errorf("subscribe %s: %w", streamSubject, err)
	}
	defer func() { _ = streamSub.Unsubscribe() }()
	inbox := "a2a.reply." + randToken()
	replySub, err := c.nc.Subscribe(inbox, col.onReply)
	if err != nil {
		return domain.Reply{}, fmt.Errorf("subscribe reply inbox: %w", err)
	}
	defer func() { _ = replySub.Unsubscribe() }()
	if err := c.nc.Flush(); err != nil {
		return domain.Reply{}, fmt.Errorf("flush subscriptions: %w", err)
	}

	dctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// The request goroutine records the ack (and any service-error header);
	// the wait loop below watches for completion, the service error, the
	// deadline, or the v0.3 ack-no-answer heuristic.
	var (
		ackMu   sync.Mutex
		ackedAt time.Time
		reqErr  error
	)
	msg := nats.NewMsg(subj)
	msg.Header.Set("reply_to", inbox)
	msg.Data = body
	go func() {
		resp, rerr := c.nc.RequestMsgWithContext(dctx, msg)
		ackMu.Lock()
		defer ackMu.Unlock()
		if rerr != nil {
			reqErr = rerr
			return
		}
		ackedAt = time.Now()
		code := resp.Header.Get("Nats-Service-Error-Code")
		if code != "" && code != "200" {
			reqErr = &ServiceError{Code: code, Detail: resp.Header.Get("Nats-Service-Error")}
		}
	}()

	v03 := false
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
wait:
	for {
		select {
		case <-col.done:
			break wait
		case <-dctx.Done():
			break wait
		case <-ticker.C:
			ackMu.Lock()
			rerr, acked := reqErr, ackedAt
			ackMu.Unlock()
			if rerr != nil {
				break wait
			}
			if !acked.IsZero() && time.Since(acked) > ackGrace && col.quiet() {
				v03 = true
				break wait
			}
		}
	}
	time.Sleep(settle) // absorb trailing frames, as the TS reference does

	text, states := col.answer()
	ackMu.Lock()
	rerr := reqErr
	ackMu.Unlock()

	if text != "" {
		return domain.Reply{Parts: []domain.Part{{Text: text}}}, nil
	}
	if v03 {
		return domain.Reply{}, domain.NewPermanentError(ErrAckedNoV2Answer)
	}
	if rerr != nil {
		var se *ServiceError
		if errors.As(rerr, &se) && strings.HasPrefix(se.Code, "4") {
			return domain.Reply{}, domain.NewPermanentError(se)
		}
		return domain.Reply{}, fmt.Errorf("mesh dispatch %s: %w", subj, rerr)
	}
	if st := lastTerminal(states); st != "" {
		// An explicit terminal with no answer text: the task itself failed
		// (or completed empty) — redelivery would replay the same outcome.
		return domain.Reply{}, domain.NewPermanentError(
			fmt.Errorf("task ended %s with no answer", st),
		)
	}
	return domain.Reply{}, fmt.Errorf("no answer from %s within %s", subj, c.timeout)
}

// lastTerminal returns the final terminal state recorded, if any.
func lastTerminal(states []string) string {
	for i := len(states) - 1; i >= 0; i-- {
		if terminalState.MatchString(states[i]) {
			return states[i]
		}
	}
	return ""
}

// Prompt renders the turn for the v1 wire: the bounded transcript prepended
// as "role: text" lines under a short header, then the new message (RFC 0002
// §4 — the v2 envelope carries no history field, so history rides in-band).
func Prompt(t domain.Turn) string {
	msg := contentText(t.Message.Content)
	if len(t.History) == 0 {
		return msg
	}
	var b strings.Builder
	b.WriteString("Conversation so far:\n")
	for _, a := range t.History {
		b.WriteString(a.Role)
		b.WriteString(": ")
		b.WriteString(contentText(a.Content))
		b.WriteString("\n")
	}
	b.WriteString("---\n")
	b.WriteString(msg)
	return b.String()
}

// contentText extracts the joined part text from an activity content body
// {"parts":[{"text":...}]}. Undecodable content degrades to its raw JSON so a
// malformed activity is still visible to the agent rather than dropped.
func contentText(raw json.RawMessage) string {
	var c struct {
		Parts []domain.Part `json:"parts"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return string(raw)
	}
	return joinParts(c.Parts)
}

// joinParts concatenates part text, mirroring the TS textOf helper.
func joinParts(parts []domain.Part) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// SubjectAllowed reports whether subject matches the NATS wildcard pattern:
// tokens split on ".", "*" matches exactly one token, ">" matches one or more
// trailing tokens.
func SubjectAllowed(pattern, subject string) bool {
	if pattern == "" || subject == "" {
		return false
	}
	pt := strings.Split(pattern, ".")
	st := strings.Split(subject, ".")
	for i, p := range pt {
		if p == ">" {
			return len(st) > i
		}
		if i >= len(st) || (p != "*" && p != st[i]) {
			return false
		}
	}
	return len(st) == len(pt)
}

// randToken returns a 24-hex-char random token for message ids and inboxes.
func randToken() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(b[:])
}

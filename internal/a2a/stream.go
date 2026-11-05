package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/danmestas/notify/internal/a2aauth"
	agentdomain "github.com/danmestas/notify/internal/agents/domain"
)

const (
	// streamMax bounds one stream; a client resubscribes for a longer turn.
	streamMax = 15 * time.Minute
	// streamPoll is how often a stream re-reads the task when no signal
	// arrives (a turn running in another process never signals this one).
	streamPoll = 250 * time.Millisecond
	// keepAlive spaces the comment lines / pings that hold an idle stream open.
	keepAlive = 15 * time.Second
	// maxSocketStreams caps the requests in flight on one WebSocket.
	maxSocketStreams = 8
	writeTimeout     = 10 * time.Second
	codeRateLimited  = -32000
)

// emitFunc delivers one stream result (a Task, a status-update or an
// artifact-update) as the result of a JSON-RPC response with the request id.
type emitFunc func(result any) error

type statusUpdate struct {
	Kind      string         `json:"kind"`
	TaskID    string         `json:"taskId"`
	ContextID string         `json:"contextId"`
	Status    taskStatus     `json:"status"`
	Final     bool           `json:"final"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type artifactUpdate struct {
	Kind      string   `json:"kind"`
	TaskID    string   `json:"taskId"`
	ContextID string   `json:"contextId"`
	Artifact  artifact `json:"artifact"`
	Append    bool     `json:"append"`
	LastChunk bool     `json:"lastChunk"`
}

// openStream resolves a message/stream or tasks/resubscribe request to the
// task to follow. message/stream starts a turn; tasks/resubscribe follows an
// owned one from its current state.
func (s *Service) openStream(ctx context.Context, p a2aauth.Principal, agent agentdomain.Agent, req rpcRequest) (string, agentdomain.Conversation, agentdomain.Activity, *rpcError) {
	if req.Method == "message/stream" {
		return s.start(ctx, p, agent, req.Params)
	}
	return s.owned(ctx, p, agent, req.Params)
}

// follow emits the task's current state, then each change until it ends: new
// notes as working status updates, then the reply as an artifact and a final
// status update. It returns when the task ends, emit fails, or ctx is done.
func (s *Service) follow(ctx context.Context, p a2aauth.Principal, ctxID string, conv agentdomain.Conversation, user agentdomain.Activity, emit emitFunc, beat func() error) {
	ctx, cancel := context.WithTimeout(ctx, streamMax)
	defer cancel()
	var wake <-chan struct{}
	if s.tasks != nil {
		var stop func()
		wake, stop = s.tasks.Watch(p.Scope, user.ID)
		defer stop()
	}
	tick := time.NewTicker(streamPoll)
	defer tick.Stop()
	hb := time.NewTicker(keepAlive)
	defer hb.Stop()

	lastRunCheck := time.Now()
	read := func() (task, []agentdomain.TaskNote) {
		check := time.Since(lastRunCheck) >= runCheckEvery
		if check {
			lastRunCheck = time.Now()
		}
		rec := s.record(ctx, p.Scope, user.ID)
		return s.task(ctx, p.Scope, ctxID, conv, user, check), rec.Notes
	}

	t, notes := read()
	first := t
	if first.Status.State == stateWorking && len(notes) == 0 {
		first.Status.State = agentdomain.TaskSubmitted
	}
	if emit(first) != nil {
		return
	}
	sent := len(notes)
	for {
		if t.Status.State != stateWorking {
			s.finish(t, emit)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-tick.C:
		case <-hb.C:
			if beat != nil && beat() != nil {
				return
			}
			continue
		}
		t, notes = read()
		for ; sent < len(notes); sent++ {
			n := notes[sent]
			ev := statusUpdate{
				Kind: "status-update", TaskID: user.ID, ContextID: ctxID,
				Status: taskStatus{State: stateWorking, Timestamp: n.At, Message: agentMessage(user.ID+".note", ctxID, user.ID, n.Text)},
			}
			if emit(ev) != nil {
				return
			}
		}
	}
}

// finish emits a task's closing events: its answer artifact, then the final
// status update, which carries the usage metadata.
func (s *Service) finish(t task, emit emitFunc) {
	for _, a := range t.Artifacts {
		if emit(artifactUpdate{
			Kind: "artifact-update", TaskID: t.ID, ContextID: t.ContextID, Artifact: a, LastChunk: true,
		}) != nil {
			return
		}
	}
	var meta map[string]any
	if u, ok := t.Metadata["usage"]; ok {
		meta = map[string]any{"usage": u}
	}
	_ = emit(statusUpdate{
		Kind: "status-update", TaskID: t.ID, ContextID: t.ContextID,
		Status: t.Status, Final: true, Metadata: meta,
	})
}

// serveSSE answers message/stream and tasks/resubscribe as server-sent
// events, one JSON-RPC response per event. A request that fails before the
// stream begins gets an ordinary JSON-RPC error.
func (s *Service) serveSSE(w http.ResponseWriter, r *http.Request, p a2aauth.Principal, agent agentdomain.Agent, req rpcRequest) {
	ctxID, conv, user, rerr := s.openStream(r.Context(), p, agent, req)
	if rerr != nil {
		writeRPC(w, http.StatusOK, req.ID, nil, rerr)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeRPC(w, http.StatusOK, req.ID, nil, &rpcError{Code: codeInternal, Message: "streaming unsupported"})
		return
	}
	// A stream outlives the server's write timeout.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	id := req.ID
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	emit := func(result any) error {
		body, err := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
		if err != nil {
			return err
		}
		if _, err := w.Write(append(append([]byte("data: "), body...), '\n', '\n')); err != nil {
			return err
		}
		fl.Flush()
		return nil
	}
	beat := func() error {
		if _, err := w.Write([]byte(": keep-alive\n\n")); err != nil {
			return err
		}
		fl.Flush()
		return nil
	}
	s.follow(r.Context(), p, ctxID, conv, user, emit, beat)
}

// serveWS handles GET /a2a/{agentId}/ws: a WebSocket carrying JSON-RPC
// requests from the client (message/send, message/stream, tasks/resubscribe,
// tasks/get) and, for each, JSON-RPC responses in the same shapes as the
// HTTP endpoint and its event stream. Auth and ownership are the HTTP
// endpoint's; each request spends the same rate-limit budgets.
func (s *Service) serveWS(w http.ResponseWriter, r *http.Request) {
	p, agent, ok := s.caller(w, r)
	if !ok || !s.allow(w, r, p, agent) {
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept has answered
	}
	defer func() { _ = ws.CloseNow() }()
	ws.SetReadLimit(maxBodyBytes)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		t := time.NewTicker(keepAlive)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, c := context.WithTimeout(ctx, writeTimeout)
				err := ws.Ping(pctx)
				c()
				if err != nil {
					_ = ws.CloseNow()
					return
				}
			}
		}
	}()

	var wg sync.WaitGroup
	defer wg.Wait()
	slots := make(chan struct{}, maxSocketStreams)
	send := func(id json.RawMessage, result any, e *rpcError) error {
		if len(id) == 0 {
			id = json.RawMessage("null")
		}
		body, err := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: e})
		if err != nil {
			return err
		}
		wctx, c := context.WithTimeout(ctx, writeTimeout)
		defer c()
		return ws.Write(wctx, websocket.MessageText, body)
	}
	first := true
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var req rpcRequest
		if json.Unmarshal(data, &req) != nil {
			_ = send(nil, nil, &rpcError{Code: codeParse, Message: "parse error"})
			continue
		}
		if req.JSONRPC != "2.0" || req.Method == "" {
			_ = send(req.ID, nil, &rpcError{Code: codeInvalidRequest, Message: "invalid request"})
			continue
		}
		// The upgrade already spent the budget for the first request.
		if !first {
			if code := s.spend(ctx, p, agent); code != http.StatusOK {
				_ = send(req.ID, nil, &rpcError{Code: codeRateLimited, Message: "rate limit exceeded"})
				continue
			}
		}
		first = false
		select {
		case slots <- struct{}{}:
		default:
			_ = send(req.ID, nil, &rpcError{Code: codeRateLimited, Message: "too many requests in flight"})
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			s.wsRequest(ctx, p, agent, req, send)
		}()
	}
}

func (s *Service) wsRequest(ctx context.Context, p a2aauth.Principal, agent agentdomain.Agent, req rpcRequest, send func(json.RawMessage, any, *rpcError) error) {
	if req.Method != "message/stream" && req.Method != "tasks/resubscribe" {
		result, rerr := s.call(ctx, p, agent, req)
		_ = send(req.ID, result, rerr)
		return
	}
	ctxID, conv, user, rerr := s.openStream(ctx, p, agent, req)
	if rerr != nil {
		_ = send(req.ID, nil, rerr)
		return
	}
	s.follow(ctx, p, ctxID, conv, user, func(result any) error {
		return send(req.ID, result, nil)
	}, nil)
}

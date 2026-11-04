package a2a

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danmestas/notify/internal/a2aauth"
	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/ingress"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/domain"
)

// JSON-RPC 2.0 and A2A error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeInvalidParams  = -32602
	codeInternal       = -32603
	codeTaskNotFound   = -32001
	codeUnsupportedOp  = -32004
	maxContextIDLen    = 128
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

func invalidParams(format string, args ...any) *rpcError {
	return &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func writeRPC(w http.ResponseWriter, status int, id json.RawMessage, result any, e *rpcError) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: e})
}

// rpc handles POST /a2a/{agentId}.
func (s *Service) rpc(w http.ResponseWriter, r *http.Request) {
	p, agent, ok := s.caller(w, r)
	if !ok {
		return
	}
	var req rpcRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		writeRPC(w, http.StatusOK, nil, nil, &rpcError{Code: codeParse, Message: "parse error"})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, http.StatusOK, req.ID, nil, &rpcError{Code: codeInvalidRequest, Message: "invalid request"})
		return
	}

	var (
		result any
		rerr   *rpcError
	)
	if !s.allow(w, r, p, agent) {
		return
	}
	switch req.Method {
	case "message/send":
		result, rerr = s.messageSend(r.Context(), p, agent, req.Params)
	case "tasks/get":
		result, rerr = s.tasksGet(r.Context(), p, agent, req.Params)
	default:
		// message/stream and tasks/resubscribe arrive with streaming (#76).
		rerr = &rpcError{Code: codeUnsupportedOp, Message: "unsupported operation: " + req.Method}
	}
	writeRPC(w, http.StatusOK, req.ID, result, rerr)
}

// allow spends the per-credential and per-agent budgets. false means a
// response has been written.
func (s *Service) allow(w http.ResponseWriter, r *http.Request, p a2aauth.Principal, a agentdomain.Agent) bool {
	if s.limiter == nil {
		return true
	}
	for _, b := range []struct {
		key   string
		limit int64
	}{
		{"a2a:cred:" + p.Scope.EnvID + ":" + p.CredentialID, credentialLimit},
		{"a2a:agent:" + a.ID, agentLimit},
	} {
		ok, err := s.limiter.BudgetAllow(b.key, 1, b.limit, limitWindow)
		if err != nil {
			s.log.ErrorContext(r.Context(), "a2a: limiter", "err", err)
			http.Error(w, "rate limiter unavailable", http.StatusServiceUnavailable)
			return false
		}
		if !ok {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return false
		}
	}
	return true
}

// A2A wire types (v0.3).

type part struct {
	Kind string          `json:"kind"`
	Text string          `json:"text,omitempty"`
	File *filePart       `json:"file,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

type filePart struct {
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
	Bytes    string `json:"bytes"`
	URI      string `json:"uri"`
}

type message struct {
	Kind      string `json:"kind"`
	MessageID string `json:"messageId"`
	Role      string `json:"role"`
	Parts     []part `json:"parts"`
	ContextID string `json:"contextId,omitempty"`
	TaskID    string `json:"taskId,omitempty"`
}

type taskStatus struct {
	State     string   `json:"state"`
	Timestamp string   `json:"timestamp,omitempty"`
	Message   *message `json:"message,omitempty"`
}

type artifact struct {
	ArtifactID string `json:"artifactId"`
	Name       string `json:"name"`
	Parts      []part `json:"parts"`
}

type task struct {
	Kind      string     `json:"kind"`
	ID        string     `json:"id"`
	ContextID string     `json:"contextId"`
	Status    taskStatus `json:"status"`
	Artifacts []artifact `json:"artifacts,omitempty"`
}

const (
	stateWorking   = "working"
	stateCompleted = "completed"
	stateFailed    = "failed"
)

func (s *Service) messageSend(ctx context.Context, p a2aauth.Principal, agent agentdomain.Agent, params json.RawMessage) (any, *rpcError) {
	var in struct {
		Message *message `json:"message"`
	}
	if err := json.Unmarshal(params, &in); err != nil || in.Message == nil {
		return nil, invalidParams("params.message is required")
	}
	m := in.Message
	if m.MessageID == "" {
		return nil, invalidParams("message.messageId is required")
	}
	if m.Role != "" && m.Role != "user" {
		return nil, invalidParams("message.role must be user")
	}
	if m.TaskID != "" {
		return nil, invalidParams("continuing a task is not supported; send a new message in the same contextId")
	}
	text, atts, rerr := convertParts(m.Parts)
	if rerr != nil {
		return nil, rerr
	}

	key := idempotencyKey(p.CredentialID, m.MessageID)
	ctxID := m.ContextID
	switch {
	case ctxID == "":
		// Derived from the credential and message id, so a retry of the
		// opening message lands in the same context.
		sum := sha256.Sum256([]byte(p.CredentialID + "\x00" + m.MessageID))
		ctxID = "c" + hex.EncodeToString(sum[:])[:31]
	case len(ctxID) > maxContextIDLen || strings.ContainsFunc(ctxID, isControl):
		return nil, invalidParams("message.contextId must be 1-%d printable characters", maxContextIDLen)
	}

	res, err := s.compose.Compose(ctx, p.Scope, ingress.ComposeInput{
		AgentIdentifier: agent.Identifier,
		ConversationID:  threadPrefix(p.CredentialID) + ctxID,
		SubscriberID:    p.SubscriberID,
		Anonymous:       p.SubscriberID == "",
		Text:            text,
		Attachments:     atts,
		IdempotencyKey:  key,
		RequireActive:   true,
	})
	switch {
	case errors.Is(err, ingress.ErrIdempotencyConflict):
		return nil, invalidParams("messageId %q was already used for a different message", m.MessageID)
	case errors.Is(err, ingress.ErrInvalidAttachments):
		return nil, invalidParams("%v", err)
	case errors.Is(err, ingress.ErrAgentInactive), errors.Is(err, ingress.ErrUnknownRuntime):
		return nil, &rpcError{Code: codeInternal, Message: "agent cannot take messages"}
	case err != nil:
		s.log.ErrorContext(ctx, "a2a: compose", "err", err)
		return nil, &rpcError{Code: codeInternal, Message: "internal error"}
	}

	conv, err := s.agents.Conversations().GetByThread(ctx, p.Scope, agent.ID, "", threadPrefix(p.CredentialID)+ctxID)
	if err != nil {
		s.log.ErrorContext(ctx, "a2a: load conversation", "err", err)
		return nil, &rpcError{Code: codeInternal, Message: "internal error"}
	}
	user := agentdomain.Activity{ID: res.ActivityID, ConversationID: conv.ID, RunID: res.RunID}
	return s.waitTask(ctx, p.Scope, ctxID, conv, user), nil
}

// waitTask polls for the turn's outcome for up to the configured wait.
func (s *Service) waitTask(ctx context.Context, scope domain.Scope, ctxID string, conv agentdomain.Conversation, user agentdomain.Activity) task {
	deadline := time.NewTimer(s.wait)
	defer deadline.Stop()
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	lastRunCheck := time.Now()
	for {
		checkRun := time.Since(lastRunCheck) >= runCheckEvery
		if checkRun {
			lastRunCheck = time.Now()
		}
		t := s.task(ctx, scope, ctxID, conv, user, checkRun)
		if t.Status.State != stateWorking {
			return t
		}
		select {
		case <-ctx.Done():
			return t
		case <-deadline.C:
			return t
		case <-tick.C:
		}
	}
}

func (s *Service) tasksGet(ctx context.Context, p a2aauth.Principal, agent agentdomain.Agent, params json.RawMessage) (any, *rpcError) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(params, &in); err != nil || in.ID == "" {
		return nil, invalidParams("params.id is required")
	}
	notFound := &rpcError{Code: codeTaskNotFound, Message: "task not found"}
	user, err := s.agents.Activities().Get(ctx, p.Scope, in.ID)
	if errors.Is(err, agentstore.ErrNotFound) || (err == nil && user.Role != "user") {
		return nil, notFound
	}
	if err != nil {
		return nil, s.internal(ctx, "load task", err)
	}
	conv, err := s.agents.Conversations().Get(ctx, p.Scope, user.ConversationID)
	if err != nil {
		return nil, s.internal(ctx, "load task conversation", err)
	}
	prefix := threadPrefix(p.CredentialID)
	if conv.AgentID != agent.ID || conv.IntegrationID != "" || !strings.HasPrefix(conv.PlatformThreadID, prefix) {
		return nil, notFound
	}
	return s.task(ctx, p.Scope, strings.TrimPrefix(conv.PlatformThreadID, prefix), conv, user, true), nil
}

func (s *Service) internal(ctx context.Context, what string, err error) *rpcError {
	s.log.ErrorContext(ctx, "a2a: "+what, "err", err)
	return &rpcError{Code: codeInternal, Message: "internal error"}
}

// task reads the task's current state: completed when the turn's reply
// activity exists, failed when the turn's run ended without succeeding (only
// consulted when checkRun), working otherwise.
func (s *Service) task(ctx context.Context, scope domain.Scope, ctxID string, conv agentdomain.Conversation, user agentdomain.Activity, checkRun bool) task {
	t := task{Kind: "task", ID: user.ID, ContextID: ctxID, Status: taskStatus{State: stateWorking}}
	if user.RunID == "" {
		return t
	}
	if reply, ok := s.reply(ctx, scope, conv.ID, user); ok {
		var body struct {
			Parts []agentdomain.Part `json:"parts"`
		}
		_ = json.Unmarshal(reply.Content, &body)
		var parts []part
		for _, bp := range body.Parts {
			parts = append(parts, part{Kind: "text", Text: bp.Text})
		}
		t.Status = taskStatus{
			State:     stateCompleted,
			Timestamp: reply.CreatedAt,
			Message: &message{
				Kind: "message", MessageID: reply.ID, Role: "agent", Parts: parts,
				ContextID: ctxID, TaskID: user.ID,
			},
		}
		t.Artifacts = []artifact{{ArtifactID: reply.ID, Name: "answer", Parts: parts}}
		return t
	}
	if checkRun && s.runs != nil {
		if snap, err := s.runs.GetRun(ctx, user.RunID); err == nil && snap.IsTerminal() && !snap.Succeeded() {
			t.Status.State = stateFailed
			t.Status.Message = &message{
				Kind: "message", MessageID: user.ID + ".failed", Role: "agent",
				Parts:     []part{{Kind: "text", Text: "The agent could not complete this turn."}},
				ContextID: ctxID, TaskID: user.ID,
			}
		}
	}
	return t
}

// reply finds the agent activity the user activity's run produced.
func (s *Service) reply(ctx context.Context, scope domain.Scope, convID string, user agentdomain.Activity) (agentdomain.Activity, bool) {
	acts, err := s.agents.Activities().Page(ctx, scope, convID, 50, "", user.ID)
	if err != nil {
		return agentdomain.Activity{}, false
	}
	for _, a := range acts {
		if a.Role == "agent" && a.RunID == user.RunID {
			return a, true
		}
	}
	return agentdomain.Activity{}, false
}

// convertParts turns message parts into the activity text and attachments.
// Text parts join into the text. A file part with inline text content becomes
// an attachment under the usual caps; a file by URI, binary content and data
// parts are refused.
func convertParts(parts []part) (string, []agentdomain.Attachment, *rpcError) {
	var texts []string
	var atts []agentdomain.Attachment
	for i, p := range parts {
		switch p.Kind {
		case "text":
			texts = append(texts, p.Text)
		case "file":
			if p.File == nil || p.File.URI != "" || p.File.Bytes == "" {
				return "", nil, invalidParams("parts[%d]: only inline file bytes are supported", i)
			}
			raw, err := base64.StdEncoding.DecodeString(p.File.Bytes)
			if err != nil {
				return "", nil, invalidParams("parts[%d]: file bytes are not valid base64", i)
			}
			if !utf8.Valid(raw) || !textual(p.File.MimeType) {
				return "", nil, invalidParams("parts[%d]: only text files are supported", i)
			}
			name := p.File.Name
			if name == "" {
				name = fmt.Sprintf("attachment-%d", len(atts)+1)
			}
			atts = append(atts, agentdomain.Attachment{Name: name, MediaType: p.File.MimeType, Text: string(raw)})
		default:
			return "", nil, invalidParams("parts[%d]: %q parts are not supported", i, p.Kind)
		}
	}
	text := strings.Join(texts, "\n")
	if strings.TrimSpace(text) == "" {
		return "", nil, invalidParams("message needs a text part")
	}
	return text, atts, nil
}

func textual(mime string) bool {
	mime, _, _ = strings.Cut(mime, ";")
	return strings.HasPrefix(mime, "text/") || mime == "application/json"
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// idempotencyKey maps a caller's messageId to a client_message_id. It is
// always a digest of the credential and the messageId, never the caller's
// text: client_message_id is also the key other parts of notify match on
// (review-mode drafts, per environment), so a caller must not be able to
// choose one.
func idempotencyKey(credentialID, messageID string) string {
	sum := sha256.Sum256([]byte("a2a\x00" + credentialID + "\x00" + messageID))
	return "a" + hex.EncodeToString(sum[:])[:40]
}

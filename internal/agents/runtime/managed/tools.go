package managed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/runtime/managed/mcp"
	"github.com/danmestas/notify/internal/aibuilder"
)

// Per-turn limits on tool use. The whole loop runs in memory inside the turn
// task; a crash reruns the turn from its start.
const (
	maxMCPServers      = 8
	maxModelRounds     = 6
	maxToolCalls       = 12
	toolCallTimeout    = 10 * time.Second
	maxResultBytes     = 64 << 10
	maxTurnResultBytes = 256 << 10
	// minResultBytes is the least budget worth handing a result; below it the
	// call is answered too_large.
	minResultBytes = 1 << 10
)

// Correlation headers name who a call is for. They carry no authority: the
// server grants access from the auth header alone.
const (
	headerAgentID      = "Notify-Agent-Id"
	headerSubscriberID = "Notify-Subscriber-Id"
)

// Server names exclude "_" so the first "__" in an offered tool name always
// ends the server name.
var (
	serverNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	headerNamePattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	// toolNamePattern is the name rule both model APIs accept.
	toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	recordIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)
)

// reservedHeaders are headers the auth header may not replace.
var reservedHeaders = map[string]bool{
	"Accept": true, "Content-Type": true, "Content-Length": true, "Host": true,
	"Mcp-Session-Id": true, "Mcp-Protocol-Version": true,
	headerAgentID: true, headerSubscriberID: true,
}

// recordIDKeys are the argument keys whose values the log may record.
var recordIDKeys = map[string]bool{
	"id": true, "ids": true, "recordId": true, "recordIds": true, "record_id": true, "record_ids": true,
}

const maxLoggedRecordIDs = 32

// checkMCPServers validates the servers and fills in the default auth header.
func checkMCPServers(raw json.RawMessage, servers []MCPServer) error {
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) == nil {
		if _, ok := probe["mcp_servers"]; ok {
			return errors.New("managed agent settings name tool servers mcpServers, not mcp_servers")
		}
	}
	if len(servers) > maxMCPServers {
		return fmt.Errorf("managed agent may name at most %d mcpServers", maxMCPServers)
	}
	seen := map[string]bool{}
	for i := range servers {
		s := &servers[i]
		if !serverNamePattern.MatchString(s.Name) {
			return fmt.Errorf("mcpServers name %q must be 1-32 lowercase letters, digits or hyphens, starting with a letter or digit", s.Name)
		}
		if seen[s.Name] {
			return fmt.Errorf("mcpServers name %q is used twice", s.Name)
		}
		seen[s.Name] = true
		if err := mcp.CheckURL(s.URL, true); err != nil {
			return fmt.Errorf("mcpServers %q: %w", s.Name, err)
		}
		if s.AuthHeader == "" {
			s.AuthHeader = "Authorization"
		}
		if !headerNamePattern.MatchString(s.AuthHeader) || reservedHeaders[http.CanonicalHeaderKey(s.AuthHeader)] {
			return fmt.Errorf("mcpServers %q: authHeader %q is not a header notify can send a secret in", s.Name, s.AuthHeader)
		}
	}
	return nil
}

// toolSet is the tools offered for one turn and what the turn has spent.
type toolSet struct {
	defs   []aibuilder.ToolDef
	routes map[string]route
	calls  int
	bytes  int
	turn   agentdomain.Turn
}

type route struct {
	server string
	tool   string
	client *mcp.Client
}

// openTools connects to the agent's servers and lists their tools. A server
// that cannot be listed is skipped: the turn goes on without its tools. Only
// a secret that cannot be opened fails the turn, since no retry fixes it.
func (e *Executor) openTools(ctx context.Context, cfg Config, t agentdomain.Turn) (*toolSet, error) {
	if len(cfg.MCPServers) == 0 {
		return nil, nil
	}
	ts := &toolSet{routes: map[string]route{}, turn: t}
	for _, s := range cfg.MCPServers {
		secret, err := e.open(s.AuthSecret, fmt.Sprintf("mcpServers %q secret", s.Name))
		if err != nil {
			return nil, err
		}
		h := http.Header{}
		if secret != "" {
			h.Set(s.AuthHeader, secret)
		}
		h.Set(headerAgentID, t.Agent.ID)
		if t.Conversation.SubscriberID != "" {
			h.Set(headerSubscriberID, t.Conversation.SubscriberID)
		}
		client := mcp.New(e.mcpHTTP, s.URL, h)
		lctx, cancel := context.WithTimeout(ctx, e.toolTimeout)
		tools, err := listTools(lctx, client)
		cancel()
		if err != nil {
			e.log.LogAttrs(ctx, slog.LevelWarn, "managed agent tool server unavailable",
				slog.String("agent_id", t.Agent.ID), slog.String("server", s.Name),
				slog.String("error_class", string(mcp.ClassOf(err))))
			continue
		}
		for _, tool := range tools {
			name := s.Name + "__" + tool.Name
			if !toolNamePattern.MatchString(name) {
				continue
			}
			if _, dup := ts.routes[name]; dup {
				continue
			}
			schema := tool.InputSchema
			if len(bytes.TrimSpace(schema)) == 0 || string(schema) == "null" {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			ts.routes[name] = route{server: s.Name, tool: tool.Name, client: client}
			ts.defs = append(ts.defs, aibuilder.ToolDef{Name: name, Description: tool.Description, InputSchema: schema})
		}
	}
	return ts, nil
}

func listTools(ctx context.Context, c *mcp.Client) ([]mcp.Tool, error) {
	if err := c.Initialize(ctx); err != nil {
		return nil, err
	}
	return c.ListTools(ctx)
}

// run answers the turn. With no tools it is exactly one model call. With
// tools, each round's calls are answered and fed back; the last round forbids
// tools so the agent always ends with an answer.
func (e *Executor) run(ctx context.Context, c aibuilder.Client, req aibuilder.MessageRequest, ts *toolSet) (aibuilder.MessageResponse, error) {
	if ts == nil || len(ts.defs) == 0 {
		return e.callModel(ctx, c, req)
	}
	req.Tools = ts.defs
	for round := 1; ; round++ {
		if round == maxModelRounds {
			req.ToolChoice = "none"
		}
		resp, err := e.callModel(ctx, c, req)
		if err != nil || len(resp.ToolCalls) == 0 || round == maxModelRounds {
			return resp, err
		}
		results := make([]aibuilder.ToolResult, len(resp.ToolCalls))
		for i, call := range resp.ToolCalls {
			results[i] = e.callTool(ctx, ts, call)
		}
		req.Messages = append(req.Messages,
			aibuilder.Message{Role: aibuilder.RoleAssistant, Text: resp.Text, ToolCalls: resp.ToolCalls},
			aibuilder.Message{Role: aibuilder.RoleUser, ToolResults: results})
	}
}

// callTool is the one place a turn calls a tool. It never fails the turn: a
// failure is answered to the model as a typed tool error.
func (e *Executor) callTool(ctx context.Context, ts *toolSet, call aibuilder.ToolCall) aibuilder.ToolResult {
	start := time.Now()
	var (
		st        mcp.Stats
		class     string
		truncated bool
		result    = aibuilder.ToolResult{ToolCallID: call.ID}
	)
	r, known := ts.routes[call.Name]
	args := bytes.TrimSpace(call.Input)
	if len(args) == 0 {
		args = []byte(`{}`)
	}
	switch {
	case !known:
		class, result = toolError(call.ID, mcp.Rejected, "no such tool")
	case !isObject(args):
		class, result = toolError(call.ID, mcp.Rejected, "arguments must be a JSON object")
	case ts.calls >= maxToolCalls:
		class, result = toolError(call.ID, mcp.Rejected, fmt.Sprintf("this turn has used its %d tool calls; answer with what you have", maxToolCalls))
	case maxTurnResultBytes-ts.bytes < minResultBytes:
		class, result = toolError(call.ID, mcp.TooLarge, "this turn's tool results are at their size limit; answer with what you have")
	default:
		ts.calls++
		cctx, cancel := context.WithTimeout(ctx, e.toolTimeout)
		out, stats, err := r.client.CallTool(cctx, r.tool, args)
		cancel()
		st = stats
		if err != nil {
			c := mcp.ClassOf(err)
			class, result = toolError(call.ID, c, err.Error())
			break
		}
		limit := min(maxResultBytes, maxTurnResultBytes-ts.bytes)
		if out.Structured != nil {
			result.Content, truncated = fitJSON(out.Structured, limit)
		} else {
			result.Content, truncated = fitText(out.Text, limit)
		}
		// Only tool output counts toward the turn budget; error results are
		// notify's own messages, bounded by maxErrorMessage.
		ts.bytes += len(result.Content)
		if out.IsError {
			result.IsError = true
			class = "tool_error"
		}
	}
	e.log.LogAttrs(ctx, slog.LevelInfo, "managed agent tool call",
		slog.String("agent_id", ts.turn.Agent.ID),
		slog.String("conversation_id", ts.turn.Conversation.ID),
		slog.String("server", r.server),
		slog.String("tool", call.Name),
		slog.String("call_id", call.ID),
		slog.Any("record_ids", recordIDs(args)),
		slog.Int("request_bytes", st.RequestBytes),
		slog.Int("response_bytes", st.ResponseBytes),
		slog.Int("result_bytes", len(result.Content)),
		slog.Bool("truncated", truncated),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		slog.String("error_class", class))
	return result
}

// maxErrorMessage bounds the message in a tool error, which may quote the
// server.
const maxErrorMessage = 512

func toolError(id string, c mcp.Class, msg string) (string, aibuilder.ToolResult) {
	if len(msg) > maxErrorMessage {
		msg = strings.ToValidUTF8(msg[:maxErrorMessage], "")
	}
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"class": string(c), "message": msg}})
	return string(c), aibuilder.ToolResult{ToolCallID: id, Content: string(body), IsError: true}
}

func isObject(raw []byte) bool {
	var m map[string]json.RawMessage
	return raw[0] == '{' && json.Unmarshal(raw, &m) == nil
}

// recordIDs picks the record ids out of a call's top-level arguments: only
// whitelisted keys, only id-shaped strings or numbers, so free text never
// reaches the log.
func recordIDs(args []byte) []string {
	var m map[string]json.RawMessage
	if json.Unmarshal(args, &m) != nil {
		return []string{}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if recordIDKeys[k] {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keyRank(keys[i]) < keyRank(keys[j]) })
	ids := []string{}
	add := func(raw json.RawMessage) {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			var n json.Number
			if json.Unmarshal(raw, &n) != nil {
				return
			}
			s = n.String()
		}
		if recordIDPattern.MatchString(s) && len(ids) < maxLoggedRecordIDs {
			ids = append(ids, s)
		}
	}
	for _, k := range keys {
		var list []json.RawMessage
		if json.Unmarshal(m[k], &list) == nil {
			for _, v := range list {
				add(v)
			}
			continue
		}
		add(m[k])
	}
	return ids
}

// keyRank orders the id keys so the log reads singular ids first.
func keyRank(k string) string {
	return strings.ToLower(strings.ReplaceAll(k, "_", "")) + "\x00" + k
}

// truncationOverhead is the size of the wrapper around a cut value:
// {"content":<value>,"truncated":true}.
var truncationOverhead = len(`{"content":,"truncated":true}`)

// fitText returns text as the model's tool result when it fits in limit
// bytes, otherwise a truncation wrapper holding as much of it as fits, cut on
// a rune boundary.
func fitText(text string, limit int) (string, bool) {
	if len(text) <= limit {
		return text, false
	}
	return wrapTruncated(text, limit)
}

// fitJSON returns raw compacted when it fits in limit bytes, otherwise a
// truncation wrapper holding as much of the value as fits without cutting
// any number, key or array element mid-way.
func fitJSON(raw json.RawMessage, limit int) (string, bool) {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) == nil && buf.Len() <= limit {
		return buf.String(), false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return wrapTruncated(nil, limit)
	}
	return wrapTruncated(v, limit)
}

func wrapTruncated(v any, limit int) (string, bool) {
	cut, ok := shrink(v, limit-truncationOverhead)
	if !ok {
		return `{"truncated":true}`, true
	}
	return string(encode(map[string]any{"truncated": true, "content": cut})), true
}

// encode marshals v without HTML escaping, so sizes measured are sizes sent.
func encode(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// shrink returns the largest leading part of v whose encoding fits budget.
func shrink(v any, budget int) (any, bool) {
	if len(encode(v)) <= budget {
		return v, true
	}
	switch x := v.(type) {
	case string:
		if budget < 2 {
			return nil, false
		}
		// Binary search on the rune count; each rune encodes to at least one byte.
		runes := []rune(x)
		lo, hi := 0, len(runes)
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if len(encode(string(runes[:mid]))) <= budget {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		s := string(runes[:lo])
		if !utf8.ValidString(s) {
			return nil, false
		}
		return s, true
	case []any:
		if budget < 2 {
			return nil, false
		}
		out := []any{}
		size := 2
		for _, el := range x {
			sep := 0
			if len(out) > 0 {
				sep = 1
			}
			if n := len(encode(el)); size+sep+n <= budget {
				out = append(out, el)
				size += sep + n
				continue
			}
			if part, ok := shrink(el, budget-size-sep); ok {
				out = append(out, part)
			}
			break
		}
		return out, true
	case map[string]any:
		if budget < 2 {
			return nil, false
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := map[string]any{}
		size := 2
		for _, k := range keys {
			sep := 0
			if len(out) > 0 {
				sep = 1
			}
			head := len(encode(k)) + 1
			if n := len(encode(x[k])); size+sep+head+n <= budget {
				out[k] = x[k]
				size += sep + head + n
				continue
			}
			if part, ok := shrink(x[k], budget-size-sep-head); ok {
				out[k] = part
			}
			break
		}
		return out, true
	}
	return nil, false
}

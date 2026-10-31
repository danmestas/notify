package managed

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/aibuilder"
)

const storedPrompt = "STORED-PROMPT"

// textResult is an MCP tool result carrying v as JSON text.
func textResult(v any) map[string]any {
	body, _ := json.Marshal(v)
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(body)}}}
}

func liveBody(instructions string, skills ...map[string]string) map[string]any {
	return map[string]any{"version": "v7", "instructions": instructions, "skills": skills}
}

func skillOf(name string) map[string]string {
	return map[string]string{"name": name, "whenToUse": "when " + name, "body": "BODY-OF-" + name}
}

// liveHarness points the agent at the vault's get_instructions tool, with a
// stored prompt to fall back on.
func liveHarness(t *testing.T, instr map[string]any, script func(int, aibuilder.MessageRequest) aibuilder.MessageResponse) *toolHarness {
	t.Helper()
	h := newToolHarness(t, script)
	h.vault.instr = instr
	var cfg Config
	if err := json.Unmarshal(h.turn.Agent.Managed, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.SystemPrompt = storedPrompt
	cfg.InstructionsFrom = &InstructionsSource{Server: "vault", Tool: "get_instructions"}
	h.turn.Agent.Managed, _ = json.Marshal(cfg)
	return h
}

func dispatchOK(t *testing.T, h *toolHarness) {
	t.Helper()
	if _, err := h.exec.Dispatch(context.Background(), h.turn); err != nil {
		t.Fatal(err)
	}
}

// TestInstructions_LiveReplacesStored: the live instructions are the system
// prompt, not appended to the stored one; the tool is called once with the
// agent id and does not count as a source.
func TestInstructions_LiveReplacesStored(t *testing.T) {
	h := liveHarness(t, textResult(liveBody("LIVE-PROMPT")), answer)
	reply, err := h.exec.Dispatch(context.Background(), h.turn)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.model.reqs[0].System; got != "LIVE-PROMPT" {
		t.Fatalf("system = %q", got)
	}
	if len(h.vault.args) != 1 || h.vault.args[0]["agentId"] != "agt_1" || len(h.vault.args[0]) != 1 {
		t.Fatalf("instructions call args = %v", h.vault.args)
	}
	if len(reply.Sources) != 0 {
		t.Fatalf("sources = %+v", reply.Sources)
	}
}

// TestInstructions_StructuredContent: a result with only structuredContent is read too.
func TestInstructions_StructuredContent(t *testing.T) {
	h := liveHarness(t, map[string]any{"content": []any{}, "structuredContent": liveBody("LIVE-STRUCT")}, answer)
	dispatchOK(t, h)
	if got := h.model.reqs[0].System; got != "LIVE-STRUCT" {
		t.Fatalf("system = %q", got)
	}
}

// TestInstructions_SkillIndexAndLoad: skills are indexed in the prompt and
// load_skill returns a body locally, counted and recorded as a skill source.
func TestInstructions_SkillIndexAndLoad(t *testing.T) {
	h := liveHarness(t, textResult(liveBody("LIVE-PROMPT", skillOf("triage"), skillOf("billing"))), func(n int, _ aibuilder.MessageRequest) aibuilder.MessageResponse {
		if n == 1 {
			return aibuilder.MessageResponse{ToolCalls: []aibuilder.ToolCall{call("c1", "load_skill", `{"name":"billing"}`)}}
		}
		return aibuilder.MessageResponse{Text: "done"}
	})
	reply, err := h.exec.Dispatch(context.Background(), h.turn)
	if err != nil {
		t.Fatal(err)
	}
	sys := h.model.reqs[0].System
	if !strings.HasPrefix(sys, "LIVE-PROMPT") || !strings.Contains(sys, "- triage: when triage\n") || !strings.Contains(sys, "- billing: when billing") || !strings.Contains(sys, "load_skill") {
		t.Fatalf("system = %q", sys)
	}
	if strings.Contains(sys, "BODY-OF") || strings.Contains(sys, storedPrompt) {
		t.Fatalf("system leaks a body or the stored prompt: %q", sys)
	}
	if got := offered(h); strings.Join(got, ",") != "vault__get_record,vault__search_vault,vault__fail,vault__slow,vault__big,vault__wide,load_skill" {
		t.Fatalf("offered = %v", got)
	}
	res := results(h.model.reqs[1])
	if len(res) != 1 || res[0].IsError || res[0].Content != "BODY-OF-billing" {
		t.Fatalf("results = %+v", res)
	}
	if h.vault.callCount() != 1 { // the instructions fetch only
		t.Fatalf("vault calls = %v", h.vault.calls)
	}
	if len(reply.Sources) != 1 || reply.Sources[0].Tool != "skill" || reply.Sources[0].Label != "billing" {
		t.Fatalf("sources = %+v", reply.Sources)
	}
}

// TestInstructions_UnknownSkill: an unknown name is a typed error result.
func TestInstructions_UnknownSkill(t *testing.T) {
	h := liveHarness(t, textResult(liveBody("LIVE-PROMPT", skillOf("triage"))), func(n int, _ aibuilder.MessageRequest) aibuilder.MessageResponse {
		if n == 1 {
			return aibuilder.MessageResponse{ToolCalls: []aibuilder.ToolCall{call("c1", "load_skill", `{"name":"nope"}`)}}
		}
		return aibuilder.MessageResponse{Text: "done"}
	})
	reply, err := h.exec.Dispatch(context.Background(), h.turn)
	if err != nil {
		t.Fatal(err)
	}
	res := results(h.model.reqs[1])
	if len(res) != 1 || !res[0].IsError || !strings.Contains(res[0].Content, `"class":"rejected"`) {
		t.Fatalf("results = %+v", res)
	}
	if len(reply.Sources) != 1 || !reply.Sources[0].Error {
		t.Fatalf("sources = %+v", reply.Sources)
	}
}

// TestInstructions_ToolNotOffered: the instructions tool is hidden from the
// model, and calling its alias is rejected without reaching the server.
func TestInstructions_ToolNotOffered(t *testing.T) {
	h := liveHarness(t, textResult(liveBody("LIVE-PROMPT")), func(n int, _ aibuilder.MessageRequest) aibuilder.MessageResponse {
		if n == 1 {
			return aibuilder.MessageResponse{ToolCalls: []aibuilder.ToolCall{call("c1", "vault__get_instructions", `{}`)}}
		}
		return aibuilder.MessageResponse{Text: "done"}
	})
	dispatchOK(t, h)
	for _, n := range offered(h) {
		if strings.Contains(n, "instructions") {
			t.Fatalf("offered = %v", offered(h))
		}
	}
	res := results(h.model.reqs[1])
	if len(res) != 1 || !res[0].IsError || !strings.Contains(res[0].Content, "no such tool") {
		t.Fatalf("results = %+v", res)
	}
	if h.vault.callCount() != 1 {
		t.Fatalf("vault calls = %v", h.vault.calls)
	}
}

// TestInstructions_FallsBack: whatever goes wrong, the stored prompt is used
// with no skills and the turn still answers.
func TestInstructions_FallsBack(t *testing.T) {
	big := strings.Repeat("a", 32<<10+1)
	var many []map[string]string
	for i := 0; i < 17; i++ {
		many = append(many, skillOf(fmt.Sprintf("s%d", i)))
	}
	longBody := skillOf("long")
	longBody["body"] = strings.Repeat("b", 16<<10+1)
	longWhen := skillOf("when")
	longWhen["whenToUse"] = strings.Repeat("é", 301)
	cases := map[string]struct {
		instr map[string]any
		class string
	}{
		"is error":        {map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "nothing live"}}}, "tool_error"},
		"malformed":       {map[string]any{"content": []any{map[string]any{"type": "text", "text": "{not json"}}}, "invalid"},
		"empty":           {textResult(liveBody("")), "invalid"},
		"big instruction": {textResult(liveBody(big)), "invalid"},
		"17 skills":       {textResult(liveBody("x", many...)), "invalid"},
		"bad name":        {textResult(liveBody("x", skillOf("Bad_Name"))), "invalid"},
		"dup name":        {textResult(liveBody("x", skillOf("a"), skillOf("a"))), "invalid"},
		"long body":       {textResult(liveBody("x", longBody)), "invalid"},
		"long whenToUse":  {textResult(liveBody("x", longWhen)), "invalid"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := liveHarness(t, tc.instr, answer)
			reply, err := h.exec.Dispatch(context.Background(), h.turn)
			if err != nil || reply.Parts[0].Text != "ok" {
				t.Fatalf("reply = %+v, %v", reply, err)
			}
			if h.model.reqs[0].System != storedPrompt {
				t.Fatalf("system = %q", h.model.reqs[0].System)
			}
			for _, n := range offered(h) {
				if n == "load_skill" {
					t.Fatalf("offered = %v", offered(h))
				}
			}
			if !strings.Contains(h.logs.String(), `"source":"fallback"`) || !strings.Contains(h.logs.String(), `"error_class":"`+tc.class+`"`) {
				t.Fatalf("log = %s", h.logs)
			}
		})
	}
}

// TestInstructions_ServerUnreachable: a dead server falls back to the stored prompt.
func TestInstructions_ServerUnreachable(t *testing.T) {
	h := liveHarness(t, textResult(liveBody("LIVE-PROMPT")), answer)
	var cfg Config
	_ = json.Unmarshal(h.turn.Agent.Managed, &cfg)
	cfg.MCPServers[0].URL = "http://127.0.0.1:1"
	h.turn.Agent.Managed, _ = json.Marshal(cfg)
	dispatchOK(t, h)
	if h.model.reqs[0].System != storedPrompt {
		t.Fatalf("system = %q", h.model.reqs[0].System)
	}
	if !strings.Contains(h.logs.String(), `"source":"fallback"`) || !strings.Contains(h.logs.String(), `"error_class":"unavailable"`) {
		t.Fatalf("log = %s", h.logs)
	}
}

// TestInstructions_AbsentLeavesRequestUnchanged: without instructionsFrom the
// model request is exactly what it was.
func TestInstructions_AbsentLeavesRequestUnchanged(t *testing.T) {
	h := newToolHarness(t, answer)
	h.vault.instr = textResult(liveBody("LIVE-PROMPT", skillOf("triage")))
	var cfg Config
	_ = json.Unmarshal(h.turn.Agent.Managed, &cfg)
	cfg.SystemPrompt = storedPrompt
	h.turn.Agent.Managed, _ = json.Marshal(cfg)
	dispatchOK(t, h)
	req := h.model.reqs[0]
	if req.System != storedPrompt {
		t.Fatalf("system = %q", req.System)
	}
	// The fetch tool is an ordinary tool here: nothing hides or adds any.
	if got := strings.Join(offered(h), ","); !strings.Contains(got, "vault__get_instructions") || strings.Contains(got, "load_skill") {
		t.Fatalf("offered = %v", got)
	}
	if h.vault.callCount() != 0 || strings.Contains(h.logs.String(), "managed agent instructions") {
		t.Fatalf("calls = %v, log = %s", h.vault.calls, h.logs)
	}
}

// TestInstructions_LogCarriesVersionNotBody: the log line names the version
// and never the instructions or a skill body.
func TestInstructions_LogCarriesVersionNotBody(t *testing.T) {
	h := liveHarness(t, textResult(liveBody("SECRET-INSTRUCTIONS", skillOf("triage"))), answer)
	dispatchOK(t, h)
	logs := h.logs.String()
	if !strings.Contains(logs, `"msg":"managed agent instructions"`) || !strings.Contains(logs, `"source":"live"`) ||
		!strings.Contains(logs, `"version":"v7"`) || !strings.Contains(logs, `"agent_id":"agt_1"`) {
		t.Fatalf("log = %s", logs)
	}
	if strings.Contains(logs, "SECRET-INSTRUCTIONS") || strings.Contains(logs, "BODY-OF") {
		t.Fatalf("log leaks a body: %s", logs)
	}
}

// TestParseConfig_InstructionsFrom: the source must name one of the agent's
// servers and a tool.
func TestParseConfig_InstructionsFrom(t *testing.T) {
	cfg := func(src string) json.RawMessage {
		return json.RawMessage(`{"provider":"openai","model":"m","mcpServers":[{"name":"vault","url":"http://127.0.0.1:1","authSecret":"tok-secret"}],"instructionsFrom":` + src + `}`)
	}
	c, err := ParseConfig(cfg(`{"server":"vault","tool":"get_instructions"}`), true)
	if err != nil || c.InstructionsFrom == nil || c.InstructionsFrom.Server != "vault" || c.InstructionsFrom.Tool != "get_instructions" {
		t.Fatalf("cfg = %+v, err = %v", c, err)
	}
	for _, src := range []string{`{"server":"other","tool":"t"}`, `{"server":"vault","tool":""}`, `{"tool":"t"}`} {
		_, err := ParseConfig(cfg(src), true)
		if err == nil || !strings.Contains(err.Error(), "instructionsFrom") || strings.Contains(err.Error(), "tok-secret") {
			t.Fatalf("%s: err = %v", src, err)
		}
	}
}

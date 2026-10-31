package managed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/runtime/managed/mcp"
	"github.com/danmestas/notify/internal/aibuilder"
)

// Live instructions come from one MCP tool each turn and replace the stored
// system prompt. Anything wrong with them falls back to the stored prompt;
// nothing is ever cut to fit.
const (
	loadSkillName = "load_skill"

	maxInstructionsBytes = 32 << 10
	maxSkills            = 16
	maxSkillBodyBytes    = 16 << 10
	maxWhenToUseRunes    = 300
	maxLoggedVersion     = 64

	// errInvalid is the error_class of a result that fails validation.
	errInvalid = "invalid"
)

var skillNamePattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

type skill struct {
	Name      string `json:"name"`
	WhenToUse string `json:"whenToUse"`
	Body      string `json:"body"`
}

// liveInstructions is the instructions tool's result.
type liveInstructions struct {
	Version      string  `json:"version"`
	Instructions string  `json:"instructions"`
	Skills       []skill `json:"skills"`
}

// systemPrompt is the system prompt for the turn: the live instructions with
// an index of their skills, else the stored prompt. When there are skills it
// also arms ts to serve load_skill. It logs one line and never fails the turn.
func (e *Executor) systemPrompt(ctx context.Context, cfg Config, t agentdomain.Turn, ts *toolSet) string {
	if cfg.InstructionsFrom == nil {
		return cfg.SystemPrompt
	}
	live, class := e.fetchInstructions(ctx, cfg.InstructionsFrom, t, ts)
	if class != "" {
		e.log.LogAttrs(ctx, slog.LevelWarn, "managed agent instructions",
			slog.String("agent_id", t.Agent.ID), slog.String("source", "fallback"),
			slog.String("error_class", class))
		return cfg.SystemPrompt
	}
	e.log.LogAttrs(ctx, slog.LevelInfo, "managed agent instructions",
		slog.String("agent_id", t.Agent.ID), slog.String("source", "live"),
		slog.String("version", truncateRunes(live.Version, maxLoggedVersion)))
	return ts.useSkills(live)
}

// fetchInstructions calls the instructions tool and validates its result. A
// non-empty class says why it cannot be used.
func (e *Executor) fetchInstructions(ctx context.Context, src *InstructionsSource, t agentdomain.Turn, ts *toolSet) (liveInstructions, string) {
	var live liveInstructions
	args, _ := json.Marshal(map[string]string{"agentId": t.Agent.ID})
	c, ok := ts.clients[src.Server]
	if !ok {
		// Its listing already failed this turn; don't wait on it twice.
		return live, string(mcp.Unavailable)
	}
	cctx, cancel := context.WithTimeout(ctx, e.toolTimeout)
	defer cancel()
	out, _, err := c.CallTool(cctx, src.Tool, args)
	if err != nil {
		return live, string(mcp.ClassOf(err))
	}
	if out.IsError {
		return live, "tool_error"
	}
	payload := []byte(out.Text)
	if strings.TrimSpace(out.Text) == "" {
		payload = out.Structured
	}
	if err := json.Unmarshal(payload, &live); err != nil || validateInstructions(live) != nil {
		return liveInstructions{}, errInvalid
	}
	return live, ""
}

func validateInstructions(l liveInstructions) error {
	if strings.TrimSpace(l.Instructions) == "" || len(l.Instructions) > maxInstructionsBytes {
		return errors.New("instructions are empty or over the limit")
	}
	if len(l.Skills) > maxSkills {
		return fmt.Errorf("more than %d skills", maxSkills)
	}
	seen := map[string]bool{}
	for _, s := range l.Skills {
		switch {
		case !skillNamePattern.MatchString(s.Name) || seen[s.Name]:
			return errors.New("skill name is invalid or used twice")
		case len(s.Body) > maxSkillBodyBytes:
			return errors.New("skill body over the limit")
		case utf8.RuneCountInString(s.WhenToUse) > maxWhenToUseRunes:
			return errors.New("skill whenToUse over the limit")
		}
		seen[s.Name] = true
	}
	return nil
}

// useSkills makes the live skills available to load_skill and returns the
// system prompt: the instructions, then one generated index of the skills.
func (ts *toolSet) useSkills(l liveInstructions) string {
	if len(l.Skills) == 0 {
		return l.Instructions
	}
	ts.skills = make(map[string]skill, len(l.Skills))
	var b strings.Builder
	b.WriteString(l.Instructions)
	b.WriteString("\n\nSkills you can load. Before following a skill, call " + loadSkillName + " with its name.\n")
	for _, s := range l.Skills {
		ts.skills[s.Name] = s
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.WhenToUse)
	}
	ts.defs = append(ts.defs, aibuilder.ToolDef{
		Name:        loadSkillName,
		Description: "Load the full text of one of the skills listed in your instructions.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
	})
	return strings.TrimRight(b.String(), "\n")
}

// loadSkill answers a load_skill call from the turn's skills, with no network
// call. An unknown name is a rejected call.
func (ts *toolSet) loadSkill(args []byte) (mcp.Result, error) {
	s, ok := ts.skills[skillArg(args)]
	if !ok {
		return mcp.Result{}, &mcp.Error{Class: mcp.Rejected, Err: errors.New("no such skill")}
	}
	return mcp.Result{Text: s.Body}, nil
}

func skillArg(args []byte) string {
	var a struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(args, &a)
	return a.Name
}

func skillSource(args []byte, failed bool) agentdomain.Source {
	return agentdomain.Source{Tool: "skill", Label: truncateRunes(skillArg(args), maxSourceLabel), Error: failed}
}

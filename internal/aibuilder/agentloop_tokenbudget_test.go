package aibuilder

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// TestTokenBudgetFromEnv asserts the NOTIFY_AI_TOKEN_BUDGET env-var parsing
// NewAgentLoopBuilder uses for its default: unset falls back to
// defaultTokenBudget, a valid non-negative integer overrides it, "0" disables
// the budget, and an unparsable or negative value falls back to the default
// rather than silently disabling the budget.
func TestTokenBudgetFromEnv(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want int
	}{
		{"unset uses default", "", defaultTokenBudget},
		{"explicit override", "5000", 5000},
		{"zero disables", "0", 0},
		{"unparsable falls back to default", "not-a-number", defaultTokenBudget},
		{"negative falls back to default", "-5", defaultTokenBudget},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			getenv := func(key string) string {
				if key == tokenBudgetEnvVar {
					return c.val
				}
				return ""
			}
			if got := tokenBudgetFromEnv(getenv); got != c.want {
				t.Errorf("tokenBudgetFromEnv(%q) = %d, want %d", c.val, got, c.want)
			}
		})
	}
}

// TestNewAgentLoopBuilder_TokenBudgetOptionOverridesEnvDefault asserts
// WithAgentLoopTokenBudget wins over the env-derived default set inside
// NewAgentLoopBuilder.
func TestNewAgentLoopBuilder_TokenBudgetOptionOverridesEnvDefault(t *testing.T) {
	b := NewAgentLoopBuilder(nil, nil, emailActive(), nil, WithAgentLoopTokenBudget(42))
	if b.tokenBudget != 42 {
		t.Errorf("tokenBudget = %d, want 42", b.tokenBudget)
	}
}

// TestAgentLoop_TokenBudgetExceeded_EmitsErrorAndTerminates proves that once
// accumulated InputTokens+OutputTokens reaches the configured budget, handle
// fails fast with a KindError event and a terminal error — well before
// DefaultMaxIterations — and that TokensUsed survives the checkpoint
// round-trip (iteration 1's starting counter carries iteration 0's spend).
func TestAgentLoop_TokenBudgetExceeded_EmitsErrorAndTerminates(t *testing.T) {
	s := openTempStore(t)
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-a"}
	sid := seedSession(t, s, scope, "build something")

	client := &fnClient{fn: func(call int, _ MessageRequest) MessageResponse {
		return MessageResponse{
			StopReason: "tool_use",
			ToolCalls: []ToolCall{
				tc(fmt.Sprintf("t%d", call), toolReasoning, `{"thought":"thinking"}`),
			},
			Usage: Usage{InputTokens: 1000, OutputTokens: 500},
		}
	}}

	b := NewAgentLoopBuilder(nil, client, emailActive(), s.BuilderSessions(), WithAgentLoopTokenBudget(2000))

	var (
		checkpoint []byte
		lastErr    error
		lc         *fakeLoopCtx
	)
	for i := 0; i < 5; i++ {
		lc = &fakeLoopCtx{iter: i, checkpoint: checkpoint, input: runInputJSON(t, scope, sid)}
		lastErr = b.handle(context.Background(), lc)
		if lastErr != nil {
			break
		}
		if lc.completed {
			t.Fatal("loop unexpectedly Completed")
		}
		checkpoint = lc.nextCheckpoint

		// Checkpoint round-trip: iteration 0's 1500-token spend (1000+500) must
		// survive into what iteration 1 decodes as its starting TokensUsed.
		if i == 0 {
			var st loopState
			if err := json.Unmarshal(checkpoint, &st); err != nil {
				t.Fatalf("decode checkpoint: %v", err)
			}
			if st.TokensUsed != 1500 {
				t.Fatalf("checkpoint TokensUsed = %d, want 1500", st.TokensUsed)
			}
		}
	}

	if lastErr == nil {
		t.Fatal("expected handle to fail once the token budget is exceeded")
	}
	if client.n > 2 {
		t.Fatalf("model called %d times, want <=2 (budget trips before a 3rd call)", client.n)
	}
	if !strings.Contains(lastErr.Error(), "token budget exceeded") {
		t.Fatalf("error = %v, want it to mention the token budget", lastErr)
	}

	var sawError bool
	for _, raw := range lc.streams {
		var ev streamEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("decode stream event: %v", err)
		}
		if ev.Kind == KindError {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("no KindError event streamed when the token budget was exceeded")
	}
}

// TestAgentLoop_TokenBudgetDisabled_ZeroNeverTrips asserts tokenBudget=0
// (explicitly disabled via WithAgentLoopTokenBudget(0)) never fails the run,
// no matter how much usage the model reports.
func TestAgentLoop_TokenBudgetDisabled_ZeroNeverTrips(t *testing.T) {
	s := openTempStore(t)
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-a"}
	sid := seedSession(t, s, scope, "welcome new users by email")

	client := &fnClient{fn: func(call int, _ MessageRequest) MessageResponse {
		switch call {
		case 1:
			return MessageResponse{
				StopReason: "tool_use",
				ToolCalls:  []ToolCall{tc("t1", toolSetWorkflowMetadata, `{"name":"Welcome"}`)},
				Usage:      Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
			}
		case 2:
			return MessageResponse{
				StopReason: "tool_use",
				ToolCalls:  []ToolCall{tc("t2", toolAddStep, `{"type":"email","name":"Welcome"}`)},
				Usage:      Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
			}
		default:
			return MessageResponse{StopReason: "end_turn", Text: "Done.", Usage: Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}}
		}
	}}

	// tokenBudget=0 (explicitly disabled) must never trip regardless of spend.
	b := NewAgentLoopBuilder(nil, client, emailActive(), s.BuilderSessions(), WithAgentLoopTokenBudget(0))
	var cur WorkflowDraft
	lc, _ := driveLoop(t, b, runInputJSON(t, scope, sid), &cur, 10)
	if !lc.completed {
		t.Fatal("loop with a disabled token budget did not Complete despite heavy simulated spend")
	}
}

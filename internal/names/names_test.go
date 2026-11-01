package names

import (
	"strings"
	"testing"
)

func TestNames_AllUnderPrefix(t *testing.T) {
	all := append(TaskTypes(), Workflows()...)
	all = append(all, Send("email"), AgentTurn("mesh"), Workflow("env", "slug"), AgentTurnWorkflow("a1"))
	for _, n := range all {
		if !strings.HasPrefix(n, "notify.") {
			t.Errorf("%q is not under notify.", n)
		}
	}
}

func TestNames_LiteralValues(t *testing.T) {
	cases := map[string]string{
		SendEmail:                "notify.send.email",
		AgentTurnMesh:            "notify.agent.turn.mesh",
		AIBuild:                  "notify.ai.build",
		CredentialVerifyWorkflow: "notify.credential-verify",
		AIBuilderWorkflow:        "notify.ai-builder",
		Workflow("env", "slug"):  "notify.env-slug",
		AgentTurnWorkflow("a1"):  "notify.agent-turn-a1",
		Send("sms"):              "notify.send.sms",
		AgentTurn("managed"):     "notify.agent.turn.managed",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestChannel_BothSpellings(t *testing.T) {
	for _, in := range []string{"notify.send.chat", "send.chat"} {
		if ch, ok := Channel(in); !ok || ch != "chat" {
			t.Errorf("Channel(%q) = %q, %v", in, ch, ok)
		}
	}
	if _, ok := Channel("notify.digest.wait"); ok {
		t.Error("digest.wait is not a send task")
	}
}

func TestUpgrade_Idempotent(t *testing.T) {
	if got := Upgrade("send.email"); got != "notify.send.email" {
		t.Errorf("got %q", got)
	}
	if got := Upgrade("notify.send.email"); got != "notify.send.email" {
		t.Errorf("got %q", got)
	}
	if got := Upgrade(""); got != "" {
		t.Errorf("got %q", got)
	}
}

// Package names is the single source for every dagnats task type and workflow
// name notify uses. dagnats admits a program only when each is "notify.<rest>"
// (program.Manifest), and a worker token for another program's prefix must not
// match notify's tasks, so no literal task type or workflow name lives outside
// this package.
package names

import "strings"

// Prefix is notify's program name root in dagnats.
const Prefix = "notify."

// Task types notify handles.
const (
	SendInApp  = Prefix + "send.in_app"
	SendEmail  = Prefix + "send.email"
	SendPush   = Prefix + "send.push"
	SendChat   = Prefix + "send.chat"
	SendSMS    = Prefix + "send.sms"
	SendStream = Prefix + "send.stream"

	DelayScheduled = Prefix + "delay.scheduled"
	DelayDynamic   = Prefix + "delay.dynamic"
	DigestWait     = Prefix + "digest.wait"
	CheckThrottle  = Prefix + "check.throttle"
	HTTPRequest    = Prefix + "http_request"
	AgentMessage   = Prefix + "agent_message"

	VerifyCredentials = Prefix + "verify.credentials"
	AgentTurnMesh     = Prefix + "agent.turn.mesh"
	AgentTurnManaged  = Prefix + "agent.turn.managed"
	AIBuild           = Prefix + "ai.build"
)

// Fixed workflow names notify registers. Per-tenant and per-agent workflows are
// built with Workflow and AgentTurnWorkflow.
const (
	CredentialVerifyWorkflow = Prefix + "credential-verify"
	AIBuilderWorkflow        = Prefix + "ai-builder"
)

// TaskTypes lists every task type notify handles, in Manifest order.
func TaskTypes() []string {
	return []string{
		SendInApp, SendEmail, SendPush, SendChat, SendSMS, SendStream,
		DelayScheduled, DelayDynamic, DigestWait, CheckThrottle,
		HTTPRequest, AgentMessage,
		VerifyCredentials, AgentTurnMesh, AgentTurnManaged, AIBuild,
	}
}

// Workflows lists the fixed workflow names notify registers.
func Workflows() []string {
	return []string{CredentialVerifyWorkflow, AIBuilderWorkflow}
}

// Send is the task type of the channel send step ("email" -> notify.send.email).
func Send(channel string) string { return Prefix + "send." + channel }

// Channel reports the channel of a send task type, in either the current or the
// pre-prefix spelling.
func Channel(taskType string) (string, bool) {
	return strings.CutPrefix(strings.TrimPrefix(taskType, Prefix), "send.")
}

// AgentTurn is the task type of an agent turn for a runtime.
func AgentTurn(runtime string) string { return Prefix + "agent.turn." + runtime }

// Workflow is the engine workflow name of an authored workflow: the
// env-scoped trigger identifier. '-' joins them because dagnats maps the name
// onto a NATS KV key, which rejects ':'.
func Workflow(envID, triggerIdentifier string) string {
	return Prefix + envID + "-" + triggerIdentifier
}

// AgentTurnWorkflow is the per-agent turn workflow name.
func AgentTurnWorkflow(agentID string) string { return Prefix + "agent-turn-" + agentID }

// Legacy returns the pre-prefix spelling of a task type.
func Legacy(taskType string) string { return strings.TrimPrefix(taskType, Prefix) }

// Upgrade returns name with the prefix added when it lacks it. It is how a
// definition persisted before the prefix existed is read.
func Upgrade(name string) string {
	if name == "" || strings.HasPrefix(name, Prefix) {
		return name
	}
	return Prefix + name
}

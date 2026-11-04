package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nats-io/nats.go"
)

// ErrConfigUnavailable covers an agent that is not listening, does not answer
// in time, or answers something unreadable.
var ErrConfigUnavailable = errors.New("agent config endpoint unavailable")

// ConfigError is an agent's refusal of a config change: the reply's
// {ok:false, error:{code, message}}. Code is "invalid" or "forbidden".
type ConfigError struct {
	Code    string
	Message string
}

func (e *ConfigError) Error() string { return "agent config " + e.Code + ": " + e.Message }

// ConfigSubject returns the config subject for an agent's prompt subject
// (agents.prompt.<tail> becomes agents.config.<tail>), or "" when meshSubject
// is not a prompt subject.
func ConfigSubject(meshSubject string) string {
	tail, ok := strings.CutPrefix(meshSubject, promptRoot)
	if !ok || tail == "" {
		return ""
	}
	return configRoot + tail
}

// Configure relays body, opaque JSON, to the config endpoint of the agent that
// listens on meshSubject and returns the card from its reply. body may carry
// a provider key, so it is never logged or put in an error.
func (c *Client) Configure(ctx context.Context, meshSubject string, body []byte) (json.RawMessage, error) {
	subj := ConfigSubject(meshSubject)
	if subj == "" || !SubjectAllowed(c.allow, meshSubject) {
		return nil, fmt.Errorf("mesh subject %q is not a configurable prompt subject", meshSubject)
	}
	if len(body) > MaxConfigBytes {
		return nil, &ConfigError{Code: "invalid", Message: "config body too large"}
	}
	ctx, cancel := context.WithTimeout(ctx, c.configTimeout)
	defer cancel()
	resp, err := c.nc.RequestWithContext(ctx, subj, body)
	if err != nil {
		if errors.Is(err, nats.ErrNoResponders) {
			return nil, fmt.Errorf("%w: no agent on %s", ErrConfigUnavailable, subj)
		}
		return nil, fmt.Errorf("%w: %v", ErrConfigUnavailable, err)
	}
	var reply struct {
		OK    bool            `json:"ok"`
		Card  json.RawMessage `json:"card"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.Data, &reply); err != nil {
		return nil, fmt.Errorf("%w: unreadable reply", ErrConfigUnavailable)
	}
	if reply.OK {
		return reply.Card, nil
	}
	if reply.Error == nil {
		return nil, fmt.Errorf("%w: reply carries neither ok nor error", ErrConfigUnavailable)
	}
	switch reply.Error.Code {
	case "invalid", "forbidden":
		return nil, &ConfigError{Code: reply.Error.Code, Message: reply.Error.Message}
	}
	return nil, fmt.Errorf("%w: %s", ErrConfigUnavailable, reply.Error.Message)
}

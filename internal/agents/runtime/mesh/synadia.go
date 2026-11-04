package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/notify/internal/agents/domain"
)

// Turn identity rides in NATS headers: the v0.3 spec defines no turn-identity
// field, and headers leave the {prompt} body exactly as the spec shapes it.
// Agents that do not know them ignore them.
const (
	headerContextID    = "Notify-Context-Id"
	headerMessageID    = "Notify-Message-Id"
	headerSubscriberID = "Notify-Subscriber-Id"
)

// synadiaRequest is the v0.3 request envelope (spec §5.1). Attachments are
// not sent: a notify turn carries only text, which rides in the prompt.
type synadiaRequest struct {
	Prompt string `json:"prompt"`
}

// synadiaChunk is one typed response-stream chunk (spec §6.2).
type synadiaChunk struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// dispatchSynadia runs one turn against a v0.3 agent: it publishes {prompt}
// with a private reply inbox and consumes the typed chunk stream until the
// empty terminator. Response chunks concatenate into the answer; status
// chunks (other than the "ack" lifecycle token) become Reply.Status. A query
// chunk is a permanent error: mid-stream queries are not supported yet.
//
// History follows the agent's setting, as for a2a: inline sends the
// transcript in the prompt, none sends the new message only (v0.3 harnesses
// hold their own session per subject).
func (c *Client) dispatchSynadia(ctx context.Context, t domain.Turn) (domain.Reply, error) {
	subj := t.Agent.MeshSubject
	prompt := Prompt(t)
	if t.Agent.HistoryMode() == domain.HistoryNone {
		prompt = contentText(t.Message.Content)
	}
	body, err := json.Marshal(synadiaRequest{Prompt: prompt})
	if err != nil {
		return domain.Reply{}, domain.NewPermanentError(fmt.Errorf("marshal request: %w", err))
	}

	inbox := replyRoot + "." + randToken()
	chunks := make(chan *nats.Msg, 256)
	sub, err := c.nc.ChanSubscribe(inbox, chunks)
	if err != nil {
		return domain.Reply{}, fmt.Errorf("subscribe reply inbox: %w", err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := c.nc.Flush(); err != nil {
		return domain.Reply{}, fmt.Errorf("flush subscriptions: %w", err)
	}

	msg := nats.NewMsg(subj)
	msg.Reply = inbox
	msg.Data = body
	setHeader(msg, headerContextID, t.Conversation.ID)
	setHeader(msg, headerMessageID, t.Message.ID)
	setHeader(msg, headerSubscriberID, t.Conversation.SubscriberID)
	if err := c.nc.PublishMsg(msg); err != nil {
		return domain.Reply{}, fmt.Errorf("mesh dispatch %s: %w", subj, err)
	}

	dctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return readSynadiaStream(dctx, chunks, subj, c.timeout)
}

// setHeader sets a header only when the value is non-empty.
func setHeader(m *nats.Msg, k, v string) {
	if v != "" {
		m.Header.Set(k, v)
	}
}

// readSynadiaStream consumes chunks until the terminator, a service error,
// the deadline, or an unsupported chunk.
func readSynadiaStream(ctx context.Context, chunks <-chan *nats.Msg, subj string, timeout time.Duration) (domain.Reply, error) {
	var (
		answer strings.Builder
		status []string
	)
	for {
		select {
		case <-ctx.Done():
			return domain.Reply{}, fmt.Errorf("no complete answer from %s within %s", subj, timeout)
		case m := <-chunks:
			if m.Header.Get("Status") == "503" {
				return domain.Reply{}, fmt.Errorf("mesh dispatch %s: %w", subj, nats.ErrNoResponders)
			}
			if code := m.Header.Get("Nats-Service-Error-Code"); code != "" && code != "200" {
				se := &ServiceError{Code: code, Detail: m.Header.Get("Nats-Service-Error")}
				if strings.HasPrefix(code, "4") {
					return domain.Reply{}, domain.NewPermanentError(se)
				}
				return domain.Reply{}, fmt.Errorf("mesh dispatch %s: %w", subj, se)
			}
			if len(m.Data) == 0 && len(m.Header) == 0 {
				return finishSynadia(answer.String(), status, subj)
			}
			var ch synadiaChunk
			if json.Unmarshal(m.Data, &ch) != nil {
				continue // not a typed chunk: ignore, per forward-compat (§6.6)
			}
			switch ch.Type {
			case "response":
				answer.WriteString(chunkText(ch.Data))
			case "status":
				var s string
				if json.Unmarshal(ch.Data, &s) == nil && s != "" && s != "ack" {
					status = append(status, s)
				}
			case "query":
				return domain.Reply{}, domain.NewPermanentError(errors.New(
					"agent sent a query chunk (mid-stream queries are not supported)",
				))
			}
		}
	}
}

// finishSynadia turns a terminated stream into a Reply.
func finishSynadia(answer string, status []string, subj string) (domain.Reply, error) {
	text := strings.TrimSpace(answer)
	if text == "" {
		return domain.Reply{}, domain.NewPermanentError(
			fmt.Errorf("agent %s ended the stream with no response", subj),
		)
	}
	return domain.Reply{Parts: []domain.Part{{Text: text}}, Status: status}, nil
}

// chunkText reads a response chunk's data: a bare string or {text} (§6.3).
func chunkText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return o.Text
	}
	return ""
}

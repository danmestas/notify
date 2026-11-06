package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/providers"
)

// refusingEmail is an EmailProvider that fails every send with err.
type refusingEmail struct{ err error }

func (r refusingEmail) Send(context.Context, providers.EmailMessage) (providers.Result, error) {
	return providers.Result{}, r.err
}

// A failed send records the class of the provider's error beside its text,
// with the integration that sent it (#100).
func TestHandleEmail_FailedSendRecordsErrorClass(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	sendErr := fmt.Errorf("cloudflare email send: status 401: %s", `{"errors":[{"message":"Authentication error"}]}`)
	h := NewEmail(s, stubResolver{provider: refusingEmail{sendErr}, from: "noreply@acme.io", integrationID: "integ-cf"})

	input, _ := json.Marshal(emailEnvelope())
	if _, err := h.HandleEmail(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleEmail: %v", err)
	}
	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_1", "email")
	if err != nil || len(msgs) != 1 {
		t.Fatalf("messages = %v, err = %v", msgs, err)
	}
	m := msgs[0]
	if m.Status != "error" || m.ErrorClass != "auth" || m.ErrorText != sendErr.Error() || m.IntegrationID != "integ-cf" {
		t.Fatalf("message = status %q class %q text %q integration %q", m.Status, m.ErrorClass, m.ErrorText, m.IntegrationID)
	}
}

func TestHandleEmail_SentMessageHasNoErrorClass(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := NewEmail(s, stubResolver{provider: providers.NewCapture(), from: "noreply@acme.io"})
	input, _ := json.Marshal(emailEnvelope())
	if _, err := h.HandleEmail(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleEmail: %v", err)
	}
	msgs, _ := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_1", "email")
	if len(msgs) != 1 || msgs[0].ErrorClass != "" {
		t.Fatalf("messages = %+v", msgs)
	}
}

package credverify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
)

// recordingObserver records every verdict it hears.
type recordingObserver struct {
	calls []string
}

func (o *recordingObserver) CredentialChecked(_ context.Context, scope domain.Scope, id string, ok bool, detail string) {
	o.calls = append(o.calls, fmt.Sprintf("%s/%s/%s ok=%v %s", scope.OrgID, scope.EnvID, id, ok, detail))
}

func TestHandler_ReportsVerdictToObserver(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  connectivity.VerifyResult
		want []string
	}{
		{"pass", connectivity.VerifyResult{OK: true, Detail: "token accepted"}, []string{"o/e/int-1 ok=true token accepted"}},
		{"rejected", connectivity.VerifyResult{Detail: "credentials rejected (status 401)"}, []string{"o/e/int-1 ok=false credentials rejected (status 401)"}},
		{"unsupported", connectivity.VerifyResult{Unsupported: true, Detail: "unsupported provider for verification: twilio"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := &recordingObserver{}
			svc := New(&stubEngine{}, &stubVerifier{res: tc.res}).WithObserver(obs)
			input, _ := json.Marshal(runInput{OrgID: "o", EnvID: "e", IntegrationID: "int-1"})
			if _, err := svc.Handler(context.Background(), engine.TaskPayload{Input: input}); err != nil {
				t.Fatalf("Handler: %v", err)
			}
			if fmt.Sprint(obs.calls) != fmt.Sprint(tc.want) {
				t.Fatalf("observer heard %q, want %q", obs.calls, tc.want)
			}
		})
	}
}

func TestHandler_ProbeFailureIsNotReported(t *testing.T) {
	obs := &recordingObserver{}
	svc := New(&stubEngine{}, &stubVerifier{err: errors.New("decrypt failed")}).WithObserver(obs)
	input, _ := json.Marshal(runInput{IntegrationID: "int-1"})
	if _, err := svc.Handler(context.Background(), engine.TaskPayload{Input: input}); err == nil {
		t.Fatal("probe failure should error")
	}
	if len(obs.calls) != 0 {
		t.Fatalf("observer heard %q for an incomplete probe", obs.calls)
	}
}

// startSignal is an Engine whose StartRun reports each start on a channel.
type startSignal struct {
	stubEngine
	started chan json.RawMessage
}

func (s *startSignal) StartRun(ctx context.Context, _ string, input json.RawMessage) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	s.started <- input
	return "run-1", nil
}

func TestCheckSoon_StartsAVerifyRunThatOutlivesTheRequest(t *testing.T) {
	eng := &startSignal{started: make(chan json.RawMessage, 1)}
	svc := New(eng, &stubVerifier{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the HTTP request that changed the credentials has returned
	svc.CheckSoon(ctx, domain.Scope{OrgID: "o", EnvID: "e"}, "int-9")
	select {
	case in := <-eng.started:
		var got runInput
		if err := json.Unmarshal(in, &got); err != nil {
			t.Fatal(err)
		}
		if got != (runInput{OrgID: "o", EnvID: "e", IntegrationID: "int-9"}) {
			t.Fatalf("run input = %+v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no verify run started")
	}
}

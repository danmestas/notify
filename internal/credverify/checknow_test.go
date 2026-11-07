package credverify

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/domain"
)

func TestCheckNow_ReportsVerdictBeforeReturning(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  connectivity.VerifyResult
		want []string
	}{
		{"pass", connectivity.VerifyResult{OK: true, Detail: "token accepted"}, []string{"o/e/int-1 ok=true token accepted"}},
		{"rejected", connectivity.VerifyResult{Detail: "rejected (status 401)"}, []string{"o/e/int-1 ok=false rejected (status 401)"}},
		{"unsupported", connectivity.VerifyResult{Unsupported: true}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := &recordingObserver{}
			svc := New(&stubEngine{}, &stubVerifier{res: tc.res}).WithObserver(obs)
			res, err := svc.CheckNow(context.Background(), domain.Scope{OrgID: "o", EnvID: "e"}, "int-1")
			if err != nil {
				t.Fatalf("CheckNow: %v", err)
			}
			if res.OK != tc.res.OK || res.Unsupported != tc.res.Unsupported {
				t.Fatalf("result = %+v, want %+v", res, tc.res)
			}
			if fmt.Sprint(obs.calls) != fmt.Sprint(tc.want) {
				t.Fatalf("observer heard %q, want %q", obs.calls, tc.want)
			}
		})
	}
}

func TestCheckNow_ProbeFailureIsAnErrorAndNotReported(t *testing.T) {
	obs := &recordingObserver{}
	svc := New(&stubEngine{}, &stubVerifier{err: errors.New("decrypt failed")}).WithObserver(obs)
	if _, err := svc.CheckNow(context.Background(), domain.Scope{}, "int-1"); err == nil {
		t.Fatal("CheckNow should return the probe error")
	}
	if len(obs.calls) != 0 {
		t.Fatalf("observer heard %q for an incomplete probe", obs.calls)
	}
}

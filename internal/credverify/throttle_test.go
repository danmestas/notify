package credverify

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/domain"
)

// countingVerifier counts probes; when block is non-nil each probe waits on it.
type countingVerifier struct {
	calls   atomic.Int32
	res     connectivity.VerifyResult
	err     error
	block   chan struct{}
	started chan struct{}
}

func (v *countingVerifier) VerifyByID(context.Context, domain.Scope, string) (connectivity.VerifyResult, error) {
	v.calls.Add(1)
	if v.started != nil {
		select {
		case v.started <- struct{}{}:
		default:
		}
	}
	if v.block != nil {
		<-v.block
	}
	return v.res, v.err
}

func TestCheckNow_WithinIntervalReturnsLastVerdictWithoutProbing(t *testing.T) {
	ver := &countingVerifier{res: connectivity.VerifyResult{Detail: "rejected (status 401)"}}
	obs := &recordingObserver{}
	svc := New(&stubEngine{}, ver).WithObserver(obs)
	now := time.Unix(1000, 0)
	svc.now = func() time.Time { return now }
	scope := domain.Scope{OrgID: "o", EnvID: "e"}

	for i := 0; i < 4; i++ {
		res, err := svc.CheckNow(context.Background(), scope, "int-1")
		if err != nil || res.Detail != "rejected (status 401)" {
			t.Fatalf("call %d: res=%+v err=%v", i, res, err)
		}
	}
	if n := ver.calls.Load(); n != 1 || len(obs.calls) != 1 {
		t.Fatalf("probes=%d reports=%d, want 1 and 1", n, len(obs.calls))
	}

	// Another integration, and another environment, are separate.
	if _, err := svc.CheckNow(context.Background(), scope, "int-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CheckNow(context.Background(), domain.Scope{OrgID: "o", EnvID: "e2"}, "int-1"); err != nil {
		t.Fatal(err)
	}
	if n := ver.calls.Load(); n != 3 {
		t.Fatalf("probes=%d, want 3", n)
	}

	// Once the interval has passed it probes again.
	now = now.Add(MinCheckInterval)
	if _, err := svc.CheckNow(context.Background(), scope, "int-1"); err != nil {
		t.Fatal(err)
	}
	if n := ver.calls.Load(); n != 4 {
		t.Fatalf("probes=%d after the interval, want 4", n)
	}
}

func TestCheckNow_ConcurrentCallsShareOneProbe(t *testing.T) {
	ver := &countingVerifier{res: connectivity.VerifyResult{OK: true}, block: make(chan struct{}), started: make(chan struct{}, 1)}
	svc := New(&stubEngine{}, ver).WithMinCheckInterval(0) // only singleflight can dedupe
	scope := domain.Scope{OrgID: "o", EnvID: "e"}

	var wg sync.WaitGroup
	results := make(chan bool, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := svc.CheckNow(context.Background(), scope, "int-1")
			results <- err == nil && res.OK
		}()
	}
	<-ver.started
	time.Sleep(50 * time.Millisecond) // let the other callers join the flight
	close(ver.block)
	wg.Wait()
	close(results)
	for ok := range results {
		if !ok {
			t.Fatal("a concurrent caller did not get the shared verdict")
		}
	}
	if n := ver.calls.Load(); n != 1 {
		t.Fatalf("probes=%d for 8 concurrent checks, want 1", n)
	}
}

func TestCheckNow_ErrorsAreNotRemembered(t *testing.T) {
	ver := &countingVerifier{err: errors.New("decrypt failed")}
	svc := New(&stubEngine{}, ver)
	for i := 0; i < 2; i++ {
		if _, err := svc.CheckNow(context.Background(), domain.Scope{}, "int-1"); err == nil {
			t.Fatal("want the probe error")
		}
	}
	if n := ver.calls.Load(); n != 2 {
		t.Fatalf("probes=%d, want 2: a failed probe must not throttle the retry", n)
	}
}

func TestCheckSoon_ForgetsTheLastVerdict(t *testing.T) {
	ver := &countingVerifier{res: connectivity.VerifyResult{OK: true}}
	svc := New(&stubEngine{runID: "r"}, ver)
	scope := domain.Scope{OrgID: "o", EnvID: "e"}
	if _, err := svc.CheckNow(context.Background(), scope, "int-1"); err != nil {
		t.Fatal(err)
	}
	// Credentials changed: the next check must probe, not reuse the verdict.
	svc.forget(scope, "int-1")
	if _, err := svc.CheckNow(context.Background(), scope, "int-1"); err != nil {
		t.Fatal(err)
	}
	if n := ver.calls.Load(); n != 2 {
		t.Fatalf("probes=%d, want 2", n)
	}
}

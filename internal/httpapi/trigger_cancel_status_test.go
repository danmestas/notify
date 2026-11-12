package httpapi

import (
	"net/http"
	"testing"

	"github.com/danmestas/notify/internal/trigger"
)

// TestCancelFailureStatus: the least retryable failure of a partial cancel
// decides its status.
func TestCancelFailureStatus(t *testing.T) {
	f := func(reasons ...string) []trigger.CancelFailure {
		out := make([]trigger.CancelFailure, len(reasons))
		for i, r := range reasons {
			out[i] = trigger.CancelFailure{NotificationID: "n", Reason: r}
		}
		return out
	}
	tests := []struct {
		failed []trigger.CancelFailure
		want   int
	}{
		{f("engine_unavailable"), http.StatusServiceUnavailable},
		{f("engine_unavailable", "engine_error"), http.StatusBadGateway},
		{f("engine_error", "internal"), http.StatusInternalServerError},
	}
	for _, tc := range tests {
		if got := cancelFailureStatus(tc.failed); got != tc.want {
			t.Errorf("cancelFailureStatus(%v) = %d, want %d", tc.failed, got, tc.want)
		}
	}
}

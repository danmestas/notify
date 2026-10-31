package aibuilder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// TestAnthropicClientMessage_BoundedAcrossRetries proves one model call has a
// whole-call bound, not just a per-request one: the SDK retries an overloaded
// API and honours any Retry-After the server sends, so without the bound a
// call could wait as long as the server asked. Task max-run-times are derived
// from MaxModelCallDuration, so it has to hold.
func TestAnthropicClientMessage_BoundedAcrossRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(529)
	}))
	defer srv.Close()

	prev := modelCallMax
	modelCallMax = time.Second
	t.Cleanup(func() { modelCallMax = prev })

	c := &anthropicClient{sdk: anthropic.NewClient(option.WithAPIKey("sk-test"), option.WithBaseURL(srv.URL))}
	begin := time.Now()
	if _, err := c.Message(context.Background(), MessageRequest{Model: DefaultModel, Messages: []Message{{Role: RoleUser, Text: "hi"}}}); err == nil {
		t.Fatal("Message succeeded against an overloaded API")
	}
	if took := time.Since(begin); took > 5*time.Second {
		t.Fatalf("Message took %v; the call must end at its bound", took)
	}
}

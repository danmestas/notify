package main_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/store"
)

// TestFullJourney_RealBinary is the whole-system validation: it builds and boots
// the actual `notify` binary as a server process against a real dagnats sidecar
// and a shared SQLite database, then drives a complete
//
//	subscriber -> trigger -> delivery -> inbox -> analytics
//
// journey over real HTTP. The single rendered-message assertion exercises
// Handlebars + i18n ({{t}}) + layouts composed together, and the analytics
// assertions exercise both /v1/analytics/deliveries and /v1/analytics/timeseries
// served by the running binary. Control-plane setup (tenant bootstrap, i18n,
// layout, workflow authoring/publish) is done in-process because authoring is a
// library/control-plane concern; the data plane runs entirely through the
// deployed artifact. No mocks: real binary, real sidecar, real SQLite.
func TestFullJourney_RealBinary(t *testing.T) {
	natsURL, bridgeURL := dagtest.StartSidecar(t)
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "notify.db")

	// One master key shared by the in-process bootstrap and the serve binary so
	// the AES-GCM-encrypted API key minted here decrypts inside the binary.
	keyRaw := make([]byte, 32)
	if _, err := rand.Read(keyRaw); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	keyHex := hex.EncodeToString(keyRaw)

	apiKey := seedTenant(t, ctx, dbPath, keyRaw, natsURL, bridgeURL)

	// Boot the real artifact.
	addr := freeAddr(t)
	bin := buildNotify(t)
	serve := exec.Command(bin, "serve",
		"--db", dbPath, "--nats-url", natsURL, "--bridge-url", bridgeURL, "--addr", addr)
	serve.Env = append(os.Environ(), "NOTIFY_MASTER_KEY_HEX="+keyHex)
	serve.Stdout = os.Stderr
	serve.Stderr = os.Stderr
	if err := serve.Start(); err != nil {
		t.Fatalf("start serve binary: %v", err)
	}
	t.Cleanup(func() {
		_ = serve.Process.Kill()
		_ = serve.Wait()
	})

	base := "http://" + addr
	authHeader := "ApiKey " + apiKey
	waitServeReady(t, base, authHeader)

	// Subscriber with a Spanish locale (drives the {{t}} translation).
	if st, body := doReq(t, http.MethodPost, base+"/v1/subscribers", authHeader,
		`{"subscriberId":"e2e_user","firstName":"Ada","locale":"es"}`); st != http.StatusCreated {
		t.Fatalf("POST /v1/subscribers: status=%d body=%s", st, body)
	}

	// Trigger the workflow twice (two delivery observations for analytics).
	for _, tx := range []string{"tx1", "tx2"} {
		body := `{"name":"welcome","to":{"subscriberId":"e2e_user"},"payload":{"itemCount":3},"transactionId":"` + tx + `"}`
		if st, rb := doReq(t, http.MethodPost, base+"/v1/events/trigger", authHeader, body); st != http.StatusCreated {
			t.Fatalf("POST /v1/events/trigger %s: status=%d body=%s", tx, st, rb)
		}
	}

	// Poll the in-app inbox until the rendered message lands.
	var msg domain.Message
	deadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		st, body := doReq(t, http.MethodGet, base+"/v1/messages?subscriberId=e2e_user&channel=in_app", authHeader, "")
		if st != http.StatusOK {
			t.Fatalf("GET /v1/messages: status=%d body=%s", st, body)
		}
		var msgs []domain.Message
		if err := json.Unmarshal([]byte(body), &msgs); err != nil {
			t.Fatalf("decode messages: %v (body=%s)", err, body)
		}
		if len(msgs) > 0 {
			msg = msgs[0]
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if msg.ID == "" {
		t.Fatal("no in_app message delivered by the serve binary within 35s")
	}
	// Handlebars ({{subscriber.firstName}}) + i18n ({{t "greeting"}} -> "Hola"
	// for locale es) + layout ("[ {{content}} ]") composed through the binary.
	if msg.Content != "[ Hola Ada ]" {
		t.Errorf("rendered in_app content = %q, want %q", msg.Content, "[ Hola Ada ]")
	}
	if msg.Channel != "in_app" {
		t.Errorf("message channel = %q, want in_app", msg.Channel)
	}

	// Analytics: counts endpoint must observe both sends.
	var counts map[string]map[string]int
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		st, body := doReq(t, http.MethodGet, base+"/v1/analytics/deliveries", authHeader, "")
		if st != http.StatusOK {
			t.Fatalf("GET /v1/analytics/deliveries: status=%d body=%s", st, body)
		}
		if err := json.Unmarshal([]byte(body), &counts); err != nil {
			t.Fatalf("decode analytics: %v (body=%s)", err, body)
		}
		if counts["in_app"]["sent"] >= 2 {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	if counts["in_app"]["sent"] < 2 {
		t.Errorf("analytics in_app/sent = %d, want >= 2", counts["in_app"]["sent"])
	}

	// Analytics: time-series endpoint must include today's in_app/sent bucket.
	st, body := doReq(t, http.MethodGet, base+"/v1/analytics/timeseries", authHeader, "")
	if st != http.StatusOK {
		t.Fatalf("GET /v1/analytics/timeseries: status=%d body=%s", st, body)
	}
	var series []domain.DeliveryDayCount
	if err := json.Unmarshal([]byte(body), &series); err != nil {
		t.Fatalf("decode timeseries: %v (body=%s)", err, body)
	}
	var tsInAppSent int
	for _, b := range series {
		if b.Channel == "in_app" && b.Status == "sent" {
			tsInAppSent += b.Count
		}
	}
	if tsInAppSent < 2 {
		t.Errorf("timeseries in_app/sent = %d, want >= 2 (body=%s)", tsInAppSent, body)
	}
	t.Logf("full journey OK: content=%q deliveries=%v timeseries=%s", msg.Content, counts, body)
}

// seedTenant provisions the tenant (org+env+API key), i18n + layout rows, and an
// in_app workflow whose body uses Handlebars + a {{t}} translation wrapped in a
// layout, then registers the DAG with the sidecar. It returns the API key. The
// store and engine are released before the serve binary opens the same DB.
func seedTenant(t *testing.T, ctx context.Context, dbPath string, keyRaw []byte, natsURL, bridgeURL string) string {
	t.Helper()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	apiKey, scope, err := identity.New(st, keyRaw).Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := st.Translations().Upsert(ctx, scope, "es", "greeting", "Hola"); err != nil {
		t.Fatalf("translation upsert: %v", err)
	}
	if err := st.Layouts().Upsert(ctx, scope, "default", "Default", "[ {{content}} ]"); err != nil {
		t.Fatalf("layout upsert: %v", err)
	}

	eng, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	auth := authoring.New(st, eng)
	controls := json.RawMessage(`{"subject":"Hi","body":"{{t \"greeting\"}} {{subscriber.firstName}}","layoutKey":"default"}`)
	if _, err := auth.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Welcome",
		TriggerIdentifier: "welcome",
		Steps:             []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: controls}},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := auth.Publish(ctx, scope, "welcome"); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}
	// Release the control-plane engine + DB; the serve binary owns them next.
	if err := eng.Stop(); err != nil {
		t.Fatalf("engine.Stop: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	return apiKey
}

// buildNotify compiles the notify binary (default tags) into a temp path.
func buildNotify(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "notify")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build notify: %v", err)
	}
	return bin
}

// freeAddr grabs an unused 127.0.0.1 port for the serve binary to bind.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

// waitServeReady polls an authenticated read endpoint until the binary serves
// 200, proving the HTTP surface (and everything wired before ListenAndServe,
// including the dagnats workers) is up.
func waitServeReady(t *testing.T, base, authHeader string) {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := doReq(t, http.MethodGet, base+"/v1/messages?subscriberId=probe&channel=in_app", authHeader, "")
		if st == http.StatusOK {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("serve binary did not become ready within 25s")
}

// doReq performs an HTTP request and returns the status and body. A connection
// error (binary still booting) is reported as status 0 so callers can retry.
func doReq(t *testing.T, method, url, authHeader, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", authHeader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

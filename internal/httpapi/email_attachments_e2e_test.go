package httpapi_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/blobstore"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/delivery"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// attachmentEnv is a trigger API over a real store and a real filesystem blob
// store. eng is nil unless the test starts a sidecar.
type attachmentEnv struct {
	st    *store.Store
	blobs *blobstore.FS
	aud   *audience.Service
	srv   *httptest.Server
	auth  string
	scope domain.Scope
	cap   *providers.Capture
	svc   *authoring.Service
}

func newAttachmentEnv(t *testing.T, withEngine, withBlobs bool) *attachmentEnv {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	blobs, err := blobstore.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &attachmentEnv{st: st, blobs: blobs, cap: providers.NewCapture()}

	var eng *engine.Engine
	if withEngine {
		eng, err = engine.New(dagtest.StartSidecar(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = eng.Stop() })
		eng.Handle("notify.send.email", delivery.NewEmail(st, capResolver{c: e.cap, from: "noreply@acme.io"}).WithBlobStore(blobs).HandleEmail)
		if err := eng.StartWorkers(ctx); err != nil {
			t.Fatal(err)
		}
	}

	id := identity.New(st, bytesRepeat(0x2a, 32))
	e.aud = audience.New(st).WithBlobStore(blobs)
	trg := trigger.New(e.aud, st, nil)
	if withEngine {
		trg = trigger.New(e.aud, st, eng)
		e.svc = authoring.New(st, eng)
	}
	if withBlobs {
		trg = trg.WithBlobStore(blobs)
	}
	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatal(err)
	}
	e.scope, e.auth = scope, "ApiKey "+key
	e.srv = httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: e.aud, Trigger: trg, Inbox: inboxapi.New(st), Store: st}))
	t.Cleanup(e.srv.Close)
	if status, body := doPost(t, e.srv.URL+"/v1/subscribers", e.auth, `{"subscriberId":"user_9","firstName":"Ada","email":"ada@acme.io"}`); status != http.StatusCreated {
		t.Fatalf("create subscriber: %d %s", status, body)
	}
	return e
}

func (e *attachmentEnv) publishEmailWorkflow(t *testing.T, steps int) {
	t.Helper()
	ctx := context.Background()
	var s []domain.Step
	for i := 1; i <= steps; i++ {
		s = append(s, domain.Step{
			StepID: fmt.Sprintf("E%d", i), Type: "email",
			ControlValues: json.RawMessage(`{"subject":"Invoice","body":"Attached."}`),
		})
	}
	if _, err := e.svc.Create(ctx, e.scope, authoring.WorkflowInput{Name: "Invoice", TriggerIdentifier: "invoice", Steps: s}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Publish(ctx, e.scope, "invoice"); err != nil {
		t.Fatal(err)
	}
}

func triggerBody(attachments string) string {
	if attachments != "" {
		attachments = `,"attachments":` + attachments
	}
	return `{"name":"invoice","to":{"subscriberId":"user_9"},"payload":{}` + attachments + `}`
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func attJSON(name, ctype string, content []byte) string {
	return fmt.Sprintf(`{"filename":%q,"contentType":%q,"content":%q}`, name, ctype, b64(content))
}

func TestEmailAttachments_SentToEveryEmailStepAndShownAsMetadata(t *testing.T) {
	e := newAttachmentEnv(t, true, true)
	e.publishEmailWorkflow(t, 2)
	pdf := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte{0, 0xff, 0x10}, 5000)...)

	status, body := doPost(t, e.srv.URL+"/v1/events/trigger", e.auth,
		triggerBody("["+attJSON("invoice-1042.pdf", "application/pdf", pdf)+"]"))
	if status != http.StatusCreated {
		t.Fatalf("trigger: %d %s", status, body)
	}
	var resp trigger.TriggerResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil || len(resp.NotificationIDs) != 1 {
		t.Fatalf("response %s: %v", body, err)
	}

	// The run carries references only: the bytes are in the blob store, once.
	rows, err := e.st.EmailAttachments().ListByNotification(context.Background(), e.scope, resp.NotificationIDs[0])
	if err != nil || len(rows) != 1 || rows[0].Size != int64(len(pdf)) || rows[0].SubscriberID != "user_9" {
		t.Fatalf("attachment rows = %+v, %v", rows, err)
	}
	if _, err := e.blobs.Head(context.Background(), rows[0].SHA256); err != nil {
		t.Fatalf("blob not stored: %v", err)
	}

	// Both email steps of the run send the file.
	var sent []providers.EmailMessage
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if sent = e.cap.Sent(); len(sent) == 2 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(sent) != 2 {
		t.Fatalf("sent %d emails, want 2", len(sent))
	}
	for i, em := range sent {
		if len(em.Attachments) != 1 {
			t.Fatalf("email %d has %d attachments", i, len(em.Attachments))
		}
		a := em.Attachments[0]
		if a.Filename != "invoice-1042.pdf" || a.ContentType != "application/pdf" || !bytes.Equal(a.Content, pdf) {
			t.Fatalf("email %d attachment = %q %q (%d bytes)", i, a.Filename, a.ContentType, len(a.Content))
		}
	}

	// The message API names the file and never carries it.
	status, msgs := doGet(t, e.srv.URL+"/v1/notifications/"+resp.NotificationIDs[0]+"/messages", e.auth)
	if status != http.StatusOK {
		t.Fatalf("messages: %d %s", status, msgs)
	}
	var out []map[string]any
	if err := json.Unmarshal([]byte(msgs), &out); err != nil || len(out) != 2 {
		t.Fatalf("messages = %s: %v", msgs, err)
	}
	for _, m := range out {
		atts, _ := m["attachments"].([]any)
		if len(atts) != 1 {
			t.Fatalf("message attachments = %v", m["attachments"])
		}
		a := atts[0].(map[string]any)
		if a["filename"] != "invoice-1042.pdf" || a["contentType"] != "application/pdf" || a["size"] != float64(len(pdf)) || len(a) != 3 {
			t.Fatalf("attachment metadata = %v", a)
		}
	}
	if strings.Contains(msgs, b64(pdf[:60])) || strings.Contains(msgs, `"content":"JVBER`) {
		t.Fatal("the message API carried attachment content")
	}
}

func TestEmailAttachments_AbsentLeavesEmailAndMessageUnchanged(t *testing.T) {
	e := newAttachmentEnv(t, true, true)
	e.publishEmailWorkflow(t, 1)
	status, body := doPost(t, e.srv.URL+"/v1/events/trigger", e.auth, triggerBody(""))
	if status != http.StatusCreated {
		t.Fatalf("trigger: %d %s", status, body)
	}
	var resp trigger.TriggerResponse
	_ = json.Unmarshal([]byte(body), &resp)
	deadline := time.Now().Add(30 * time.Second)
	for len(e.cap.Sent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	sent := e.cap.Sent()
	if len(sent) != 1 || sent[0].Attachments != nil {
		t.Fatalf("sent = %+v", sent)
	}
	_, msgs := doGet(t, e.srv.URL+"/v1/notifications/"+resp.NotificationIDs[0]+"/messages", e.auth)
	if strings.Contains(msgs, "attachments") {
		t.Fatalf("a message without attachments mentions them: %s", msgs)
	}
}

func TestEmailAttachments_RejectedAtTheEdge(t *testing.T) {
	e := newAttachmentEnv(t, false, true)
	small := []byte("%PDF")
	many := make([]string, 11)
	for i := range many {
		many[i] = attJSON("a.pdf", "application/pdf", small)
	}
	big := bytes.Repeat([]byte{'x'}, 5<<20+1)
	four := bytes.Repeat([]byte{'x'}, 7<<19) // 3.5 MB: three of them pass the 15 MB body cap but not 10 MB of files

	for name, c := range map[string]struct{ atts, want string }{
		"bad base64":      {`[{"filename":"a.pdf","contentType":"application/pdf","content":"@@not base64@@"}]`, "not valid base64"},
		"disallowed type": {"[" + attJSON("a.exe", "application/x-msdownload", small) + "]", "not allowed"},
		"svg":             {"[" + attJSON("a.svg", "image/svg+xml", small) + "]", "not allowed"},
		"too big":         {"[" + attJSON("big.pdf", "application/pdf", big) + "]", "larger than 5 MB"},
		"too many":        {"[" + strings.Join(many, ",") + "]", "at most 10 files"},
		"total too big": {"[" + strings.Join([]string{
			attJSON("1.pdf", "application/pdf", four), attJSON("2.pdf", "application/pdf", four), attJSON("3.pdf", "application/pdf", four),
		}, ",") + "]", "together are larger than 10 MB"},
		"empty filename":  {"[" + attJSON("", "application/pdf", small) + "]", "filename is required"},
		"path separator":  {"[" + attJSON("../etc/passwd", "application/pdf", small) + "]", "path separators"},
		"backslash":       {"[" + attJSON(`a\b.pdf`, "application/pdf", small) + "]", "path separators"},
		"quote":           {"[" + attJSON(`a".pdf`, "application/pdf", small) + "]", "quotes"},
		"header break":    {"[" + attJSON("a.pdf\r\nBcc: x@y.z", "application/pdf", small) + "]", "control characters"},
		"long filename":   {"[" + attJSON(strings.Repeat("a", 201), "application/pdf", small) + "]", "longer than 200"},
		"empty content":   {`[{"filename":"a.pdf","contentType":"application/pdf","content":""}]`, "content is empty"},
		"dot filename":    {"[" + attJSON("..", "application/pdf", small) + "]", "not a file name"},
		"missing content": {`[{"filename":"a.pdf","contentType":"application/pdf"}]`, "content is empty"},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := doPost(t, e.srv.URL+"/v1/events/trigger", e.auth, triggerBody(c.atts))
			if status != http.StatusUnprocessableEntity || !strings.Contains(body, c.want) || !strings.Contains(body, `"acknowledged":false`) {
				t.Fatalf("status = %d body = %.300s, want 422 mentioning %q", status, body, c.want)
			}
		})
	}

	t.Run("body over the route limit", func(t *testing.T) {
		huge := `{"name":"invoice","to":{"subscriberId":"user_9"},"payload":{"x":"` + strings.Repeat("a", 16<<20) + `"}}`
		if status, _ := doPost(t, e.srv.URL+"/v1/events/trigger", e.auth, huge); status != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", status)
		}
	})

	t.Run("exactly at the limits is accepted", func(t *testing.T) {
		// Passes validation; the unknown workflow then answers 404, not 422.
		five := bytes.Repeat([]byte{'x'}, 5<<20)
		status, body := doPost(t, e.srv.URL+"/v1/events/trigger", e.auth,
			triggerBody("["+attJSON("a.pdf", "application/pdf", five)+","+attJSON("b.png", "image/png", five)+"]"))
		if status != http.StatusNotFound {
			t.Fatalf("status = %d body = %.200s, want 404 (valid attachments, unknown workflow)", status, body)
		}
	})
}

func TestEmailAttachments_RefusedWithoutBlobStore(t *testing.T) {
	e := newAttachmentEnv(t, false, false)
	status, body := doPost(t, e.srv.URL+"/v1/events/trigger", e.auth,
		triggerBody("["+attJSON("a.pdf", "application/pdf", []byte("%PDF"))+"]"))
	if status != http.StatusUnprocessableEntity || !strings.Contains(body, "no blob store") {
		t.Fatalf("status = %d body = %s", status, body)
	}
}

// TestEmailAttachments_PurgeRemovesRowsAndOnlyUnreferencedBlobs: purging a
// subscriber deletes their attachment rows and each blob nothing else uses; a
// blob another subscriber's attachment or an inbound mail still points at stays.
func TestEmailAttachments_PurgeRemovesRowsAndOnlyUnreferencedBlobs(t *testing.T) {
	ctx := context.Background()
	e := newAttachmentEnv(t, false, true)
	put := func(s string) blobstore.Ref {
		ref, err := e.blobs.Put(ctx, strings.NewReader(s))
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	own, shared, mail := put("only user_9 has this"), put("two subscribers have this"), put("an inbound mail")
	att := func(name string, r blobstore.Ref) domain.EmailAttachment {
		return domain.EmailAttachment{Filename: name, ContentType: "application/pdf", SHA256: r.SHA256, Size: r.Size}
	}
	atts := e.st.EmailAttachments()
	if err := atts.Add(ctx, e.scope, "n1", "user_9", []domain.EmailAttachment{att("own.pdf", own), att("shared.pdf", shared), att("mail.pdf", mail)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.aud.Upsert(ctx, e.scope, audience.SubscriberInput{SubscriberID: "user_10", Email: "b@acme.io"}); err != nil {
		t.Fatal(err)
	}
	if err := atts.Add(ctx, e.scope, "n2", "user_10", []domain.EmailAttachment{att("shared.pdf", shared)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.InboundMessages().Record(ctx, e.scope, store.InboundMessage{SHA256: mail.SHA256, Size: mail.Size, Recipient: "r@x.io"}); err != nil {
		t.Fatal(err)
	}

	sub, err := e.aud.Get(ctx, e.scope, "user_9")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.aud.DeleteSubscriber(ctx, e.scope, sub.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := e.aud.PurgeDeleted(ctx, e.scope, "user_9"); err != nil || n != 1 {
		t.Fatalf("purge = %d, %v", n, err)
	}

	if rows, _ := atts.ListByNotification(ctx, e.scope, "n1"); len(rows) != 0 {
		t.Fatalf("purged subscriber still has rows: %+v", rows)
	}
	if rows, _ := atts.ListByNotification(ctx, e.scope, "n2"); len(rows) != 1 {
		t.Fatalf("the other subscriber's row is gone: %+v", rows)
	}
	for _, c := range []struct {
		name string
		ref  blobstore.Ref
		want bool
	}{{"own", own, false}, {"shared", shared, true}, {"mail", mail, true}} {
		_, err := e.blobs.Head(ctx, c.ref.SHA256)
		if exists := err == nil; exists != c.want {
			t.Errorf("blob %q exists = %v (%v), want %v", c.name, exists, err, c.want)
		}
	}

	// Once the last subscriber using the shared blob goes, it goes too.
	sub, _ = e.aud.Get(ctx, e.scope, "user_10")
	if err := e.aud.DeleteSubscriber(ctx, e.scope, sub.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.aud.PurgeDeleted(ctx, e.scope, "user_10"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.blobs.Head(ctx, shared.SHA256); err == nil {
		t.Error("the shared blob outlived its last reference")
	}
	if _, err := e.blobs.Head(ctx, mail.SHA256); err != nil {
		t.Errorf("the inbound mail's blob was deleted: %v", err)
	}
}

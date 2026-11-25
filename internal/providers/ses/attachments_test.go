package ses

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/providers"
)

// TestClient_Send_Attachments pins the SESv2 Simple.Attachments entries. An
// email with none carries no Attachments key.
func TestClient_Send_Attachments(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"MessageId":"m1"}`)
	c := &Client{AccessKeyID: "k", SecretAccessKey: "s", Region: "us-east-1", From: "a@b.com", BaseURL: srv.URL, now: fixedClock()}
	_, err := c.Send(context.Background(), providers.EmailMessage{
		To: "u@example.com", Subject: "s", Text: "t",
		Attachments: []providers.Attachment{{Filename: "invoice.pdf", ContentType: "application/pdf", Content: []byte("%PDF")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `"Attachments":[{"FileName":"invoice.pdf","RawContent":"JVBERg==","ContentType":"application/pdf","ContentDisposition":"ATTACHMENT","ContentTransferEncoding":"BASE64"}]`
	if !strings.Contains(got.body, want) {
		t.Fatalf("body = %s\nwant it to contain %s", got.body, want)
	}

	if _, err := c.Send(context.Background(), providers.EmailMessage{To: "u@example.com", Subject: "s", Text: "t"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.body, "Attachments") {
		t.Fatalf("an email with no attachments sent %s", got.body)
	}
}

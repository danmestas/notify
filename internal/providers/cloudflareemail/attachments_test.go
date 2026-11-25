package cloudflareemail

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/providers"
)

// TestClient_Send_Attachments pins the attachments array of the Email Sending
// API: base64 content, filename, type and disposition. An email with none
// carries no attachments key.
func TestClient_Send_Attachments(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"success":true,"result":{"delivered":["u@example.com"]}}`)
	c := &Client{AccountID: "acc", APIToken: "tok", From: "n@example.com", BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.EmailMessage{
		To: "u@example.com", Subject: "s", Text: "t",
		Attachments: []providers.Attachment{{Filename: "invoice.pdf", ContentType: "application/pdf", Content: []byte("%PDF")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `"attachments":[{"content":"JVBERg==","filename":"invoice.pdf","type":"application/pdf","disposition":"attachment"}]`
	if !strings.Contains(got.body, want) {
		t.Fatalf("body = %s\nwant it to contain %s", got.body, want)
	}

	if _, err := c.Send(context.Background(), providers.EmailMessage{To: "u@example.com", Subject: "s", Text: "t"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.body, "attachments") {
		t.Fatalf("an email with no attachments sent %s", got.body)
	}
}

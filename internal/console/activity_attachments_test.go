package console

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// TestFragment_ActivityDetail_ShowsEmailAttachmentMetadata: the detail panel
// names an email's attachment with its type and size.
func TestFragment_ActivityDetail_ShowsEmailAttachmentMetadata(t *testing.T) {
	tc := newTestConsole(t)
	n := tc.seedNotification(t, domain.Notification{WorkflowID: "wf", SubscriberID: "sub-att", RunID: "run-att"})
	tc.seedMessage(t, domain.Message{
		WorkflowID: "wf", NotificationID: n.ID, RunID: "run-att", StepID: "E1", SubscriberID: "sub-att",
		Channel: "email", ProviderID: "smtp", Status: "sent", Subject: "Invoice", Content: "Attached.",
	})
	if err := tc.st.EmailAttachments().Add(context.Background(), tc.scope, n.ID, "sub-att", []domain.EmailAttachment{
		{Filename: "invoice-1042.pdf", ContentType: "application/pdf", SHA256: strings.Repeat("ab", 32), Size: 48213},
	}); err != nil {
		t.Fatal(err)
	}
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/api/fragments/activity-detail?notification_id="+n.ID, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "invoice-1042.pdf (application/pdf, 48213 bytes)") {
		t.Fatalf("detail panel does not show the attachment: %s", body)
	}
}

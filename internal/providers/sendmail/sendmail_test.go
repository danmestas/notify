package sendmail

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/providers"
)

// fakeMTA writes a shell script that stands in for sendmail and returns its
// path. body is the script after the shebang; $OUT is the capture file.
func fakeMTA(t *testing.T, body string) (script, capture string) {
	t.Helper()
	dir := t.TempDir()
	capture = filepath.Join(dir, "captured.eml")
	script = filepath.Join(dir, "sendmail")
	src := "#!/bin/sh\nOUT=" + capture + "\n" + body + "\n"
	if err := os.WriteFile(script, []byte(src), 0o755); err != nil {
		t.Fatalf("write fake mta: %v", err)
	}
	return script, capture
}

var msg = providers.EmailMessage{
	From:    "Quarry <forge@example.org>",
	To:      "dev@example.com",
	Subject: "Build failed",
	Text:    "caf\u00e9: it broke",
	ReplyTo: "reply+tok@mail.example.org",
}

// TestSend_PipesMessageToCommand is the contract with the MTA: the whole
// RFC 5322 message arrives on stdin with the headers `-t` needs to route it.
func TestSend_PipesMessageToCommand(t *testing.T) {
	script, capture := fakeMTA(t, `cat > "$OUT"; exit 0`)
	c := &Client{Command: []string{script, "-ti"}}

	res, err := c.Send(context.Background(), msg)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	got := string(raw)
	for _, want := range []string{
		"From: Quarry <forge@example.org>\r\n",
		"To: dev@example.com\r\n",
		"Reply-To: reply+tok@mail.example.org\r\n",
		"Subject: Build failed\r\n",
		"Date: ",
		"Message-ID: <" + res.ProviderMessageID + ">\r\n",
		"Content-Transfer-Encoding: quoted-printable\r\n",
		"caf=C3=A9: it broke",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("piped message missing %q:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(res.ProviderMessageID, "@example.org") {
		t.Errorf("ProviderMessageID = %q, want minted at the sender's domain", res.ProviderMessageID)
	}
}

// TestSend_NonZeroExitIsPermanentWithStderr: an MTA that refuses says why on
// stderr, and that line -- not a generic "exit 1" -- is what the delivery
// record must carry so the failure is diagnosable from the console.
func TestSend_NonZeroExitIsPermanentWithStderr(t *testing.T) {
	script, _ := fakeMTA(t, `cat >/dev/null; echo "sendmail: fatal: Recipient address rejected: relay denied" >&2; exit 75`)
	c := &Client{Command: []string{script, "-ti"}}

	_, err := c.Send(context.Background(), msg)
	if err == nil {
		t.Fatal("Send succeeded against a refusing MTA")
	}
	if providers.IsTransient(err) {
		t.Fatalf("a non-zero exit was classed transient; the engine would retry a permanent refusal: %v", err)
	}
	for _, want := range []string{"exited 75", "relay denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}
}

// TestSend_StderrIsBounded: a chatty MTA cannot grow the error text without
// bound.
func TestSend_StderrIsBounded(t *testing.T) {
	script, _ := fakeMTA(t, `cat >/dev/null; yes "diagnostic line that repeats" | head -c 100000 >&2; exit 1`)
	c := &Client{Command: []string{script}}

	_, err := c.Send(context.Background(), msg)
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(err.Error()) > stderrCap+256 {
		t.Fatalf("error text is %d bytes; stderr was not capped", len(err.Error()))
	}
}

// TestSend_MissingCommandIsPermanent: no binary at the configured path is a
// misconfiguration, not a blip.
func TestSend_MissingCommandIsPermanent(t *testing.T) {
	c := &Client{Command: []string{filepath.Join(t.TempDir(), "no-such-sendmail"), "-ti"}}
	_, err := c.Send(context.Background(), msg)
	if err == nil {
		t.Fatal("Send succeeded with no MTA binary")
	}
	if providers.IsTransient(err) {
		t.Fatalf("a missing binary was classed transient: %v", err)
	}
}

// TestSend_TimeoutIsTransient: an MTA that accepted the pipe but never finished
// is the one case worth a retry.
func TestSend_TimeoutIsTransient(t *testing.T) {
	script, _ := fakeMTA(t, `cat >/dev/null; sleep 5; exit 0`)
	c := &Client{Command: []string{script}, Timeout: 200 * time.Millisecond}

	start := time.Now()
	_, err := c.Send(context.Background(), msg)
	if err == nil {
		t.Fatal("Send succeeded past its timeout")
	}
	if !providers.IsTransient(err) {
		t.Fatalf("timeout was classed permanent: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("Send waited on the MTA past its own timeout")
	}
}

// TestSend_DefaultsFromAndCommand: an empty Command falls back to the package
// default and an empty From to the client's.
func TestSend_DefaultsFromAndCommand(t *testing.T) {
	c := &Client{}
	if _, err := c.Send(context.Background(), providers.EmailMessage{To: "x@y"}); err == nil ||
		!strings.Contains(err.Error(), "empty from") {
		t.Fatalf("want empty-from error, got %v", err)
	}
	if _, err := c.Send(context.Background(), providers.EmailMessage{From: "a@b"}); err == nil ||
		!strings.Contains(err.Error(), "empty recipient") {
		t.Fatalf("want empty-recipient error, got %v", err)
	}
	c = &Client{From: "default@b", Command: []string{filepath.Join(t.TempDir(), "absent")}}
	_, err := c.Send(context.Background(), providers.EmailMessage{To: "x@y", Subject: "s", Text: "t"})
	if err == nil || strings.Contains(err.Error(), "empty from") {
		t.Fatalf("client From was not used as the default: %v", err)
	}
}

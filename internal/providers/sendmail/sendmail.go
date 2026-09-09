// Package sendmail is the pipe-to-MTA email provider: it writes an RFC 5322
// message to the standard input of a local command, `sendmail -ti` by default,
// which every Unix MTA (Postfix, Exim, OpenSMTPD, msmtp) accepts unchanged.
//
// It is the lowest-common-denominator integration and the one Fossil defaults
// to: a self-hoster with any MTA sends email with no API key and no vendor.
// With -t the MTA reads the recipients from the message headers; with -i a
// lone "." line does not end the message.
package sendmail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/providers"
)

// DefaultCommand is the argv used when the operator configures nothing.
var DefaultCommand = []string{"/usr/sbin/sendmail", "-ti"}

// DefaultTimeout bounds one pipe invocation. A local MTA accepts a message in
// milliseconds; anything approaching this is a wedged MTA, not a slow one.
const DefaultTimeout = 30 * time.Second

// stderrCap bounds how much of the MTA's stderr is carried into the delivery
// record. An MTA that refuses says why in its first line or two; capping keeps
// a runaway diagnostic from becoming the error text.
const stderrCap = 4096

// Client pipes messages to Command. Command is operator configuration fixed at
// boot, never tenant data: it is executed as-is with no shell, so there is
// nothing to inject and nothing to template.
type Client struct {
	// Command is the argv to run. Empty falls back to DefaultCommand.
	Command []string
	// From is the default sender when the message carries none.
	From string
	// Timeout bounds one invocation. Zero means DefaultTimeout.
	Timeout time.Duration
}

// Send builds the message and pipes it to the command. A non-zero exit is a
// permanent failure whose reason is the MTA's stderr; the command being absent
// is permanent too. Only a timeout is transient: the MTA exists and accepted
// the pipe but did not finish, which is worth one more try.
func (c *Client) Send(ctx context.Context, msg providers.EmailMessage) (providers.Result, error) {
	if msg.To == "" {
		return providers.Result{}, fmt.Errorf("sendmail: empty recipient")
	}
	if msg.From == "" {
		msg.From = c.From
	}
	if msg.From == "" {
		return providers.Result{}, fmt.Errorf("sendmail: empty from")
	}
	argv := c.Command
	if len(argv) == 0 {
		argv = DefaultCommand
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	mime, messageID := providers.BuildMIME(msg)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(mime)
	cmd.Stdout = io.Discard
	var stderr limitedBuffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	switch {
	case err == nil:
		return providers.Result{ProviderMessageID: messageID}, nil
	case ctx.Err() != nil:
		return providers.Result{}, providers.NewTransientError(
			fmt.Errorf("sendmail: %s did not finish within %s", argv[0], timeout),
		)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		reason := strings.TrimSpace(stderr.String())
		if reason == "" {
			reason = "no diagnostic on stderr"
		}
		return providers.Result{}, fmt.Errorf("sendmail: %s exited %d: %s", argv[0], exitErr.ExitCode(), reason)
	}
	// Not found, not executable, or could not start at all.
	return providers.Result{}, fmt.Errorf("sendmail: run %s: %w", argv[0], err)
}

// limitedBuffer keeps the first stderrCap bytes written and drops the rest,
// so a chatty MTA cannot grow the error text without bound.
type limitedBuffer struct {
	buf bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := stderrCap - b.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		b.buf.Write(p)
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

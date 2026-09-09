package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/danmestas/notify/internal/emailloop"
)

// `notify inbound` is the pipe mode: a system MTA hands a raw message on
// stdin -- in /etc/aliases, `reply: "|notify inbound"` -- and this parses it
// with the same parser the embedded receiver uses and POSTs the result to the
// running notify's /v1/inbound/email with the forwarder secret.
//
// It is a thin client, not Fossil's in-process `fossil email inbound`: the
// inbound path needs the store, NATS and the agent runtime, i.e. the server.
// An operator who already runs Postfix uses this and never opens a second
// port; one who runs nothing uses -smtp. Both feed the identical parser.
//
// Exit codes follow sysexits.h, which is what an MTA reads to decide between
// bouncing and retrying: 0 delivered (including every intentional drop, which
// the server acknowledges), 75 EX_TEMPFAIL for a server or network fault so
// the MTA queues and retries, 65 EX_DATAERR for a message that will never
// parse, 78 EX_CONFIG for a secret or URL that is wrong.
const (
	exitOK       = 0
	exitDataErr  = 65
	exitTempFail = 75
	exitConfig   = 78
)

// exitError carries the sysexits code a failure should map to.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// exitCodeFor maps an error from runInbound to a process exit code.
func exitCodeFor(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return 1
}

func runInbound(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("inbound", flag.ExitOnError)
	server := fs.String("server", envOr("NOTIFY_SERVER_URL", "http://127.0.0.1:3000"), "base URL of the running notify")
	secret := fs.String("secret", os.Getenv("NOTIFY_INBOUND_SECRET"), "inbound forwarder secret (X-Inbound-Secret)")
	timeout := fs.Duration("timeout", 30*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return &exitError{exitConfig, err}
	}
	if *secret == "" {
		return &exitError{exitConfig, errors.New("no inbound secret: set -secret or NOTIFY_INBOUND_SECRET")}
	}
	status, err := postInbound(context.Background(), stdin, *server, *secret, &http.Client{Timeout: *timeout})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, status)
	return nil
}

// postInbound is runInbound with the I/O injected. It returns the server's
// accept status on 2xx.
func postInbound(ctx context.Context, stdin io.Reader, server, secret string, client *http.Client) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(stdin, emailloop.MaxMessageBytes+1))
	if err != nil {
		return "", &exitError{exitTempFail, fmt.Errorf("read stdin: %w", err)}
	}
	parsed, err := emailloop.ParseMessage(raw)
	if err != nil {
		// Including ErrMessageTooLarge: the same bytes will never succeed.
		return "", &exitError{exitDataErr, err}
	}
	body, err := json.Marshal(parsed)
	if err != nil {
		return "", &exitError{exitDataErr, err}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/v1/inbound/email", bytes.NewReader(body))
	if err != nil {
		return "", &exitError{exitConfig, err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Inbound-Secret", secret)
	resp, err := client.Do(req)
	if err != nil {
		return "", &exitError{exitTempFail, fmt.Errorf("post to %s: %w", server, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var ack struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(respBody, &ack)
		return ack.Status, nil
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusServiceUnavailable:
		// Wrong secret, or inbound not configured on the server: retrying
		// cannot help and bouncing would blame the sender. EX_CONFIG.
		return "", &exitError{exitConfig, fmt.Errorf("server answered %d: %s", resp.StatusCode, bytes.TrimSpace(respBody))}
	case resp.StatusCode >= 500:
		return "", &exitError{exitTempFail, fmt.Errorf("server answered %d: %s", resp.StatusCode, bytes.TrimSpace(respBody))}
	default:
		return "", &exitError{exitDataErr, fmt.Errorf("server answered %d: %s", resp.StatusCode, bytes.TrimSpace(respBody))}
	}
}

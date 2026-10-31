package providers

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// stallingSMTP accepts connections and never says hello, so a client without
// a deadline would wait forever for the 220 greeting.
func stallingSMTP(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	return ln.Addr().String()
}

func TestSMTP_HonoursContextDeadline(t *testing.T) {
	addr := stallingSMTP(t)
	s := &SMTP{Addr: addr, Host: "127.0.0.1"}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := s.Send(ctx, EmailMessage{From: "a@x.test", To: "b@x.test", Subject: "s", Text: "t"})
	if err == nil {
		t.Fatal("want an error from a server that never greets")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Send took %v; it must stop at the context deadline", d)
	}
}

func TestSMTP_HonoursCancel(t *testing.T) {
	addr := stallingSMTP(t)
	s := &SMTP{Addr: addr, Host: "127.0.0.1"}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)

	done := make(chan error, 1)
	go func() {
		_, err := s.Send(ctx, EmailMessage{From: "a@x.test", To: "b@x.test", Subject: "s", Text: "t"})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send ignored cancellation")
	}
}

func TestSMTP_DefaultTimeoutWithoutDeadline(t *testing.T) {
	old := smtpTimeout
	smtpTimeout = 200 * time.Millisecond
	t.Cleanup(func() { smtpTimeout = old })

	addr := stallingSMTP(t)
	s := &SMTP{Addr: addr, Host: "127.0.0.1"}
	start := time.Now()
	if _, err := s.Send(context.Background(), EmailMessage{From: "a@x.test", To: "b@x.test", Subject: "s", Text: "t"}); err == nil {
		t.Fatal("want a timeout error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Send took %v with no context deadline; want the default timeout", d)
	}
}

// TestSMTP_SendsMessage drives a scripted server through EHLO, MAIL, RCPT,
// DATA and QUIT, and checks the message body arrives.
func TestSMTP_SendsMessage(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		say := func(s string) { fmt.Fprintf(c, "%s\r\n", s) }
		say("220 test ready")
		var data strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					say("250 queued")
					continue
				}
				data.WriteString(line)
				continue
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "EHLO"):
				say("250 test")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				say("250 ok")
			case cmd == "DATA":
				inData = true
				say("354 go")
			case cmd == "QUIT":
				say("221 bye")
				got <- data.String()
				return
			default:
				say("500 what")
			}
		}
	}()

	s := &SMTP{Addr: ln.Addr().String(), Host: "127.0.0.1"}
	res, err := s.Send(context.Background(), EmailMessage{From: "a@x.test", To: "b@x.test", Subject: "Hello", Text: "body text"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.ProviderMessageID == "" {
		t.Fatal("want a Message-ID")
	}
	select {
	case body := <-got:
		if !strings.Contains(body, "Subject: Hello") || !strings.Contains(body, "body text") {
			t.Fatalf("server got %q", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server never saw QUIT")
	}
}

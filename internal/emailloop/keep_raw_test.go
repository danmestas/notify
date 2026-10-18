package emailloop

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/danmestas/notify/internal/blobstore"
)

// rawMail is a reply with bytes a parser would normalise away: 8-bit text,
// trailing spaces and a CRLF body.
func rawMail(token string) []byte {
	return crlf(`
From: Ann <ann@example.com>
To: reply+` + token + `@mail.example.org
Subject: Re: invoice
Message-ID: <m-1@example.com>
Content-Type: text/plain; charset=utf-8

Paid today, café receipt attached.  
`)
}

// TestAccept_KeepsTheRawMessage: an accepted message's bytes are stored
// exactly as received and recorded once, before it is routed; a sender's retry
// of the same message adds nothing.
func TestAccept_KeepsTheRawMessage(t *testing.T) {
	f := newLoopFixture(t, nil)
	ctx := t.Context()
	scope, threadID, _ := f.armScope(t, ctx, "org1", "env1", "helper", "mail.example.org", "ann@example.com")
	blobs, err := blobstore.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.svc.WithBlobStore(blobs)

	raw := rawMail(f.token(t, scope, threadID))
	for i := 0; i < 2; i++ {
		parsed, err := ParseMessage(raw)
		if err != nil {
			t.Fatal(err)
		}
		if status, err := f.svc.Accept(ctx, parsed); err != nil || status != StatusThreaded {
			t.Fatalf("accept %d: %q %v", i, status, err)
		}
	}
	msgs, err := f.st.InboundMessages().List(ctx, scope, 0, 50)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("recorded = %d %v, want 1", len(msgs), err)
	}
	m := msgs[0]
	if m.Sender != "Ann <ann@example.com>" || m.Subject != "Re: invoice" || m.MessageID != "<m-1@example.com>" || m.Size != int64(len(raw)) {
		t.Fatalf("record = %+v", m)
	}
	rc, err := blobs.Get(ctx, m.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, raw) {
		t.Fatalf("stored bytes differ from the message received")
	}
}

type failingBlobs struct{ blobstore.Store }

func (failingBlobs) Put(context.Context, io.Reader) (blobstore.Ref, error) {
	return blobstore.Ref{}, errors.New("disk full")
}

// TestAccept_NoRawStoreNoAccept: if the bytes cannot be kept, the message is
// not routed and the error is returned for the transport to ask the sender to
// retry, so mail is never accepted without its original.
func TestAccept_NoRawStoreNoAccept(t *testing.T) {
	f := newLoopFixture(t, nil)
	ctx := t.Context()
	scope, threadID, _ := f.armScope(t, ctx, "org1", "env1", "helper", "mail.example.org", "ann@example.com")
	f.svc.WithBlobStore(failingBlobs{})
	parsed, err := ParseMessage(rawMail(f.token(t, scope, threadID)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Accept(ctx, parsed); err == nil {
		t.Fatal("accepted a message whose bytes were not kept")
	}
	if f.dispatchCount() != 0 {
		t.Fatalf("the message was routed anyway: %+v", f.dispatches)
	}
}

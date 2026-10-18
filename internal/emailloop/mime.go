package emailloop

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"

	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/transform"
)

// MaxMessageBytes bounds a raw message ParseMessage will accept. It matches
// the webhook's MaxBytesReader so a transport cannot smuggle a larger body
// past the limit by choosing a different door.
const MaxMessageBytes = 1 << 20

// maxMIMEDepth bounds multipart nesting. Real mail is two or three deep;
// anything past this is either hostile or broken, and either way the
// interesting parts were already found.
const maxMIMEDepth = 8

// ErrMessageTooLarge reports a raw message over MaxMessageBytes.
var ErrMessageTooLarge = errors.New("emailloop: message exceeds size limit")

// ParseMessage turns a raw RFC 5322 message into the same InboundEmail the
// JSON webhook receives, so an SMTP listener and a piped message feed the one
// inbound path (Accept) and can never disagree with the forwarder about what
// a message contained.
//
// It selects the first text/plain part as Text and the first text/html part
// as HTML, walking multipart/alternative and multipart/mixed, decoding
// base64 and quoted-printable transfer encodings and any IANA charset the
// message names. Everything else with a body is recorded as an attachment by
// name, type and size only; attachment bytes are not retained -- the reply
// loop threads text, and the webhook shape stores attachments by reference.
//
// EnvelopeTo is taken from Delivered-To, which an MTA piping to a command
// adds for the envelope recipient. An SMTP listener overwrites it with the
// actual RCPT TO, which is more reliable still.
func ParseMessage(raw []byte) (InboundEmail, error) {
	if len(raw) > MaxMessageBytes {
		return InboundEmail{}, ErrMessageTooLarge
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return InboundEmail{}, fmt.Errorf("emailloop: parse message: %w", err)
	}

	dec := mime.WordDecoder{CharsetReader: charsetReader}
	decodeWord := func(s string) string {
		if out, err := dec.DecodeHeader(s); err == nil {
			return out
		}
		return s
	}

	out := InboundEmail{
		From:       decodeWord(msg.Header.Get("From")),
		To:         decodeWord(msg.Header.Get("To")),
		Subject:    decodeWord(msg.Header.Get("Subject")),
		EnvelopeTo: strings.TrimSpace(msg.Header.Get("Delivered-To")),
		InReplyTo:  strings.TrimSpace(msg.Header.Get("In-Reply-To")),
		References: strings.Fields(msg.Header.Get("References")),
		Headers:    make(map[string]string, len(msg.Header)),
	}
	for k, v := range msg.Header {
		if len(v) > 0 {
			out.Headers[k] = decodeWord(v[0])
		}
	}

	if err := walkPart(textproto.MIMEHeader(msg.Header), msg.Body, &out, 0); err != nil {
		return InboundEmail{}, err
	}
	out.Raw = raw
	return out, nil
}

// walkPart handles one entity: recursing into multipart containers, decoding
// leaf bodies, and filing each leaf as Text, HTML or an attachment.
func walkPart(h textproto.MIMEHeader, body io.Reader, out *InboundEmail, depth int) error {
	if depth > maxMIMEDepth {
		return nil
	}
	ctype := h.Get("Content-Type")
	if ctype == "" {
		ctype = "text/plain; charset=us-ascii"
	}
	mediaType, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		// An unparseable Content-Type is treated as plain text rather than
		// failing the message: a reply with a mangled header is still a reply.
		mediaType, params = "text/plain", map[string]string{}
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return fmt.Errorf("emailloop: %s part has no boundary", mediaType)
		}
		mr := multipart.NewReader(body, boundary)
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				// A truncated or malformed container: keep what was already
				// filed rather than dropping the whole message.
				return nil
			}
			if err := walkPart(part.Header, part, out, depth+1); err != nil {
				return err
			}
		}
	}

	decoded, err := io.ReadAll(decodeTransfer(h.Get("Content-Transfer-Encoding"), body))
	if err != nil {
		return fmt.Errorf("emailloop: decode %s part: %w", mediaType, err)
	}

	disposition, dparams, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
	filename := dparams["filename"]
	if filename == "" {
		filename = params["name"]
	}
	isText := mediaType == "text/plain" || mediaType == "text/html"
	if disposition == "attachment" || (!isText && len(decoded) > 0) {
		out.Attachments = append(out.Attachments, InboundEmailAttachment{
			Filename:    filename,
			ContentType: mediaType,
			Size:        len(decoded),
		})
		return nil
	}

	text := normalizeText(decodeCharset(params["charset"], decoded))
	switch mediaType {
	case "text/plain":
		if out.Text == "" {
			out.Text = text
		}
	case "text/html":
		if out.HTML == "" {
			out.HTML = text
		}
	}
	return nil
}

// decodeTransfer wraps body in the reader that undoes its
// Content-Transfer-Encoding. 7bit, 8bit and binary are identity.
func decodeTransfer(encoding string, body io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		return quotedprintable.NewReader(body)
	default:
		return body
	}
}

// decodeCharset converts b from the named charset to UTF-8. UTF-8 and ASCII
// pass through; an unknown or unsupported charset passes through too, on the
// grounds that mojibake in a reply is better than no reply.
func decodeCharset(charset string, b []byte) string {
	r, err := charsetReader(charset, bytes.NewReader(b))
	if err != nil {
		return string(b)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return string(b)
	}
	return string(out)
}

// charsetReader is the shared charset resolver for bodies and RFC 2047
// encoded header words. It is the signature mime.WordDecoder expects.
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return input, nil
	}
	enc, err := ianaindex.IANA.Encoding(charset)
	if err != nil || enc == nil {
		return nil, fmt.Errorf("emailloop: unsupported charset %q", charset)
	}
	return transform.NewReader(input, enc.NewDecoder()), nil
}

// normalizeText gives text bodies LF line endings. Mail on the wire is CRLF;
// everything downstream (quote stripping, storage, the webhook shape) works
// in LF, and a stray CR at the end of every line would defeat the
// quoted-history markers.
func normalizeText(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

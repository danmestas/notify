package emailloop

import (
	"strings"
)

// InboundEmailAttachment mirrors the reference InboundEmailAttachment
// (packages/shared/src/types/inbound-email.ts): a parsed attachment reference.
// notify does not persist attachment bodies yet; the metadata is accepted and
// carried so the wire shape matches and a future consumer can fetch by url.
type InboundEmailAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int    `json:"size"`
	URL         string `json:"url,omitempty"`
	StoragePath string `json:"storagePath,omitempty"`
}

// InboundEmail is the parsed-email webhook payload, mirroring the reference
// inbound-email shape (the augmented mailparser output): from/to/subject/
// text/html/headers/attachments plus the envelope and threading headers a mail
// forwarder (Cloudflare Email Worker, Resend inbound) POSTs to
// /v1/inbound/email.
//
// The tokenized reply mailbox is read from To (or, when the forwarder supplies
// it, the more reliable envelope recipient EnvelopeTo): the address of the form
// reply+<token>@<inbound-domain>.
type InboundEmail struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
	HTML    string `json:"html,omitempty"`

	// EnvelopeTo is the SMTP RCPT TO address. When present it is preferred over
	// the header To for token extraction, because header To can be rewritten by
	// forwarders while the envelope recipient is the address mail was actually
	// delivered to. the reference platform reads the token from the envelope recipient for the
	// same reason (apps/inbound-mail getAddressTo uses envelopeTo[0]).
	EnvelopeTo string `json:"envelopeTo,omitempty"`

	// Threading headers, normalized as in the reference platform's finalizeMessage.
	InReplyTo  string            `json:"inReplyTo,omitempty"`
	References []string          `json:"references,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`

	Attachments []InboundEmailAttachment `json:"attachments,omitempty"`
}

// recipient returns the address the token should be extracted from: the
// envelope recipient when supplied, else the header To.
func (e InboundEmail) recipient() string {
	if e.EnvelopeTo != "" {
		return e.EnvelopeTo
	}
	return e.To
}

// extractToken pulls the opaque token from a reply mailbox of the form
// reply+<token>@<domain>. It returns ("", "", false) when addr is not a reply
// mailbox. The returned domain is the inbound-domain host, used to scope the
// verified-domain check.
//
// The address may carry a display name ("Name <reply+tok@d>"); the angle-bracket
// form is unwrapped first.
func extractToken(addr string) (token, domainHost string, ok bool) {
	addr = strings.TrimSpace(addr)
	if i := strings.LastIndexByte(addr, '<'); i >= 0 {
		if j := strings.IndexByte(addr[i:], '>'); j >= 0 {
			addr = addr[i+1 : i+j]
		}
	}
	at := strings.LastIndexByte(addr, '@')
	if at <= 0 || at == len(addr)-1 {
		return "", "", false
	}
	local, host := addr[:at], addr[at+1:]
	const prefix = "reply+"
	if !strings.HasPrefix(local, prefix) {
		return "", "", false
	}
	tok := local[len(prefix):]
	if tok == "" {
		return "", "", false
	}
	return tok, strings.ToLower(host), true
}

// bestBody returns the body to thread onto the conversation: the text part with
// quoted reply history stripped when cheap, falling back to the raw text, then
// to a crude HTML-tag strip of the HTML part.
func bestBody(e InboundEmail) string {
	if e.Text != "" {
		return stripQuotedHistory(e.Text)
	}
	if e.HTML != "" {
		return stripQuotedHistory(stripTags(e.HTML))
	}
	return ""
}

// stripQuotedHistory removes the common reply-quote tail: everything from the
// first quoted-history marker onward. It is deliberately conservative — only
// the cheap, high-precision markers — so it never eats real reply text. When no
// marker is found the whole body is returned.
func stripQuotedHistory(body string) string {
	lines := strings.Split(body, "\n")
	cut := len(lines)
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		switch {
		// "On <date>, <person> wrote:" — Gmail/Apple Mail attribution line.
		// Both wrap it at ~78 columns, so "wrote:" may sit alone one or two
		// lines down (#352).
		case strings.HasPrefix(line, "On ") && endsWithWrote(lines[i:min(i+3, len(lines))]):
			cut = i
		// Outlook / many clients original-message divider.
		case line == "-----Original Message-----":
			cut = i
		// Forwarded-message divider.
		case strings.HasPrefix(line, "-------- Forwarded message"):
			cut = i
		}
		if cut != len(lines) {
			break
		}
	}
	// A run of ">" lines that reaches the end of the body is quoted history
	// even with no attribution line above it. The same run followed by more
	// text is someone answering inline, and is left alone.
	if tail := quotedTail(lines[:cut]); tail < cut {
		cut = tail
	}
	return strings.TrimSpace(strings.Join(lines[:cut], "\n"))
}

// endsWithWrote reports whether any of lines ends with "wrote:".
func endsWithWrote(lines []string) bool {
	for _, l := range lines {
		if strings.HasSuffix(strings.TrimSpace(l), "wrote:") {
			return true
		}
	}
	return false
}

// quotedTail returns the index of the first line of a ">"-quoted run that
// continues, blank lines allowed, to the end of lines; len(lines) when the
// body does not end in one.
func quotedTail(lines []string) int {
	tail := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, ">") {
			break
		}
		tail = i
	}
	return tail
}

// stripTags is a crude HTML-to-text fallback: it drops everything between
// angle brackets. Used only when no text part exists.
func stripTags(html string) string {
	var b strings.Builder
	depth := 0
	for _, r := range html {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

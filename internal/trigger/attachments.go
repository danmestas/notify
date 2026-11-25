package trigger

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/danmestas/notify/internal/blobstore"
	"github.com/danmestas/notify/internal/domain"
)

// Attachment limits (#177). A request is rejected whole when any is exceeded.
const (
	MaxAttachments        = 10
	MaxAttachmentBytes    = 5 << 20  // decoded, per file
	MaxAttachmentsBytes   = 10 << 20 // decoded, all files
	maxAttachmentFilename = 200
)

// attachmentTypes is the allow-list of content types an attachment may have.
var attachmentTypes = map[string]bool{
	"application/pdf": true,
	"image/png":       true,
	"image/jpeg":      true,
}

// AttachmentInput is one file on a trigger request: base64 content with the
// name and type the recipient sees.
type AttachmentInput struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

// AttachmentError is a refused attachment: the caller's request is wrong, not
// the server.
type AttachmentError struct{ Msg string }

func (e *AttachmentError) Error() string { return e.Msg }

func attachmentErr(i int, format string, args ...any) *AttachmentError {
	return &AttachmentError{Msg: fmt.Sprintf("attachments[%d]: ", i) + fmt.Sprintf(format, args...)}
}

// decodedAttachment is a validated attachment with its bytes decoded.
type decodedAttachment struct {
	filename, contentType string
	content               []byte
}

// decodeAttachments checks every limit and decodes the content, or returns an
// *AttachmentError naming the first problem.
func decodeAttachments(in []AttachmentInput) ([]decodedAttachment, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > MaxAttachments {
		return nil, &AttachmentError{Msg: fmt.Sprintf("attachments: at most %d files are allowed, got %d", MaxAttachments, len(in))}
	}
	out := make([]decodedAttachment, 0, len(in))
	total := 0
	for i, a := range in {
		if err := checkFilename(a.Filename); err != "" {
			return nil, attachmentErr(i, "filename %s", err)
		}
		if !attachmentTypes[a.ContentType] {
			return nil, attachmentErr(i, "contentType %q is not allowed (use application/pdf, image/png or image/jpeg)", a.ContentType)
		}
		// DecodedLen is an upper bound: refuse an oversized file before
		// allocating for it.
		if base64.StdEncoding.DecodedLen(len(a.Content)) > MaxAttachmentBytes+2 {
			return nil, attachmentErr(i, "content is larger than %d MB", MaxAttachmentBytes>>20)
		}
		raw, err := base64.StdEncoding.DecodeString(a.Content)
		if err != nil {
			return nil, attachmentErr(i, "content is not valid base64")
		}
		if len(raw) == 0 {
			return nil, attachmentErr(i, "content is empty")
		}
		if len(raw) > MaxAttachmentBytes {
			return nil, attachmentErr(i, "content is larger than %d MB", MaxAttachmentBytes>>20)
		}
		total += len(raw)
		if total > MaxAttachmentsBytes {
			return nil, attachmentErr(i, "attachments together are larger than %d MB", MaxAttachmentsBytes>>20)
		}
		out = append(out, decodedAttachment{filename: a.Filename, contentType: a.ContentType, content: raw})
	}
	return out, nil
}

// checkFilename returns what is wrong with a filename, or "" when it is fine.
// The name lands in MIME headers and in provider JSON, so anything that could
// break a header line or name a path is refused rather than rewritten.
func checkFilename(name string) string {
	switch {
	case strings.TrimSpace(name) == "":
		return "is required"
	case utf8.RuneCountInString(name) > maxAttachmentFilename:
		return fmt.Sprintf("is longer than %d characters", maxAttachmentFilename)
	case name == "." || name == "..":
		return "is not a file name"
	case !utf8.ValidString(name):
		return "is not valid UTF-8"
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' || r == '"' {
			return `must not contain control characters, path separators or quotes`
		}
	}
	return ""
}

// storeAttachments puts each file in the blob store and returns the
// references a notification keeps. The bytes go no further: the run carries
// nothing of them.
func storeAttachments(ctx context.Context, blobs blobstore.Store, atts []decodedAttachment) ([]domain.EmailAttachment, error) {
	refs := make([]domain.EmailAttachment, 0, len(atts))
	for _, a := range atts {
		ref, err := blobs.Put(ctx, bytes.NewReader(a.content))
		if err != nil {
			return nil, fmt.Errorf("trigger: store attachment %q: %w", a.filename, err)
		}
		refs = append(refs, domain.EmailAttachment{
			Filename: a.filename, ContentType: a.contentType, SHA256: ref.SHA256, Size: ref.Size,
		})
	}
	return refs, nil
}

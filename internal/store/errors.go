package store

import "errors"

// ErrNotFound is returned by repository reads when no row matches the scoped
// query. Callers compare with errors.Is.
var ErrNotFound = errors.New("store: not found")

// ErrInvalidCursor is returned by keyset-paginated reads when the supplied
// opaque cursor token cannot be decoded (bad base64 or malformed JSON). It is
// a caller error, not a server fault — HTTP handlers map it to 400, not 500.
// Callers compare with errors.Is.
var ErrInvalidCursor = errors.New("store: invalid cursor")

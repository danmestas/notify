package store

import "errors"

// ErrNotFound is returned by repository reads when no row matches the scoped
// query. Callers compare with errors.Is.
var ErrNotFound = errors.New("store: not found")

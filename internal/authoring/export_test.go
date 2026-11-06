package authoring

import "time"

// SetRepublishBackoff shrinks RepublishAll's retry wait for a test and returns
// a func restoring it.
func SetRepublishBackoff(d time.Duration) (restore func()) {
	old := republishBackoff
	republishBackoff = d
	return func() { republishBackoff = old }
}

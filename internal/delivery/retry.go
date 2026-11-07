package delivery

import (
	"context"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/store"
)

// retryTransient is how every channel hands a transient provider failure
// (5xx, timeout, unreachable) back to the engine for retry. The attempt writes
// no message, so it reports the classified failure to the store's delivery
// observer first: an outage on a channel that retries must still count
// against the integration's health (#100). The retry that later succeeds
// writes a sent message, which counts as the success.
func retryTransient(ctx context.Context, s *store.Store, scope domain.Scope, integrationID string, err error) error {
	s.ReportFailedAttempt(ctx, scope, integrationID, string(providers.ClassifyError(err)), err.Error())
	return engine.NewRetryableError(err)
}

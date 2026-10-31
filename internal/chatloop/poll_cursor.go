package chatloop

import (
	"context"

	"github.com/danmestas/notify/internal/domain"
)

// loadPollCursor returns the persisted cursor for provider, or "" when there
// is none or it cannot be read: a poller then starts from scratch, as it did
// before cursors were persisted.
func (s *Service) loadPollCursor(ctx context.Context, scope domain.Scope, provider string) string {
	cursor, err := s.core.ChatPollCursors().Get(ctx, scope, provider)
	if err != nil {
		s.log.Warn("chatloop: load poll cursor; starting empty", "provider", provider, "err", err)
		return ""
	}
	return cursor
}

// savePollCursor persists the cursor after a batch has been dispatched. A
// failure is logged and the poll carries on; the cost is a replay on restart.
func (s *Service) savePollCursor(ctx context.Context, scope domain.Scope, provider, cursor string) {
	if err := s.core.ChatPollCursors().Set(ctx, scope, provider, cursor); err != nil && ctx.Err() == nil {
		s.log.Warn("chatloop: save poll cursor", "provider", provider, "err", err)
	}
}

// claimInbound records that the poller is about to dispatch the inbound
// message (provider, id). dispatch is true only for the first claim, so each
// message produces at most one agent turn even if the process dies between
// dispatch and the cursor save, or the platform returns it again. When the
// claim cannot be recorded, dispatch is false and retry is true: the message
// is not dispatched unclaimed (that risks a duplicate public reply), and the
// caller must keep its cursor at the batch start so the next poll re-reads
// the batch. Re-reading is safe because the messages already claimed are
// skipped.
func (s *Service) claimInbound(ctx context.Context, scope domain.Scope, provider, id string) (dispatch, retry bool) {
	fresh, err := s.core.ChatInbound().MarkHandled(ctx, scope, provider, id)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("chatloop: claim inbound message; retrying next poll", "provider", provider, "id", id, "err", err)
		}
		return false, true
	}
	return fresh, false
}

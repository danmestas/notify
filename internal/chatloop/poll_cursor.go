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
// message (provider, id). It reports true only for the first claim, so each
// message produces at most one agent turn even if the process dies between
// dispatch and the cursor save, or the platform returns it again. When the
// claim cannot be recorded the message is skipped rather than dispatched
// unclaimed, since an unclaimed dispatch risks a duplicate public reply; the
// platform returns it again on a later poll.
func (s *Service) claimInbound(ctx context.Context, scope domain.Scope, provider, id string) bool {
	fresh, err := s.core.ChatInbound().MarkHandled(ctx, scope, provider, id)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("chatloop: claim inbound message; skipping", "provider", provider, "id", id, "err", err)
		}
		return false
	}
	return fresh
}

package domain

import "context"

// scopeCtxKey is the private context key under which the request Scope is
// stored. Unexported so only this package can set/read the slot.
type scopeCtxKey struct{}

// ContextWithScope returns ctx carrying the tenancy Scope. The auth
// middleware sets it once per request; handlers read it via ScopeFromContext.
func ContextWithScope(ctx context.Context, s Scope) context.Context {
	return context.WithValue(ctx, scopeCtxKey{}, s)
}

// ScopeFromContext extracts the Scope placed by ContextWithScope. ok is false
// when no scope is present (request not behind the auth middleware).
func ScopeFromContext(ctx context.Context) (Scope, bool) {
	s, ok := ctx.Value(scopeCtxKey{}).(Scope)
	return s, ok
}

// pinnedSubscriberCtxKey is the private context key under which a
// subscriber-scoped credential records WHICH subscriber it authenticates.
type pinnedSubscriberCtxKey struct{}

// ContextWithPinnedSubscriber returns ctx carrying the subscriber a
// subscriber-scoped credential authenticates. It is set only by the auth
// middleware that accepts such credentials; a request authenticated with a
// tenant-wide API key carries no pinned subscriber and may address any
// subscriber in its scope, as before.
func ContextWithPinnedSubscriber(ctx context.Context, subscriberID string) context.Context {
	return context.WithValue(ctx, pinnedSubscriberCtxKey{}, subscriberID)
}

// PinnedSubscriberFromContext reports the subscriber the request's credential
// is confined to. ok is false for a tenant-wide credential.
//
// Handlers MUST treat ok==true as an override of any caller-supplied
// subscriber id rather than as something to compare against it: the whole
// point of the pin is that the request does not get to nominate whose inbox
// it is reading.
func PinnedSubscriberFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(pinnedSubscriberCtxKey{}).(string)
	return id, ok && id != ""
}

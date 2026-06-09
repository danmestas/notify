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

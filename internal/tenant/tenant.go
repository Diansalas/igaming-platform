// Package tenant defines the platform's tenant-context type and how it
// is carried through a request. Per CLAUDE.md: tenant_id is authoritative
// from server-side authenticated context only, and this package is the
// only sanctioned way to read or attach it - there is deliberately no
// function here that accepts a tenant id from a header, query parameter,
// or request body.
package tenant

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// ErrNoTenant is returned by FromContext when no tenant has been
// resolved onto the context - callers must treat this as unauthenticated/
// unauthorized, never fall back to a default tenant.
var ErrNoTenant = errors.New("tenant: no tenant in context")

type contextKey struct{}

// Context is the resolved, authenticated identity for a request. It is
// populated exclusively by the auth middleware after verifying the
// caller's JWT - see internal/auth.
//
// TenantID may legitimately be uuid.Nil for a platform-scoped principal
// (see docs/decisions/0011-platform-scoped-identity-tokens.md) - callers
// that require a specific tenant must check for this explicitly (or use
// auth.RequireTenantScope) rather than assume TenantID is always usable.
type Context struct {
	TenantID uuid.UUID
	// Role is the caller's role (see internal/auth.Role).
	Role string
	// PrincipalType distinguishes a player, staff, or service caller
	// (see internal/auth.PrincipalType).
	PrincipalType string
	// Subject is the authenticated principal id (player_account id,
	// staff_user id, or service id) from the JWT "sub" claim.
	Subject string
}

// WithContext attaches a resolved tenant Context. Only internal/auth's
// middleware should call this - business logic must never construct a
// tenant.Context from client-supplied data.
func WithContext(ctx context.Context, tc Context) context.Context {
	return context.WithValue(ctx, contextKey{}, tc)
}

// FromContext retrieves the tenant Context attached by the auth
// middleware. Every tenant-scoped handler and repository call must go
// through this rather than trusting any client-supplied tenant
// identifier (e.g. a path parameter or header).
func FromContext(ctx context.Context) (Context, error) {
	tc, ok := ctx.Value(contextKey{}).(Context)
	if !ok {
		return Context{}, ErrNoTenant
	}
	return tc, nil
}

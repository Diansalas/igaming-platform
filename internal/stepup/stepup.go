// Package stepup is the dependency-free core of ADR 0119 step-up (slice S4b):
// the closed class vocabulary, the freshness rule and the request-context
// assertion. It imports nothing from the platform, so the leaf packages that
// consume an assertion - internal/actorproof (the signer backstop, section
// 9.1) and internal/withdrawal (the require_step_up policy, section 9.3) - do
// not depend on internal/auth (which depends on internal/db and
// internal/tenant). internal/auth re-exports every name (auth.StepUpClass,
// auth.StepUpAssertion, ...); the HTTP step-up gate in internal/httpserver is
// the only production code that places an assertion in a context.
package stepup

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Class is the step-up class of a staff route or proof operation (ADR 0119
// section 8): a closed vocabulary.
type Class string

const (
	None      Class = "none"
	Financial Class = "financial"
	Critical  Class = "critical"
)

// Valid reports whether c is one of the three classes.
func (c Class) Valid() bool {
	switch c {
	case None, Financial, Critical:
		return true
	}
	return false
}

// ClockSkew is the tolerated amount by which an mfa_at may lie in the future
// (database clock vs application clock), section 7.3.
const ClockSkew = 5 * time.Second

// PolicyMaxAge is the withdrawal policy's require_step_up freshness (ADR 0119
// section 9.3, [ORCH-DEFAULT]): stricter than the route's financial window.
const PolicyMaxAge = 2 * time.Minute

// Fresh is the single freshness rule: mfaAt is set, not more than ClockSkew in
// the future, and now - mfaAt <= maxAge.
func Fresh(mfaAt, now time.Time, maxAge time.Duration) bool {
	if mfaAt.IsZero() || maxAge <= 0 {
		return false
	}
	age := now.Sub(mfaAt)
	return age >= -ClockSkew && age <= maxAge
}

// Assertion records that the step-up gate verified a fresh second factor for
// Subject's session SessionID at MFAAt (the older of the token claim and the
// sealed session row), for a route of class Class.
type Assertion struct {
	Subject   uuid.UUID
	SessionID uuid.UUID
	MFAAt     time.Time
	Class     Class
}

// Present reports whether a is a real assertion (the zero value means none).
func (a Assertion) Present() bool {
	return a.Subject != uuid.Nil && a.SessionID != uuid.Nil && !a.MFAAt.IsZero()
}

// FreshFor reports whether a is present, names subject, and is fresh for
// maxAge at now.
func (a Assertion) FreshFor(subject uuid.UUID, now time.Time, maxAge time.Duration) bool {
	return a.Present() && subject != uuid.Nil && a.Subject == subject && Fresh(a.MFAAt, now, maxAge)
}

type assertionKey struct{}

// ContextWith attaches a to ctx. Production code calls it only through
// auth.ContextWithStepUpAssertion from the HTTP step-up gate; integration-test
// support (internal/actorproof/prooftest) places a synthetic one. A static
// test pins the callers.
func ContextWith(ctx context.Context, a Assertion) context.Context {
	return context.WithValue(ctx, assertionKey{}, a)
}

type staffRequestKey struct{}

// WithStaffRequest marks ctx as a request authenticated by a STAFF bearer
// token (auth.Middleware is the only production caller). It carries no
// assurance: it only tells the actor-proof backstop's integration-test fallback
// (internal/actorproof, integration builds only) that this is a real HTTP
// staff request, to which the fallback never applies.
func WithStaffRequest(ctx context.Context) context.Context {
	return context.WithValue(ctx, staffRequestKey{}, true)
}

// IsStaffRequest reports whether ctx was marked by WithStaffRequest.
func IsStaffRequest(ctx context.Context) bool {
	v, _ := ctx.Value(staffRequestKey{}).(bool)
	return v
}

// From returns the request's assertion, if one is present.
func From(ctx context.Context) (Assertion, bool) {
	a, ok := ctx.Value(assertionKey{}).(Assertion)
	return a, ok && a.Present()
}

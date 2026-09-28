package alerting

import (
	"context"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// ScopeKind is the originating scope a ScopedRunner reopens for a
// detached retry (ADR §5 "Scope"; C-102-1/SR-1). It is captured from the
// transaction that is ALREADY BEING OPENED by the business call site,
// never supplied by, or derived from, the Alert being raised.
type ScopeKind int

const (
	ScopeTenant ScopeKind = iota
	ScopeTenantPrincipal
	ScopePlatformAdmin
)

// RaiseScope is the originating scope of a raise site, recorded once by a
// ScopedRunner and reopened verbatim by a detached retry or the
// per-transaction collector's Flush (§5, §7.3). Deliberately has no
// notion of "which alert" - that is the whole point: the alert's contents
// can never influence which scope a retry runs in.
type RaiseScope struct {
	Kind            ScopeKind
	TenantID        uuid.UUID
	PrincipalID     uuid.UUID
	PlatformAdminID uuid.UUID
}

// ScopedRunner wraps exactly one of db.WithTenant, db.WithPrincipalScope
// or db.WithPlatformAdmin, and records the RaiseScope that call opens.
// There is deliberately no constructor here for a dispatcher-scoped
// runner (db.ServiceAlertDispatcher) - business code can only ever be
// handed one of the three constructors below, so it structurally cannot
// "borrow" the dispatcher identity (AL-10). The dispatcher and the §6.3
// fallback use db.WithPlatformService directly, never through this
// interface - see dispatcher.go and fallback.go.
type ScopedRunner interface {
	// Scope returns the originating scope this runner reopens.
	Scope() RaiseScope
	// Run executes fn in a freshly opened transaction of this runner's
	// scope, exactly like the underlying db.With* call.
	Run(ctx context.Context, fn db.TxFunc) error
	// Pool returns the *db.Pool this runner opens transactions against -
	// used only by RaiseDetached's terminal fallback (fallback.go), which
	// must open its OWN WithPlatformService transaction (never this
	// runner's scope) to persist alerting.raise_failed.
	Pool() *db.Pool
}

type tenantRunner struct {
	pool     *db.Pool
	tenantID uuid.UUID
}

// NewTenantRunner is a ScopedRunner over db.WithTenant(tenantID, ...) -
// used by every tenant-session raise site (T10/T13d, reconciliation runs,
// handler-integrity detached raises).
func NewTenantRunner(pool *db.Pool, tenantID uuid.UUID) ScopedRunner {
	return tenantRunner{pool: pool, tenantID: tenantID}
}

func (r tenantRunner) Scope() RaiseScope { return RaiseScope{Kind: ScopeTenant, TenantID: r.tenantID} }
func (r tenantRunner) Run(ctx context.Context, fn db.TxFunc) error {
	return r.pool.WithTenant(ctx, r.tenantID, fn)
}
func (r tenantRunner) Pool() *db.Pool { return r.pool }

type principalRunner struct {
	pool        *db.Pool
	tenantID    uuid.UUID
	principalID uuid.UUID
}

// NewPrincipalRunner is a ScopedRunner over
// db.WithPrincipalScope(tenantID, principalID, ...) - used by raise sites
// that need the acting staff principal in scope (e.g. a tenant-filed
// kill-switch engage).
func NewPrincipalRunner(pool *db.Pool, tenantID, principalID uuid.UUID) ScopedRunner {
	return principalRunner{pool: pool, tenantID: tenantID, principalID: principalID}
}

func (r principalRunner) Scope() RaiseScope {
	return RaiseScope{Kind: ScopeTenantPrincipal, TenantID: r.tenantID, PrincipalID: r.principalID}
}
func (r principalRunner) Run(ctx context.Context, fn db.TxFunc) error {
	return r.pool.WithPrincipalScope(ctx, r.tenantID, r.principalID, fn)
}
func (r principalRunner) Pool() *db.Pool { return r.pool }

type platformAdminRunner struct {
	pool        *db.Pool
	principalID uuid.UUID
}

// NewPlatformAdminRunner is a ScopedRunner over
// db.WithPlatformAdmin(principalID, ...) - used by platform-filed raise
// sites (e.g. a platform takeover kill-switch engage).
func NewPlatformAdminRunner(pool *db.Pool, principalID uuid.UUID) ScopedRunner {
	return platformAdminRunner{pool: pool, principalID: principalID}
}

func (r platformAdminRunner) Scope() RaiseScope {
	return RaiseScope{Kind: ScopePlatformAdmin, PlatformAdminID: r.principalID}
}
func (r platformAdminRunner) Run(ctx context.Context, fn db.TxFunc) error {
	return r.pool.WithPlatformAdmin(ctx, r.principalID, fn)
}
func (r platformAdminRunner) Pool() *db.Pool { return r.pool }

// subjectTenant returns the tenant id a ScopedRunner's own scope
// represents, for the Go-side refusal (C-102-1): "a platform-owned Kind
// raised from a tenant scope whose subject differs from the scope's
// tenant is refused in Go before any SQL". PlatformAdmin scope has no
// tenant of its own, so it never fails this check (a platform admin may
// legitimately raise with any subject).
func (s RaiseScope) subjectTenant() (uuid.UUID, bool) {
	switch s.Kind {
	case ScopeTenant, ScopeTenantPrincipal:
		return s.TenantID, true
	default:
		return uuid.Nil, false
	}
}

// checkSubjectMatchesScope implements the C-102-1 Go refusal: a
// platform-owned Kind's subject must equal the raising scope's own
// tenant, whenever that scope has one. This runs in Go, before any SQL,
// so a mismatch is refused deterministically and cheaply - the database
// policy (alerts_subject_tenant_raise) refuses it independently too, but
// this is what keeps the refusal out of the SQLSTATE-swallow path
// entirely for the common case.
func checkSubjectMatchesScope(scope RaiseScope, a Alert) error {
	def, ok := Def(a.Kind)
	if !ok {
		return &invalidAlertError{kind: a.Kind, msg: "unknown kind"}
	}
	if !def.RequiresSubject {
		return nil
	}
	scopeTenant, hasTenant := scope.subjectTenant()
	if hasTenant && scopeTenant != a.SubjectTenantID {
		return &invalidAlertError{kind: a.Kind, msg: "subject tenant does not match the raising session's own tenant"}
	}
	return nil
}

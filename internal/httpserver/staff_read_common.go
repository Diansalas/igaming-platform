package httpserver

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// ADR 0114 slices S2 and S4: shared plumbing of the new READ-ONLY staff routes under
// /v1/admin/tenants/{tenantID}/ (R1 payment attempts, R2 reconciliation). Everything here is a
// read: the only write a read path may make is the platform-action audit row of a platform
// caller (ADR 0114 section 4, ADR 0104), which is committed BEFORE any tenant data is read, so a
// platform read that cannot be audited returns nothing (fail closed).

// staffReadCall is the verified caller and the canActOnTenant-validated {tenantID} path value.
// tenantID is never read from a query string, a body or a header: the one source for a tenant
// caller is its token, and a platform caller names the tenant in the path only.
type staffReadCall struct {
	requestID string
	logger    observabilityLogger
	tc        tenant.Context
	subject   uuid.UUID
	target    uuid.UUID
	ip        string
	ua        string
}

func (c staffReadCall) platform() bool { return c.tc.TenantID == uuid.Nil }

// beginStaffRead resolves the caller. A malformed path tenant is 400. A tenant-scoped caller
// naming a tenant other than its own gets 404 with no data and no body detail (the existence of
// another tenant is not confirmed); a platform caller may name any tenant. The route permission
// has already been enforced by the middleware (R1) or is enforced by the handler per stream (R2),
// in both cases before any query.
func beginStaffRead(deps Deps, w http.ResponseWriter, r *http.Request) (staffReadCall, bool) {
	c := staffReadCall{
		requestID: observability.RequestIDFromContext(r.Context()),
		logger:    observability.LoggerFromContext(r.Context(), deps.Logger),
		ip:        clientIP(r),
		ua:        r.UserAgent(),
	}
	tc, err := tenant.FromContext(r.Context())
	if err != nil {
		apierror.Write(w, c.requestID, apierror.CodeUnauthorized, "no authenticated context")
		return c, false
	}
	subject, err := uuid.Parse(tc.Subject)
	if err != nil || subject == uuid.Nil {
		apierror.Write(w, c.requestID, apierror.CodeUnauthorized, "no authenticated context")
		return c, false
	}
	target, err := uuid.Parse(r.PathValue("tenantID"))
	if err != nil || target == uuid.Nil {
		apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid tenant id")
		return c, false
	}
	c.tc, c.subject, c.target = tc, subject, target
	if !canActOnTenant(tc, target) {
		c.logger.Warn("staff_read_foreign_tenant", "path", r.URL.Path)
		apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
		return c, false
	}
	return c, true
}

// auditPlatformRead records the tenant-visible platform-action audit row (ADR 0104: a platform
// row, tenant_id NULL, subject_tenant_id = the route-validated target) for a platform caller's
// read, and reports whether the target tenant exists. It is a no-op for a tenant caller (a
// tenant reading its own data is not a platform action). The row carries closed codes and the
// route-validated target only: no filter values that could hold free text, no subject ids of
// the rows read.
func (c staffReadCall) auditPlatformRead(ctx context.Context, deps Deps, action, targetType, targetID string, meta map[string]any) (tenantExists bool, err error) {
	if !c.platform() {
		return true, nil
	}
	md := map[string]any{"target_tenant_id": c.target.String()}
	for k, v := range meta {
		md[k] = v
	}
	entry := audit.Entry{
		ActorType: audit.ActorStaff, ActorID: c.subject, SubjectTenantID: c.target,
		Action: action, TargetType: targetType, TargetID: targetID, Outcome: audit.OutcomeSuccess,
		IPAddress: c.ip, UserAgent: c.ua, RequestID: c.requestID, Metadata: md,
	}
	err = deps.DB.WithPlatformAdmin(ctx, c.subject, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1)`, c.target).Scan(&tenantExists); err != nil {
			return err
		}
		if !tenantExists {
			return nil
		}
		return audit.Record(ctx, tx, entry)
	})
	return tenantExists, err
}

// strictPage is the strict ?limit=&offset= parser of the new read routes (ADR 0114: 400 on bad
// input rather than the clamping of parsePageParams). limit 1..200, default 50; offset >= 0.
func strictPage(q url.Values) (limit, offset int, ok bool) {
	limit, offset = defaultPageLimit, 0
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > maxPageLimit {
			return 0, 0, false
		}
		limit = n
	}
	if q.Has("offset") {
		n, err := strconv.Atoi(q.Get("offset"))
		if err != nil || n < 0 || n > 1_000_000 {
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

// onlyParams reports whether every query parameter name is in allowed. An unknown name (for
// example a client-supplied tenant_id, or a misspelt filter) is refused rather than ignored.
func onlyParams(q url.Values, allowed ...string) bool {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	for k := range q {
		if !ok[k] {
			return false
		}
	}
	return true
}

// singleParam returns the value of a query parameter that must appear at most once.
func singleParam(q url.Values, name string) (string, bool) {
	vs := q[name]
	switch len(vs) {
	case 0:
		return "", true
	case 1:
		return vs[0], true
	}
	return "", false
}

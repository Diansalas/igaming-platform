package httpserver

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// ADR 0114 slice S0 (H1): staff-identity redaction on tenant-readable DTOs.
//
// A tenant-scoped caller never sees the staff id of a PLATFORM actor (ADR 0114 section 1
// principle 7, invariant 2; ADR 0112 slice 6 review condition, whose launchCall.dto is the
// precedent). It sees the actor's scope marker (the sibling `*_scope` field) only. A tenant caller
// keeps seeing its own tenant's staff ids; a platform caller sees everything.
//
// The only way to put a staff id into a response DTO is a staffRef, and the only constructors are
// the staffViewer methods below. A staffRef's zero value marshals as JSON null, so a DTO that is
// built without the helper fails closed. TestStaffIdentity_StaticPin_* walks every response struct
// in this package and fails the build on any staff-id-shaped JSON field that is neither a staffRef
// nor an explicitly justified allowlist entry.

// Actor scope markers as stored beside every governed actor id (`*_by_scope` columns).
const (
	staffScopeTenant   = "tenant"
	staffScopePlatform = "platform"
)

// staffRef is a staff id that has been through the redaction helper. Zero value = hidden = null.
type staffRef struct{ id string }

// MarshalJSON renders the id, or null when it was redacted (or never set).
func (s staffRef) MarshalJSON() ([]byte, error) {
	if s.id == "" {
		return []byte("null"), nil
	}
	return json.Marshal(s.id)
}

// staffViewer is the caller, as far as redaction is concerned: platform (sees all) or
// tenant-scoped (sees tenant-scope actors only).
type staffViewer struct{ platform bool }

// staffViewerOf derives the viewer from the server-side authenticated context only. A nil tenant
// id is a platform principal (the same test canActOnTenant uses); anything else is tenant-scoped.
func staffViewerOf(tc tenant.Context) staffViewer {
	return staffViewer{platform: tc.TenantID == uuid.Nil}
}

// sees reports whether this viewer may see the identity of an actor of the given scope. Fail
// closed: for a tenant viewer only the exact scope "tenant" is visible; an empty or unknown scope
// is hidden.
func (v staffViewer) sees(actorScope string) bool {
	return v.platform || actorScope == staffScopeTenant
}

// ref returns the staff id for an actor of actorScope, or a hidden (null) ref.
func (v staffViewer) ref(actorScope string, id uuid.UUID) staffRef {
	if id == uuid.Nil || !v.sees(actorScope) {
		return staffRef{}
	}
	return staffRef{id: id.String()}
}

// optRef is ref for an optional actor: nil id (no such actor yet) stays nil so an omitempty field
// is omitted; a present actor the viewer may not see becomes a non-nil hidden ref (JSON null).
func (v staffViewer) optRef(actorScope *string, id *uuid.UUID) *staffRef {
	if id == nil {
		return nil
	}
	scope := ""
	if actorScope != nil {
		scope = *actorScope
	}
	r := v.ref(scope, *id)
	return &r
}

// tenantStaffIDs returns which of ids are staff of tenantID. It is for rows that carry an actor
// id but no scope column (provider-credential revoked_by, the generic tenant audit log). It runs
// inside the caller's tenant-scoped transaction and also filters on tenant_id explicitly, so a
// platform staff id (tenant_id IS NULL, hidden by staff_users RLS from a tenant session) is never
// in the result. The result is only used to choose between id and null.
func tenantStaffIDs(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(ids) == 0 || tenantID == uuid.Nil {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT id FROM staff_users WHERE tenant_id = $1 AND id = ANY($2)`, tenantID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// scopeFromMembership maps "is a staff user of the viewer's tenant" to an actor scope marker.
func scopeFromMembership(own bool) string {
	if own {
		return staffScopeTenant
	}
	return staffScopePlatform
}

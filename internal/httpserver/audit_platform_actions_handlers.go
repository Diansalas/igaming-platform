// PRH-2 G1 (ADR 0104 §5, KS-AUDIT-TENANT-1): GET /v1/admin/audit-log/
// platform-actions - a tenant's read-only projection of platform-scope
// audit rows that concern it (today: kill-switch actions on that tenant,
// via migration 0109's subject_tenant_id/subject_tenant_read RLS policy).
// This is ADDITIVE: newListAuditLogHandler and newListPlatformAuditLogHandler
// (admin_routes.go) are unchanged (ADR 0104 AT-7).
package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// platformActionsAuditListQuery is the tenant-scoped read of subject
// rows. The explicit WHERE is defence in depth (ADR 0104 §5.1) - RLS
// (subject_tenant_read, migration 0109) is the actual enforcement, and
// this runs inside deps.DB.WithTenant(tc.TenantID, ...) so a caller
// naming a different tenant here would simply see zero rows regardless
// of what $1 is, since RLS binds subject_tenant_id to the SESSION's own
// app.tenant_id, never to a query parameter.
const platformActionsAuditListQuery = `
	SELECT id, actor_type, actor_id, action, target_type, target_id, outcome, created_at, metadata, count(*) OVER()
	  FROM audit_log
	 WHERE tenant_id IS NULL AND subject_tenant_id = $1
	 ORDER BY created_at DESC
	 LIMIT $2 OFFSET $3`

// platformActionsChainSourceQuery is F-5 (code review; orchestrator
// decision: conform to ADR §5.5, "the tenant's own rows; subject rows for
// that tenant"): the approval chain must be built from EVERY RLS-visible
// row for the tenant - not just the subject rows platformActionsAuditListQuery
// returns, and not just the current PAGE. This query runs in the exact
// SAME WithTenant(tc.TenantID) session as the page query above, with an
// explicit filter covering both arms dual_scope_isolation/subject_tenant_read
// already admit under that one session: the tenant's own rows
// (tenant_id = $1) and the tenant's subject rows (tenant_id IS NULL AND
// subject_tenant_id = $1). It is NEVER a platform or unscoped query - no
// WithoutTenant/WithPlatformAdmin call is involved anywhere in chain
// assembly. Scoped to the kill-switch action family (the only actions
// with a chain key at all today) so this stays bounded regardless of a
// tenant's overall audit_log volume; unbounded (no LIMIT) so a chain
// member on a DIFFERENT page than the row that names it is still found
// (fixes ADR §5.5's "b) chains split across pages").
const platformActionsChainSourceQuery = `
	SELECT id, actor_type, actor_id, action, target_type, target_id, outcome, created_at, metadata
	  FROM audit_log
	 WHERE action LIKE 'payments_kill_switch.%'
	   AND (tenant_id = $1 OR (tenant_id IS NULL AND subject_tenant_id = $1))`

// staffDisplayNameLookupQuery is ADR 0104 §5.4/AT-8's second, short read:
// ONLY id and display_name, NEVER email - a mutant that adds `email` here
// must be caught by TestPlatformActionsAudit_NameLookupNeverSelectsEmail's
// assertion on this exact string.
const staffDisplayNameLookupQuery = `SELECT id, display_name FROM staff_users WHERE id = ANY($1)`

type platformActionAuditActor struct {
	Scope       string  `json:"scope"`
	StaffID     string  `json:"staff_id"`
	DisplayName *string `json:"display_name"`
}

type platformActionAuditChainEntry struct {
	ID               string  `json:"id"`
	Action           string  `json:"action"`
	Outcome          string  `json:"outcome"`
	CreatedAt        string  `json:"created_at"`
	ActorStaffID     string  `json:"actor_staff_id,omitempty"`
	ActorDisplayName *string `json:"actor_display_name"`
}

type platformActionAuditEntry struct {
	ID            string                          `json:"id"`
	CreatedAt     string                          `json:"created_at"`
	Action        string                          `json:"action"`
	TargetType    string                          `json:"target_type,omitempty"`
	TargetID      string                          `json:"target_id,omitempty"`
	Outcome       string                          `json:"outcome"`
	Actor         platformActionAuditActor        `json:"actor"`
	ReasonCode    string                          `json:"reason_code,omitempty"`
	Before        any                             `json:"before,omitempty"`
	After         any                             `json:"after,omitempty"`
	ExtraMetadata map[string]any                  `json:"extra_metadata,omitempty"`
	ApprovalChain []platformActionAuditChainEntry `json:"approval_chain,omitempty"`
}

// platformActionRawRow is one audit_log row read under the tenant's own
// RLS-scoped session - nothing here ever comes from a second, wider-scope
// query (AT-9), except names, which are attached afterwards from a
// separate id-restricted lookup (§5.4).
type platformActionRawRow struct {
	id, actorType, action, outcome string
	actorID                        *uuid.UUID
	targetType, targetID           *string
	createdAt                      time.Time
	metadata                       map[string]any
}

// newListPlatformActionsAuditLogHandler implements ADR 0104 §5.1.
// RequireTenantScope already refuses a nil-tenant (platform) caller
// before this handler runs; the tc.TenantID == uuid.Nil check below is
// defence in depth against that guard ever regressing.
func newListPlatformActionsAuditLogHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		if tc.TenantID == uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeForbidden, "requires a tenant-scoped session")
			return
		}

		p := parsePageParams(r)

		var rows, chainRows []platformActionRawRow
		var total int
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			qr, qerr := tx.Query(ctx, platformActionsAuditListQuery, tc.TenantID, p.Limit, p.Offset)
			if qerr != nil {
				return qerr
			}
			for qr.Next() {
				var row platformActionRawRow
				var id uuid.UUID
				var metadataJSON []byte
				if err := qr.Scan(&id, &row.actorType, &row.actorID, &row.action, &row.targetType, &row.targetID, &row.outcome, &row.createdAt, &metadataJSON, &total); err != nil {
					qr.Close()
					return err
				}
				row.id = id.String()
				if len(metadataJSON) > 0 {
					if err := json.Unmarshal(metadataJSON, &row.metadata); err != nil {
						qr.Close()
						return err
					}
				}
				rows = append(rows, row)
			}
			qerr = qr.Err()
			qr.Close()
			if qerr != nil {
				return qerr
			}

			// F-5: the chain source, in the SAME tenant session, with its
			// own explicit filter - see platformActionsChainSourceQuery's
			// own doc comment.
			cr, cerr := tx.Query(ctx, platformActionsChainSourceQuery, tc.TenantID)
			if cerr != nil {
				return cerr
			}
			defer cr.Close()
			for cr.Next() {
				var row platformActionRawRow
				var id uuid.UUID
				var metadataJSON []byte
				if err := cr.Scan(&id, &row.actorType, &row.actorID, &row.action, &row.targetType, &row.targetID, &row.outcome, &row.createdAt, &metadataJSON); err != nil {
					return err
				}
				row.id = id.String()
				if len(metadataJSON) > 0 {
					if err := json.Unmarshal(metadataJSON, &row.metadata); err != nil {
						return err
					}
				}
				chainRows = append(chainRows, row)
			}
			return cr.Err()
		})
		if err != nil {
			logger.Error("list_platform_actions_audit_log_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to load audit log")
			return
		}

		// §5.4/AT-8: a second, SHORT lookup restricted to the actor ids
		// present in the RLS-filtered results above (both the page and the
		// chain source - the chain source includes the tenant's OWN rows
		// too, per F-5, whose actor may be a tenant staff member; the
		// lookup itself still never widens beyond id/display_name, never
		// email, and a tenant staff id simply resolves to no row under
		// WithoutTenant, exactly like any other name this lookup cannot
		// find - see security confirmation N-3's own note on this). A
		// failure here fails the WHOLE request (§5.5's "or the request
		// fails") rather than silently falling back to showing no names as
		// if that were a successful read.
		names, err := lookupStaffDisplayNames(r.Context(), deps, append(append([]platformActionRawRow{}, rows...), chainRows...))
		if err != nil {
			logger.Error("platform_actions_audit_name_lookup_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to load audit log")
			return
		}

		presentation := audit.ResolvePresentationOrRestrictive(r.Context(), deps.AuditPresentationResolver, tc.TenantID)

		entries := buildPlatformActionEntries(rows, chainRows, names, presentation)
		writeJSON(w, http.StatusOK, newPagedResponse(entries, p, total))
	}
}

func lookupStaffDisplayNames(ctx context.Context, deps Deps, rows []platformActionRawRow) (map[uuid.UUID]*string, error) {
	idSet := map[uuid.UUID]struct{}{}
	for _, row := range rows {
		if row.actorID != nil {
			idSet[*row.actorID] = struct{}{}
		}
	}
	names := map[uuid.UUID]*string{}
	if len(idSet) == 0 {
		return names, nil
	}
	ids := make([]uuid.UUID, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	// WithoutTenant (ADR 0104 §5.4): can see platform staff, which a
	// tenant-scoped session structurally cannot (staff_users'
	// dual_scope_isolation, migration 0011). This does not widen what the
	// CALLER's own tenant session can read from audit_log - it is a
	// separate, narrower connection used only to resolve names.
	err := deps.DB.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		qr, qerr := tx.Query(ctx, staffDisplayNameLookupQuery, ids)
		if qerr != nil {
			return qerr
		}
		defer qr.Close()
		for qr.Next() {
			var id uuid.UUID
			var dn *string
			if err := qr.Scan(&id, &dn); err != nil {
				return err
			}
			names[id] = dn
		}
		return qr.Err()
	})
	if err != nil {
		return nil, err
	}
	return names, nil
}

// killSwitchChainKey (ADR 0104 §5.5) is a best-effort grouping key linking
// a kill-switch audit row back to the switch it concerns, using ONLY
// fields already present in the RLS-filtered row itself - metadata's own
// "kill_switch_id" (request_release, cancel_release) when present,
// otherwise the row's own target_id when target_type is
// "payment_kill_switch" (engage, approve_release, and a denied
// request_release, which targets the switch directly). Rows this cannot
// key (e.g. a denied engage with no switch yet, or a denied
// approve/cancel naming only a release_request_id) return "" and are
// never grouped into any chain - they still appear in the flat action
// list. This never uses a wider-scope query (AT-9).
func killSwitchChainKey(row platformActionRawRow) string {
	if v, ok := row.metadata["kill_switch_id"].(string); ok && v != "" {
		return v
	}
	if row.targetType != nil && *row.targetType == "payment_kill_switch" && row.targetID != nil && *row.targetID != "" {
		return *row.targetID
	}
	return ""
}

// buildPlatformActionEntries projects pageRows (the current page of
// SUBJECT rows only - what this endpoint actually lists, per ADR §5.1)
// into response entries, but builds each entry's approval_chain from
// chainRows - EVERY RLS-visible kill-switch-action row for the tenant
// (its own rows plus its subject rows, unbounded by pagination) - per
// F-5/ADR §5.5. chainRows always includes pageRows' own rows too (the
// chain source query's WHERE admits every subject row, which is exactly
// what pageRows is), so no separate merge is needed here.
func buildPlatformActionEntries(pageRows, chainRows []platformActionRawRow, names map[uuid.UUID]*string, presentation audit.Presentation) []platformActionAuditEntry {
	byKey := map[string][]platformActionRawRow{}
	for _, row := range chainRows {
		if key := killSwitchChainKey(row); key != "" {
			byKey[key] = append(byKey[key], row)
		}
	}

	entries := make([]platformActionAuditEntry, 0, len(pageRows))
	for _, row := range pageRows {
		entries = append(entries, toPlatformActionEntry(row, names, presentation, byKey))
	}
	return entries
}

func toPlatformActionEntry(row platformActionRawRow, names map[uuid.UUID]*string, presentation audit.Presentation, byKey map[string][]platformActionRawRow) platformActionAuditEntry {
	entry := platformActionAuditEntry{
		ID: row.id, CreatedAt: row.createdAt.UTC().Format(time.RFC3339Nano),
		Action: row.action, Outcome: row.outcome,
	}
	if row.targetType != nil {
		entry.TargetType = *row.targetType
	}
	if row.targetID != nil {
		entry.TargetID = *row.targetID
	}
	entry.Actor = platformActionAuditActor{Scope: "platform"}
	if row.actorID != nil {
		entry.Actor.StaffID = row.actorID.String()
		entry.Actor.DisplayName = names[*row.actorID]
	}

	// §5.2: an action with NO TenantPresentation entry projects only the
	// base columns above - fail closed on disclosure. ip_address,
	// user_agent and request_id are never read here at all (AT-6).
	if allow, ok := audit.TenantPresentation[row.action]; ok {
		if allow.ReasonCode {
			if rc, ok := row.metadata["reason_code"].(string); ok {
				entry.ReasonCode = rc
			}
		}
		if allow.BeforeAfter {
			entry.Before = row.metadata["before"]
			entry.After = row.metadata["after"]
		}
	}

	// SA-6/§5.3: never true in PRH-2's compiled-in defaults or its
	// fail-closed fallback - kept as a real branch (not deleted) so
	// AUDIT-PRESENTATION-POLICY-1 has somewhere to plug in without a
	// rewrite, and so a forced-error test can prove the wide branch is
	// unreachable when the resolver fails.
	if presentation.ShowFreeFormMetadata {
		extra := map[string]any{}
		for k, v := range row.metadata {
			if k == "reason_code" || k == "before" || k == "after" {
				continue
			}
			extra[k] = v
		}
		if len(extra) > 0 {
			entry.ExtraMetadata = extra
		}
	}

	if key := killSwitchChainKey(row); key != "" {
		for _, sib := range byKey[key] {
			if sib.id == row.id {
				continue
			}
			chainEntry := platformActionAuditChainEntry{
				ID: sib.id, Action: sib.action, Outcome: sib.outcome,
				CreatedAt: sib.createdAt.UTC().Format(time.RFC3339Nano),
			}
			if sib.actorID != nil {
				chainEntry.ActorStaffID = sib.actorID.String()
				chainEntry.ActorDisplayName = names[*sib.actorID]
			}
			entry.ApprovalChain = append(entry.ApprovalChain, chainEntry)
		}
	}
	return entry
}

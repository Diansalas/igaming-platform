package audit

// TenantPresentationEntry lists exactly which metadata keys a tenant's
// platform-actions projection (ADR 0104 §5.2) may surface for one action.
// An action with NO entry in TenantPresentation projects ONLY the base
// columns (id, created_at, action, target_type, target_id, outcome,
// actor, approval chain) - "an action without an entry projects only the
// columns: fail closed on disclosure" (§5.2). ip_address, user_agent,
// request_id and any metadata key not covered by an entry's own fields
// are NEVER surfaced by default (AT-6), regardless of this map, unless a
// future Presentation.ShowFreeFormMetadata is true (never true in PRH-2 -
// see presentation.go).
type TenantPresentationEntry struct {
	// ReasonCode surfaces metadata["reason_code"] (when present) as the
	// response's reason_code field.
	ReasonCode bool
	// BeforeAfter surfaces metadata["before"]/metadata["after"] (when
	// present) as the response's before/after fields.
	BeforeAfter bool
}

// TenantPresentation is the per-action allowlist (ADR 0104 §5.2; §7 MUT
// "the projection including ip_address" must never happen regardless of
// this map - ip_address/user_agent/request_id are never read from this
// map at all, by construction, in the read path). Keyed by audit_log
// action. PRH-2 populates it for every platform-scope kill-switch action
// G1 puts in tenant scope (ADR 0104 §4's "in scope" list) - the only
// actions that can ever carry a subject_tenant_id today.
var TenantPresentation = map[string]TenantPresentationEntry{
	"payments_kill_switch.engage":          {ReasonCode: true, BeforeAfter: true},
	"payments_kill_switch.request_release": {ReasonCode: true, BeforeAfter: true},
	"payments_kill_switch.approve_release": {ReasonCode: false, BeforeAfter: true},
	"payments_kill_switch.cancel_release":  {ReasonCode: false, BeforeAfter: true},
}

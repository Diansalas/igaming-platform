package licensingchange

// Migration 0190 (c4): the two screening loosening kinds. Extension recipe (docs/plans/lic2-maker-checker-record.md section 6): the
// registry rows, the classifier branches and the executors (screening_policy_loosen_apply / screening_loosening_settings_apply) are
// in migration 0190; this file is the Go register() of the two kinds. The executors call the database functions only: every rule
// (configured and enabled, the cooling period, the interval floor, the effective-time window, the production gate guard, "this is a
// loosening") is enforced by the DATABASE; the Go decode only refuses a malformed payload early.
//
//	aml_screening_policy_loosen        ALWAYS widening: two distinct platform_admin Persons. Writes the next aml_screening_policies
//	                                   version in the transaction of the final approval. Refused (LC050, closed hint) unless the
//	                                   tenant capability is configured AND enabled, and until the cooling period (from filing) has
//	                                   elapsed.
//	aml_screening_loosening_configure  Sets the per-tenant capability. Widening iff it enables loosening or shortens the cooling
//	                                   period (two Persons); disabling or lengthening is narrowing (single actor, at once).
//
// INERT BY DEFAULT: no capability row ships; "not configured" refuses every loosening. NO VALUE ships: the cooling period, the
// interval, the grace and the lists are all in the payload of a recorded decision. Tenant staff never request, approve or see these
// requests (platform only, no subject tenant projection: the content is AML configuration).
//
// Migration 0191 (d1, D4) adds the third kind, aml_screening_trigger_policy_loosen, for LOOSENING the screening TRIGGER policy (an event
// removed, a withdrawal gate switched off, a longer gate alert threshold). It is gated by the SAME per-tenant capability row, follows the same
// rules (two Persons, officer grants + owner-scope quorum for requester, counted approvers and executor, cooling period, advisory lock, table
// guard) and can NEVER enable a withdrawal gate. The routed trigger policy write is tighten-only since 0191.
//
// Nothing here changes the withdrawal gate or its production guard (aml_screening_trigger_policy_set keeps its body; it is owner-only since 0191).

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
)

// The screening loosening kinds (migration 0190).
const (
	KindAMLScreeningPolicyLoosen       Kind = "aml_screening_policy_loosen"
	KindAMLScreeningLooseningConfigure Kind = "aml_screening_loosening_configure"
	// KindAMLScreeningTriggerPolicyLoosen (migration 0191, d1 D4) loosens the screening TRIGGER policy under two Persons.
	KindAMLScreeningTriggerPolicyLoosen Kind = "aml_screening_trigger_policy_loosen"
)

// MaxLooseningCoolingSeconds is the structural bound of the cooling period: a request expires 72 h after filing (the database CHECK is
// 255600 s = 71 h). It is NOT a policy value.
const MaxLooseningCoolingSeconds = 255600

var evidenceRefRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/#-]{0,199}$`)

// ScreeningPolicyLoosenPayload is the payload of KindAMLScreeningPolicyLoosen. Every field is required (there is no default).
// EffectiveOffsetSeconds is how long after EXECUTION the version takes effect (0 = at execution), 0..2592000 (30 days), so the effective
// time is always inside the window of the screening policy route.
type ScreeningPolicyLoosenPayload struct {
	TenantID                uuid.UUID `json:"tenant_id"`
	JurisdictionID          uuid.UUID `json:"jurisdiction_id"`
	Lists                   []string  `json:"lists"`
	RescreenIntervalSeconds int64     `json:"rescreen_interval_seconds"`
	BackfillGraceSeconds    int64     `json:"backfill_grace_seconds"`
	EffectiveOffsetSeconds  int64     `json:"effective_offset_seconds"`
	LegalReviewReference    string    `json:"legal_review_reference"`
	EvidenceReference       string    `json:"evidence_reference"`
}

// ScreeningTriggerPolicyLoosenPayload is the payload of KindAMLScreeningTriggerPolicyLoosen. Every field is required (no default).
// GateAlertAfterSeconds is null iff GateWithdrawal is false. EffectiveOffsetSeconds is how long after EXECUTION the version takes effect.
type ScreeningTriggerPolicyLoosenPayload struct {
	TenantID               uuid.UUID `json:"tenant_id"`
	JurisdictionID         uuid.UUID `json:"jurisdiction_id"`
	Events                 []string  `json:"events"`
	GateWithdrawal         bool      `json:"gate_withdrawal"`
	GateAlertAfterSeconds  *int64    `json:"gate_alert_after_seconds"`
	EffectiveOffsetSeconds int64     `json:"effective_offset_seconds"`
	LegalReviewReference   string    `json:"legal_review_reference"`
	EvidenceReference      string    `json:"evidence_reference"`
}

// NormalizeScreeningTriggerPolicyLoosen validates and canonicalises (events sorted, distinct, known; possibly none) a trigger loosen payload.
func NormalizeScreeningTriggerPolicyLoosen(p ScreeningTriggerPolicyLoosenPayload) (ScreeningTriggerPolicyLoosenPayload, error) {
	if p.TenantID == uuid.Nil || p.JurisdictionID == uuid.Nil {
		return p, fmt.Errorf("%w: tenant_id and jurisdiction_id are required", ErrInvalidInput)
	}
	if p.Events == nil || len(p.Events) > 3 {
		return p, fmt.Errorf("%w: events is a list of at most three trigger events (empty means nothing triggers)", ErrInvalidInput)
	}
	seen := map[string]bool{}
	for _, e := range p.Events {
		if (e != "registration" && e != "kyc_approval" && e != "scheduled") || seen[e] {
			return p, fmt.Errorf("%w: events holds distinct values of registration, kyc_approval, scheduled", ErrInvalidInput)
		}
		seen[e] = true
	}
	events := append([]string{}, p.Events...) // never nil: an empty set is the JSON array [], not null
	sort.Strings(events)
	p.Events = events
	if p.GateWithdrawal {
		if !seen["scheduled"] {
			return p, fmt.Errorf("%w: a gating version needs the scheduled event", ErrInvalidInput)
		}
		if p.GateAlertAfterSeconds == nil || *p.GateAlertAfterSeconds < 60 || *p.GateAlertAfterSeconds > 2592000 {
			return p, fmt.Errorf("%w: gate_alert_after_seconds is 60..2592000 for a gating version", ErrInvalidInput)
		}
	} else if p.GateAlertAfterSeconds != nil {
		return p, fmt.Errorf("%w: gate_alert_after_seconds must be null without a gate", ErrInvalidInput)
	}
	if p.EffectiveOffsetSeconds < 0 || p.EffectiveOffsetSeconds > 2592000 {
		return p, fmt.Errorf("%w: effective_offset_seconds is 0..2592000", ErrInvalidInput)
	}
	if err := lintLooseningRef("legal_review_reference", p.LegalReviewReference); err != nil {
		return p, err
	}
	if !evidenceRefRE.MatchString(p.EvidenceReference) {
		return p, fmt.Errorf("%w: evidence_reference must be one opaque token of 1..200 characters [A-Za-z0-9._:/#-]", ErrInvalidInput)
	}
	return p, nil
}

// ScreeningLooseningConfigurePayload is the payload of KindAMLScreeningLooseningConfigure. CoolingSeconds is required when Enabled
// and must be null when not.
type ScreeningLooseningConfigurePayload struct {
	TenantID          uuid.UUID `json:"tenant_id"`
	Enabled           bool      `json:"enabled"`
	CoolingSeconds    *int64    `json:"cooling_seconds"`
	EvidenceReference string    `json:"evidence_reference"`
}

// NormalizeScreeningPolicyLoosen validates and canonicalises (lists sorted, distinct, known) a loosen payload.
func NormalizeScreeningPolicyLoosen(p ScreeningPolicyLoosenPayload) (ScreeningPolicyLoosenPayload, error) {
	if p.TenantID == uuid.Nil || p.JurisdictionID == uuid.Nil {
		return p, fmt.Errorf("%w: tenant_id and jurisdiction_id are required", ErrInvalidInput)
	}
	if len(p.Lists) < 1 || len(p.Lists) > 2 {
		return p, fmt.Errorf("%w: lists names one or two lists", ErrInvalidInput)
	}
	seen := map[string]bool{}
	for _, l := range p.Lists {
		if (l != "sanctions" && l != "pep") || seen[l] {
			return p, fmt.Errorf("%w: lists holds distinct values of sanctions, pep", ErrInvalidInput)
		}
		seen[l] = true
	}
	lists := append([]string(nil), p.Lists...)
	sort.Strings(lists)
	p.Lists = lists
	if p.RescreenIntervalSeconds < 1 || p.RescreenIntervalSeconds > 31536000 || p.BackfillGraceSeconds < 0 || p.BackfillGraceSeconds > 31536000 {
		return p, fmt.Errorf("%w: the re-screen interval and the backfill grace are whole seconds, 1..31536000 and 0..31536000", ErrInvalidInput)
	}
	if p.EffectiveOffsetSeconds < 0 || p.EffectiveOffsetSeconds > 2592000 {
		return p, fmt.Errorf("%w: effective_offset_seconds is 0..2592000", ErrInvalidInput)
	}
	if err := lintLooseningRef("legal_review_reference", p.LegalReviewReference); err != nil {
		return p, err
	}
	if !evidenceRefRE.MatchString(p.EvidenceReference) {
		return p, fmt.Errorf("%w: evidence_reference must be one opaque token of 1..200 characters [A-Za-z0-9._:/#-]", ErrInvalidInput)
	}
	return p, nil
}

func lintLooseningRef(field, s string) error {
	if s == "" || len(s) > 200 || s != strings.TrimSpace(s) {
		return fmt.Errorf("%w: %s is required (1..200 characters, trimmed)", ErrInvalidInput, field)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: %s holds a control character", ErrInvalidInput, field)
		}
	}
	return nil
}

// NormalizeScreeningLooseningConfigure validates a configure payload.
func NormalizeScreeningLooseningConfigure(p ScreeningLooseningConfigurePayload) (ScreeningLooseningConfigurePayload, error) {
	if p.TenantID == uuid.Nil {
		return p, fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	if p.Enabled {
		if p.CoolingSeconds == nil || *p.CoolingSeconds < 0 || *p.CoolingSeconds > MaxLooseningCoolingSeconds {
			return p, fmt.Errorf("%w: enabling needs cooling_seconds, 0..%d", ErrInvalidInput, MaxLooseningCoolingSeconds)
		}
	} else if p.CoolingSeconds != nil {
		return p, fmt.Errorf("%w: cooling_seconds must be null when loosening is disabled", ErrInvalidInput)
	}
	if !evidenceRefRE.MatchString(p.EvidenceReference) {
		return p, fmt.Errorf("%w: evidence_reference must be one opaque token of 1..200 characters [A-Za-z0-9._:/#-]", ErrInvalidInput)
	}
	return p, nil
}

// strictDecodeRequired decodes raw into dst (unknown fields refused) and requires every key of want to be present (a JSON null is a
// stated value for a nullable field; an absent key is not).
func strictDecodeRequired(raw json.RawMessage, dst any, want []string) error {
	if err := strictDecode(raw, dst); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return fmt.Errorf("%w: payload: %v", ErrInvalidInput, err)
	}
	for _, k := range want {
		if _, ok := keys[k]; !ok {
			return fmt.Errorf("%w: payload key %q is required", ErrInvalidInput, k)
		}
	}
	return nil
}

func init() {
	register(KindAMLScreeningPolicyLoosen, kindSpec{
		platformOnly: true,
		tenantPerm:   func(any) auth.Permission { return "" },
		decode: func(raw json.RawMessage) (any, uuid.UUID, error) {
			var p ScreeningPolicyLoosenPayload
			if err := strictDecodeRequired(raw, &p, []string{"tenant_id", "jurisdiction_id", "lists", "rescreen_interval_seconds",
				"backfill_grace_seconds", "effective_offset_seconds", "legal_review_reference", "evidence_reference"}); err != nil {
				return nil, uuid.Nil, err
			}
			p, err := NormalizeScreeningPolicyLoosen(p)
			return p, uuid.Nil, err
		},
		execute: func(ctx context.Context, tx pgx.Tx, ex execContext, payload any) (map[string]any, error) {
			p := payload.(ScreeningPolicyLoosenPayload)
			var raw []byte
			if err := tx.QueryRow(ctx, `SELECT screening_policy_loosen_apply($1)`, ex.req.ID).Scan(&raw); err != nil {
				return nil, err
			}
			out := map[string]any{}
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, fmt.Errorf("licensingchange: decode the loosening outcome: %w", err)
			}
			if err := auditLoosening(ctx, tx, ex, "aml.screening_policy_loosened", out, p.TenantID); err != nil {
				return nil, err
			}
			return out, nil
		},
	})
	register(KindAMLScreeningTriggerPolicyLoosen, kindSpec{
		platformOnly: true,
		tenantPerm:   func(any) auth.Permission { return "" },
		decode: func(raw json.RawMessage) (any, uuid.UUID, error) {
			var p ScreeningTriggerPolicyLoosenPayload
			if err := strictDecodeRequired(raw, &p, []string{"tenant_id", "jurisdiction_id", "events", "gate_withdrawal", "gate_alert_after_seconds",
				"effective_offset_seconds", "legal_review_reference", "evidence_reference"}); err != nil {
				return nil, uuid.Nil, err
			}
			p, err := NormalizeScreeningTriggerPolicyLoosen(p)
			return p, uuid.Nil, err
		},
		execute: func(ctx context.Context, tx pgx.Tx, ex execContext, payload any) (map[string]any, error) {
			p := payload.(ScreeningTriggerPolicyLoosenPayload)
			var raw []byte
			if err := tx.QueryRow(ctx, `SELECT screening_trigger_loosen_apply($1)`, ex.req.ID).Scan(&raw); err != nil {
				return nil, err
			}
			out := map[string]any{}
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, fmt.Errorf("licensingchange: decode the trigger loosening outcome: %w", err)
			}
			if err := auditLoosening(ctx, tx, ex, "aml.screening_trigger_policy_loosened", out, p.TenantID); err != nil {
				return nil, err
			}
			return out, nil
		},
	})
	register(KindAMLScreeningLooseningConfigure, kindSpec{
		platformOnly: true,
		tenantPerm:   func(any) auth.Permission { return "" },
		decode: func(raw json.RawMessage) (any, uuid.UUID, error) {
			var p ScreeningLooseningConfigurePayload
			if err := strictDecodeRequired(raw, &p, []string{"tenant_id", "enabled", "cooling_seconds", "evidence_reference"}); err != nil {
				return nil, uuid.Nil, err
			}
			p, err := NormalizeScreeningLooseningConfigure(p)
			return p, uuid.Nil, err
		},
		execute: func(ctx context.Context, tx pgx.Tx, ex execContext, payload any) (map[string]any, error) {
			p := payload.(ScreeningLooseningConfigurePayload)
			var raw []byte
			if err := tx.QueryRow(ctx, `SELECT screening_loosening_settings_apply($1)`, ex.req.ID).Scan(&raw); err != nil {
				return nil, err
			}
			out := map[string]any{}
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, fmt.Errorf("licensingchange: decode the loosening configuration outcome: %w", err)
			}
			if err := auditLoosening(ctx, tx, ex, "aml.screening_loosening_configured", out, p.TenantID); err != nil {
				return nil, err
			}
			return out, nil
		},
	})
}

// auditLoosening writes the before / after audit row of the governed write in the SAME transaction (platform-scoped; the tenant is
// named in the metadata only: these rows are not projected to the tenant).
func auditLoosening(ctx context.Context, tx pgx.Tx, ex execContext, action string, outcome map[string]any, tenantID uuid.UUID) error {
	md := map[string]any{
		"change_request_id": ex.req.ID.String(), "tenant_id": tenantID.String(), "reason_code": ex.req.ReasonCode,
		"payload_hash": ex.req.PayloadHash, "outcome": outcome,
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		ActorType: audit.ActorStaff, ActorID: ex.actor.StaffID, Action: action, TargetType: "licensing_change_request", TargetID: ex.req.ID.String(),
		Outcome: audit.OutcomeSuccess, IPAddress: ex.actor.IPAddress, UserAgent: ex.actor.UserAgent, RequestID: ex.actor.RequestID, Metadata: md,
	}); err != nil {
		return fmt.Errorf("licensingchange: audit %s: %w", action, err)
	}
	return nil
}

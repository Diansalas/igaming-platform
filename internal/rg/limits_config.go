package rg

// ADR 0115 SLICE S2: the RG configuration write APIs - OPERATOR bounds (tenant / brand rows, written
// by the tenant's own compliance staff) and the jurisdiction FLOOR change-request flow (platform
// staff, four-eyes). No legal value is supplied here (OQ-2): every number arrives from the caller and
// is validated for SHAPE only; compliance supplies the content.
//
// Where the rules live:
//   - Shape: every field of the control kind explicit (a value or an attested `none`), every field
//     that does not apply absent - validated here for a clear 4xx and by the migration-0135 CHECKs
//     as the backstop (a partial configuration cannot exist).
//   - Tighten-only / widen-only (operator rows), no-backdating, the synthetic guard, floor four-eyes
//     (distinct staff AND Person), the payload hash and the signed actor proofs are the DATABASE's
//     (triggers RL003..RL008); this file maps their refusals and audits the denial. The ADR's "Go
//     layer" tighten-only check is this audited mapping of the same SQL function's refusal: Go does
//     not re-implement the comparison.
//   - jurisdiction_code of an operator row is derived from the tenant licence, never taken from the
//     request; tenant_id is the session's.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/audit"
)

// Attested is a field that is either a value or the attested literal `none` ("no bound"). On the
// wire: {"none": true} or {"value": X}. Absent (nil pointer) is rejected: no partial configuration.
type Attested[T any] struct {
	None  bool
	Value *T
}

// UnmarshalJSON accepts exactly {"none":true} or {"value":X}.
func (a *Attested[T]) UnmarshalJSON(b []byte) error {
	var raw struct {
		None  *bool `json:"none"`
		Value *T    `json:"value"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	switch {
	case raw.None != nil && *raw.None && raw.Value == nil:
		a.None, a.Value = true, nil
	case raw.None == nil && raw.Value != nil:
		a.None, a.Value = false, raw.Value
	default:
		return errors.New(`an attested field is exactly {"none":true} or {"value":...}`)
	}
	return nil
}

// MarshalJSON mirrors UnmarshalJSON.
func (a Attested[T]) MarshalJSON() ([]byte, error) {
	if a.None || a.Value == nil {
		return []byte(`{"none":true}`), nil
	}
	return json.Marshal(struct {
		Value T `json:"value"`
	}{*a.Value})
}

// ConfigContent is the caller-supplied content of one rg_limit_configs row (the API shape). Intervals
// are whole seconds. Which fields are required / forbidden depends on ControlKind.
type ConfigContent struct {
	ControlKind   ControlKind `json:"control_kind"`
	AssetCode     *string     `json:"asset_code,omitempty"`
	EffectiveFrom *time.Time  `json:"effective_from,omitempty"`

	Enabled   *bool `json:"enabled,omitempty"`
	Mandatory *bool `json:"mandatory,omitempty"`

	Ceiling                        *Attested[string] `json:"ceiling,omitempty"`
	IncreaseCooldownSeconds        *int64            `json:"increase_cooldown_seconds,omitempty"`
	IncreaseRequiresReconfirmation *bool             `json:"increase_requires_reconfirmation,omitempty"`
	Scope                          *string           `json:"scope,omitempty"`

	AllowedPeriods []string `json:"allowed_periods,omitempty"`
	CalendarTZ     *string  `json:"calendar_tz,omitempty"`
	WeekStart      *int     `json:"week_start,omitempty"`

	CountBonusFunded *bool `json:"count_bonus_funded,omitempty"`

	LossComputation   *string         `json:"loss_computation,omitempty"`
	LossMayGoNegative *Attested[bool] `json:"loss_may_go_negative,omitempty"`

	PostSessionBreakSeconds    *Attested[int64] `json:"post_session_break,omitempty"`
	ConcurrentChainAggregation *Attested[bool]  `json:"concurrent_chain_aggregation,omitempty"`

	PendingIntentTTLSeconds *Attested[int64] `json:"pending_intent_ttl,omitempty"`

	AllowedDurationsSeconds []int64 `json:"allowed_durations_seconds,omitempty"`
	AllowedDurationsAny     *bool   `json:"allowed_durations_any,omitempty"`

	AllowedIntervalsSeconds []int64 `json:"allowed_intervals_seconds,omitempty"`

	ReasonCode     string `json:"reason_code"`
	AttestationRef string `json:"attestation_ref"`
}

// configContentColumns are the caller-supplied columns of rg_limit_configs (id, change_request_id and
// created_* are forced / set by the database).
var configContentColumns = []string{
	"jurisdiction_code", "tenant_id", "brand_id", "control_kind", "asset_code", "effective_from",
	"enabled", "mandatory", "ceiling", "ceiling_none", "allowed_periods", "calendar_tz", "week_start",
	"increase_cooldown", "increase_requires_reconfirmation", "scope", "count_bonus_funded",
	"loss_computation", "loss_may_go_negative", "loss_may_go_negative_none", "post_session_break",
	"post_session_break_none", "concurrent_chain_aggregation", "concurrent_chain_aggregation_none",
	"pending_intent_ttl", "pending_intent_ttl_none", "allowed_durations", "allowed_durations_any",
	"allowed_intervals", "reason_code", "attestation_ref",
}

const maxIntervalSeconds = 10 * 365 * 24 * 3600 // engineering sanity bound, not a legal value

var tzRe = regexp.MustCompile(`^[A-Za-z0-9_+./-]{1,64}$`)

func secondsInterval(n int64) string { return fmt.Sprintf("%d seconds", n) }

// fieldApplicability is the set of optional-content JSON keys that apply to each control kind.
func applicableFields(k ControlKind) map[string]bool {
	m := map[string]bool{"enabled": true, "mandatory": true}
	switch k {
	case ControlDeposit, ControlLoss, ControlWager, ControlSessionDuration:
		for _, f := range []string{"ceiling", "increase_cooldown_seconds", "increase_requires_reconfirmation", "scope"} {
			m[f] = true
		}
	}
	if k.IsAmountKind() {
		for _, f := range []string{"allowed_periods", "calendar_tz", "week_start", "asset_code"} {
			m[f] = true
		}
	}
	switch k {
	case ControlLoss:
		m["count_bonus_funded"], m["loss_computation"], m["loss_may_go_negative"] = true, true, true
	case ControlWager:
		m["count_bonus_funded"] = true
	case ControlSessionDuration:
		m["post_session_break"], m["concurrent_chain_aggregation"] = true, true
	case ControlDeposit:
		m["pending_intent_ttl"] = true
	case ControlTimeOut:
		m["allowed_durations_seconds"], m["allowed_durations_any"], m["scope"] = true, true, true
	case ControlRealityCheckInterval:
		m["allowed_intervals_seconds"], m["scope"] = true, true
	}
	return m
}

func (c ConfigContent) presentFields() map[string]bool {
	p := map[string]bool{}
	set := func(name string, present bool) {
		if present {
			p[name] = true
		}
	}
	set("asset_code", c.AssetCode != nil)
	set("enabled", c.Enabled != nil)
	set("mandatory", c.Mandatory != nil)
	set("ceiling", c.Ceiling != nil)
	set("increase_cooldown_seconds", c.IncreaseCooldownSeconds != nil)
	set("increase_requires_reconfirmation", c.IncreaseRequiresReconfirmation != nil)
	set("scope", c.Scope != nil)
	set("allowed_periods", len(c.AllowedPeriods) > 0)
	set("calendar_tz", c.CalendarTZ != nil)
	set("week_start", c.WeekStart != nil)
	set("count_bonus_funded", c.CountBonusFunded != nil)
	set("loss_computation", c.LossComputation != nil)
	set("loss_may_go_negative", c.LossMayGoNegative != nil)
	set("post_session_break", c.PostSessionBreakSeconds != nil)
	set("concurrent_chain_aggregation", c.ConcurrentChainAggregation != nil)
	set("pending_intent_ttl", c.PendingIntentTTLSeconds != nil)
	set("allowed_durations_seconds", len(c.AllowedDurationsSeconds) > 0)
	set("allowed_durations_any", c.AllowedDurationsAny != nil)
	set("allowed_intervals_seconds", len(c.AllowedIntervalsSeconds) > 0)
	return p
}

func requiredFields(k ControlKind, c ConfigContent) []string {
	req := []string{"enabled", "mandatory"}
	switch k {
	case ControlDeposit, ControlLoss, ControlWager, ControlSessionDuration:
		req = append(req, "ceiling", "increase_cooldown_seconds", "increase_requires_reconfirmation", "scope")
	}
	if k.IsAmountKind() {
		req = append(req, "allowed_periods", "calendar_tz", "week_start")
	}
	switch k {
	case ControlLoss:
		req = append(req, "count_bonus_funded", "loss_computation", "loss_may_go_negative")
	case ControlWager:
		req = append(req, "count_bonus_funded")
	case ControlSessionDuration:
		req = append(req, "post_session_break", "concurrent_chain_aggregation")
	case ControlDeposit:
		req = append(req, "pending_intent_ttl")
	case ControlTimeOut:
		req = append(req, "allowed_durations_any", "scope")
		if c.AllowedDurationsAny != nil && !*c.AllowedDurationsAny {
			req = append(req, "allowed_durations_seconds")
		}
	case ControlRealityCheckInterval:
		req = append(req, "allowed_intervals_seconds", "scope")
	}
	return req
}

func validSeconds(n int64) bool { return n > 0 && n <= maxIntervalSeconds }

func distinctPositive(v []int64) bool {
	seen := map[int64]bool{}
	for _, n := range v {
		if !validSeconds(n) || seen[n] {
			return false
		}
		seen[n] = true
	}
	return len(v) > 0
}

// Validate checks the SHAPE of the content (never a legal value).
func (c ConfigContent) Validate() error {
	known := false
	for _, k := range ConfigControlKinds {
		if c.ControlKind == k {
			known = true
		}
	}
	if !known {
		return invalidf("control_kind is not a known control kind")
	}
	app, pres := applicableFields(c.ControlKind), c.presentFields()
	names := make([]string, 0, len(pres))
	for n := range pres {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !app[n] {
			return invalidf("%s does not apply to control_kind %s", n, c.ControlKind)
		}
	}
	for _, n := range requiredFields(c.ControlKind, c) {
		if !pres[n] {
			return invalidf("%s is required for control_kind %s (a value or an attested none; no partial configuration)", n, c.ControlKind)
		}
	}
	if c.ReasonCode == "" || len(c.ReasonCode) > 64 || !printableASCII(c.ReasonCode) {
		return invalidf("reason_code (1-64 printable ASCII) is required")
	}
	if c.AttestationRef == "" || len(c.AttestationRef) > 200 || !printableASCII(c.AttestationRef) {
		return invalidf("attestation_ref (1-200 printable ASCII) is required")
	}
	if c.AssetCode != nil && !assetRe.MatchString(*c.AssetCode) {
		return invalidf("asset_code is malformed")
	}
	if c.ControlKind.IsAmountKind() {
		if c.AssetCode == nil && (c.Ceiling == nil || !c.Ceiling.None) {
			return invalidf("an all-assets row (no asset_code) must attest ceiling none: minor units are per asset")
		}
		seen := map[string]bool{}
		for _, p := range c.AllowedPeriods {
			okP := false
			for _, k := range PeriodKinds {
				if string(k) == p {
					okP = true
				}
			}
			if !okP || seen[p] {
				return invalidf("allowed_periods must be distinct known periods")
			}
			seen[p] = true
		}
		if c.CalendarTZ == nil || !tzRe.MatchString(*c.CalendarTZ) {
			return invalidf("calendar_tz is malformed")
		}
		if c.WeekStart == nil || *c.WeekStart < 1 || *c.WeekStart > 7 {
			return invalidf("week_start must be 1..7")
		}
	}
	if c.Ceiling != nil && !c.Ceiling.None && (c.Ceiling.Value == nil || !valueRe.MatchString(*c.Ceiling.Value)) {
		return invalidf("ceiling value must be a positive integer of at most 38 digits")
	}
	if c.IncreaseCooldownSeconds != nil && !validSeconds(*c.IncreaseCooldownSeconds) {
		return invalidf("increase_cooldown_seconds must be a positive number of seconds (zero is rejected, there is no none)")
	}
	if c.Scope != nil {
		okScope := false
		switch c.ControlKind {
		case ControlTimeOut:
			okScope = *c.Scope == "account" || *c.Scope == "tenant_person" || *c.Scope == "platform_person"
		case ControlRealityCheckInterval:
			okScope = *c.Scope == "account"
		default:
			okScope = *c.Scope == "account" || *c.Scope == "tenant_person"
		}
		if !okScope {
			return invalidf("scope is not valid for control_kind %s", c.ControlKind)
		}
	}
	if c.LossComputation != nil && *c.LossComputation != "net_of_wins" {
		return invalidf("loss_computation supports only net_of_wins")
	}
	for name, a := range map[string]*Attested[int64]{"post_session_break": c.PostSessionBreakSeconds, "pending_intent_ttl": c.PendingIntentTTLSeconds} {
		if a != nil && !a.None && (a.Value == nil || !validSeconds(*a.Value)) {
			return invalidf("%s value must be a positive number of seconds", name)
		}
	}
	if c.ControlKind == ControlTimeOut && c.AllowedDurationsAny != nil {
		if *c.AllowedDurationsAny && len(c.AllowedDurationsSeconds) > 0 {
			return invalidf("allowed_durations_seconds must be absent when allowed_durations_any is true")
		}
		if !*c.AllowedDurationsAny && !distinctPositive(c.AllowedDurationsSeconds) {
			return invalidf("allowed_durations_seconds must be distinct positive seconds")
		}
	}
	if c.ControlKind == ControlRealityCheckInterval && !distinctPositive(c.AllowedIntervalsSeconds) {
		return invalidf("allowed_intervals_seconds must be distinct positive seconds")
	}
	return nil
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func intervals(v []int64) []string {
	if len(v) == 0 {
		return nil
	}
	out := make([]string, len(v))
	for i, n := range v {
		out[i] = secondsInterval(n)
	}
	return out
}

func attestedInterval(a *Attested[int64]) (val, none any) {
	if a == nil {
		return nil, nil
	}
	if a.None {
		return nil, true
	}
	return secondsInterval(*a.Value), false
}

// Row renders the content as the column-keyed JSON object jsonb_populate_record(NULL::rg_limit_configs)
// reads. jurisdiction / tenant / brand are server-derived by the caller. Call Validate first.
func (c ConfigContent) Row(jurisdiction string, tenantID, brandID *uuid.UUID) map[string]any {
	r := map[string]any{}
	for _, col := range configContentColumns {
		r[col] = nil
	}
	r["jurisdiction_code"], r["control_kind"], r["asset_code"] = jurisdiction, string(c.ControlKind), c.AssetCode
	if tenantID != nil {
		r["tenant_id"] = tenantID.String()
	}
	if brandID != nil {
		r["brand_id"] = brandID.String()
	}
	if c.EffectiveFrom != nil {
		r["effective_from"] = c.EffectiveFrom.UTC().Format(time.RFC3339Nano)
	}
	r["enabled"], r["mandatory"] = c.Enabled, c.Mandatory
	if c.Ceiling != nil {
		if c.Ceiling.None {
			r["ceiling_none"] = true
		} else {
			r["ceiling"], r["ceiling_none"] = *c.Ceiling.Value, false
		}
	}
	if c.IncreaseCooldownSeconds != nil {
		r["increase_cooldown"] = secondsInterval(*c.IncreaseCooldownSeconds)
	}
	r["increase_requires_reconfirmation"], r["scope"] = c.IncreaseRequiresReconfirmation, c.Scope
	if len(c.AllowedPeriods) > 0 {
		r["allowed_periods"] = c.AllowedPeriods
	}
	r["calendar_tz"], r["week_start"], r["count_bonus_funded"], r["loss_computation"] = c.CalendarTZ, c.WeekStart, c.CountBonusFunded, c.LossComputation
	if a := c.LossMayGoNegative; a != nil {
		if a.None {
			r["loss_may_go_negative_none"] = true
		} else {
			r["loss_may_go_negative"], r["loss_may_go_negative_none"] = *a.Value, false
		}
	}
	r["post_session_break"], r["post_session_break_none"] = attestedInterval(c.PostSessionBreakSeconds)
	if a := c.ConcurrentChainAggregation; a != nil {
		if a.None {
			r["concurrent_chain_aggregation_none"] = true
		} else {
			r["concurrent_chain_aggregation"], r["concurrent_chain_aggregation_none"] = *a.Value, false
		}
	}
	r["pending_intent_ttl"], r["pending_intent_ttl_none"] = attestedInterval(c.PendingIntentTTLSeconds)
	if len(c.AllowedDurationsSeconds) > 0 {
		r["allowed_durations"] = intervals(c.AllowedDurationsSeconds)
	}
	r["allowed_durations_any"] = c.AllowedDurationsAny
	if len(c.AllowedIntervalsSeconds) > 0 {
		r["allowed_intervals"] = intervals(c.AllowedIntervalsSeconds)
	}
	r["reason_code"], r["attestation_ref"] = c.ReasonCode, c.AttestationRef
	return r
}

// ConfigRow is a stored rg_limit_configs row in API shape.
type ConfigRow struct {
	ID                uuid.UUID
	JurisdictionCode  string
	TenantID, BrandID *uuid.UUID
	Content           ConfigContent
	EffectiveFrom     time.Time
	CreatedByActorID  uuid.UUID
	CreatedAt         time.Time
	ChangeRequestID   *uuid.UUID
}

const configSelect = `SELECT id, jurisdiction_code, tenant_id, brand_id, control_kind, asset_code, effective_from, enabled, mandatory,
	ceiling::text, ceiling_none, allowed_periods, calendar_tz, week_start::int, extract(epoch FROM increase_cooldown)::bigint,
	increase_requires_reconfirmation, scope, count_bonus_funded, loss_computation, loss_may_go_negative, loss_may_go_negative_none,
	extract(epoch FROM post_session_break)::bigint, post_session_break_none, concurrent_chain_aggregation, concurrent_chain_aggregation_none,
	extract(epoch FROM pending_intent_ttl)::bigint, pending_intent_ttl_none,
	(SELECT array_agg(extract(epoch FROM x)::bigint ORDER BY x) FROM unnest(allowed_durations) AS x), allowed_durations_any,
	(SELECT array_agg(extract(epoch FROM x)::bigint ORDER BY x) FROM unnest(allowed_intervals) AS x),
	reason_code, attestation_ref, created_by_actor_id, created_at, change_request_id
	FROM rg_limit_configs`

func scanConfigRow(row pgx.Row) (ConfigRow, error) {
	var r ConfigRow
	var kind string
	var ceiling *string
	var ceilingNone, lossNeg, lossNegNone, concur, concurNone *bool
	var cooldown, psb, ttl *int64
	var psbNone, ttlNone *bool
	err := row.Scan(&r.ID, &r.JurisdictionCode, &r.TenantID, &r.BrandID, &kind, &r.Content.AssetCode, &r.EffectiveFrom,
		&r.Content.Enabled, &r.Content.Mandatory, &ceiling, &ceilingNone, &r.Content.AllowedPeriods, &r.Content.CalendarTZ,
		&r.Content.WeekStart, &cooldown, &r.Content.IncreaseRequiresReconfirmation, &r.Content.Scope, &r.Content.CountBonusFunded,
		&r.Content.LossComputation, &lossNeg, &lossNegNone, &psb, &psbNone, &concur, &concurNone, &ttl, &ttlNone,
		&r.Content.AllowedDurationsSeconds, &r.Content.AllowedDurationsAny, &r.Content.AllowedIntervalsSeconds,
		&r.Content.ReasonCode, &r.Content.AttestationRef, &r.CreatedByActorID, &r.CreatedAt, &r.ChangeRequestID)
	if err != nil {
		return ConfigRow{}, err
	}
	r.Content.ControlKind = ControlKind(kind)
	r.Content.IncreaseCooldownSeconds = cooldown
	ef := r.EffectiveFrom
	r.Content.EffectiveFrom = &ef
	if ceilingNone != nil {
		if *ceilingNone {
			r.Content.Ceiling = &Attested[string]{None: true}
		} else {
			r.Content.Ceiling = &Attested[string]{Value: ceiling}
		}
	}
	r.Content.LossMayGoNegative = attestedBool(lossNeg, lossNegNone)
	r.Content.ConcurrentChainAggregation = attestedBool(concur, concurNone)
	r.Content.PostSessionBreakSeconds = attestedInt(psb, psbNone)
	r.Content.PendingIntentTTLSeconds = attestedInt(ttl, ttlNone)
	return r, nil
}

func attestedBool(v, none *bool) *Attested[bool] {
	if none == nil {
		return nil
	}
	if *none {
		return &Attested[bool]{None: true}
	}
	return &Attested[bool]{Value: v}
}

func attestedInt(v *int64, none *bool) *Attested[int64] {
	if none == nil {
		return nil
	}
	if *none {
		return &Attested[int64]{None: true}
	}
	return &Attested[int64]{Value: v}
}

func insertConfigRow(ctx context.Context, tx pgx.Tx, row map[string]any, changeRequestID *uuid.UUID) (uuid.UUID, error) {
	b, err := json.Marshal(row)
	if err != nil {
		return uuid.Nil, err
	}
	return insertConfigJSON(ctx, tx, b, changeRequestID)
}

// insertConfigJSON inserts a row from its column-keyed JSON bytes as they are (no decode / re-encode:
// a number is never routed through float64).
func insertConfigJSON(ctx context.Context, tx pgx.Tx, b []byte, changeRequestID *uuid.UUID) (uuid.UUID, error) {
	cols, sel := "", ""
	for i, c := range configContentColumns {
		if i > 0 {
			cols += ", "
			sel += ", "
		}
		cols += c
		if c == "effective_from" {
			sel += "COALESCE(r.effective_from, clock_timestamp())"
		} else {
			sel += "r." + c
		}
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `INSERT INTO rg_limit_configs (`+cols+`, change_request_id)
		SELECT `+sel+`, $2 FROM jsonb_populate_record(NULL::rg_limit_configs, $1::jsonb) AS r RETURNING id`, b, changeRequestID).Scan(&id)
	return id, err
}

// ---- operator rows ------------------------------------------------------------------------

// OperatorConfigInput is one operator-bound write. tx MUST come from db.Pool.WithPrincipalScope
// (tenant, staff). TenantID and StaffID are the authenticated session's; BrandID (optional) must be a
// brand of that tenant.
type OperatorConfigInput struct {
	TenantID, StaffID uuid.UUID
	BrandID           *uuid.UUID
	Content           ConfigContent
	Meta              RequestMeta
}

// WriteOperatorConfig appends a tenant (or brand) operator row. The database refuses a row that
// loosens a restrictive field or narrows an option set of the floor / tenant row in force (RL004), a
// row with no floor in force (RL005), a backdated row (RL003) and a synthetic row outside dev/test
// (RL008).
func (l *Limits) WriteOperatorConfig(ctx context.Context, tx pgx.Tx, in OperatorConfigInput) (ConfigRow, error) {
	if err := in.Content.Validate(); err != nil {
		return ConfigRow{}, err
	}
	jur, err := tenantJurisdiction(ctx, tx, in.TenantID)
	if err != nil {
		return ConfigRow{}, err
	}
	if jur == nil {
		return ConfigRow{}, refuse(RefusedNoFloor, "the tenant has no licence jurisdiction")
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, writeLockTimeout); err != nil {
		return ConfigRow{}, fmt.Errorf("rg: set lock_timeout: %w", err)
	}
	tenant := in.TenantID
	// The advisory scope lock of config writers (ADR 0115 3.3: writer-only, never co-held with L0.x / L1+).
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('rg_limit_configs'), hashtext($1::text))`,
		fmt.Sprintf("%s|%s|%s", *jur, tenant, in.Content.ControlKind)); err != nil {
		return ConfigRow{}, MapDBError(err)
	}
	before, err := l.exactScopeBefore(ctx, tx, *jur, &tenant, in.BrandID, in.Content.ControlKind, in.Content.AssetCode)
	if err != nil {
		return ConfigRow{}, err
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return ConfigRow{}, fmt.Errorf("rg: open savepoint: %w", err)
	}
	id, err := insertConfigRow(ctx, sp, in.Content.Row(*jur, &tenant, in.BrandID), nil)
	if err != nil {
		_ = sp.Rollback(ctx)
		return ConfigRow{}, MapDBError(err)
	}
	if err := sp.Commit(ctx); err != nil {
		return ConfigRow{}, fmt.Errorf("rg: release savepoint: %w", err)
	}
	stored, err := scanConfigRow(tx.QueryRow(ctx, configSelect+` WHERE id = $1`, id))
	if err != nil {
		return ConfigRow{}, fmt.Errorf("rg: read back operator row: %w", err)
	}
	if err := l.cfg.Audit(ctx, tx, audit.Entry{
		TenantID: in.TenantID, ActorType: audit.ActorStaff, ActorID: in.StaffID, Action: "rg.limit_config.operator_written",
		TargetType: "rg_limit_config", TargetID: id.String(), Outcome: audit.OutcomeSuccess,
		IPAddress: in.Meta.IPAddress, UserAgent: in.Meta.UserAgent, RequestID: in.Meta.RequestID,
		Metadata: map[string]any{
			"jurisdiction_code": *jur, "control_kind": string(stored.Content.ControlKind), "asset_code": stored.Content.AssetCode,
			"brand_id": in.BrandID, "before": before, "after": configAuditView(stored), "reason_code": stored.Content.ReasonCode,
			"attestation_ref": stored.Content.AttestationRef,
		},
	}); err != nil {
		return ConfigRow{}, fmt.Errorf("rg: audit operator config: %w", err)
	}
	return stored, nil
}

func configAuditView(r ConfigRow) map[string]any {
	b, _ := json.Marshal(r.Content)
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // never float64: a large value must round-trip into the audit record exactly
	_ = dec.Decode(&m)
	m["id"], m["effective_from"] = r.ID.String(), r.EffectiveFrom.UTC().Format(time.RFC3339Nano)
	return m
}

// exactScopeBefore returns the row in force for the exact scope key (audit "before"), or nil.
func (l *Limits) exactScopeBefore(ctx context.Context, tx pgx.Tx, jur string, tenantID, brandID *uuid.UUID, kind ControlKind, asset *string) (map[string]any, error) {
	r, err := scanConfigRow(tx.QueryRow(ctx, configSelect+` WHERE jurisdiction_code = $1 AND tenant_id IS NOT DISTINCT FROM $2
		AND brand_id IS NOT DISTINCT FROM $3 AND control_kind = $4 AND asset_code IS NOT DISTINCT FROM $5 AND effective_from <= clock_timestamp()
		ORDER BY effective_from DESC, created_at DESC LIMIT 1`, jur, tenantID, brandID, string(kind), asset))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rg: read row in force: %w", err)
	}
	return configAuditView(r), nil
}

// ListConfigs returns the configuration rows visible to the session (the floors of every
// jurisdiction and the session tenant's own rows - RLS), newest first.
func ListConfigs(ctx context.Context, tx pgx.Tx, kind *ControlKind, limit, offset int) ([]ConfigRow, int, error) {
	var k *string
	if kind != nil {
		s := string(*kind)
		k = &s
	}
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM rg_limit_configs WHERE ($1::text IS NULL OR control_kind = $1)`, k).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("rg: count configs: %w", err)
	}
	rows, err := tx.Query(ctx, configSelect+` WHERE ($1::text IS NULL OR control_kind = $1)
		ORDER BY effective_from DESC, created_at DESC, id LIMIT $2 OFFSET $3`, k, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("rg: list configs: %w", err)
	}
	defer rows.Close()
	var out []ConfigRow
	for rows.Next() {
		r, err := scanConfigRow(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("rg: scan config: %w", err)
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// ---- jurisdiction floor change requests (platform, four-eyes) -----------------------------------

// FloorRequest is a stored rg_limit_config_change_requests row.
type FloorRequest struct {
	ID                   uuid.UUID
	JurisdictionCode     string
	ControlKind          ControlKind
	AssetCode            *string
	ProposedConfig       json.RawMessage
	PayloadHash          string
	ReasonCode           string
	RequestedBy          uuid.UUID
	Status               string
	DecidedBy            *uuid.UUID
	DecidedAt            *time.Time
	CreatedAt, ExpiresAt time.Time
	AppliedConfigID      *uuid.UUID
}

const floorRequestSelect = `SELECT r.id, r.jurisdiction_code, r.control_kind, r.asset_code, r.proposed_config, r.payload_hash, r.reason_code,
	r.requested_by, r.status, r.decided_by, r.decided_at, r.created_at, r.expires_at,
	(SELECT c.id FROM rg_limit_configs c WHERE c.change_request_id = r.id) FROM rg_limit_config_change_requests r`

func scanFloorRequest(row pgx.Row) (FloorRequest, error) {
	var f FloorRequest
	var kind string
	err := row.Scan(&f.ID, &f.JurisdictionCode, &kind, &f.AssetCode, &f.ProposedConfig, &f.PayloadHash, &f.ReasonCode, &f.RequestedBy,
		&f.Status, &f.DecidedBy, &f.DecidedAt, &f.CreatedAt, &f.ExpiresAt, &f.AppliedConfigID)
	f.ControlKind = ControlKind(kind)
	return f, err
}

// FloorRequestInput proposes a jurisdiction floor row. tx MUST come from db.Pool.WithPlatformAdmin.
// EffectiveFrom is REQUIRED and must lie in the future (the row is hash-bound at request time and the
// database refuses a backdated row at apply time).
type FloorRequestInput struct {
	StaffID          uuid.UUID
	JurisdictionCode string
	Content          ConfigContent
	Meta             RequestMeta
}

func (l *Limits) attach(ctx context.Context, tx pgx.Tx, staff uuid.UUID, op, target, payload string) error {
	iss := l.issuer()
	if iss == nil {
		return refuse(RefusedUnavailable, "no actor-proof issuer")
	}
	if err := iss.Attach(ctx, tx, actorproof.Claims{Actor: staff, Scope: actorproof.ScopePlatform, Operation: op, Target: target, PayloadHash: payload}); err != nil {
		return fmt.Errorf("rg: sign actor proof: %w", err)
	}
	return nil
}

// RequestFloor files a floor change request (proposal) with a signed request proof.
func (l *Limits) RequestFloor(ctx context.Context, tx pgx.Tx, in FloorRequestInput) (FloorRequest, error) {
	if err := in.Content.Validate(); err != nil {
		return FloorRequest{}, err
	}
	if in.JurisdictionCode == "" || len(in.JurisdictionCode) > 64 || !printableASCII(in.JurisdictionCode) {
		return FloorRequest{}, invalidf("jurisdiction_code is required")
	}
	if in.Content.EffectiveFrom == nil {
		return FloorRequest{}, invalidf("effective_from is required for a floor proposal")
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return FloorRequest{}, fmt.Errorf("rg: database clock: %w", err)
	}
	if !in.Content.EffectiveFrom.After(now) || in.Content.EffectiveFrom.After(now.Add(72*time.Hour)) {
		return FloorRequest{}, invalidf("effective_from must be in the future and within the 72h request lifetime")
	}
	row := in.Content.Row(in.JurisdictionCode, nil, nil)
	b, err := json.Marshal(row)
	if err != nil {
		return FloorRequest{}, err
	}
	var hash string
	if err := tx.QueryRow(ctx, `SELECT rg_limit_config_payload_hash(jsonb_populate_record(NULL::rg_limit_configs, $1::jsonb))`, b).Scan(&hash); err != nil {
		return FloorRequest{}, MapDBError(err)
	}
	digest := actorproof.Digest(actorproof.S(in.JurisdictionCode), actorproof.S(string(in.Content.ControlKind)), actorproof.SP(derefStr(in.Content.AssetCode)),
		actorproof.S(hash), actorproof.S(in.Content.ReasonCode))
	if err := l.attach(ctx, tx, in.StaffID, actorproof.OpRGConfigRequest, actorproof.TargetNew, digest); err != nil {
		return FloorRequest{}, err
	}
	before, err := l.exactScopeBefore(ctx, tx, in.JurisdictionCode, nil, nil, in.Content.ControlKind, in.Content.AssetCode)
	if err != nil {
		return FloorRequest{}, err
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return FloorRequest{}, fmt.Errorf("rg: open savepoint: %w", err)
	}
	fr, err := scanFloorRequest(sp.QueryRow(ctx, `WITH ins AS (INSERT INTO rg_limit_config_change_requests (jurisdiction_code, control_kind, asset_code, proposed_config, reason_code)
		VALUES ($1, $2, $3, $4::jsonb, $5) RETURNING *)
		SELECT r.id, r.jurisdiction_code, r.control_kind, r.asset_code, r.proposed_config, r.payload_hash, r.reason_code,
		       r.requested_by, r.status, r.decided_by, r.decided_at, r.created_at, r.expires_at, NULL::uuid FROM ins r`,
		in.JurisdictionCode, string(in.Content.ControlKind), in.Content.AssetCode, b, in.Content.ReasonCode))
	if err != nil {
		_ = sp.Rollback(ctx)
		return FloorRequest{}, MapDBError(err)
	}
	if err := sp.Commit(ctx); err != nil {
		return FloorRequest{}, fmt.Errorf("rg: release savepoint: %w", err)
	}
	if err := l.cfg.Audit(ctx, tx, audit.Entry{
		ActorType: audit.ActorStaff, ActorID: in.StaffID, Action: "rg.limit_config.floor_requested", TargetType: "rg_limit_config_change_request",
		TargetID: fr.ID.String(), Outcome: audit.OutcomeSuccess, IPAddress: in.Meta.IPAddress, UserAgent: in.Meta.UserAgent, RequestID: in.Meta.RequestID,
		Metadata: map[string]any{
			"jurisdiction_code": in.JurisdictionCode, "control_kind": string(in.Content.ControlKind), "asset_code": in.Content.AssetCode,
			"payload_hash": fr.PayloadHash, "before": before, "after": json.RawMessage(fr.ProposedConfig),
			"reason_code": in.Content.ReasonCode, "attestation_ref": in.Content.AttestationRef,
		},
	}); err != nil {
		return FloorRequest{}, fmt.Errorf("rg: audit floor request: %w", err)
	}
	return fr, nil
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// FloorDecision is approve / reject / cancel.
type FloorDecision string

const (
	FloorApprove FloorDecision = "approved"
	FloorReject  FloorDecision = "rejected"
	FloorCancel  FloorDecision = "cancelled"
)

// DecideFloorInput decides a floor change request. tx MUST come from db.Pool.WithPlatformAdmin.
type DecideFloorInput struct {
	StaffID, RequestID uuid.UUID
	Decision           FloorDecision
	Meta               RequestMeta
}

// DecideFloor approves, rejects or cancels a pending request, each with a signed proof bound to the
// request id and payload hash. An approval APPLIES the approved payload in the same transaction (a
// second signed proof, rg_limit_config:apply): the approver therefore cannot approve one thing and
// apply another, and a request is single-use. The database refuses the requester's Person (RL007),
// an expired request (RL010), a backdated apply (RL003) and a missing / foreign proof (AP0xx); any
// refusal rolls the whole transaction back.
func (l *Limits) DecideFloor(ctx context.Context, tx pgx.Tx, in DecideFloorInput) (FloorRequest, error) {
	op := map[FloorDecision]string{FloorApprove: actorproof.OpRGConfigApprove, FloorReject: actorproof.OpRGConfigReject, FloorCancel: actorproof.OpRGConfigCancel}[in.Decision]
	if op == "" {
		return FloorRequest{}, invalidf("decision must be approve, reject or cancel")
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, writeLockTimeout); err != nil {
		return FloorRequest{}, fmt.Errorf("rg: set lock_timeout: %w", err)
	}
	cur, err := scanFloorRequest(tx.QueryRow(ctx, floorRequestSelect+` WHERE r.id = $1 FOR UPDATE OF r`, in.RequestID))
	if errors.Is(err, pgx.ErrNoRows) {
		return FloorRequest{}, refuse(RefusedNotFound, "")
	}
	if err != nil {
		return FloorRequest{}, MapDBError(fmt.Errorf("rg: load floor request: %w", err))
	}
	if cur.Status != "pending" {
		return FloorRequest{}, refuse(RefusedRequestNotPending, "")
	}
	if in.Decision == FloorCancel && cur.RequestedBy != in.StaffID {
		return FloorRequest{}, refuse(RefusedForbidden, "only the requester may cancel")
	}
	if err := l.attach(ctx, tx, in.StaffID, op, cur.ID.String(), cur.PayloadHash); err != nil {
		return FloorRequest{}, err
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return FloorRequest{}, fmt.Errorf("rg: open savepoint: %w", err)
	}
	if _, err := sp.Exec(ctx, `UPDATE rg_limit_config_change_requests SET status = $2 WHERE id = $1`, cur.ID, string(in.Decision)); err != nil {
		_ = sp.Rollback(ctx)
		return FloorRequest{}, MapDBError(err)
	}
	if err := sp.Commit(ctx); err != nil {
		return FloorRequest{}, fmt.Errorf("rg: release savepoint: %w", err)
	}
	action := map[FloorDecision]string{FloorApprove: "rg.limit_config.floor_approved", FloorReject: "rg.limit_config.floor_rejected", FloorCancel: "rg.limit_config.floor_cancelled"}[in.Decision]
	md := map[string]any{
		"jurisdiction_code": cur.JurisdictionCode, "control_kind": string(cur.ControlKind), "asset_code": cur.AssetCode,
		"payload_hash": cur.PayloadHash, "request_id": cur.ID.String(), "reason_code": cur.ReasonCode, "status_before": "pending", "status_after": string(in.Decision),
	}
	if in.Decision == FloorApprove {
		if err := l.attach(ctx, tx, in.StaffID, actorproof.OpRGConfigApply, cur.ID.String(), cur.PayloadHash); err != nil {
			return FloorRequest{}, err
		}
		before, err := l.exactScopeBefore(ctx, tx, cur.JurisdictionCode, nil, nil, cur.ControlKind, cur.AssetCode)
		if err != nil {
			return FloorRequest{}, err
		}
		sp2, err := tx.Begin(ctx)
		if err != nil {
			return FloorRequest{}, fmt.Errorf("rg: open savepoint: %w", err)
		}
		id, err := insertConfigJSON(ctx, sp2, cur.ProposedConfig, &cur.ID)
		if err != nil {
			_ = sp2.Rollback(ctx)
			return FloorRequest{}, MapDBError(err)
		}
		if err := sp2.Commit(ctx); err != nil {
			return FloorRequest{}, fmt.Errorf("rg: release savepoint: %w", err)
		}
		stored, err := scanConfigRow(tx.QueryRow(ctx, configSelect+` WHERE id = $1`, id))
		if err != nil {
			return FloorRequest{}, fmt.Errorf("rg: read back floor row: %w", err)
		}
		md["applied_config_id"], md["before"], md["after"] = id.String(), before, configAuditView(stored)
		md["attestation_ref"] = stored.Content.AttestationRef
	}
	if err := l.cfg.Audit(ctx, tx, audit.Entry{
		ActorType: audit.ActorStaff, ActorID: in.StaffID, Action: action, TargetType: "rg_limit_config_change_request", TargetID: cur.ID.String(),
		Outcome: audit.OutcomeSuccess, IPAddress: in.Meta.IPAddress, UserAgent: in.Meta.UserAgent, RequestID: in.Meta.RequestID, Metadata: md,
	}); err != nil {
		return FloorRequest{}, fmt.Errorf("rg: audit floor decision: %w", err)
	}
	out, err := scanFloorRequest(tx.QueryRow(ctx, floorRequestSelect+` WHERE r.id = $1`, cur.ID))
	if err != nil {
		return FloorRequest{}, fmt.Errorf("rg: read back floor request: %w", err)
	}
	return out, nil
}

// ListFloorRequests returns the platform queue (RLS: platform sessions only), newest first.
func ListFloorRequests(ctx context.Context, tx pgx.Tx, status string, limit, offset int) ([]FloorRequest, int, error) {
	var st *string
	if status != "" {
		st = &status
	}
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM rg_limit_config_change_requests WHERE ($1::text IS NULL OR status = $1)`, st).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("rg: count floor requests: %w", err)
	}
	rows, err := tx.Query(ctx, floorRequestSelect+` WHERE ($1::text IS NULL OR r.status = $1) ORDER BY r.created_at DESC, r.id LIMIT $2 OFFSET $3`, st, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("rg: list floor requests: %w", err)
	}
	defer rows.Close()
	var out []FloorRequest
	for rows.Next() {
		f, err := scanFloorRequest(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("rg: scan floor request: %w", err)
		}
		out = append(out, f)
	}
	return out, total, rows.Err()
}

// RecordDenial writes a denial audit entry (outcome denied). It is called in a SEPARATE transaction
// after the refused write rolled back, so a denial leaves a record without any state change.
func (l *Limits) RecordDenial(ctx context.Context, tx pgx.Tx, e audit.Entry) error {
	e.Outcome = audit.OutcomeDenied
	return l.cfg.Audit(ctx, tx, e)
}

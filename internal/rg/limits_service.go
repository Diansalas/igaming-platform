package rg

// ADR 0115 SLICE S2: the RG limit WRITE service (player set / decrease / increase-request / cancel /
// confirm, staff-on-player-request decreases). API only: nothing here enforces a limit on a deposit,
// a wager, a launch or a login (S4), and no usage is measured (S3).
//
// What is decided where:
//   - The CLASSIFICATION of a change (initial / decrease / increase / removal / cancel_pending /
//     confirm), its effective_at and its confirmation need are decided by the ONE SQL function the
//     migration-0135 BEFORE INSERT trigger calls. This file previews it through ClassifyLimitChange
//     (the same SQL function) and never re-implements it; the stored row is the trigger's.
//   - What the database does NOT do in S1 (ADR 0115 "S2 must", ledger-finance LF 7) is done here: the
//     value is refused above the composed ceiling, the period outside the composed allowed_periods,
//     the pending-change cap is enforced, and an Idempotency-Key reused with a different request is
//     a refusal (HTTP 409).
//   - tenant_id, person_id, brand_id, the jurisdiction and the limit scope are server-derived
//     (session, account row, tenant licence, composed configuration); the caller names none of them.
//
// Every write takes the person lock (L0.4, ADR 0082) FIRST and nothing else after it, with a SET LOCAL
// lock_timeout; a timeout is a retryable refusal, never a silent success. A mutation and its audit
// record commit or roll back together (the audit function is injectable so a test can prove the
// fail-closed path).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// DefaultPendingCap is the engineering anti-abuse bound on a player's open pending changes
// (ADR 0115 section 2.2, security condition 6). It is NOT a legal value.
const DefaultPendingCap = 5

// MaxTimeOutHours is an engineering sanity bound on a time-out request (10 years). It is NOT a legal
// value; the jurisdiction's allowed_durations (when configured) narrow it.
const MaxTimeOutHours = 87600

// writeLockTimeout bounds how long a write waits for the person lock (an operational setting).
const writeLockTimeout = "3s"

// AuditFunc records one audit entry in tx (default audit.Record).
type AuditFunc func(ctx context.Context, tx pgx.Tx, e audit.Entry) error

// LimitsConfig configures Limits. The zero value is production behaviour.
type LimitsConfig struct {
	Audit      AuditFunc          // nil = audit.Record
	Issuer     *actorproof.Issuer // nil = actorproof.Default() at call time
	PendingCap int                // <= 0 = DefaultPendingCap
	// TestHookBeforeInsert, when set, runs before each insert attempt of a limit change (attempt = 0..3).
	// It exists only so a test can move the realization instant between the preview and the insert and
	// exercise the re-classification loop; production leaves it nil.
	TestHookBeforeInsert func(attempt int)
}

// Limits is the RG limit write service.
type Limits struct{ cfg LimitsConfig }

// NewLimits builds the service.
func NewLimits(cfg LimitsConfig) *Limits {
	if cfg.Audit == nil {
		cfg.Audit = audit.Record
	}
	if cfg.PendingCap <= 0 {
		cfg.PendingCap = DefaultPendingCap
	}
	return &Limits{cfg: cfg}
}

func (l *Limits) issuer() *actorproof.Issuer {
	if l.cfg.Issuer != nil {
		return l.cfg.Issuer
	}
	return actorproof.Default()
}

// RequestMeta is the audit context of one request (server-observed values only).
type RequestMeta struct {
	IPAddress string
	UserAgent string
	RequestID string
}

// LimitChange is one stored player_rg_limit_changes row (seq is deliberately not exposed).
type LimitChange struct {
	ID                   uuid.UUID
	TenantID             uuid.UUID
	PlayerAccountID      uuid.UUID
	PersonID             uuid.UUID
	BrandID              uuid.UUID
	Scope                LimitScope
	Kind                 ControlKind
	AssetCode            *string
	Period               *PeriodKind
	Value                *string
	ChangeKind           ChangeKind
	RequiresConfirmation bool
	RequestedAt          time.Time
	EffectiveAt          time.Time
	SupersedesChangeID   *uuid.UUID
	Source               ChangeSource
	ReasonCode           string
	EvidenceRef          *string
	CreatedByActorType   string
	CreatedByActorID     uuid.UUID
	ConfigVersionIDs     []uuid.UUID
	RequestHash          string
}

const limitChangeColumns = `id, tenant_id, player_account_id, person_id, brand_id, scope, control_kind, asset_code, period_kind,
	value::text, change_kind, requires_confirmation, requested_at, effective_at, supersedes_change_id, source, reason_code,
	evidence_ref, created_by_actor_type, created_by_actor_id, config_version_ids, request_hash`

func scanLimitChange(row pgx.Row) (LimitChange, error) {
	var c LimitChange
	var scope, kind, change, source string
	var period *string
	err := row.Scan(&c.ID, &c.TenantID, &c.PlayerAccountID, &c.PersonID, &c.BrandID, &scope, &kind, &c.AssetCode, &period,
		&c.Value, &change, &c.RequiresConfirmation, &c.RequestedAt, &c.EffectiveAt, &c.SupersedesChangeID, &source, &c.ReasonCode,
		&c.EvidenceRef, &c.CreatedByActorType, &c.CreatedByActorID, &c.ConfigVersionIDs, &c.RequestHash)
	if err != nil {
		return LimitChange{}, err
	}
	c.Scope, c.Kind, c.ChangeKind, c.Source = LimitScope(scope), ControlKind(kind), ChangeKind(change), ChangeSource(source)
	if period != nil {
		p := PeriodKind(*period)
		c.Period = &p
	}
	return c, nil
}

// ChangeResult is the outcome of a limit write.
type ChangeResult struct {
	Change   LimitChange
	Replayed bool // the Idempotency-Key matched a stored request: nothing was written
}

// Pending reports whether the change is an increase/removal that has not yet taken effect (cooling
// off, or awaiting the player's own confirm). A pending change is HTTP 202.
func (c LimitChange) Pending() bool {
	return c.ChangeKind == ChangeIncrease || c.ChangeKind == ChangeRemoval
}

// ---- input ------------------------------------------------------------------------------

var (
	valueRe = regexp.MustCompile(`^[1-9][0-9]{0,37}$`)
	keyRe   = regexp.MustCompile(`^[\x21-\x7e]{1,120}$`)
	assetRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)
)

// LimitInput names one limit and its new value. Value nil removes the limit (a removal is an
// increase: it waits out the cooling-off period).
type LimitInput struct {
	Kind           ControlKind
	AssetCode      *string
	Period         *PeriodKind
	Value          *string // decimal minor units / seconds; nil = removal
	IdempotencyKey string
}

func (in LimitInput) validate() error {
	isPlayerKind := false
	for _, k := range PlayerLimitKinds {
		if in.Kind == k {
			isPlayerKind = true
		}
	}
	if !isPlayerKind {
		return invalidf("kind must be one of deposit, loss, wager, session_duration")
	}
	if in.Kind.IsAmountKind() {
		if in.AssetCode == nil || !assetRe.MatchString(*in.AssetCode) {
			return invalidf("asset_code is required for an amount limit")
		}
		if in.Period == nil {
			return invalidf("period is required for an amount limit")
		}
		okPeriod := false
		for _, p := range PeriodKinds {
			if *in.Period == p {
				okPeriod = true
			}
		}
		if !okPeriod {
			return invalidf("period is not a known period")
		}
	} else if in.AssetCode != nil || in.Period != nil {
		return invalidf("asset_code and period do not apply to a session_duration limit")
	}
	if in.Value != nil && !valueRe.MatchString(*in.Value) {
		return invalidf("value must be a positive integer of at most 38 digits (or null to remove)")
	}
	if !keyRe.MatchString(in.IdempotencyKey) {
		return invalidf("Idempotency-Key must be 1 to 120 printable ASCII characters")
	}
	return nil
}

// hash is the request fingerprint bound to an Idempotency-Key (never includes time or the key).
func (in LimitInput) hash(parts ...string) string {
	h := sha256.New()
	write := func(s string) { _, _ = fmt.Fprintf(h, "%d:%s|", len(s), s) }
	write("rg.limit.set.v1")
	write(string(in.Kind))
	write(strOrTilde(in.AssetCode))
	if in.Period != nil {
		write(string(*in.Period))
	} else {
		write("~")
	}
	write(strOrTilde(in.Value))
	for _, p := range parts {
		write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func strOrTilde(p *string) string {
	if p == nil {
		return "~"
	}
	return *p
}

func fingerprint(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = fmt.Fprintf(h, "%d:%s|", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ---- the shared write path --------------------------------------------------------------

type playerAcct struct {
	ID, TenantID, BrandID, PersonID uuid.UUID
	Status                          string
}

func loadAccount(ctx context.Context, tx pgx.Tx, tenantID, accountID uuid.UUID) (playerAcct, error) {
	a, err := identity.GetPlayerAccountByID(ctx, tx, accountID)
	if errors.Is(err, identity.ErrNotFound) {
		return playerAcct{}, refuse(RefusedNotFound, "")
	}
	if err != nil {
		return playerAcct{}, err
	}
	if a.TenantID != tenantID { // defence in depth: RLS already scopes the read
		return playerAcct{}, refuse(RefusedNotFound, "")
	}
	return playerAcct{ID: a.ID, TenantID: a.TenantID, BrandID: a.BrandID, PersonID: a.PersonID, Status: string(a.Status)}, nil
}

// A player session cannot read tenants / licences (their RLS keys on app.player_account_id being
// unset), so, exactly as the migration-0135 trigger does (the 0121 / 0130 technique), the player GUC is
// cleared for this one read and restored; the tenant id is the session's own.
func tenantJurisdiction(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (*string, error) {
	var player string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(current_setting('app.player_account_id', true), '')`).Scan(&player); err != nil {
		return nil, fmt.Errorf("rg: read player scope: %w", err)
	}
	if player != "" {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.player_account_id', '', true)`); err != nil {
			return nil, fmt.Errorf("rg: clear player scope: %w", err)
		}
	}
	var code *string
	err := tx.QueryRow(ctx, `SELECT j.code FROM tenants t JOIN licences l ON l.id = t.licence_id
		JOIN jurisdictions j ON j.id = l.jurisdiction_id WHERE t.id = $1`, tenantID).Scan(&code)
	if player != "" {
		if _, rerr := tx.Exec(ctx, `SELECT set_config('app.player_account_id', $1, true)`, player); rerr != nil {
			return nil, fmt.Errorf("rg: restore player scope: %w", rerr)
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rg: tenant jurisdiction: %w", err)
	}
	return code, nil
}

// ComposedConfig is the read-time composition of the configuration in force (strictest / UNION / OR),
// as the SQL function rg_limit_config_compose returns it.
type ComposedConfig struct {
	Found                bool
	VersionIDs           []uuid.UUID
	Enabled, Mandatory   bool
	Ceiling              *string
	CeilingNone          bool
	AllowedPeriods       []string
	Scope                string
	AllowedDurationsAny  bool
	AllowedDurationsSecs []int64
}

func (l *Limits) compose(ctx context.Context, tx pgx.Tx, jur *string, tenantID, brandID uuid.UUID, kind ControlKind, asset *string) (ComposedConfig, error) {
	if jur == nil {
		return ComposedConfig{}, nil
	}
	var c ComposedConfig
	var enabled, mandatory, none, anyDur *bool
	var scope *string
	err := tx.QueryRow(ctx, `SELECT found, version_ids, enabled, mandatory, ceiling::text, ceiling_none, allowed_periods, scope,
			allowed_durations_any,
			(SELECT array_agg(extract(epoch FROM x)::bigint ORDER BY x) FROM unnest(allowed_durations) AS x)
		FROM rg_limit_config_compose($1, $2, $3, $4, $5, clock_timestamp())`,
		*jur, tenantID, brandID, string(kind), asset,
	).Scan(&c.Found, &c.VersionIDs, &enabled, &mandatory, &c.Ceiling, &none, &c.AllowedPeriods, &scope, &anyDur, &c.AllowedDurationsSecs)
	if err != nil {
		return ComposedConfig{}, fmt.Errorf("rg: compose configuration: %w", err)
	}
	if !c.Found {
		return ComposedConfig{}, nil
	}
	c.Enabled, c.Mandatory = deref(enabled), deref(mandatory)
	c.CeilingNone, c.AllowedDurationsAny = deref(none), deref(anyDur)
	if scope != nil {
		c.Scope = *scope
	}
	return c, nil
}

// periodRefused reports whether an amount limit names a period the composed configuration does not
// allow. With no configuration in force there is nothing to check against (the period is still one
// of the closed PeriodKinds, validated earlier).
func (c ComposedConfig) periodRefused(in LimitInput) bool {
	return c.Found && in.Kind.IsAmountKind() && in.Period != nil && !contains(c.AllowedPeriods, string(*in.Period))
}

// exceedsCeiling reports whether value is above the composed ceiling. A removal (nil value), no
// configuration, or an attested-none ceiling never exceeds. A ceiling that cannot be parsed is an
// error (the caller fails closed).
func (c ComposedConfig) exceedsCeiling(value *string) (bool, error) {
	if !c.Found || value == nil || c.Ceiling == nil {
		return false, nil
	}
	v, ok1 := new(big.Int).SetString(*value, 10)
	ceil, ok2 := new(big.Int).SetString(*c.Ceiling, 10)
	if !ok1 || !ok2 {
		return false, errors.New("rg: unparsable value or ceiling")
	}
	return v.Cmp(ceil) > 0, nil
}

// durationAllowed reports whether a time-out of the given hours is allowed: with no configuration,
// or `any`, every positive duration; otherwise only a configured one.
func (c ComposedConfig) durationAllowed(hours int) bool {
	if !c.Found || c.AllowedDurationsAny || len(c.AllowedDurationsSecs) == 0 {
		return true
	}
	for _, s := range c.AllowedDurationsSecs {
		if s == int64(hours)*3600 {
			return true
		}
	}
	return false
}

func deref(b *bool) bool { return b != nil && *b }

func contains(set []string, s string) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}

func (l *Limits) beginWrite(ctx context.Context, tx pgx.Tx, personID uuid.UUID) error {
	var iso string
	if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&iso); err != nil {
		return fmt.Errorf("rg: read isolation level: %w", err)
	}
	if iso != "read committed" {
		return refuse(RefusedUnavailable, "isolation")
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, writeLockTimeout); err != nil {
		return fmt.Errorf("rg: set lock_timeout: %w", err)
	}
	if err := lockPerson(ctx, tx, personID); err != nil {
		return MapDBError(err)
	}
	return nil
}

func lookupByKey(ctx context.Context, tx pgx.Tx, tenantID, accountID uuid.UUID, key string) (*LimitChange, error) {
	c, err := scanLimitChange(tx.QueryRow(ctx, `SELECT `+limitChangeColumns+` FROM player_rg_limit_changes
		WHERE tenant_id = $1 AND player_account_id = $2 AND idempotency_key = $3`, tenantID, accountID, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rg: idempotency lookup: %w", err)
	}
	return &c, nil
}

func replayOrMismatch(prior *LimitChange, hash string) (ChangeResult, error) {
	if prior.RequestHash != hash {
		return ChangeResult{}, refuse(RefusedIdempotencyMismatch, "")
	}
	return ChangeResult{Change: *prior, Replayed: true}, nil
}

// openPendingCount counts the open pending changes (an increase / removal that is the latest request
// of its key and has not yet taken effect or awaits a confirm) VISIBLE to the session for the player.
func openPendingCount(ctx context.Context, tx pgx.Tx, tenantID, accountID, personID uuid.UUID) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM (
			SELECT DISTINCT ON (scope, control_kind, asset_code, period_kind)
			       change_kind, requires_confirmation, effective_at
			  FROM player_rg_limit_changes
			 WHERE tenant_id = $1 AND (player_account_id = $2 OR (scope = 'tenant_person' AND person_id = $3))
			 ORDER BY scope, control_kind, asset_code, period_kind, requested_at DESC, seq DESC
		) latest
		WHERE change_kind IN ('increase', 'removal') AND (requires_confirmation OR effective_at > clock_timestamp())`,
		tenantID, accountID, personID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("rg: count pending changes: %w", err)
	}
	return n, nil
}

type changeCtx struct {
	acct      playerAcct
	source    ChangeSource
	actorType audit.ActorType
	actorID   uuid.UUID
	key       string // the STORED idempotency key
	hash      string
	reason    string
	evidence  *string
	meta      RequestMeta

	// set: the limit named by the caller
	in LimitInput
	// confirm / cancel: the superseded request and the intent
	supersedes *LimitChange
	intent     ChangeKind
	// staff acting on a player's request
	staffRequest bool
}

func (c changeCtx) limitKey(scope LimitScope) LimitKey {
	k := LimitKey{TenantID: c.acct.TenantID, PlayerAccountID: c.acct.ID, PersonID: c.acct.PersonID, Scope: scope}
	if c.supersedes != nil {
		k.Scope, k.Kind, k.AssetCode, k.Period = c.supersedes.Scope, c.supersedes.Kind, c.supersedes.AssetCode, c.supersedes.Period
		if k.Scope == LimitScopeAccount {
			k.PlayerAccountID = c.supersedes.PlayerAccountID
		}
		return k
	}
	k.Kind, k.AssetCode, k.Period = c.in.Kind, c.in.AssetCode, c.in.Period
	return k
}

func (l *Limits) apply(ctx context.Context, tx pgx.Tx, c changeCtx) (res ChangeResult, err error) {
	var cfg ComposedConfig
	defer func() {
		// The config version ids in force travel with a refusal so the denial audit can replay the decision.
		if rf, ok := IsRefusal(err); ok && rf.ConfigVersionIDs == nil && len(cfg.VersionIDs) > 0 {
			rf.ConfigVersionIDs = uuidStrings(cfg.VersionIDs)
		}
	}()
	if err := l.beginWrite(ctx, tx, c.acct.PersonID); err != nil {
		return ChangeResult{}, err
	}
	if prior, err := lookupByKey(ctx, tx, c.acct.TenantID, c.acct.ID, c.key); err != nil {
		return ChangeResult{}, err
	} else if prior != nil {
		return replayOrMismatch(prior, c.hash)
	}

	jur, err := tenantJurisdiction(ctx, tx, c.acct.TenantID)
	if err != nil {
		return ChangeResult{}, err
	}
	var value *string
	scope := LimitScopeAccount
	if c.supersedes == nil {
		value = c.in.Value
		cfg, err = l.compose(ctx, tx, jur, c.acct.TenantID, c.acct.BrandID, c.in.Kind, c.in.AssetCode)
		if err != nil {
			return ChangeResult{}, err
		}
		if cfg.Found && cfg.Scope == string(LimitScopeTenantPerson) {
			scope = LimitScopeTenantPerson
		}
	}
	key := c.limitKey(scope)

	for attempt := 0; ; attempt++ {
		cp := ClassifyParams{Key: key, BrandID: c.acct.BrandID, JurisdictionCode: jur, Value: value, Source: c.source}
		if c.supersedes != nil {
			id, intent := c.supersedes.ID, c.intent
			cp.Supersedes, cp.Intent = &id, &intent
		}
		cl, err := ClassifyLimitChange(ctx, tx, cp)
		if err != nil {
			return ChangeResult{}, MapDBError(err)
		}
		// ADR 0115 S2 duties (ledger-finance LF 7): the database classifies and times a change but clamps
		// the ceiling only at read time and checks no period, so the API refuses both - for an initial
		// value, an increase or a removal ONLY. A decrease is a protective write (ADR 3.1) and succeeds
		// above a since-lowered ceiling or on a since-dropped period.
		if c.supersedes == nil && cl.ChangeKind != ChangeDecrease {
			if cfg.periodRefused(c.in) {
				return ChangeResult{}, refuse(RefusedPeriodNotAllowed, "")
			}
			above, cerr := cfg.exceedsCeiling(value)
			if cerr != nil {
				return ChangeResult{}, refuse(RefusedUnavailable, "ceiling")
			}
			if above {
				return ChangeResult{}, refuse(RefusedAboveCeiling, "")
			}
		}
		if cl.ChangeKind == ChangeConfirm {
			// Security F1: a confirm LOOSENS the limit. A person-level limit is not confirmable from an
			// account under identity review, and only the account that made the request confirms it.
			if key.Scope == LimitScopeTenantPerson && c.acct.Status == "identity_review_required" {
				return ChangeResult{}, refuse(RefusedIdentityReview, "")
			}
			if c.supersedes.PlayerAccountID != c.acct.ID {
				return ChangeResult{}, refuse(RefusedForbidden, "only the requesting account confirms")
			}
		}
		if cl.ChangeKind == ChangeIncrease || cl.ChangeKind == ChangeRemoval {
			if c.staffRequest {
				// ADR 0017 step-up is NOT IMPLEMENTED: a staff-originated increase / removal fails closed.
				return ChangeResult{}, refuse(RefusedStepUpRequired, "")
			}
			if cfg.Found && !cfg.Enabled {
				return ChangeResult{}, refuse(RefusedNotOffered, "")
			}
			if key.Scope == LimitScopeTenantPerson && c.acct.Status == "identity_review_required" {
				return ChangeResult{}, refuse(RefusedIdentityReview, "")
			}
			n, err := openPendingCount(ctx, tx, c.acct.TenantID, c.acct.ID, c.acct.PersonID)
			if err != nil {
				return ChangeResult{}, err
			}
			if n >= l.cfg.PendingCap {
				return ChangeResult{}, refuse(RefusedPendingCap, "")
			}
		}

		before, err := RealizedLimit(ctx, tx, key, nil)
		if err != nil {
			return ChangeResult{}, err
		}

		var intent *string
		var supersedesID *uuid.UUID
		if c.supersedes != nil {
			s := string(c.intent)
			intent, supersedesID = &s, &c.supersedes.ID
		}
		if l.cfg.TestHookBeforeInsert != nil {
			l.cfg.TestHookBeforeInsert(attempt)
		}
		sp, err := tx.Begin(ctx)
		if err != nil {
			return ChangeResult{}, fmt.Errorf("rg: open savepoint: %w", err)
		}
		row := sp.QueryRow(ctx, `INSERT INTO player_rg_limit_changes
			(tenant_id, player_account_id, scope, control_kind, asset_code, period_kind, value, change_kind, supersedes_change_id,
			 source, reason_code, evidence_ref, idempotency_key, request_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8, $9, $10, $11, $12, $13, $14)
		RETURNING `+limitChangeColumns,
			c.acct.TenantID, c.acct.ID, string(key.Scope), string(key.Kind), key.AssetCode, key.period(), value, intent, supersedesID,
			string(c.source), c.reason, c.evidence, c.key, c.hash)
		stored, ierr := scanLimitChange(row)
		if ierr != nil {
			_ = sp.Rollback(ctx)
			// A concurrent / replayed request with the same key may have won: the stored request decides.
			if prior, lerr := lookupByKey(ctx, tx, c.acct.TenantID, c.acct.ID, c.key); lerr == nil && prior != nil {
				return replayOrMismatch(prior, c.hash)
			}
			return ChangeResult{}, MapDBError(ierr)
		}
		if stored.ChangeKind != cl.ChangeKind {
			// The realization instant moved between the preview and the insert (an earlier pending change
			// became effective): undo this attempt and classify again, still under the person lock. The
			// stored row is always the trigger's classification; this only keeps the checks above honest.
			_ = sp.Rollback(ctx)
			if attempt >= 3 {
				return ChangeResult{}, refuse(RefusedUnavailable, "classification_changed")
			}
			continue
		}
		if c.staffRequest && stored.Pending() {
			_ = sp.Rollback(ctx)
			return ChangeResult{}, refuse(RefusedStepUpRequired, "")
		}
		if err := sp.Commit(ctx); err != nil {
			return ChangeResult{}, fmt.Errorf("rg: release savepoint: %w", err)
		}

		if err := l.cfg.Audit(ctx, tx, l.changeAudit(c, stored, before)); err != nil {
			return ChangeResult{}, fmt.Errorf("rg: audit limit change: %w", err)
		}
		return ChangeResult{Change: stored}, nil
	}
}

func (l *Limits) changeAudit(c changeCtx, s LimitChange, before Realized) audit.Entry {
	action := "rg.limit.change_requested"
	switch s.ChangeKind {
	case ChangeConfirm:
		action = "rg.limit.confirmed"
	case ChangeCancelPending:
		action = "rg.limit.cancelled"
	}
	var effAfter, pending *string
	effAfter = before.Value
	if s.Pending() {
		pending = s.Value
	} else {
		effAfter = s.Value
	}
	md := map[string]any{
		"person_id": s.PersonID.String(), "player_account_id": s.PlayerAccountID.String(), "brand_id": s.BrandID.String(),
		"control_kind": string(s.Kind), "scope": string(s.Scope), "asset_code": s.AssetCode, "period": s.Period,
		"change_kind": string(s.ChangeKind), "effective_value_before": before.Value, "effective_value_after": effAfter,
		"pending_value": pending, "effective_at": s.EffectiveAt.UTC().Format(time.RFC3339Nano),
		"requires_confirmation": s.RequiresConfirmation, "config_version_ids": uuidStrings(s.ConfigVersionIDs),
		"source": string(s.Source), "reason_code": s.ReasonCode, "evidence_ref": s.EvidenceRef,
		"supersedes_change_id": s.SupersedesChangeID,
	}
	return audit.Entry{TenantID: s.TenantID, ActorType: c.actorType, ActorID: c.actorID, Action: action,
		TargetType: "player_rg_limit_change", TargetID: s.ID.String(), Outcome: audit.OutcomeSuccess,
		IPAddress: c.meta.IPAddress, UserAgent: c.meta.UserAgent, RequestID: c.meta.RequestID, Metadata: md}
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

// ---- player entry points ------------------------------------------------------------------

// SetLimitInput is a player's own limit request. tx MUST come from db.Pool.WithPlayerScope.
type SetLimitInput struct {
	TenantID, PlayerAccountID uuid.UUID
	Limit                     LimitInput
	Meta                      RequestMeta
}

// SetLimit records the player's own limit change (initial / decrease apply now; an increase or a
// removal waits out the cooling-off period and, when configured, the player's own confirm).
func (l *Limits) SetLimit(ctx context.Context, tx pgx.Tx, in SetLimitInput) (ChangeResult, error) {
	if err := in.Limit.validate(); err != nil {
		return ChangeResult{}, err
	}
	acct, err := loadAccount(ctx, tx, in.TenantID, in.PlayerAccountID)
	if err != nil {
		return ChangeResult{}, err
	}
	return l.apply(ctx, tx, changeCtx{acct: acct, source: ChangeSourcePlayerSelfService, actorType: audit.ActorPlayer, actorID: acct.ID,
		key: in.Limit.IdempotencyKey, hash: in.Limit.hash("player"), reason: "player_self_service", meta: in.Meta, in: in.Limit})
}

// ResolveInput confirms or cancels a pending change. tx MUST come from db.Pool.WithPlayerScope.
type ResolveInput struct {
	TenantID, PlayerAccountID uuid.UUID
	ChangeID                  uuid.UUID
	Intent                    ChangeKind // ChangeConfirm | ChangeCancelPending
	IdempotencyKey            string
	Meta                      RequestMeta
}

// ResolvePending confirms (after the cooling-off period, by the player's own session only) or
// cancels a pending increase / removal. The superseded request must be the latest of its limit.
func (l *Limits) ResolvePending(ctx context.Context, tx pgx.Tx, in ResolveInput) (ChangeResult, error) {
	if in.Intent != ChangeConfirm && in.Intent != ChangeCancelPending {
		return ChangeResult{}, invalidf("intent must be confirm or cancel")
	}
	if !keyRe.MatchString(in.IdempotencyKey) {
		return ChangeResult{}, invalidf("Idempotency-Key must be 1 to 120 printable ASCII characters")
	}
	acct, err := loadAccount(ctx, tx, in.TenantID, in.PlayerAccountID)
	if err != nil {
		return ChangeResult{}, err
	}
	target, err := scanLimitChange(tx.QueryRow(ctx, `SELECT `+limitChangeColumns+` FROM player_rg_limit_changes
		WHERE id = $1 AND tenant_id = $2`, in.ChangeID, in.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ChangeResult{}, refuse(RefusedNotFound, "") // RLS: only the player's own / person-level rows are visible
	}
	if err != nil {
		return ChangeResult{}, fmt.Errorf("rg: load change: %w", err)
	}
	return l.apply(ctx, tx, changeCtx{acct: acct, source: ChangeSourcePlayerSelfService, actorType: audit.ActorPlayer, actorID: acct.ID,
		key: in.IdempotencyKey, hash: fingerprint("rg.limit.resolve.v1", string(in.Intent), in.ChangeID.String()),
		reason: "player_self_service", meta: in.Meta, supersedes: &target, intent: in.Intent})
}

// ---- staff entry point --------------------------------------------------------------------

// StaffSetLimitInput is a limit change a staff member records on a player's request. tx MUST come
// from db.Pool.WithPrincipalScope(tenant, staff). Only decreases / initial values are accepted: an
// increase or a removal is refused with RefusedStepUpRequired (ADR 0017 is NOT IMPLEMENTED), and even
// after step-up ships a staff-originated increase is never realized without the player's own confirm.
type StaffSetLimitInput struct {
	TenantID, StaffID, PlayerAccountID uuid.UUID
	Limit                              LimitInput
	ReasonCode, EvidenceRef            string
	Meta                               RequestMeta
}

// SetLimitOnPlayerRequest records the staff-originated change.
func (l *Limits) SetLimitOnPlayerRequest(ctx context.Context, tx pgx.Tx, in StaffSetLimitInput) (ChangeResult, error) {
	if err := in.Limit.validate(); err != nil {
		return ChangeResult{}, err
	}
	if len(in.ReasonCode) == 0 || len(in.ReasonCode) > 64 || len(in.EvidenceRef) == 0 || len(in.EvidenceRef) > 200 ||
		!printableASCII(in.ReasonCode) || !printableASCII(in.EvidenceRef) ||
		strings.TrimSpace(in.ReasonCode) == "" || strings.TrimSpace(in.EvidenceRef) == "" {
		return ChangeResult{}, invalidf("reason_code (1-64) and evidence_ref (1-200), printable ASCII, are required")
	}
	acct, err := loadAccount(ctx, tx, in.TenantID, in.PlayerAccountID)
	if err != nil {
		return ChangeResult{}, err
	}
	var staffPerson *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT person_id FROM staff_users WHERE id = $1 AND tenant_id = $2 AND status = 'active'`,
		in.StaffID, in.TenantID).Scan(&staffPerson); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ChangeResult{}, refuse(RefusedForbidden, "")
		}
		return ChangeResult{}, fmt.Errorf("rg: load staff person: %w", err)
	}
	if staffPerson != nil && *staffPerson == acct.PersonID {
		return ChangeResult{}, refuse(RefusedSelfTarget, "")
	}
	ev := in.EvidenceRef
	// The staff key namespace is separate from the player's (a staff key cannot squat a player key).
	return l.apply(ctx, tx, changeCtx{acct: acct, source: ChangeSourceStaffOnPlayerRequest, actorType: audit.ActorStaff, actorID: in.StaffID,
		key: "staff " + in.Limit.IdempotencyKey, hash: in.Limit.hash("staff", in.StaffID.String(), in.ReasonCode, in.EvidenceRef),
		reason: in.ReasonCode, evidence: &ev, meta: in.Meta, in: in.Limit, staffRequest: true})
}

// ---- reads ----------------------------------------------------------------------------------

// LimitState is the current state of one limit key.
type LimitState struct {
	Scope       LimitScope
	Kind        ControlKind
	AssetCode   *string
	Period      *PeriodKind
	HasValue    bool
	Value       *string // the realized (effective) value of THIS key; nil + HasValue = removed
	RealizedID  *uuid.UUID
	Pending     *LimitChange
	LatestKnown LimitChange
}

// ListChanges returns up to limit most recent changes visible to the session for the account (a
// player: own account + own person-level rows; staff: the account's rows plus person-level rows of
// its Person), newest first.
func ListChanges(ctx context.Context, tx pgx.Tx, tenantID, accountID uuid.UUID, limit int) ([]LimitChange, error) {
	acct, err := loadAccount(ctx, tx, tenantID, accountID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := tx.Query(ctx, `SELECT `+limitChangeColumns+` FROM player_rg_limit_changes
		WHERE tenant_id = $1 AND (player_account_id = $2 OR (scope = 'tenant_person' AND person_id = $3))
		ORDER BY requested_at DESC, seq DESC LIMIT $4`, tenantID, accountID, acct.PersonID, limit)
	if err != nil {
		return nil, fmt.Errorf("rg: list changes: %w", err)
	}
	defer rows.Close()
	var out []LimitChange
	for rows.Next() {
		c, err := scanLimitChange(rows)
		if err != nil {
			return nil, fmt.Errorf("rg: scan change: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CurrentLimits derives the per-key state in ONE query: the latest request of every limit key visible to
// the session for the account (DISTINCT ON, no row cap), its realized value from the ONE SQL realization
// function (LATERAL), and whether it is an open pending change. The MIN across scopes and the ceiling is
// the enforcement read (S3/S4), not computed here.
func CurrentLimits(ctx context.Context, tx pgx.Tx, tenantID, accountID uuid.UUID) ([]LimitState, error) {
	acct, err := loadAccount(ctx, tx, tenantID, accountID)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `WITH latest AS (
			SELECT DISTINCT ON (scope, control_kind, asset_code, period_kind) `+limitChangeColumns+`,
			       (change_kind IN ('increase', 'removal') AND (requires_confirmation OR effective_at > clock_timestamp())) AS is_pending
			  FROM player_rg_limit_changes
			 WHERE tenant_id = $1 AND (player_account_id = $2 OR (scope = 'tenant_person' AND person_id = $3))
			 ORDER BY scope, control_kind, asset_code, period_kind, requested_at DESC, seq DESC
		)
		SELECT l.*, r.has_row, r.value::text, r.change_id
		  FROM latest l
		 CROSS JOIN LATERAL rg_limit_realized(l.tenant_id, l.player_account_id, l.person_id, l.scope, l.control_kind,
		                                       l.asset_code, l.period_kind, clock_timestamp()) r
		 ORDER BY l.control_kind, l.scope, l.asset_code, l.period_kind`, tenantID, accountID, acct.PersonID)
	if err != nil {
		return nil, fmt.Errorf("rg: current limits: %w", err)
	}
	defer rows.Close()
	var out []LimitState
	for rows.Next() {
		var c LimitChange
		var scope, kind, change, source string
		var period *string
		var pending bool
		var st LimitState
		if err := rows.Scan(&c.ID, &c.TenantID, &c.PlayerAccountID, &c.PersonID, &c.BrandID, &scope, &kind, &c.AssetCode, &period,
			&c.Value, &change, &c.RequiresConfirmation, &c.RequestedAt, &c.EffectiveAt, &c.SupersedesChangeID, &source, &c.ReasonCode,
			&c.EvidenceRef, &c.CreatedByActorType, &c.CreatedByActorID, &c.ConfigVersionIDs, &c.RequestHash,
			&pending, &st.HasValue, &st.Value, &st.RealizedID); err != nil {
			return nil, fmt.Errorf("rg: scan current limit: %w", err)
		}
		c.Scope, c.Kind, c.ChangeKind, c.Source = LimitScope(scope), ControlKind(kind), ChangeKind(change), ChangeSource(source)
		if period != nil {
			p := PeriodKind(*period)
			c.Period = &p
		}
		st.Scope, st.Kind, st.AssetCode, st.Period, st.LatestKnown = c.Scope, c.Kind, c.AssetCode, c.Period, c
		if pending {
			cc := c
			st.Pending = &cc
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

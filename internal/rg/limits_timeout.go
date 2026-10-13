package rg

// ADR 0115 SLICE S2: the player time-out (cooling-off break) write. API only: EvaluateEligibility
// does not read a time-out yet (S4), so S2 records the restriction and nothing blocks play.
//
// A time-out is a PROTECTIVE write (ADR 0115 3.1): it succeeds whatever the enabled / mandatory state
// of the configuration, and with no configuration in force it accepts any positive duration (up to
// the engineering bound MaxTimeOutHours). It can never be ended early (player_restrictions is
// append-only) and never changes wallet status, so withdrawals keep working.
//
// Scope. The configured time_out scope decides the reach; absent configuration it is platform_person
// (the protective default, ADR 0115 1.1). player_restrictions has no column that encodes an
// ACCOUNT-scope time-out without a migration, so a configured `account` scope is applied as
// tenant_person - the next broader, MORE protective reach - and the audit record names both
// (requested_scope / applied_scope). Recorded as a residual in the ADR.
//
// Idempotency. player_restrictions has no idempotency key column or unique constraint (S2 has no
// migration), so the Idempotency-Key is honoured under the person lock (L0.4) that serialises every
// writer of that Person, against the audit record of the earlier request (action
// rg.time_out.created, same actor, same key, committed in the same transaction as the restriction).
// It is therefore race-free for one Person but is NOT a database unique constraint (unlike the limit
// changes); a database-enforced key needs a migration. Recorded as a residual in the ADR.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// TimeOutInput is a player's own time-out request. tx MUST come from db.Pool.WithPlayerScope.
type TimeOutInput struct {
	TenantID, PlayerAccountID uuid.UUID
	DurationHours             int
	IdempotencyKey            string
	Meta                      RequestMeta
}

// TimeOutResult is the outcome of CreateTimeOut.
type TimeOutResult struct {
	Restriction    Restriction
	Replayed       bool
	RequestedScope string
	AppliedScope   string
}

// CreateTimeOut records the player's own time-out.
func (l *Limits) CreateTimeOut(ctx context.Context, tx pgx.Tx, in TimeOutInput) (TimeOutResult, error) {
	if in.DurationHours <= 0 || in.DurationHours > MaxTimeOutHours {
		return TimeOutResult{}, invalidf("duration_hours must be between 1 and %d", MaxTimeOutHours)
	}
	if !keyRe.MatchString(in.IdempotencyKey) {
		return TimeOutResult{}, invalidf("Idempotency-Key must be 1 to 120 printable ASCII characters")
	}
	acct, err := loadAccount(ctx, tx, in.TenantID, in.PlayerAccountID)
	if err != nil {
		return TimeOutResult{}, err
	}
	hash := fingerprint("rg.time_out.v1", fmt.Sprint(in.DurationHours))
	if err := l.beginWrite(ctx, tx, acct.PersonID); err != nil {
		return TimeOutResult{}, err
	}

	var priorID, priorHash *string
	err = tx.QueryRow(ctx, `SELECT target_id, metadata->>'request_hash' FROM audit_log
		WHERE tenant_id = $1 AND actor_type = 'player' AND actor_id = $2 AND action = 'rg.time_out.created'
		  AND metadata->>'idempotency_key' = $3 ORDER BY created_at LIMIT 1`,
		in.TenantID, in.PlayerAccountID, in.IdempotencyKey).Scan(&priorID, &priorHash)
	if err == nil {
		if priorHash == nil || *priorHash != hash || priorID == nil {
			return TimeOutResult{}, refuse(RefusedIdempotencyMismatch, "")
		}
		id, perr := uuid.Parse(*priorID)
		if perr != nil {
			return TimeOutResult{}, fmt.Errorf("rg: stored time-out id: %w", perr)
		}
		rows, qerr := tx.Query(ctx, `SELECT `+restrictionColumns+` FROM player_restrictions WHERE id = $1`, id)
		if qerr != nil {
			return TimeOutResult{}, fmt.Errorf("rg: load time-out: %w", qerr)
		}
		rs, serr := scanRestrictions(rows)
		if serr != nil || len(rs) != 1 {
			return TimeOutResult{}, fmt.Errorf("rg: load time-out: %d rows: %v", len(rs), serr)
		}
		return TimeOutResult{Restriction: rs[0], Replayed: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return TimeOutResult{}, fmt.Errorf("rg: time-out idempotency lookup: %w", err)
	}

	jur, err := tenantJurisdiction(ctx, tx, acct.TenantID)
	if err != nil {
		return TimeOutResult{}, err
	}
	cfg, err := l.compose(ctx, tx, jur, acct.TenantID, acct.BrandID, ControlTimeOut, nil)
	if err != nil {
		return TimeOutResult{}, err
	}
	requested := string(LimitScopePlatformPerson)
	if cfg.Found && cfg.Scope != "" {
		requested = cfg.Scope
	}
	applied := requested
	if requested == string(LimitScopeAccount) {
		applied = string(LimitScopeTenantPerson) // see the file comment: the broader, more protective reach
	}
	if !cfg.durationAllowed(in.DurationHours) {
		return TimeOutResult{}, refuse(RefusedDurationNotAllowed, "")
	}

	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return TimeOutResult{}, fmt.Errorf("rg: database clock: %w", err)
	}
	now = now.UTC()
	ends := now.Add(time.Duration(in.DurationHours) * time.Hour) // fixed hours: never DST-sensitive
	r := Restriction{PersonID: acct.PersonID, PlayerAccountID: &acct.ID, RestrictionType: RestrictionTimeOut,
		StartsAt: now, EndsAt: &ends, Source: SourcePlayerSelfService, CreatedByActorType: audit.ActorPlayer, CreatedByActorID: acct.ID}
	if applied != string(LimitScopePlatformPerson) {
		t := acct.TenantID // pinned from the authenticated session, never a body value
		r.TenantID = &t
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return TimeOutResult{}, fmt.Errorf("rg: open savepoint: %w", err)
	}
	stored, err := insertRestriction(ctx, sp, r)
	if err != nil {
		_ = sp.Rollback(ctx)
		return TimeOutResult{}, MapDBError(err)
	}
	if err := sp.Commit(ctx); err != nil {
		return TimeOutResult{}, fmt.Errorf("rg: release savepoint: %w", err)
	}
	if err := l.cfg.Audit(ctx, tx, audit.Entry{
		TenantID: acct.TenantID, ActorType: audit.ActorPlayer, ActorID: acct.ID, Action: "rg.time_out.created",
		TargetType: "player_restriction", TargetID: stored.ID.String(), Outcome: audit.OutcomeSuccess,
		IPAddress: in.Meta.IPAddress, UserAgent: in.Meta.UserAgent, RequestID: in.Meta.RequestID,
		Metadata: map[string]any{
			"person_id": acct.PersonID.String(), "duration_hours": in.DurationHours, "requested_scope": requested,
			"applied_scope": applied, "ends_at": ends.Format(time.RFC3339Nano), "idempotency_key": in.IdempotencyKey,
			"request_hash": hash, "config_version_ids": uuidStrings(cfg.VersionIDs), "source": string(SourcePlayerSelfService),
		},
	}); err != nil {
		return TimeOutResult{}, fmt.Errorf("rg: audit time-out: %w", err)
	}
	return TimeOutResult{Restriction: stored, RequestedScope: requested, AppliedScope: applied}, nil
}

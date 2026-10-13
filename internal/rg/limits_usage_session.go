package rg

// ADR 0115 SLICE S3: session-duration usage (section 1.5).
//
// Status: PARTIALLY IMPLEMENTED (read side). The measurement is "how long has this refresh-token
// rotation chain been running": the age of the ROOT of the chain (migrations/0012_create_sessions;
// replaced_by_session_id links a rotated session to its successor), computed in SQL from one
// statement instant. The deadline, post_session_break and the refusal are S4.
//
// BLOCKED / NOT IMPLEMENTED here: aggregation of the open chains of a Person's OTHER accounts
// (concurrent_chain_aggregation at tenant_person scope). The sessions table is row-level secured to
// the caller's own principal (migration 0018; app.principal_id), so a measurement run for one
// account cannot see another account's chains, and a "zero" would be a silent under-count. What this
// file measures is the anchor account's own chains (principal_only coverage); a cross-account read
// needs a privileged read path, which is a security decision recorded in the ADR (it is not made here).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrSessionChainNotFound: the session is not visible to the caller (RLS: own principal only) or does
// not belong to the account. Fail closed - never "zero elapsed".
var ErrSessionChainNotFound = errors.New("rg: session chain not found for the account")

// ErrSessionChainTooLong: the chain walk hit its safety bound (corrupted or cyclic chain).
var ErrSessionChainTooLong = errors.New("rg: session chain walk hit its depth bound")

// maxSessionChainDepth bounds the chain walk (a var so a test can lower it).
var maxSessionChainDepth = 100000

// SessionCoveragePrincipalOnly names the coverage of a SessionUsage: the account's own sessions.
const SessionCoveragePrincipalOnly = "principal_only"

// SessionRequest is the input of MeasureSessionUsage. The transaction must carry the account as
// app.principal_id (db.Pool.WithPrincipalScope) - the only sanctioned read of a principal's sessions.
type SessionRequest struct {
	TenantID        uuid.UUID
	PlayerAccountID uuid.UUID
	SessionID       uuid.UUID // any session of the chain being measured
	// AggregateOpenChains also reports the earliest root among the account's open chains
	// (concurrent_chain_aggregation), principal_only coverage.
	AggregateOpenChains bool
	At                  *time.Time // replay seam; nil = clock_timestamp()
}

// SessionUsage is the session usage at AsOf.
type SessionUsage struct {
	AsOf               time.Time
	ChainRootStartedAt time.Time
	ElapsedSeconds     int64 // whole seconds since the chain root, floored
	// Set only when AggregateOpenChains: the earliest root among the account's open chains (and the
	// measured chain's own root) and the seconds since it.
	AggregatedRootStartedAt *time.Time
	AggregatedElapsed       *int64
	OpenChains              int
	Coverage                string
}

// MeasureSessionUsage measures the age of the session's chain root (and optionally the earliest open
// chain root of the same account).
func MeasureSessionUsage(ctx context.Context, tx pgx.Tx, req SessionRequest) (SessionUsage, error) {
	if req.TenantID == uuid.Nil || req.PlayerAccountID == uuid.Nil || req.SessionID == uuid.Nil {
		return SessionUsage{}, errors.New("rg: session usage request needs a tenant, an account and a session")
	}
	if err := usageGuard(ctx, tx, req.TenantID); err != nil {
		return SessionUsage{}, err
	}
	var principal string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(current_setting('app.principal_id', true), '')`).Scan(&principal); err != nil {
		return SessionUsage{}, fmt.Errorf("rg: session usage principal: %w", err)
	}
	if principal != req.PlayerAccountID.String() {
		return SessionUsage{}, ErrSessionChainNotFound
	}
	var asOf time.Time
	var root *time.Time
	var depth *int
	err := tx.QueryRow(ctx, `WITH RECURSIVE asof AS MATERIALIZED (SELECT COALESCE($4::timestamptz, clock_timestamp()) AS t),
		up AS (
		  SELECT s.id, s.created_at, s.principal_id, s.tenant_id, 0 AS depth FROM sessions s
		   WHERE s.id = $1 AND s.tenant_id = $2 AND s.principal_type = 'player' AND s.principal_id = $3
		  UNION ALL
		  SELECT p.id, p.created_at, p.principal_id, p.tenant_id, up.depth + 1 FROM sessions p
		    JOIN up ON p.replaced_by_session_id = up.id
		   WHERE p.principal_type = 'player' AND p.principal_id = up.principal_id AND p.tenant_id = up.tenant_id
		     AND up.depth < $5
		)
		SELECT (SELECT t FROM asof), r.created_at, r.depth FROM (SELECT created_at, depth FROM up ORDER BY depth DESC LIMIT 1) r`,
		req.SessionID, req.TenantID, req.PlayerAccountID, req.At, maxSessionChainDepth,
	).Scan(&asOf, &root, &depth)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionUsage{}, ErrSessionChainNotFound
	}
	if err != nil {
		return SessionUsage{}, fmt.Errorf("rg: session chain root: %w", err)
	}
	if root == nil || depth == nil {
		return SessionUsage{}, ErrSessionChainNotFound
	}
	if *depth >= maxSessionChainDepth {
		return SessionUsage{}, ErrSessionChainTooLong
	}
	out := SessionUsage{AsOf: asOf, ChainRootStartedAt: *root, ElapsedSeconds: flooredSeconds(asOf.Sub(*root)), Coverage: SessionCoveragePrincipalOnly}
	if !req.AggregateOpenChains {
		return out, nil
	}
	var earliest *time.Time
	var open int
	var maxDepth int
	err = tx.QueryRow(ctx, `WITH RECURSIVE asof AS MATERIALIZED (SELECT COALESCE($3::timestamptz, clock_timestamp()) AS t),
		leaves AS (
		  SELECT s.id, s.created_at FROM sessions s, asof
		   WHERE s.tenant_id = $1 AND s.principal_type = 'player' AND s.principal_id = $2
		     AND s.replaced_by_session_id IS NULL AND s.revoked_at IS NULL AND s.expires_at > asof.t
		),
		upl AS (
		  SELECT l.id AS leaf, l.id AS cur, l.created_at, 0 AS depth FROM leaves l
		  UNION ALL
		  SELECT u.leaf, p.id, p.created_at, u.depth + 1 FROM upl u
		    JOIN sessions p ON p.replaced_by_session_id = u.cur
		   WHERE p.principal_type = 'player' AND p.principal_id = $2 AND p.tenant_id = $1 AND u.depth < $4
		),
		roots AS (SELECT DISTINCT ON (leaf) leaf, created_at, depth FROM upl ORDER BY leaf, depth DESC)
		SELECT min(created_at), count(*)::int, COALESCE(max(depth), 0)::int FROM roots`,
		req.TenantID, req.PlayerAccountID, req.At, maxSessionChainDepth,
	).Scan(&earliest, &open, &maxDepth)
	if err != nil {
		return SessionUsage{}, fmt.Errorf("rg: open session chains: %w", err)
	}
	if maxDepth >= maxSessionChainDepth {
		return SessionUsage{}, ErrSessionChainTooLong
	}
	agg := *root
	if earliest != nil && earliest.Before(agg) {
		agg = *earliest
	}
	secs := flooredSeconds(asOf.Sub(agg))
	out.AggregatedRootStartedAt, out.AggregatedElapsed, out.OpenChains = &agg, &secs, open
	return out, nil
}

func flooredSeconds(d time.Duration) int64 {
	if d < 0 {
		return 0
	}
	return int64(d / time.Second)
}

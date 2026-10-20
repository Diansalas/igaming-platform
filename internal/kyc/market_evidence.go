package kyc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ReadMarketResidenceEvidence is the read behind the platform's CURRENT
// operating-market evidence source (owner decision 3; docs/plans/
// market-evidence-kyc-record.md): the country of a player's KYC-VERIFIED
// residence, or ok=false when there is none. It is a pure read - it writes
// nothing and records nothing.
//
// The rule is deliberately NARROWER than GetVerifiedResidence (which is a staff
// read and ignores expiry and later supersession). A country is returned only when ALL hold:
//
//  1. the player account's LATEST DECIDED verification (the same row
//     readLatestVerificationByPlayerAccount picks for enforcement: orphan intent
//     rows excluded, ordered created_at DESC, id DESC) has status 'approved' and
//     is not expired (expires_at is NULL or in the future, database clock). A
//     newer pending / review_required / rejected / expired / unverified decided
//     row therefore withdraws the evidence immediately - exactly as it withdraws
//     the deposit and withdrawal clearance;
//  2. a verification of the SAME player account (same tenant) has status 'approved', carries a
//     reviewer-recorded residence determination and is itself not expired; the one
//     with the most recent determination wins, AND no rejected, expired or
//     review_required row of the account is newer than that determination's
//     row (SRME-2: a determination is not carried forward across a rejection,
//     an expiry or a manual-review stage; a fresh determination is needed. A
//     plain pending re-verification does not break the carry-forward). SRME-R2-1:
//     two more breakers, both treated by enforcement like rejected/expired: a
//     DECIDED 'unverified' row (provider_reference set; an orphan intent row
//     with no reference is not a decision and does not break), and an 'approved'
//     row whose expires_at has lapsed (its status stays 'approved').
//
// LIC10 (migration 0181, audit G8 / owner question Q21): a determination that the tenant's residence approval policy REQUIRES a
// second Person to approve (residence_determination_checks.approval_required) and that has none yet is PENDING and is NOT
// evidence: it is excluded from the candidate set, so the read falls back to an older approved determination that is still
// valid, or to "no evidence" (the caller refuses). The policy mode is snapshotted on each determination when it is made, so the
// default (no policy, mode none) leaves every determination exactly as effective as before the migration; a determination with
// no check row (made before the migration) is likewise unchanged. Pending fails closed.
//
// Anything else - no row, no determination, an unverified / pending / review /
// rejected / expired latest row - is "no evidence" and the caller refuses.
//
// Tenant scope: the read runs on a tenant-scoped transaction (RLS on
// kyc_verifications is the isolation control). In addition the function asserts
// that the transaction's app.tenant_id equals tenantID and repeats tenant_id in
// the predicate, so a caller whose transaction and tenantID disagree gets an
// error (fail closed), never another tenant's row. tenantID and
// playerAccountID are server-derived by every caller (authenticated token); the
// function accepts no client value.
func ReadMarketResidenceEvidence(ctx context.Context, q RowQuerier, tenantID, playerAccountID uuid.UUID) (country string, ok bool, err error) {
	if tenantID == uuid.Nil || playerAccountID == uuid.Nil {
		return "", false, fmt.Errorf("kyc: market residence evidence: tenant and player account are required")
	}
	var scoped *uuid.UUID
	if err := q.QueryRow(ctx, `SELECT NULLIF(current_setting('app.tenant_id', true), '')::uuid`).Scan(&scoped); err != nil {
		return "", false, fmt.Errorf("kyc: market residence evidence: read tenant scope: %w", err)
	}
	if scoped == nil || *scoped != tenantID {
		return "", false, fmt.Errorf("kyc: market residence evidence: transaction tenant scope does not match the requested tenant")
	}

	var c string
	err = q.QueryRow(ctx, `
		SELECT r.verified_residence_country
		  FROM (
		        SELECT status, expires_at FROM kyc_verifications
		         WHERE tenant_id = $1 AND player_account_id = $2
		           AND `+orphanRowExclusionSQL+`
		         ORDER BY created_at DESC, id DESC LIMIT 1
		       ) l
		  JOIN LATERAL (
		        SELECT kv.verified_residence_country FROM kyc_verifications kv
		         WHERE kv.tenant_id = $1 AND kv.player_account_id = $2
		           AND kv.status = 'approved'
		           AND kv.verified_residence_country IS NOT NULL
		           AND (kv.expires_at IS NULL OR kv.expires_at > clock_timestamp())
		           AND NOT EXISTS (
		                 SELECT 1 FROM residence_determination_checks rc
		                  WHERE rc.tenant_id = $1 AND rc.verification_id = kv.id
		                    AND rc.determined_at = kv.verified_residence_set_at
		                    AND rc.approval_required AND rc.approved_at IS NULL)
		           AND NOT EXISTS (
		                 SELECT 1 FROM kyc_verifications brk
		                  WHERE brk.tenant_id = $1 AND brk.player_account_id = $2
		                    AND (brk.created_at, brk.id) > (kv.created_at, kv.id)
		                    AND (brk.status IN ('rejected', 'expired', 'review_required')
		                         OR (brk.status = 'unverified' AND brk.provider_reference IS NOT NULL)
		                         OR (brk.status = 'approved' AND brk.expires_at IS NOT NULL
		                             AND brk.expires_at <= clock_timestamp())))
		         ORDER BY kv.verified_residence_set_at DESC, kv.id DESC LIMIT 1
		       ) r ON true
		 WHERE l.status = 'approved'
		   AND (l.expires_at IS NULL OR l.expires_at > clock_timestamp())`,
		tenantID, playerAccountID,
	).Scan(&c)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("kyc: market residence evidence: %w", err)
	}
	return c, true, nil
}

// RowsQuerier is the multi-row read capability (pgx.Tx satisfies it).
type RowsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// PendingResidenceApproval is one reviewer determination that is waiting for a second Person (lic10). It carries ids and times
// only: never the country (reading the residence VALUE is its own permission, HDR-J-3f).
type PendingResidenceApproval struct {
	VerificationID  uuid.UUID
	PlayerAccountID uuid.UUID
	BrandID         uuid.UUID
	DeterminedAt    time.Time
	DeterminedBy    uuid.UUID
	PolicyMode      string
}

// MaxPendingResidenceApprovals bounds one list.
const MaxPendingResidenceApprovals = 200

// ListPendingResidenceApprovals lists the CURRENT determinations that need a second Person and have none, oldest first. A
// determination replaced by a newer one is not listed (its check no longer matches the verification's current determination).
// tx must be a tenant-scoped transaction; RLS on both tables is the isolation control and the tenant is repeated in the
// predicate.
func ListPendingResidenceApprovals(ctx context.Context, q RowsQuerier, tenantID uuid.UUID, limit int) ([]PendingResidenceApproval, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("kyc: pending residence approvals: tenant is required")
	}
	if limit <= 0 || limit > MaxPendingResidenceApprovals {
		limit = MaxPendingResidenceApprovals
	}
	rows, err := q.Query(ctx, `
		SELECT c.verification_id, v.player_account_id, v.brand_id, c.determined_at, c.determined_by, c.policy_mode
		  FROM residence_determination_checks c
		  JOIN kyc_verifications v ON v.id = c.verification_id AND v.tenant_id = c.tenant_id AND v.verified_residence_set_at = c.determined_at
		 WHERE c.tenant_id = $1 AND c.approval_required AND c.approved_at IS NULL
		 ORDER BY c.determined_at, c.verification_id LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("kyc: pending residence approvals: %w", err)
	}
	defer rows.Close()
	var out []PendingResidenceApproval
	for rows.Next() {
		var p PendingResidenceApproval
		if err := rows.Scan(&p.VerificationID, &p.PlayerAccountID, &p.BrandID, &p.DeterminedAt, &p.DeterminedBy, &p.PolicyMode); err != nil {
			return nil, fmt.Errorf("kyc: pending residence approvals: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

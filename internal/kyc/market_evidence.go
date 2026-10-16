package kyc

import (
	"context"
	"errors"
	"fmt"

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
//     plain pending re-verification does not break the carry-forward).
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
		                 SELECT 1 FROM kyc_verifications brk
		                  WHERE brk.tenant_id = $1 AND brk.player_account_id = $2
		                    AND brk.status IN ('rejected', 'expired', 'review_required')
		                    AND (brk.created_at, brk.id) > (kv.created_at, kv.id))
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

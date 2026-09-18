// Cashback scheduling job (Stage 4H-B1 Wave 3 Phase 3, ledger-accounting-
// model.md §7.18.4). Closes reconnaissance gap-list item 12 ("no
// settlement-window job exists to compute NetLossAmount and invoke
// [IssueAndActivateCashback]").
//
// migration 0069's bonus_cashback_schedule_watermarks table is
// EXPLICITLY non-load-bearing for correctness (§7.18.4 item 5's own
// text: idempotency is already fully guaranteed by bonus_grants' own
// (tenant_id, campaign_id, offer_version_id, player_account_id,
// trigger_reference) unique constraint via the deterministic
// trigger_reference this file computes - "cashback:<campaign_id>:
// <player_account_id>:<window_start>:<window_end>"). The watermark
// exists purely so a scheduler tick can find "whose window has elapsed"
// without rescanning a player's entire enrollment history every tick - a
// PERFORMANCE optimization, never a correctness dependency. Stated here
// explicitly per the dispatch's own instruction to document that
// distinction: a lost/reset/stale watermark can, at worst, cause
// redundant NetLossAmount recomputation and a resolved-to-existing-Grant
// no-op via ErrAlreadyGranted - never a double payout.
//
// NAMED, DISCLOSED WAVE-3 MINIMUM-VIABLE SCOPE DECISION (bonus-engine's
// own business logic, per §7.18.4's own text: "which players/campaigns
// are due... is bonus-engine's own business logic to write" - not
// silently narrowed): this platform has no explicit Cashback-campaign
// "enrollment" concept anywhere (no opt-in table, no segment-membership
// evaluator - internal/segment does not exist, per the Orchestrator's own
// standing scope decision). This scheduler therefore treats "every
// player who placed at least one qualifying (non-nullified) casino_bet
// in the campaign's own reward asset during the window" as that window's
// candidate set - discovered via the IDENTICAL ledger_transactions/
// ledger_entries shape §7.18.4 item 4 already specifies for NetLossAmount
// itself (no second query shape invented). This is correct for a non-
// opt-in Cashback campaign (OfferVersion.OptInRequired = false, the
// common case) and is a NAMED simplification for an OptInRequired = true
// Offer (that axis remains a disclosed, unenforced gap - reconnaissance
// gap-list item 15 - this scheduler does not additionally filter by it).
package bonus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// CashbackWindowsPerTick bounds how many consecutive elapsed windows a
// single tick processes for one (campaign, player) pair, mirroring
// DepositSweepBatchSize's own reasoning - a very old, never-swept
// watermark is caught up over several ticks rather than one unbounded
// loop.
const CashbackWindowsPerTick = 12

type cashbackMatchableCampaign struct {
	CampaignID        uuid.UUID
	CampaignVersionID uuid.UUID
	OfferID           uuid.UUID
	OfferVersion      OfferVersion
	FulfillmentOwner  string
	BrandID           *uuid.UUID
	WindowSeconds     int64
	WindowAnchor      time.Time
}

// listCashbackMatchableCampaigns resolves every Active Campaign/Offer
// whose current OfferVersion is RewardKind R1 with CompletionMechanic
// "C2" - doc 10's own established shorthand for Cashback's completion
// mechanic (lifecycle.go's own IssueAndActivateCashback doc comment:
// "Completion mechanic C2 (time-window settlement)... a Cashback Offer's
// Wagering axis is a no-op"), reused here as the marker this scheduler
// selects on rather than inventing a parallel classification.
func listCashbackMatchableCampaigns(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]cashbackMatchableCampaign, error) {
	rows, err := tx.Query(ctx, `
		SELECT c.id, c.current_version_id, o.id, ov.id, c.fulfillment_owner, c.brand_id, c.created_at
		  FROM bonus_campaigns c
		  JOIN bonus_offers o ON o.campaign_id = c.id AND o.tenant_id = c.tenant_id
		  JOIN bonus_offer_versions ov ON ov.id = o.current_version_id AND ov.tenant_id = o.tenant_id
		 WHERE c.tenant_id = $1
		   AND c.status = 'active' AND o.status = 'active' AND o.grant_policy = 'auto_issue'
		   AND ov.reward_kind = 'R1' AND ov.completion_mechanic = 'C2'`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("bonus: list cashback-matchable campaigns: %w", err)
	}
	defer rows.Close()

	type raw struct {
		campaignID, campaignVersionID, offerID, offerVersionID uuid.UUID
		fulfillmentOwner                                       string
		brandID                                                *uuid.UUID
		createdAt                                              time.Time
	}
	var candidates []raw
	for rows.Next() {
		var r raw
		if err := rows.Scan(&r.campaignID, &r.campaignVersionID, &r.offerID, &r.offerVersionID, &r.fulfillmentOwner, &r.brandID, &r.createdAt); err != nil {
			return nil, fmt.Errorf("bonus: scan cashback-matchable campaign: %w", err)
		}
		candidates = append(candidates, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []cashbackMatchableCampaign
	for _, r := range candidates {
		ov, err := GetOfferVersionByID(ctx, tx, r.offerVersionID)
		if err != nil {
			return nil, err
		}
		windowSeconds, err := cashbackWindowSeconds(ov.RewardCalculation)
		if err != nil {
			// Malformed/absent cadence config - skip this campaign, audit
			// it, never guess a default window length for real money
			// (a wrong guess here would misstate NetLossAmount's own
			// window boundary, not merely a cosmetic default).
			if auditErr := audit.Record(ctx, tx, audit.Entry{
				TenantID: tenantID, ActorType: audit.ActorSystem, Action: "bonus_cashback_scheduler.malformed_window_config_skipped",
				TargetType: "bonus_offer_version", TargetID: ov.ID.String(), Outcome: audit.OutcomeFailure,
				Metadata: map[string]any{"error": err.Error()},
			}); auditErr != nil {
				return nil, auditErr
			}
			continue
		}
		out = append(out, cashbackMatchableCampaign{
			CampaignID: r.campaignID, CampaignVersionID: r.campaignVersionID, OfferID: r.offerID, OfferVersion: ov,
			FulfillmentOwner: r.fulfillmentOwner, BrandID: r.brandID,
			WindowSeconds: windowSeconds, WindowAnchor: r.createdAt.UTC(),
		})
	}
	return out, nil
}

type cashbackRewardCalculation struct {
	percentageRewardCalculation
	WindowSeconds int64 `json:"window_seconds"`
}

func cashbackWindowSeconds(raw []byte) (int64, error) {
	var p cashbackRewardCalculation
	if err := json.Unmarshal(raw, &p); err != nil {
		return 0, fmt.Errorf("bonus: offer version's reward_calculation is not a valid cashback payload: %w", err)
	}
	if p.WindowSeconds <= 0 {
		return 0, fmt.Errorf("bonus: offer version's reward_calculation has no positive window_seconds")
	}
	return p.WindowSeconds, nil
}

// cashbackWatermark reads a (campaign, player, asset)'s own resume
// cursor (migration 0069) - nil means no window processed yet.
func cashbackWatermark(ctx context.Context, tx pgx.Tx, tenantID, campaignID, playerAccountID uuid.UUID, assetCode string) (*time.Time, error) {
	var t *time.Time
	err := tx.QueryRow(ctx,
		`SELECT last_processed_window_end FROM bonus_cashback_schedule_watermarks WHERE tenant_id = $1 AND campaign_id = $2 AND player_account_id = $3 AND asset_code = $4`,
		tenantID, campaignID, playerAccountID, assetCode,
	).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("bonus: read cashback watermark: %w", err)
	}
	return t, nil
}

func advanceCashbackWatermark(ctx context.Context, tx pgx.Tx, tenantID, campaignID, playerAccountID uuid.UUID, assetCode string, windowEnd time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO bonus_cashback_schedule_watermarks (tenant_id, campaign_id, player_account_id, asset_code, last_processed_window_end)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, campaign_id, player_account_id, asset_code) DO UPDATE
		   SET last_processed_window_end = EXCLUDED.last_processed_window_end, updated_at = clock_timestamp()`,
		tenantID, campaignID, playerAccountID, assetCode, windowEnd,
	)
	if err != nil {
		return fmt.Errorf("bonus: advance cashback watermark: %w", err)
	}
	return nil
}

// cashbackCandidatesAndNetLoss implements §7.18.4 item 4's own specified
// read shape exactly: "the net of casino_bet debits to player_cash minus
// casino_win credits to player_cash over [start,end), excluding any bet
// nullified by a casino_rollback" - grouped per player, returning only
// players with a strictly positive net loss (a net win in the window
// earns no cashback).
func cashbackCandidatesAndNetLoss(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, brandID *uuid.UUID, assetCode string, windowStart, windowEnd time.Time) (map[uuid.UUID]*big.Int, error) {
	rows, err := tx.Query(ctx, `
		SELECT w.player_account_id,
		       COALESCE(SUM(CASE WHEN lt.transaction_type = 'casino_bet' AND le.direction = 'debit' THEN le.amount ELSE 0 END), 0)
		     - COALESCE(SUM(CASE WHEN lt.transaction_type = 'casino_win' AND le.direction = 'credit' THEN le.amount ELSE 0 END), 0) AS net_loss
		  FROM ledger_entries le
		  JOIN ledger_transactions lt ON lt.id = le.ledger_transaction_id AND lt.tenant_id = le.tenant_id
		  JOIN ledger_accounts la ON la.id = le.ledger_account_id AND la.tenant_id = le.tenant_id
		  JOIN wallets w ON w.id = la.wallet_id AND w.tenant_id = la.tenant_id
		 WHERE le.tenant_id = $1
		   AND la.account_type = 'player_cash'
		   AND w.asset_code = $2
		   AND ($3::uuid IS NULL OR w.brand_id = $3)
		   AND lt.posted_at >= $4 AND lt.posted_at < $5
		   AND lt.transaction_type IN ('casino_bet', 'casino_win')
		   AND NOT EXISTS (
		       SELECT 1 FROM ledger_transactions r
		        WHERE r.tenant_id = lt.tenant_id AND r.reverses_transaction_id = lt.id AND r.transaction_type = 'casino_rollback'
		   )
		 GROUP BY w.player_account_id
		HAVING COALESCE(SUM(CASE WHEN lt.transaction_type = 'casino_bet' AND le.direction = 'debit' THEN le.amount ELSE 0 END), 0)
		     - COALESCE(SUM(CASE WHEN lt.transaction_type = 'casino_win' AND le.direction = 'credit' THEN le.amount ELSE 0 END), 0) > 0`,
		tenantID, assetCode, brandID, windowStart, windowEnd,
	)
	if err != nil {
		return nil, fmt.Errorf("bonus: compute cashback candidates/net-loss: %w", err)
	}
	defer rows.Close()

	out := map[uuid.UUID]*big.Int{}
	for rows.Next() {
		var playerAccountID uuid.UUID
		var netLoss pgtype.Numeric
		if err := rows.Scan(&playerAccountID, &netLoss); err != nil {
			return nil, fmt.Errorf("bonus: scan cashback candidate: %w", err)
		}
		amt, err := numericToBigInt(netLoss)
		if err != nil {
			return nil, err
		}
		out[playerAccountID] = amt
	}
	return out, rows.Err()
}

// CashbackSchedulerOutcome summarizes one RunCashbackSchedulerForTenant
// call.
type CashbackSchedulerOutcome struct {
	WindowsEvaluated int
	GrantsIssued     int
	GrantsDenied     int
}

// RunCashbackSchedulerForTenant is §7.18.4's own scheduling job, made
// real: for every currently-active Cashback Campaign, walks every fully-
// elapsed window (clock_timestamp() - never now(), per doc 10 §2's
// already-binding rule) since each candidate player's own watermark (or
// the campaign's own creation time, for a player never seen before), up
// to CashbackWindowsPerTick windows per tick, computing NetLossAmount
// from a live ledger read and calling IssueAndActivateCashback for every
// player with a strictly positive net loss in that window.
//
// asOf is the caller's own clock_timestamp() read (RunCashbackSweep's
// job, not this function's own - keeping this function a pure,
// deterministic query given a fixed instant, easing testing).
func RunCashbackSchedulerForTenant(ctx context.Context, tx pgx.Tx, tenantID, actorID uuid.UUID, asOf time.Time) (CashbackSchedulerOutcome, error) {
	var outcome CashbackSchedulerOutcome
	campaigns, err := listCashbackMatchableCampaigns(ctx, tx, tenantID)
	if err != nil {
		return outcome, err
	}

	for _, c := range campaigns {
		exp, err := lookupDecimalExponent(ctx, tx, c.OfferVersion.RewardAssetCode)
		if err != nil {
			return outcome, err
		}
		windowStart := c.WindowAnchor
		for i := 0; i < CashbackWindowsPerTick; i++ {
			windowEnd := windowStart.Add(time.Duration(c.WindowSeconds) * time.Second)
			if !windowEnd.Before(asOf) {
				break // this window has not fully elapsed yet
			}
			outcome.WindowsEvaluated++

			candidates, err := cashbackCandidatesAndNetLoss(ctx, tx, tenantID, c.BrandID, c.OfferVersion.RewardAssetCode, windowStart, windowEnd)
			if err != nil {
				return outcome, err
			}
			for playerAccountID, netLoss := range candidates {
				w, err := cashbackWatermark(ctx, tx, tenantID, c.CampaignID, playerAccountID, c.OfferVersion.RewardAssetCode)
				if err != nil {
					return outcome, err
				}
				// A player whose OWN watermark already covers this window
				// (set by an earlier tick/campaign iteration) is skipped -
				// this loop iterates windows GLOBALLY per campaign, so a
				// player who joined later than windowStart is naturally
				// caught up from their own first candidate window instead
				// of double-processing an already-advanced one.
				if w != nil && !w.Before(windowEnd) {
					continue
				}

				rateBP, cap, parseErr := parsePercentageRewardCalculation(c.OfferVersion.RewardCalculation)
				if parseErr != nil {
					return outcome, fmt.Errorf("bonus: cashback reward calculation (offer_version=%s): %w", c.OfferVersion.ID, parseErr)
				}
				walletID, err := resolvePlayerWallet(ctx, tx, tenantID, playerAccountID, c.OfferVersion.RewardAssetCode)
				if err != nil {
					return outcome, err
				}
				brandID, err := playerBrandID(ctx, tx, tenantID, playerAccountID)
				if err != nil {
					return outcome, err
				}
				triggerRef := fmt.Sprintf("cashback:%s:%s:%s:%s", c.CampaignID, playerAccountID, windowStart.UTC().Format(time.RFC3339), windowEnd.UTC().Format(time.RFC3339))
				g := Grant{
					TenantID: tenantID, BrandID: brandID, PlayerAccountID: playerAccountID, WalletID: walletID,
					CampaignID: c.CampaignID, CampaignVersionID: c.CampaignVersionID, OfferID: c.OfferID, OfferVersionID: c.OfferVersion.ID,
					AssetCode: c.OfferVersion.RewardAssetCode, DecimalExponent: exp,
					FundingSource: c.OfferVersion.FundingSource, FulfillmentDestination: c.OfferVersion.FulfillmentDestination,
					FulfillmentOwner: c.FulfillmentOwner, TriggerReference: triggerRef,
					EligibilitySnapshot: []byte(fmt.Sprintf(`{"window_start":%q,"window_end":%q}`, windowStart.UTC().Format(time.RFC3339), windowEnd.UTC().Format(time.RFC3339))),
					CreatedByActorType:  ActorSystem,
				}
				_, activateOutcome, err := IssueAndActivateCashback(ctx, tx, CashbackParams{
					Grant: g, NetLossAmount: netLoss, RateBP: rateBP, CapAmount: cap,
					JurisdictionCode: "", ActorType: ActorSystem, ActorID: actorID,
				})
				if err != nil {
					return outcome, fmt.Errorf("bonus: cashback scheduler issue/activate (campaign=%s, player=%s, window=%s..%s): %w",
						c.CampaignID, playerAccountID, windowStart, windowEnd, err)
				}
				if activateOutcome.Allowed {
					outcome.GrantsIssued++
				} else {
					outcome.GrantsDenied++
				}
				if err := advanceCashbackWatermark(ctx, tx, tenantID, c.CampaignID, playerAccountID, c.OfferVersion.RewardAssetCode, windowEnd); err != nil {
					return outcome, err
				}
			}
			windowStart = windowEnd
		}
	}
	return outcome, nil
}

func playerBrandID(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID uuid.UUID) (uuid.UUID, error) {
	var brandID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT brand_id FROM player_accounts WHERE tenant_id = $1 AND id = $2`, tenantID, playerAccountID).Scan(&brandID); err != nil {
		return uuid.Nil, fmt.Errorf("bonus: resolve player brand id: %w", err)
	}
	return brandID, nil
}

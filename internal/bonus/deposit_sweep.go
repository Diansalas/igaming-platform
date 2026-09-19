// Deposit/Reload event-consumption sweep (Stage 4H-B1 Wave 3 Phase 3,
// ledger-accounting-model.md §7.18.2). This is the durable, PRIMARY
// mechanism (§7.18.1's binding convention) that actually triggers
// IssueAndActivateDepositBonus in production — closing reconnaissance
// gap-list item 10/11/20 ("zero callers outside its own package... no
// deposit-event consumer exists anywhere").
//
// Reads deposit_intents directly via raw SQL, joined to the deposit's
// own ledger_transactions row, rather than importing internal/payments —
// mirroring this package's own established pattern (targeting.go reads
// player_accounts/wallets/staff_users directly rather than importing
// internal/identity/internal/wallet for one narrow field) and keeping
// BI-2 (doc 29 §8: "internal/bonus is never imported by... internal/
// payments") satisfied in BOTH directions even though it only binds one:
// this package has no need to import internal/payments at all, so it
// does not.
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

// DepositSweepConsumerName is the bonus_ledger_sweep_watermarks.
// consumer_name value this sweep uses — migration 0068's own worked
// example (§7.18.2 item 1).
const DepositSweepConsumerName = "bonus_deposit_sweep"

// DepositSweepBatchSize bounds how many not-yet-processed deposits a
// single sweep tick reads, so one tenant with a large backlog cannot
// hold its advisory lock (and this tick's transaction) open
// indefinitely - a backlog larger than this is simply picked up again on
// the next tick, from the watermark this tick DID advance to.
const DepositSweepBatchSize = 500

type depositSweepWatermark struct {
	LastProcessedLedgerTransactionID *uuid.UUID
	LastProcessedPostedAt            *time.Time
}

func getDepositSweepWatermark(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, consumerName string) (depositSweepWatermark, error) {
	var w depositSweepWatermark
	err := tx.QueryRow(ctx,
		`SELECT last_processed_ledger_transaction_id, last_processed_posted_at FROM bonus_ledger_sweep_watermarks WHERE tenant_id = $1 AND consumer_name = $2`,
		tenantID, consumerName,
	).Scan(&w.LastProcessedLedgerTransactionID, &w.LastProcessedPostedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return depositSweepWatermark{}, nil
	}
	if err != nil {
		return depositSweepWatermark{}, fmt.Errorf("bonus: read deposit sweep watermark: %w", err)
	}
	return w, nil
}

// advanceDepositSweepWatermark upserts the tenant's own cursor row
// (migration 0068's own schema - the (tenant_id, consumer_name) unique
// constraint IS the upsert target). Advancing by (posted_at, id) per
// migration 0068's own documented keyset-pagination shape (a random
// UUIDv4 primary key has no insertion-order correspondence on its own).
func advanceDepositSweepWatermark(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, consumerName string, lastTxID uuid.UUID, lastPostedAt time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO bonus_ledger_sweep_watermarks (tenant_id, consumer_name, last_processed_ledger_transaction_id, last_processed_posted_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, consumer_name) DO UPDATE
		   SET last_processed_ledger_transaction_id = EXCLUDED.last_processed_ledger_transaction_id,
		       last_processed_posted_at = EXCLUDED.last_processed_posted_at,
		       updated_at = clock_timestamp()`,
		tenantID, consumerName, lastTxID, lastPostedAt,
	)
	if err != nil {
		return fmt.Errorf("bonus: advance deposit sweep watermark: %w", err)
	}
	return nil
}

// depositSweepEvent is one not-yet-processed deposit, resolved entirely
// from already-durable platform facts (§7.18.2's own data-availability
// list) - never re-derived from a client-supplied field.
type depositSweepEvent struct {
	LedgerTransactionID uuid.UUID
	PostedAt            time.Time
	BrandID             uuid.UUID
	PlayerAccountID     uuid.UUID
	WalletID            uuid.UUID
	AssetCode           string
	Amount              *big.Int
	PaymentMethod       string
}

// listUnprocessedDeposits reads up to DepositSweepBatchSize deposit rows
// for tenantID strictly after w's own cursor, ordered by (posted_at, id)
// - migration 0068's own documented ordering fix (a random ledger_
// transactions.id has no insertion-order correspondence on its own).
func listUnprocessedDeposits(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, w depositSweepWatermark) ([]depositSweepEvent, error) {
	const baseQuery = `
		SELECT lt.id, lt.posted_at, di.brand_id, di.player_account_id, di.wallet_id, di.asset_code, di.amount, di.payment_method
		  FROM ledger_transactions lt
		  JOIN deposit_intents di ON di.ledger_transaction_id = lt.id AND di.tenant_id = lt.tenant_id
		 WHERE lt.tenant_id = $1 AND lt.transaction_type = 'deposit'`

	var rows pgx.Rows
	var err error
	if w.LastProcessedPostedAt == nil {
		rows, err = tx.Query(ctx, baseQuery+` ORDER BY lt.posted_at ASC, lt.id ASC LIMIT $2`, tenantID, DepositSweepBatchSize)
	} else {
		rows, err = tx.Query(ctx, baseQuery+`
			   AND (lt.posted_at, lt.id) > ($2, $3)
			 ORDER BY lt.posted_at ASC, lt.id ASC LIMIT $4`,
			tenantID, *w.LastProcessedPostedAt, *w.LastProcessedLedgerTransactionID, DepositSweepBatchSize)
	}
	if err != nil {
		return nil, fmt.Errorf("bonus: list unprocessed deposits: %w", err)
	}
	defer rows.Close()

	var out []depositSweepEvent
	for rows.Next() {
		var e depositSweepEvent
		var amt pgtype.Numeric
		if err := rows.Scan(&e.LedgerTransactionID, &e.PostedAt, &e.BrandID, &e.PlayerAccountID, &e.WalletID, &e.AssetCode, &amt, &e.PaymentMethod); err != nil {
			return nil, fmt.Errorf("bonus: scan unprocessed deposit: %w", err)
		}
		amount, err := numericToBigInt(amt)
		if err != nil {
			return nil, err
		}
		e.Amount = amount
		out = append(out, e)
	}
	return out, rows.Err()
}

// isFirstDeposit reports whether depositTxID is the EARLIEST 'deposit'-
// type ledger transaction for playerAccountID within tenantID - the
// FirstDepositOnly eligibility axis's own live check (OfferVersion.
// FirstDepositOnly, stored since migration 0055, confirmed by the
// reconnaissance never read by any code path before this Wave).
func isFirstDeposit(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID, depositTxID uuid.UUID) (bool, error) {
	var earlierExists bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM ledger_transactions lt
			  JOIN deposit_intents di ON di.ledger_transaction_id = lt.id AND di.tenant_id = lt.tenant_id
			 WHERE lt.tenant_id = $1 AND lt.transaction_type = 'deposit' AND di.player_account_id = $2
			   AND lt.id <> $3
			   AND (lt.posted_at, lt.id) < (SELECT posted_at, id FROM ledger_transactions WHERE id = $3 AND tenant_id = $1)
		)`,
		tenantID, playerAccountID, depositTxID,
	).Scan(&earlierExists)
	if err != nil {
		return false, fmt.Errorf("bonus: check first-deposit eligibility: %w", err)
	}
	return !earlierExists, nil
}

// depositMatchableOfferVersion is one candidate Offer/OfferVersion this
// sweep may issue a Grant against for a given deposit event - the join
// of every table matching actually needs, read once per candidate.
type depositMatchableOfferVersion struct {
	CampaignID        uuid.UUID
	CampaignVersionID uuid.UUID
	OfferID           uuid.UUID
	OfferVersion      OfferVersion
	FulfillmentOwner  string
}

// listDepositMatchableOfferVersions resolves every Active Offer, under
// an Active Campaign whose current CampaignVersion's window (if any)
// covers asOf, whose GrantPolicy is auto_issue (deposit/reload triggers
// are automatic, never opt-in-gated per doc 10 §W2.2) and whose current
// OfferVersion's own RewardKind is R1 (percentage-of-qualifying-amount-
// with-cap - the only shape a deposit/reload bonus's reward formula uses,
// doc 10 §2), scoped to tenantID/brandID (brand_id NULL on the Campaign
// meaning "every brand"). Named explicitly as bonus-engine's OWN business
// rule (§7.18.2's own text: "which Offer(s) a given deposit/reload event
// resolves against... is a Bonus-domain business rule, not specified
// here") - the deeper eligibility axes NOT evaluated by this SQL
// (deposit-method/first-deposit-only/min-max-qualifying/jurisdiction) are
// each checked afterward, per candidate, in Go (matchesDepositEligibility
// below), rather than folded into one large query, so each check's own
// reasoning stays legible and independently testable.
func listDepositMatchableOfferVersions(ctx context.Context, tx pgx.Tx, tenantID, brandID uuid.UUID, assetCode string, asOf time.Time) ([]depositMatchableOfferVersion, error) {
	rows, err := tx.Query(ctx, `
		SELECT c.id, c.current_version_id, o.id, ov.id, c.fulfillment_owner
		  FROM bonus_campaigns c
		  JOIN bonus_offers o ON o.campaign_id = c.id AND o.tenant_id = c.tenant_id
		  JOIN bonus_offer_versions ov ON ov.id = o.current_version_id AND ov.tenant_id = o.tenant_id
		  JOIN bonus_campaign_versions cv ON cv.id = c.current_version_id AND cv.tenant_id = c.tenant_id
		 WHERE c.tenant_id = $1
		   AND (c.brand_id IS NULL OR c.brand_id = $2)
		   AND c.status = 'active'
		   AND o.status = 'active'
		   AND o.grant_policy = 'auto_issue'
		   AND ov.reward_kind = 'R1'
		   AND ov.reward_asset_code = $3
		   AND (cv.window_start IS NULL OR cv.window_start <= $4)
		   AND (cv.window_end IS NULL OR cv.window_end > $4)`,
		tenantID, brandID, assetCode, asOf,
	)
	if err != nil {
		return nil, fmt.Errorf("bonus: list deposit-matchable offer versions: %w", err)
	}
	defer rows.Close()

	var out []depositMatchableOfferVersion
	var candidates []struct {
		campaignID, campaignVersionID, offerID, offerVersionID uuid.UUID
		fulfillmentOwner                                       string
	}
	for rows.Next() {
		var c struct {
			campaignID, campaignVersionID, offerID, offerVersionID uuid.UUID
			fulfillmentOwner                                       string
		}
		if err := rows.Scan(&c.campaignID, &c.campaignVersionID, &c.offerID, &c.offerVersionID, &c.fulfillmentOwner); err != nil {
			return nil, fmt.Errorf("bonus: scan deposit-matchable offer version: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, c := range candidates {
		ov, err := GetOfferVersionByID(ctx, tx, c.offerVersionID)
		if err != nil {
			return nil, err
		}
		out = append(out, depositMatchableOfferVersion{
			CampaignID: c.campaignID, CampaignVersionID: c.campaignVersionID, OfferID: c.offerID,
			OfferVersion: ov, FulfillmentOwner: c.fulfillmentOwner,
		})
	}
	return out, nil
}

// matchesDepositEligibility evaluates the per-candidate eligibility axes
// listDepositMatchableOfferVersions' own SQL does not (its own doc
// comment): deposit-method allowlist and first-deposit-only. Min/max-
// qualifying-amount are deliberately NOT re-checked here - they are
// already enforced, correctly, inside IssueAndActivateDepositBonus
// itself (types.go), so duplicating them here would be a second,
// divergence-prone copy of the identical rule.
func matchesDepositEligibility(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, event depositSweepEvent, ov OfferVersion) (bool, error) {
	if len(ov.EligibilityDepositMethods) > 0 {
		matched := false
		for _, m := range ov.EligibilityDepositMethods {
			if m == event.PaymentMethod {
				matched = true
				break
			}
		}
		if !matched {
			return false, nil
		}
	}
	if ov.FirstDepositOnly {
		first, err := isFirstDeposit(ctx, tx, tenantID, event.PlayerAccountID, event.LedgerTransactionID)
		if err != nil {
			return false, err
		}
		if !first {
			return false, nil
		}
	}
	return true, nil
}

// detectMultiAccountFirstDepositSignal implements REQ-SEP-BONUS-3's own
// Bonus-domain-only DETECTION half (security-architecture.md §W15.1.5:
// "bonus-engine: own the household/linked-account detection path... refuse
// any request to make it a block") for the exact case reconnaissance
// §3.3 named as still open against Wave 3's own first-deposit-only sweep:
// a single Person holding more than one player_account_id, each account's
// OWN first deposit independently qualifying it for a FirstDepositOnly
// Offer that is meant to be granted once per Person, not once per
// account.
//
// This is a SIGNAL ONLY. It never denies, defers, or alters the Grant
// this sweep is about to attempt - it records an audit entry naming the
// Person and every OTHER player_account_id of theirs that already holds a
// bonus_grants row against the SAME Offer, for a human reviewer to act on
// (or not - a shared household/joint device is a plausible, non-abusive
// explanation §W15.1.5 explicitly warns against auto-blocking on). The
// query reuses the identical person_id primitive targeting.go's own
// playerPersonID/SEP-1 compensating check already uses - no new
// cross-domain read, no new column, no new table.
//
// Device/payment-fingerprint correlation (OfferVersion.
// DeviceFingerprintLinkingConfig) remains explicitly OUT of this signal's
// scope (reconnaissance §3.3's own boundary) - it requires a fraud/device-
// signal data source this platform does not have.
func detectMultiAccountFirstDepositSignal(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, event depositSweepEvent, offerID uuid.UUID) error {
	personID, err := playerPersonID(ctx, tx, event.PlayerAccountID)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT g.player_account_id
		  FROM bonus_grants g
		  JOIN player_accounts pa ON pa.id = g.player_account_id AND pa.tenant_id = g.tenant_id
		 WHERE g.tenant_id = $1 AND pa.person_id = $2 AND g.offer_id = $3 AND g.player_account_id <> $4`,
		tenantID, personID, offerID, event.PlayerAccountID,
	)
	if err != nil {
		return fmt.Errorf("bonus: multi-account first-deposit signal query: %w", err)
	}
	defer rows.Close()
	var otherAccounts []string
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("bonus: scan multi-account first-deposit signal row: %w", err)
		}
		otherAccounts = append(otherAccounts, id.String())
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("bonus: multi-account first-deposit signal rows: %w", err)
	}
	if len(otherAccounts) == 0 {
		return nil
	}
	return audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorSystem, Action: "bonus_deposit_sweep.multi_account_signal_detected",
		TargetType: "person", TargetID: personID.String(), Outcome: audit.OutcomeSuccess,
		Metadata: map[string]any{
			"person_id":                     personID.String(),
			"offer_id":                      offerID.String(),
			"triggering_player_account_id":  event.PlayerAccountID.String(),
			"other_player_account_ids":      otherAccounts,
			"deposit_ledger_transaction_id": event.LedgerTransactionID.String(),
			"signal_only":                   true,
		},
	})
}

// percentageRewardCalculation is the JSON shape this sweep expects an R1
// OfferVersion's own RewardCalculation blob to carry - mirroring
// internal/httpserver's own computeCouponRewardAmount precedent
// (decimal-string amounts, never a JSON number, CLAUDE.md's no-float
// rule) for the fixed-value (R2) case, extended here for R1's own
// rate_bp/cap_amount pair. A candidate whose RewardCalculation does not
// parse this way is skipped (not an error - a malformed Offer must not
// take down the whole sweep tick for every OTHER tenant/deposit), and
// recorded via audit so it is visible to an operator, not silently
// dropped.
type percentageRewardCalculation struct {
	RateBP    int32   `json:"rate_bp"`
	CapAmount *string `json:"cap_amount,omitempty"`
}

func parsePercentageRewardCalculation(raw []byte) (rateBP int32, cap *big.Int, err error) {
	var p percentageRewardCalculation
	if err := json.Unmarshal(raw, &p); err != nil {
		return 0, nil, fmt.Errorf("bonus: offer version's reward_calculation is not a valid R1 payload: %w", err)
	}
	if p.RateBP <= 0 {
		return 0, nil, fmt.Errorf("bonus: offer version's reward_calculation has no positive rate_bp")
	}
	if p.CapAmount != nil && *p.CapAmount != "" {
		c, ok := new(big.Int).SetString(*p.CapAmount, 10)
		if !ok {
			return 0, nil, fmt.Errorf("bonus: offer version's reward_calculation cap_amount is not a valid integer")
		}
		cap = c
	}
	return p.RateBP, cap, nil
}

// lookupDecimalExponent mirrors internal/httpserver's own identically-
// named helper (bonus_handlers.go) - this package needs the identical
// read (a deposit/reload Grant's own DecimalExponent, T.2's immutable
// snapshot field) from its own sweep job, which has no HTTP request to
// borrow the helper from.
func lookupDecimalExponent(ctx context.Context, tx pgx.Tx, assetCode string) (int32, error) {
	var exp int32
	if err := tx.QueryRow(ctx, `SELECT decimal_exponent FROM assets WHERE code = $1`, assetCode).Scan(&exp); err != nil {
		return 0, fmt.Errorf("bonus: resolve decimal exponent for %s: %w", assetCode, err)
	}
	return exp, nil
}

// DepositSweepOutcome summarizes one RunDepositSweepForTenant call, for
// the scheduler loop's own logging/audit trail.
type DepositSweepOutcome struct {
	EventsProcessed int
	GrantsIssued    int
	GrantsDenied    int
	Skipped         []string // human-readable reasons a candidate was skipped (malformed reward_calculation, etc.) - diagnostic only
}

// RunDepositSweepForTenant is §7.18.2 item 1's own durable, primary
// mechanism, made real: scans every not-yet-processed deposit for
// tenantID (since the tenant's own watermark), resolves every currently-
// matchable deposit/reload Offer against it, and calls
// IssueAndActivateDepositBonus for each match - idempotent by
// construction (a re-scan of an already-processed deposit resolves to
// the existing Grant via ErrAlreadyGranted inside IssueAndActivateDepositBonus
// itself, never a duplicate), advancing the watermark once each event has
// been evaluated against every candidate Offer.
//
// actorID is the system principal recorded as this Grant's activating
// actor (ActivateGrantParams.ActorType is normalized to ActorSystem by
// TriggerActorForAutomated regardless).
//
// NAMED, DISCLOSED LIMITATION (not silently narrowed): JurisdictionCode
// is passed as "" - this platform has no per-player jurisdiction
// resolver anywhere yet (the same pre-existing "TODO(jurisdiction)" gap
// internal/casino's own postBet/LaunchGame already carry, confirmed by
// grep: no jurisdiction_code column exists on player_accounts or
// deposit_intents). An empty JurisdictionCode resolves to uuid.Nil via
// resolveJurisdictionID, which AssetAuthorization.CheckEligibility
// treats as an immediate, correct denial (ADR 0037 §C.2) - so today,
// every deposit-bonus Grant this sweep attempts will be denied at the
// AssetAuthorization gate until the platform-wide jurisdiction-resolution
// gap is closed by whatever specialist eventually owns it. This is NOT a
// new gap this dispatch introduces; it is the same one already disclosed
// against casino's real-money bet path, now inherited honestly rather
// than worked around with an invented per-deposit jurisdiction value.
func RunDepositSweepForTenant(ctx context.Context, tx pgx.Tx, tenantID, actorID uuid.UUID) (DepositSweepOutcome, error) {
	var outcome DepositSweepOutcome
	w, err := getDepositSweepWatermark(ctx, tx, tenantID, DepositSweepConsumerName)
	if err != nil {
		return outcome, err
	}

	events, err := listUnprocessedDeposits(ctx, tx, tenantID, w)
	if err != nil {
		return outcome, err
	}

	for _, event := range events {
		outcome.EventsProcessed++
		candidates, err := listDepositMatchableOfferVersions(ctx, tx, tenantID, event.BrandID, event.AssetCode, event.PostedAt)
		if err != nil {
			return outcome, err
		}
		for _, c := range candidates {
			matched, err := matchesDepositEligibility(ctx, tx, tenantID, event, c.OfferVersion)
			if err != nil {
				return outcome, err
			}
			if !matched {
				continue
			}
			if c.OfferVersion.FirstDepositOnly {
				if err := detectMultiAccountFirstDepositSignal(ctx, tx, tenantID, event, c.OfferID); err != nil {
					return outcome, err
				}
			}
			rateBP, cap, err := parsePercentageRewardCalculation(c.OfferVersion.RewardCalculation)
			if err != nil {
				outcome.Skipped = append(outcome.Skipped, fmt.Sprintf("offer_version=%s: %v", c.OfferVersion.ID, err))
				if auditErr := audit.Record(ctx, tx, audit.Entry{
					TenantID: tenantID, ActorType: audit.ActorSystem, Action: "bonus_deposit_sweep.malformed_reward_calculation_skipped",
					TargetType: "bonus_offer_version", TargetID: c.OfferVersion.ID.String(), Outcome: audit.OutcomeFailure,
					Metadata: map[string]any{"deposit_ledger_transaction_id": event.LedgerTransactionID.String(), "error": err.Error()},
				}); auditErr != nil {
					return outcome, auditErr
				}
				continue
			}
			exp, err := lookupDecimalExponent(ctx, tx, c.OfferVersion.RewardAssetCode)
			if err != nil {
				return outcome, err
			}
			g := Grant{
				TenantID: tenantID, BrandID: event.BrandID, PlayerAccountID: event.PlayerAccountID, WalletID: event.WalletID,
				CampaignID: c.CampaignID, CampaignVersionID: c.CampaignVersionID, OfferID: c.OfferID, OfferVersionID: c.OfferVersion.ID,
				AssetCode: c.OfferVersion.RewardAssetCode, DecimalExponent: exp,
				FundingSource: c.OfferVersion.FundingSource, FulfillmentDestination: c.OfferVersion.FulfillmentDestination,
				FulfillmentOwner: c.FulfillmentOwner, TriggerReference: event.LedgerTransactionID.String(),
				EligibilitySnapshot: []byte(fmt.Sprintf(`{"deposit_ledger_transaction_id":%q,"payment_method":%q}`, event.LedgerTransactionID, event.PaymentMethod)),
				CreatedByActorType:  ActorSystem,
			}
			_, activateOutcome, err := IssueAndActivateDepositBonus(ctx, tx, DepositBonusParams{
				Grant: g, DepositAmount: event.Amount, RateBP: rateBP, CapAmount: cap,
				MinQualifying: c.OfferVersion.MinQualifyingAmount, MaxQualifying: c.OfferVersion.MaxQualifyingAmount,
				JurisdictionCode: "", ActorType: ActorSystem, ActorID: actorID,
				WageringTimeLimit: c.OfferVersion.WageringTimeLimit,
			})
			if err != nil {
				return outcome, fmt.Errorf("bonus: deposit sweep issue/activate (offer_version=%s, deposit_tx=%s): %w", c.OfferVersion.ID, event.LedgerTransactionID, err)
			}
			if activateOutcome.Allowed {
				outcome.GrantsIssued++
			} else {
				outcome.GrantsDenied++
			}
		}
		if err := advanceDepositSweepWatermark(ctx, tx, tenantID, DepositSweepConsumerName, event.LedgerTransactionID, event.PostedAt); err != nil {
			return outcome, err
		}
	}
	return outcome, nil
}

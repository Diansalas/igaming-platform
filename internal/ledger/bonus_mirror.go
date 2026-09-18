// Rule B2 (extended) mirror generator - the piece that makes invariant B1
// (ledger-accounting-model.md §6.1) hold BY CONSTRUCTION rather than by
// every caller remembering to add two legs (ADR 0032 §2's own
// RECOMMENDATION, which this file implements rather than relitigates).
//
// WHY THIS FILE, AND WHY invoked unconditionally from inside Post
// (ledger-accounting-model.md §7.4.1):
//
//  1. The invariant is owned by this package, not by internal/casino,
//     internal/sportsbook or the future internal/bonus - "enforcement
//     belongs where the invariant is owned, not in each caller's
//     discipline" (ADR 0032 §2). A generator anywhere else would be
//     exactly the per-caller-discipline variant that ADR rejects.
//  2. Post is already the single choke point every bonus-touching
//     posting goes through (this package's own doc comment: "No other
//     package writes to ledger_accounts, ledger_transactions, or
//     ledger_entries directly"). Generating here covers callers that do
//     not exist yet, written by specialists who have never read ADR 0032.
//  3. It needs the resolved account_type of every entry, which is a
//     ledger_accounts read this package already had to perform for HR-9
//     (now removed - see below). Resolving it anywhere else either
//     duplicates that read or trusts a caller's claim about which account
//     type an id refers to, which is the shape of the bug HR-15 exists to
//     prevent.
//
// HR-9'S REMOVAL, RECORDED HERE (ledger-accounting-model.md §7.4.4): HR-9
// required BOTH the bonus_expense account type (migration 0050) AND this
// generator to exist before any BONUS_SET posting could be accepted. Both
// now exist, in this same Stage 4H-B1 Wave 2 dispatch, so the guard
// (assertNoBonusSetEntries, ErrBonusPostingBlocked,
// bonusPostingPreconditions) is REMOVED - not commented out, not left as
// dead code, not feature-flagged - per §7.4.4's explicit instruction that
// the removal be total. bonusSetAccountTypes itself SURVIVES and changes
// role, from "the set no entry may touch" to "the set step 1 nets over" -
// unchanged in shape (still a function, not a package-level var, for
// security finding S-3's reason: a var of slice type could be reassigned
// or truncated by any code in this package, including a test, silently
// disabling the netting with no compile error).
package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BonusFunding names who bears the cost of bonus value that leaves
// BONUS_SET (ADR 0032 §6). Fixed on the Grant at grant time and immutable
// thereafter; the ledger is told, never asked to infer it.
type BonusFunding string

const (
	FundingOperator BonusFunding = "operator" // recognition -> bonus_expense
	FundingProvider BonusFunding = "provider" // recognition -> provider_payable
)

// BonusCostAttribution is required on, and only on, a posting that
// touches a BONUS_SET account (ledger-accounting-model.md §7.4.3).
type BonusCostAttribution struct {
	Funding BonusFunding
	// ProviderID is required iff Funding == FundingProvider. ADR 0032
	// §6(b): "a provider-funded attribution that the platform cannot tie
	// to a specific provider agreement is rejected, not defaulted".
	ProviderID *string
}

// Errors the Rule B2 (extended) generator returns. Each is a distinct,
// non-retryable rejection - retrying with the same input cannot change
// any of these outcomes - so a caller must fix its call rather than
// re-attempt.
var (
	// ErrBonusCostRequired: the posting touches a BONUS_SET account but
	// supplied no BonusCost. There is no default - ledger-accounting-
	// model.md §7.4.3: "a forgotten field silently books a provider's
	// marketing spend into the tenant's own P&L, and nothing downstream
	// would ever flag it."
	ErrBonusCostRequired = errors.New("ledger: BonusCost is required for a posting touching a BONUS_SET account")
	// ErrBonusCostNotAllowed: the posting touches no BONUS_SET account but
	// supplied a BonusCost anyway - silently ignoring it is how a
	// provider-funded attribution gets lost, so it is rejected instead.
	ErrBonusCostNotAllowed = errors.New("ledger: BonusCost must be nil for a posting that touches no BONUS_SET account")
	// ErrBonusFundingInvalid: BonusCost.Funding is neither FundingOperator
	// nor FundingProvider. No default arm - an unrecognized value is a
	// loud error, never a silent selection of the cheaper-looking account.
	ErrBonusFundingInvalid = errors.New("ledger: BonusCost.Funding is not a recognized value")
	// ErrBonusProviderIDRequired: Funding == FundingProvider but
	// ProviderID is nil or empty.
	ErrBonusProviderIDRequired = errors.New("ledger: BonusCost.ProviderID is required when Funding is provider")
	// ErrBonusMirrorLegSupplied is HR-17: a caller hand-assembled a
	// promo_liability/bonus_expense/provider_payable leg alongside a
	// BONUS_SET entry. There is no exemption, including for a reversal -
	// see checkReversalFundingMatches's doc comment.
	ErrBonusMirrorLegSupplied = errors.New("ledger: a caller may not hand-assemble a bonus mirror or recognition leg (HR-17)")
	// ErrBonusFundingMismatch: a reversal's BonusCost.Funding disagrees
	// with the funding the ORIGINAL transaction actually recognized
	// (read from its posted entries, never from a caller's claim about
	// it) - ledger-accounting-model.md §7.4.3's last fail-closed rule.
	ErrBonusFundingMismatch = errors.New("ledger: reversal BonusCost.Funding does not match the original transaction's recognized funding")
	// ErrBonusMirrorImbalance is step 4's assertion (§7.4.2): the
	// generated result did not balance. Believed unreachable given
	// correct arithmetic above it; kept as a hard stop rather than a
	// silent partial post if it is ever reached.
	ErrBonusMirrorImbalance = errors.New("ledger: bonus mirror generator could not balance the transaction")
)

// bonusSetAccountTypes is BONUS_SET (ledger-accounting-model.md §6.1's
// extension note, §7.7.2.3): the account types the Rule B2 (extended)
// generator nets step 1's movement over. A function, not a package-level
// var, deliberately (security finding S-3): a var of slice type can be
// reassigned or truncated by any code in this package - including a test
// - which would silently disable the netting with no compile error.
// Returning a fresh slice per call makes the set immutable by
// construction; callers may mutate only their own copy.
func bonusSetAccountTypes() []string {
	return []string{
		string(AccountPlayerBonus),
		string(AccountPlayerLockedBonus),
		string(AccountPlayerBonusHeld),
	}
}

// isBonusSetType reports whether t is a BONUS_SET member.
func isBonusSetType(t AccountType) bool {
	for _, s := range bonusSetAccountTypes() {
		if string(t) == s {
			return true
		}
	}
	return false
}

// isMirrorOrRecognitionType reports whether t is one of the three
// accounts ONLY the generator may ever post to (HR-17): the mirror leg's
// destination (promo_liability) or either possible recognition-leg
// destination (bonus_expense, provider_payable).
func isMirrorOrRecognitionType(t AccountType) bool {
	return t == AccountPromoLiability || t == AccountBonusExpense || t == AccountProviderPayable
}

// resolvedAccount is what applyBonusMirror needs to know about each
// entry's ledger_accounts row, read from the database rather than
// trusted from the caller - the same premise HR-15's immutability gate
// protects: an account's identity is only trustworthy read from
// ledger_accounts itself.
type resolvedAccount struct {
	AccountType AccountType
	AssetCode   string
}

// resolveEntryAccounts reads account_type/asset_code for every distinct
// ledger_account_id referenced by entries, in one query. Returns an error
// naming any id that failed to resolve, rather than silently treating an
// unresolvable account as "not BONUS_SET" - the same fail-closed posture
// HR-15 requires of every account-type-keyed decision.
func resolveEntryAccounts(ctx context.Context, tx pgx.Tx, entries []EntryInput) (map[uuid.UUID]resolvedAccount, error) {
	seen := make(map[uuid.UUID]bool, len(entries))
	ids := make([]uuid.UUID, 0, len(entries))
	for _, e := range entries {
		if !seen[e.LedgerAccountID] {
			seen[e.LedgerAccountID] = true
			ids = append(ids, e.LedgerAccountID)
		}
	}

	rows, err := tx.Query(ctx,
		`SELECT id, account_type, asset_code FROM ledger_accounts WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("ledger: resolve entry accounts: %w", err)
	}
	defer rows.Close()

	resolved := make(map[uuid.UUID]resolvedAccount, len(ids))
	for rows.Next() {
		var id uuid.UUID
		var ra resolvedAccount
		if err := rows.Scan(&id, &ra.AccountType, &ra.AssetCode); err != nil {
			return nil, fmt.Errorf("ledger: scan entry account: %w", err)
		}
		resolved[id] = ra
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: read entry accounts: %w", err)
	}

	for _, id := range ids {
		if _, ok := resolved[id]; !ok {
			return nil, fmt.Errorf("%w: ledger account %s does not exist", ErrInvalidEntry, id)
		}
	}
	return resolved, nil
}

// applyBonusMirror is Rule B2 (extended)'s mirror generator
// (ledger-accounting-model.md §7.4), invoked unconditionally from inside
// Post. It never mutates in.Entries; it returns the ADDITIONAL entries to
// append, or an error (with no entries) if any validation fails or the
// generated result cannot be made to balance. On error, Post writes
// nothing - no ledger_transactions row, no ledger_entries row.
//
// Runs in four steps per asset, exactly as specified (§7.4.2), for every
// asset that has at least one caller-supplied entry resolving to a
// BONUS_SET account; an asset with no such entry is left entirely alone
// (its balance is whatever the caller provides, checked by the ordinary
// deferred trigger, migration 0022):
//
//  1. net(a) = sum of credits to BONUS_SET in asset a, minus sum of debits
//     to BONUS_SET in asset a, over the CALLER's entries only.
//  2. if net(a) != 0, a mirror leg against promo_liability, opposite sign
//     to net(a), magnitude |net(a)|.
//  3. residual r(a) = sum of credits minus sum of debits, over ALL of the
//     caller's entries in asset a PLUS step 2's leg (if any). If r(a) !=
//     0, a recognition leg against the cost account (bonus_expense or
//     provider_payable, per BonusCost.Funding), opposite sign to r(a),
//     magnitude |r(a)|.
//  4. assert: the asset now balances (r(a) is zero after step 3's leg is
//     added, which is true by construction unless the arithmetic above is
//     wrong - reasserted anyway, defensively, per §7.4.2's own "return an
//     error and post nothing" instruction).
//
// No transaction_type switch appears anywhere in this function, and none
// may ever be added (ADR 0032 §2: "Rule B2 admits no exception by
// transaction type") - every posting shape ledger-accounting-model.md
// §7.4.2's worked table describes falls out of these four steps with no
// per-type branch, which is the evidence the rule is implemented
// correctly rather than merely for the cases anticipated today.
func applyBonusMirror(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, in TransactionInput) ([]EntryInput, error) {
	if len(in.Entries) == 0 {
		if in.BonusCost != nil {
			return nil, fmt.Errorf("%w: no entries to attribute it to", ErrBonusCostNotAllowed)
		}
		return nil, nil
	}

	resolved, err := resolveEntryAccounts(ctx, tx, in.Entries)
	if err != nil {
		return nil, err
	}

	touchesBonusSet := false
	for _, e := range in.Entries {
		if isBonusSetType(resolved[e.LedgerAccountID].AccountType) {
			touchesBonusSet = true
			break
		}
	}

	if !touchesBonusSet {
		if in.BonusCost != nil {
			return nil, ErrBonusCostNotAllowed
		}
		return nil, nil
	}

	// Fail-closed validation, in order (ledger-accounting-model.md
	// §7.4.3), before anything else - including before any house account
	// is minted below.
	if in.BonusCost == nil {
		return nil, ErrBonusCostRequired
	}
	switch in.BonusCost.Funding {
	case FundingOperator:
		// ProviderID is not required when operator-funded; §7.4.3 does
		// not forbid one being set, only requires one when provider-
		// funded, so none of the enumerated fail-closed rules rejects
		// this combination.
	case FundingProvider:
		if in.BonusCost.ProviderID == nil || *in.BonusCost.ProviderID == "" {
			return nil, ErrBonusProviderIDRequired
		}
	default:
		return nil, fmt.Errorf("%w: %q", ErrBonusFundingInvalid, in.BonusCost.Funding)
	}

	// HR-17: no caller-supplied entry may resolve to a mirror or
	// recognition account. No exemption for a reversal (§7.4.2's own
	// reasoning: an exemption keyed on ReversesTransactionID != nil would
	// make correctness depend on the reversal caller remembering to
	// include mirror legs, and a caller that forgot would post a
	// balanced-but-unmirrored transaction only the hourly B1 sweep would
	// ever catch).
	for _, e := range in.Entries {
		if isMirrorOrRecognitionType(resolved[e.LedgerAccountID].AccountType) {
			return nil, fmt.Errorf("%w: entry against %s", ErrBonusMirrorLegSupplied, resolved[e.LedgerAccountID].AccountType)
		}
	}

	// A reversal must carry the same BonusCost as the transaction it
	// reverses (§7.4.3's last fail-closed rule), checked against the
	// ORIGINAL's own posted entries.
	if in.ReversesTransactionID != nil {
		if err := checkReversalFundingMatches(ctx, tx, *in.ReversesTransactionID, in.BonusCost.Funding); err != nil {
			return nil, err
		}
	}

	// Group the caller's entries per asset, and mark which assets have at
	// least one BONUS_SET-touching entry - the generator only acts on
	// those (§7.4.2's own worked-table property: a direct cash reward,
	// which touches bonus_expense/player_cash only and NO BONUS_SET
	// account, is left completely untouched).
	byAsset := map[string][]EntryInput{}
	assetTouchesBonusSet := map[string]bool{}
	for _, e := range in.Entries {
		ra := resolved[e.LedgerAccountID]
		byAsset[ra.AssetCode] = append(byAsset[ra.AssetCode], e)
		if isBonusSetType(ra.AccountType) {
			assetTouchesBonusSet[ra.AssetCode] = true
		}
	}

	var assets []string
	for asset, touches := range assetTouchesBonusSet {
		if touches {
			assets = append(assets, asset)
		}
	}
	// Fixed ordering (§7.4.2): assets in sorted order, so a given logical
	// posting always produces byte-identical entry rows.
	sort.Strings(assets)

	costAccountType := AccountBonusExpense
	if in.BonusCost.Funding == FundingProvider {
		costAccountType = AccountProviderPayable
	}

	var stepTwo, stepThree []EntryInput
	for _, asset := range assets {
		signedEntries := resolveDirectionSigns(byAsset[asset], resolved, isBonusSetType)
		var net int64
		for _, s := range signedEntries {
			if s.bonusSet {
				net += s.signed
			}
		}

		var mirror *EntryInput
		if net != 0 {
			promoID, err := GetOrCreateAccount(ctx, tx, tenantID, nil, AccountPromoLiability, asset)
			if err != nil {
				return nil, fmt.Errorf("ledger: bonus mirror: get or create promo_liability account: %w", err)
			}
			leg := mirrorLeg(promoID, net)
			mirror = &leg
			stepTwo = append(stepTwo, leg)
		}

		residual := int64(0)
		for _, s := range signedEntries {
			residual += s.signed
		}
		if mirror != nil {
			residual += signedAmount(*mirror)
		}
		if residual == 0 {
			continue
		}

		costID, err := GetOrCreateAccount(ctx, tx, tenantID, nil, costAccountType, asset)
		if err != nil {
			return nil, fmt.Errorf("ledger: bonus mirror: get or create %s account: %w", costAccountType, err)
		}
		recognition := mirrorLeg(costID, residual)
		stepThree = append(stepThree, recognition)

		// Step 4, per-asset: the residual must now be exactly zero. True
		// by construction (recognition is built as the exact inverse of
		// residual); asserted anyway, per §7.4.2's own instruction, and
		// aborts the WHOLE call (no entries returned) rather than posting
		// a partial fix for this asset alone.
		if residual+signedAmount(recognition) != 0 {
			return nil, fmt.Errorf("%w: asset %s residual %d after recognition leg", ErrBonusMirrorImbalance, asset, residual)
		}
	}

	generated := make([]EntryInput, 0, len(stepTwo)+len(stepThree))
	generated = append(generated, stepTwo...)
	generated = append(generated, stepThree...)
	return generated, nil
}

// signedEntry pairs a caller entry with its signed amount (credit
// positive, debit negative - ledger-accounting-model.md §5's uniform
// convention) and whether it resolves to a BONUS_SET account.
type signedEntry struct {
	signed   int64
	bonusSet bool
}

func resolveDirectionSigns(in []EntryInput, resolved map[uuid.UUID]resolvedAccount, isBonusSet func(AccountType) bool) []signedEntry {
	out := make([]signedEntry, 0, len(in))
	for _, e := range in {
		out = append(out, signedEntry{signed: signedAmount(e), bonusSet: isBonusSet(resolved[e.LedgerAccountID].AccountType)})
	}
	return out
}

// signedAmount returns e's contribution to a credit-positive sum: +Amount
// for a credit, -Amount for a debit.
func signedAmount(e EntryInput) int64 {
	if e.Direction == Credit {
		return e.Amount
	}
	return -e.Amount
}

// mirrorLeg builds the single entry that brings a signed movement of
// magnitude|net| to zero against accountID: a net > 0 (a net CREDIT to
// the set being mirrored/recognized) is offset by a DEBIT of the same
// magnitude, and vice versa - this is what makes step 2's promo_liability
// leg and step 3's recognition leg the same shape of computation applied
// to two different quantities (net and residual).
func mirrorLeg(accountID uuid.UUID, net int64) EntryInput {
	if net > 0 {
		return EntryInput{LedgerAccountID: accountID, Direction: Debit, Amount: net}
	}
	return EntryInput{LedgerAccountID: accountID, Direction: Credit, Amount: -net}
}

// checkReversalFundingMatches implements §7.4.3's last fail-closed rule:
// a reversal must carry the same BonusCost as the transaction it
// reverses, checked against the ORIGINAL's own posted entries - never
// against a caller's claim about the original, for the same reason every
// other account-type-keyed decision in this package reads
// ledger_accounts rather than trusting a caller.
//
// If the original recognized NO cost at all - a Grant or an ordinary
// forfeiture, both of which are r(a) == 0 in §7.4.2's own worked table -
// there is nothing in its entries to compare against, and this is a
// deliberate no-op: BonusCost is still required for the reversal itself
// (the generic fail-closed check above already enforces that), but its
// Funding cannot be validated against an original that never recognized
// one.
func checkReversalFundingMatches(ctx context.Context, tx pgx.Tx, originalID uuid.UUID, funding BonusFunding) error {
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT la.account_type FROM ledger_entries le
		 JOIN ledger_accounts la ON la.id = le.ledger_account_id
		 WHERE le.ledger_transaction_id = $1 AND la.account_type = ANY($2)`,
		originalID, []string{string(AccountBonusExpense), string(AccountProviderPayable)},
	)
	if err != nil {
		return fmt.Errorf("ledger: resolve original transaction's recognized funding: %w", err)
	}
	defer rows.Close()

	var sawOperator, sawProvider bool
	for rows.Next() {
		var accountType string
		if err := rows.Scan(&accountType); err != nil {
			return fmt.Errorf("ledger: scan original transaction's account type: %w", err)
		}
		switch AccountType(accountType) {
		case AccountBonusExpense:
			sawOperator = true
		case AccountProviderPayable:
			sawProvider = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("ledger: read original transaction's recognized funding: %w", err)
	}

	if !sawOperator && !sawProvider {
		return nil
	}
	if sawOperator && funding != FundingOperator {
		return fmt.Errorf("%w: original transaction %s recognized operator-funded (bonus_expense), reversal supplied %q",
			ErrBonusFundingMismatch, originalID, funding)
	}
	if sawProvider && funding != FundingProvider {
		return fmt.Errorf("%w: original transaction %s recognized provider-funded (provider_payable), reversal supplied %q",
			ErrBonusFundingMismatch, originalID, funding)
	}
	return nil
}

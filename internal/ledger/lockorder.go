// Canonical financial lock ordering (docs/decisions/0082) - class L3.
//
// WHY THIS FILE EXISTS. Three distinct mechanisms acquire a row lock on
// wallet_balance_projection in this codebase, and only one of them is
// visible as a SELECT ... FOR UPDATE in the Go source:
//
//  1. an explicit SELECT ... FOR UPDATE (the three lockCashBalance*
//     helpers ADR 0082 §4 deletes);
//  2. migration 0023's AFTER INSERT trigger on ledger_entries, whose
//     INSERT ... ON CONFLICT (ledger_account_id) DO UPDATE takes a
//     row-level EXCLUSIVE lock - so the ORDER of the EntryInput slice
//     handed to Post IS a lock-acquisition order, one lock per entry,
//     invisible at every call site;
//  3. unique-index insertion waits (ledger_accounts, ledger_transactions'
//     idempotency key, and the trigger's own insert of a NEW projection
//     row), which participate in deadlock cycles exactly like row locks.
//
// Finding LOCK-1 (and LOCK-1b/1c/1d) were ABBA cycles across mechanisms
// 1 and 2: casino.postBet locked player_cash explicitly and then
// house_gaming implicitly, while casino.postWinDirectCash locked
// house_gaming then player_cash - same two rows, opposite order. Sorting
// the entry slice inside Post was investigated and PROVEN INSUFFICIENT
// (ADR 0082 §1.4): a sort inside Post cannot establish a total order over
// a lock postBet already held before Post was ever entered.
//
// The fix is this file: ONE pre-lock step, over the COMPLETE final
// account set (including the Rule B2 mirror/recognition legs a caller may
// not even construct - HR-17), in one canonical order (ascending
// ledger_accounts.id), owned by this package and reachable from nowhere
// else (ADR 0082 R4). A caller that needs to read a balance before
// deciding whether to post reads it from LockProjectionsForPosting's
// result; it never issues a projection FOR UPDATE of its own.
//
// This changes NO financial invariant. Nothing about what gets posted
// changes, and entry insertion order into ledger_entries is deliberately
// left exactly as it was (ADR 0082 R7, preserving §7.4.2's byte-identical
// entry rows property) - only the sequence in which rows are LOCKED
// changes. The defect being fixed is liveness (a 40P01 deadlock abort),
// not correctness: the ledger's invariants already held through a
// deadlock abort, because the whole transaction rolls back.
package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrAccountNotLocked is returned by LockedProjections.Balance for an
// account that was not part of the locked set - never a zero Balance
// (same fail-closed posture as wallet.GetSummary's erroring default arm).
// A caller asking for the balance of an account this posting does not
// touch has a bug in its TransactionInput, and must be told so loudly
// rather than shown a zero it would then compare against a stake.
var ErrAccountNotLocked = errors.New("ledger: ledger account was not part of the locked projection set")

// LockedProjections is the result of LockProjectionsForPosting: the
// complete set of wallet_balance_projection rows a posting will touch,
// all locked FOR UPDATE in the canonical order (ADR 0082 §2.1 class L3),
// with the balances read under those locks.
type LockedProjections struct {
	// AccountIDs is the set actually locked, in canonical (ascending)
	// order. Exported for tests and observability only - no production
	// caller should need it, and none may use it to re-derive a lock of
	// its own (R4).
	AccountIDs []uuid.UUID
	balances   map[uuid.UUID]Balance
}

// Balance returns ledgerAccountID's balance as read under the lock this
// LockedProjections holds.
//
// Balance.Found reports whether the projection row already existed when
// this call ran: false means the row was materialised with zero totals by
// the ensure step (ADR 0082 R6), true means a row was already there.
// Note that "already there" does not imply non-zero history - an earlier
// posting in this same transaction, or an earlier pre-lock whose posting
// was legitimately declined, can leave a present row whose totals are
// zero, exactly the case GetProjectedBalance's own Found field
// distinguishes. Either way the totals are authoritative under the lock.
func (l LockedProjections) Balance(ledgerAccountID uuid.UUID) (Balance, error) {
	b, ok := l.balances[ledgerAccountID]
	if !ok {
		return Balance{}, fmt.Errorf("%w: %s", ErrAccountNotLocked, ledgerAccountID)
	}
	return b, nil
}

// LockProjectionsForPosting acquires, in the canonical order (ADR 0082
// §2.1 class L3: ascending ledger_accounts.id), every
// wallet_balance_projection row the posting described by `in` will touch
// - INCLUDING the Rule B2 (extended) mirror and recognition legs Post
// itself generates, which a caller may not know about and may not
// construct (HR-17). Idempotent: calling it and then calling Post with
// the same TransactionInput re-acquires locks this transaction already
// holds, which is a no-op.
//
// Every call site that must read a balance before deciding whether to
// post (an insufficient-funds check) calls this FIRST and reads the
// balance from the result - never its own SELECT ... FOR UPDATE, which
// ADR 0082 R4 forbids outside this package.
//
// R3, restated as a warning to future callers: a caller may not pre-lock
// a SUBSET of the accounts it will touch. A subset is only safe if it is
// a prefix of the ascending order, which no caller can know. Passing the
// same TransactionInput here and to Post is the only correct usage -
// never a trimmed-down one built just for the balance read.
func LockProjectionsForPosting(ctx context.Context, tx pgx.Tx, in TransactionInput) (LockedProjections, error) {
	entries, err := prepareEntries(ctx, tx, in)
	if err != nil {
		return LockedProjections{}, err
	}
	ordered := canonicalAccountOrder(entryAccountIDs(entries))
	balances, err := ensureAndLockProjectionsInOrder(ctx, tx, in.TenantID, ordered)
	if err != nil {
		return LockedProjections{}, err
	}
	return LockedProjections{AccountIDs: ordered, balances: balances}, nil
}

// ErrPreLockTenantMismatch is returned by LockProjectionsForPostings when
// the inputs do not all belong to one tenant.
var ErrPreLockTenantMismatch = errors.New("ledger: pre-locked postings must all belong to one tenant")

// LockProjectionsForPostings is LockProjectionsForPosting over the union of
// several postings the caller will Post, in order, in this same
// transaction (ADR 0088 §5.2, ADR 0082 Amendment A4). An operation that
// posts more than one transaction - sportsbook void-after-settlement posts
// a rollback then a void, whose account sets differ - must take every L3
// lock it will ever need in ONE canonical-order step; locking per Post
// would acquire later accounts while holding earlier ones, the subset
// pre-lock R3 forbids. R3 generalises to: the set of inputs pre-locked is
// exactly the set subsequently posted, with identical Entries and
// BonusCost. Each later Post re-locks rows already held (a no-op).
func LockProjectionsForPostings(ctx context.Context, tx pgx.Tx, ins ...TransactionInput) (LockedProjections, error) {
	if len(ins) == 0 {
		return LockedProjections{}, fmt.Errorf("%w: no postings to pre-lock", ErrInvalidEntry)
	}
	var ids []uuid.UUID
	for _, in := range ins {
		if in.TenantID != ins[0].TenantID {
			return LockedProjections{}, ErrPreLockTenantMismatch
		}
		entries, err := prepareEntries(ctx, tx, in)
		if err != nil {
			return LockedProjections{}, err
		}
		ids = append(ids, entryAccountIDs(entries)...)
	}
	ordered := canonicalAccountOrder(ids)
	balances, err := ensureAndLockProjectionsInOrder(ctx, tx, ins[0].TenantID, ordered)
	if err != nil {
		return LockedProjections{}, err
	}
	return LockedProjections{AccountIDs: ordered, balances: balances}, nil
}

// entryAccountIDs projects an entry slice onto its ledger_account_ids, in
// slice order (deduplication and sorting are canonicalAccountOrder's job).
func entryAccountIDs(entries []EntryInput) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.LedgerAccountID)
	}
	return ids
}

// canonicalAccountOrder deduplicates ids and returns them sorted
// ascending by UUID byte order - identical to Postgres's own uuid
// ordering (uuid_cmp is a memcmp over the 16 bytes), so the Go-side order
// and any ORDER BY ledger_account_id agree exactly. Never sorts by the
// string form, which would agree only by accident of hex formatting.
//
// ADR 0082 R2: ascending id is arbitrary but STABLE, requires no domain
// knowledge, needs no central registry of account types, and is the only
// key in this system every call site can compute without knowing anything
// about the others. The rejected alternative - ordering by account_type
// (e.g. "house-level always first") - needs a totally-ordered registry
// every future account type must be added to, and a forgotten type
// silently reopens the cycle.
func canonicalAccountOrder(ids []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		return bytes.Compare(out[i][:], out[j][:]) < 0
	})
	return out
}

// ensureProjectionRowSQL is ADR 0082 R6's materialisation step.
//
// It inserts ZERO totals and nothing else, copying every identity column
// from ledger_accounts - the identical source and identical values
// migration 0023's trigger itself derives, and exactly the
// INSERT ... SELECT ... FROM ledger_accounts shape RebuildProjectionRow
// (this package's already-existing direct writer of this table) uses. ON
// CONFLICT DO NOTHING, so an EXISTING row is never read, never rewritten
// and never touched: CLAUDE.md's "never UPDATE a balance" holds literally,
// not merely in spirit. The literal 0, 0 are constants on purpose - there
// is no expression here that could ever be made to compute a total.
const ensureProjectionRowSQL = `
	INSERT INTO wallet_balance_projection
		(ledger_account_id, tenant_id, wallet_id, player_account_id, asset_code, account_type, debit_total, credit_total, updated_at)
	SELECT la.id, la.tenant_id, la.wallet_id, la.player_account_id, la.asset_code, la.account_type, 0, 0, now()
	  FROM ledger_accounts la
	 WHERE la.id = $1 AND la.tenant_id = $2
	ON CONFLICT (ledger_account_id) DO NOTHING`

// lockProjectionRowSQL is the L3 lock itself. This literal, and the one
// in RebuildProjectionRow's ON CONFLICT DO UPDATE, are the ONLY places in
// the whole tree that may take a lock on wallet_balance_projection (R4),
// enforced permanently by
// TestLockOrder_NoProjectionForUpdateOutsideLedgerPackage.
const lockProjectionRowSQL = `
	SELECT asset_code, account_type, debit_total, credit_total
	  FROM wallet_balance_projection
	 WHERE ledger_account_id = $1
	   FOR UPDATE`

// ensureAndLockProjectionsInOrder is ADR 0082 R6: for each account, in
// canonical order, (a) INSERT the projection row with zero totals if
// absent (ON CONFLICT DO NOTHING, columns copied from ledger_accounts),
// then (b) SELECT ... FOR UPDATE it. Two statements per account, executed
// strictly in ascending account order.
//
// A single batched INSERT followed by a single batched SELECT ... FOR
// UPDATE is NOT equivalent and must not be substituted: the batched
// INSERT locks only the rows it creates, so a transaction can acquire a
// high-id NEW row in phase (a) and then a low-id EXISTING row in phase
// (b) - descending, which is the very cycle this function exists to
// prevent. The interleaving below (ensure, lock, ensure, lock, ...) is
// load-bearing; do not "optimise" it into two passes.
//
// Issued as one pgx.Batch: the server executes batch statements
// sequentially, in order, in the same transaction, so pipelining costs
// nothing in ordering and saves 2N-1 round trips. If the FOR UPDATE for
// account k blocks, the server simply does not reach account k+1's
// statements until it unblocks - which is precisely the desired
// behaviour.
//
// Why R6 exists at all: a ledger account with no entries yet has NO
// projection row, and SELECT ... FOR UPDATE on an absent row locks
// nothing at all. The trigger would then CREATE it at L4, out of
// canonical order, and two transactions creating two new rows in opposite
// orders deadlock on the primary-key index. Materialising first closes
// that hole without touching any total.
func ensureAndLockProjectionsInOrder(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, accountIDs []uuid.UUID) (map[uuid.UUID]Balance, error) {
	balances := make(map[uuid.UUID]Balance, len(accountIDs))
	if len(accountIDs) == 0 {
		// A tombstone posts no entries, so it locks nothing. Deliberate:
		// there is no balance it could affect.
		return balances, nil
	}
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id is required to lock projections", ErrInvalidEntry)
	}

	batch := &pgx.Batch{}
	for _, id := range accountIDs {
		batch.Queue(ensureProjectionRowSQL, id, tenantID)
		batch.Queue(lockProjectionRowSQL, id)
	}

	results := tx.SendBatch(ctx, batch)
	var firstErr error
	for _, id := range accountIDs {
		tag, err := results.Exec()
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("ledger: materialise projection row for account %s: %w", id, err)
			}
			// Keep draining: every queued result must be consumed before
			// Close, or the connection is left with unread results.
			if _, qErr := results.Query(); qErr != nil && firstErr == nil {
				firstErr = qErr
			}
			continue
		}
		preExisting := tag.RowsAffected() == 0

		var b Balance
		b.LedgerAccountID = id
		if err := results.QueryRow().Scan(&b.AssetCode, &b.AccountType, &b.DebitTotal, &b.CreditTotal); err != nil {
			if firstErr == nil {
				if errors.Is(err, pgx.ErrNoRows) {
					// Unreachable given the ensure step immediately above
					// (and prepareEntries' own resolveEntryAccounts, which
					// already proved every caller-supplied account exists).
					// Reachable only if the account id belongs to another
					// tenant, or was deleted between the two statements -
					// both of which must fail the posting outright rather
					// than silently lock nothing and read a zero.
					firstErr = fmt.Errorf("%w: ledger account %s has no lockable projection row in this tenant", ErrInvalidEntry, id)
				} else {
					firstErr = fmt.Errorf("ledger: lock projection row for account %s: %w", id, err)
				}
			}
			continue
		}
		b.Found = preExisting
		balances[id] = b
	}
	if err := results.Close(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("ledger: close projection lock batch: %w", err)
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return balances, nil
}

// prepareEntries is Post's preamble, extracted verbatim so that Post and
// LockProjectionsForPosting compute the SAME final entry set from the
// same input: input validation, applyBonusMirror (Rule B2 extended), and
// §7.4.2's fixed-order concatenation. No behaviour change - this is a
// pure extraction, and it is the only reason a caller's pre-lock is
// guaranteed to cover every account Post will later touch, generated legs
// included.
//
// Running it twice (once from LockProjectionsForPosting, once from Post)
// is safe and deliberate: every step is a read or an idempotent
// GetOrCreateAccount, and both runs see the same transaction's state, so
// both produce an identical slice.
func prepareEntries(ctx context.Context, tx pgx.Tx, in TransactionInput) ([]EntryInput, error) {
	if in.TenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id is required", ErrInvalidEntry)
	}
	if in.IdempotencyKey == "" {
		return nil, fmt.Errorf("%w: idempotency key is required", ErrInvalidEntry)
	}
	if in.CorrelationID == uuid.Nil {
		return nil, fmt.Errorf("%w: correlation id is required", ErrInvalidEntry)
	}
	if (in.ProviderID == nil) != (in.ProviderTxID == nil) {
		return nil, fmt.Errorf("%w: provider_id and provider_tx_id must both be set or both be nil", ErrInvalidEntry)
	}
	// PRH-2 D (PAY-POLL-AMOUNT-1 / FH7-06, defence in depth): an EMPTY
	// provider_tx_id is never a valid external reference (migration 0099's
	// ledger_transactions_provider_tx_id_ref_bound CHECK refuses it too, but as an
	// untyped constraint error). Refuse it here with the typed sentinel so a
	// caller that derives the key from an unvalidated echo can never post under
	// the degenerate key "<provider>:".
	if in.ProviderTxID != nil && *in.ProviderTxID == "" {
		return nil, fmt.Errorf("%w: provider_tx_id must not be empty when set", ErrInvalidEntry)
	}
	// reason_code is required on manual_adjustment (CLAUDE.md's four-eyes
	// rule) AND on bonus_forfeiture (ADR 0032 §3.1: expiry vs. staff
	// cancellation is distinguished ONLY by reason_code) - migration
	// 0051's widened ledger_transactions_check1 enforces the same
	// equality at the database; this is the boundary-level copy with a
	// legible error (ledger-accounting-model.md §7.3).
	if (in.TransactionType == TxManualAdjustment || in.TransactionType == TxBonusForfeiture) &&
		(in.ReasonCode == nil || *in.ReasonCode == "") {
		return nil, fmt.Errorf("%w: %s requires a reason code", ErrInvalidEntry, in.TransactionType)
	}
	for _, e := range in.Entries {
		if e.Amount <= 0 {
			return nil, fmt.Errorf("%w: entry amount must be positive, got %d", ErrInvalidEntry, e.Amount)
		}
		if e.Direction != Debit && e.Direction != Credit {
			return nil, fmt.Errorf("%w: entry direction must be debit or credit, got %q", ErrInvalidEntry, e.Direction)
		}
	}

	// Rule B2 (extended) mirror generator (ledger-accounting-model.md
	// §7.4, bonus_mirror.go): resolves every entry's account_type/
	// asset_code from ledger_accounts (never trusting the caller),
	// validates BonusCost's fail-closed rules, enforces HR-17, and - for
	// every asset touched by a BONUS_SET account - returns the mirror and
	// recognition legs invariant B1 requires. Returns no entries and an
	// error for a non-bonus posting (the overwhelming majority of calls),
	// after one lightweight account-type lookup. Runs BEFORE anything is
	// locked or written, so a validation failure leaves no
	// ledger_transactions row, no ledger_entries row and no projection
	// row - "before Post writes anything at all", HR-9's own original
	// standard, preserved under the generator that replaces it.
	generatedEntries, err := applyBonusMirror(ctx, tx, in.TenantID, in)
	if err != nil {
		return nil, err
	}
	if len(generatedEntries) == 0 {
		return in.Entries, nil
	}
	// Fixed order (§7.4.2): the caller's own entries first, then the
	// generated legs in the order applyBonusMirror produced them (all
	// step-2 legs, then all step-3 legs, assets in sorted order) - so
	// a given logical posting always produces byte-identical entry
	// rows, never mutating in.Entries itself. ADR 0082 R7 keeps this
	// exactly as it was: once R1+R6 hold, entry order has no locking
	// meaning at all, so there is nothing to gain by disturbing it and
	// an auditability property to lose.
	entriesToPost := make([]EntryInput, 0, len(in.Entries)+len(generatedEntries))
	entriesToPost = append(entriesToPost, in.Entries...)
	entriesToPost = append(entriesToPost, generatedEntries...)
	return entriesToPost, nil
}

// AccountSpec names one ledger account to resolve. WalletID is nil for a
// house-level account.
type AccountSpec struct {
	WalletID    *uuid.UUID
	AccountType AccountType
	AssetCode   string
}

// canonicalKey is AccountSpec's ordering key: (wallet key, account_type,
// asset_code), where the wallet key is the empty string for a house-level
// account. NUL separators so no field's content can ever be confused with
// the next field's (a wallet uuid's text form and an account type are
// both printable, and every printable byte sorts above NUL, so the
// separator never reorders anything).
func (s AccountSpec) canonicalKey() string {
	wallet := ""
	if s.WalletID != nil {
		wallet = s.WalletID.String()
	}
	return wallet + "\x00" + string(s.AccountType) + "\x00" + s.AssetCode
}

// GetOrCreateAccounts resolves every spec, creating any that do not exist
// yet, and returns their ids IN THE SAME ORDER AS specs. Creation is
// performed in canonical spec order - ascending by
// (wallet key, account_type, asset_code), where the wallet key is the
// empty string for a house-level account - NOT in the caller's argument
// order, so two call sites that resolve the same accounts in different
// argument orders can never block on each other's uncommitted
// ledger_accounts index entries in opposite orders. (ADR 0082 §3.2; the
// concrete case is internal/withdrawal, which resolved `cash, hold` in
// RequestWithdrawal and `hold, cash` in Reject/Complete/Fail/Cancel.)
//
// ledger_accounts.id is random, so it cannot order creation; the
// (wallet, type, asset) tuple is the only key knowable before the row
// exists, and it is exactly the tuple the partial unique indexes use.
//
// GetOrCreateAccount (singular) is kept, unchanged, for genuine
// single-account call sites - one account cannot be resolved out of order
// with itself.
func GetOrCreateAccounts(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, specs ...AccountSpec) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, len(specs))
	if len(specs) == 0 {
		return out, nil
	}

	order := make([]int, len(specs))
	for i := range order {
		order[i] = i
	}
	// Stable so duplicate specs (same key) resolve in argument order,
	// making this function fully deterministic for any input.
	sort.SliceStable(order, func(a, b int) bool {
		return specs[order[a]].canonicalKey() < specs[order[b]].canonicalKey()
	})

	for _, i := range order {
		s := specs[i]
		id, err := GetOrCreateAccount(ctx, tx, tenantID, s.WalletID, s.AccountType, s.AssetCode)
		if err != nil {
			return nil, fmt.Errorf("ledger: resolve %s/%s account: %w", s.AccountType, s.AssetCode, err)
		}
		out[i] = id
	}
	return out, nil
}

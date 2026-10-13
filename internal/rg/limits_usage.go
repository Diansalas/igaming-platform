package rg

// ADR 0115 SLICE S3: RG usage measurement - wager, loss and deposit usage per window (sections 1.4,
// 3.4).
//
// Status: IMPLEMENTED as a READ-ONLY measurement. It is NOT wired into wallet, payments, casino or
// sportsbook and refuses nothing (S4). tenant_person aggregation is PROVIDER DEPENDENT: it sums the
// accounts that share a person_id inside one tenant, and person linking is the MOCK resolver of ADR
// 0027 (an Uncertain registration gets its own Person, so it is not aggregated until review links it).
//
// Design (ledger-finance BLOCKER-2, ADR 0115 3.4):
//
//   - RG-ONLY specs. Risk's cumulative specs (internal/risk/cumulative.go) are untouched: they net
//     every casino_rollback (a rollback of a casino_win would count as wager), do not declare the
//     bonus legs and have no win spec. The declared-leg / undeclared-leg-fails-closed mechanism is
//     the same: a player-owned leg of an account type the spec neither measures nor deliberately
//     ignores aborts the measurement with ErrUnrecognizedUsageLeg.
//   - No counters. Usage is derived from ledger_entries / deposit_intents at read time, in ONE
//     statement (one snapshot even under READ COMMITTED), using the indexes of migration 0135
//     (tenant_id, player_account_id, asset_code, created_at). All windows come out of that one scan.
//   - Per asset, integer minor units, NUMERIC(38,0) end to end (text -> math/big; never float64, no FX).
//   - Window starts are computed in SQL (limits_usage_window.go).
//   - Redis or any cache is never consulted.
//
// Definitions (deliberate S3 interpretations are marked INTERPRETATION and listed in the ADR record):
//
//   wager  = sum over stake transactions (casino_bet, sportsbook_bet) of the net debit on the measured
//            accounts, MINUS reversals whose ORIGINAL is a stake transaction. The original of a
//            casino_rollback / sportsbook_rollback is reverses_transaction_id; the original of a
//            sportsbook_void is the sportsbook_bet transaction of the same tenant and correlation id
//            (INTERPRETATION: sportsbook_void does not set reverses_transaction_id, but it is by
//            construction the exact inverse of the placement - verifyVoidReturnsPlacedStake). A rollback
//            of a casino_win or of a sportsbook_settlement is never a stake reversal.
//   loss   = net debit on the measured accounts over casino_bet, casino_win, casino_rollback,
//            sportsbook_bet, sportsbook_settlement, sportsbook_void, sportsbook_rollback; floored at 0
//            unless loss_may_go_negative. bonus_conversion is not in the set, so a conversion never
//            reduces loss usage.
//   reversal window rule (both) = a reversal counts only if its ORIGINAL transaction is inside the same
//            window; a stake or a settlement counts by its own created_at.
//   measured accounts = player_cash, plus player_bonus and player_bonus_held when the kind's
//            count_bonus_funded is true (INTERPRETATION: player_bonus_held is the held-win bucket of a
//            terminal bonus grant, ADR 0039 G-2; it is the bonus family's win side). player_locked_cash
//            and player_locked_bonus are declared IGNORED. With count_bonus_funded=false the whole
//            bonus family is ignored, so wins and returns on bonus-funded legs are excluded too.
//   deposit = in-flight intents (pending, ambiguous, no ledger posting yet) at the requested amount,
//            by the intent's created_at; PLUS every ledger 'deposit' credit to player_cash at the
//            credited amount, by the posting's created_at. A succeeded intent therefore counts once,
//            at the credited amount; an inbound deposit with no intent counts from the ledger; a late
//            credit on a failed/declined intent counts (the money arrived). Reversals and withdrawals
//            do not restore capacity (deposit_reversal is not in the set).
//
// Known limit (ledger design, not S3): ledger created_at is the posting transaction's START time
// (DEFAULT now()), so a posting whose transaction ran longer than a rolling window can be dated just
// outside it.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// Errors of the usage measurement. Every one means "no usage figure": the caller (S4) must fail closed.
var (
	// ErrUnrecognizedUsageLeg: a player-owned ledger leg of an undeclared account type was observed.
	ErrUnrecognizedUsageLeg = errors.New("rg: usage query observed an undeclared player-side ledger leg")
	// ErrUnresolvedReversal: a reversal transaction whose original could not be found.
	ErrUnresolvedReversal = errors.New("rg: usage query found a reversal with no resolvable original transaction")
	// ErrUsageScope: the transaction is not a tenant-scoped, non-player-scoped session of the request's tenant.
	ErrUsageScope = errors.New("rg: usage measurement needs a tenant-scoped (not player-scoped) session of the request tenant")
	// ErrUsageIsolation: the transaction is not READ COMMITTED (ADR 0115 3.1 isolation guard).
	ErrUsageIsolation = errors.New("rg: usage measurement refuses a REPEATABLE READ or SERIALIZABLE transaction")
	// ErrUsageAccountNotFound: the anchor account is not visible in the tenant.
	ErrUsageAccountNotFound = errors.New("rg: usage anchor account not found in the tenant")
)

// The RG-only specs (ledger transaction types and account types).
var (
	// RGStakeTransactionTypes are the stake transactions (wager).
	RGStakeTransactionTypes = []string{string(ledger.TxCasinoBet), string(ledger.TxSportsbookBet)}
	// RGReversalTransactionTypes are the transactions that can reverse another transaction.
	RGReversalTransactionTypes = []string{string(ledger.TxCasinoRollback), string(ledger.TxSportsbookVoid), string(ledger.TxSportsbookRollback)}
	// RGLossTransactionTypes is the closed loss set (ADR 0115 1.4).
	RGLossTransactionTypes = []string{
		string(ledger.TxCasinoBet), string(ledger.TxCasinoWin), string(ledger.TxCasinoRollback),
		string(ledger.TxSportsbookBet), string(ledger.TxSportsbookSettlement), string(ledger.TxSportsbookVoid), string(ledger.TxSportsbookRollback),
	}
	// RGCashAccountTypes are always measured.
	RGCashAccountTypes = []string{string(ledger.AccountPlayerCash)}
	// RGBonusAccountTypes are measured only when the kind's count_bonus_funded is true.
	RGBonusAccountTypes = []string{string(ledger.AccountPlayerBonus), string(ledger.AccountPlayerBonusHeld)}
	// RGIgnoredAccountTypes are declared ignored (the lock side of a stake posting).
	RGIgnoredAccountTypes = []string{string(ledger.AccountPlayerLockedCash), string(ledger.AccountPlayerLockedBonus)}
)

func inSet(set []string, s string) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}

// Amounts is a usage figure for the anchor account alone and, when the scope is tenant_person, for the
// Person's accounts of the tenant (nil otherwise). Never a float.
type Amounts struct {
	Account *big.Int
	Person  *big.Int
}

func zeroAmounts(person bool) Amounts {
	a := Amounts{Account: new(big.Int)}
	if person {
		a.Person = new(big.Int)
	}
	return a
}

// UsageRequest is the input of the amount measurements.
type UsageRequest struct {
	TenantID        uuid.UUID
	PlayerAccountID uuid.UUID // the anchor account
	// Scope account measures the anchor alone; tenant_person measures the anchor AND the Person's
	// accounts of the tenant (both figures, because the effective limit is the MIN across scopes,
	// ADR 0115 2.2). platform_person is not a usage scope (time-out only).
	Scope     LimitScope
	AssetCode string
	Windows   []WindowSpec

	// StakeUsage only: the wager and loss kinds each carry their own count_bonus_funded.
	WagerCountsBonus  bool
	LossCountsBonus   bool
	LossMayGoNegative bool

	// DepositUsage only: the deposit intent being evaluated (CandidateRef, ADR 0115 3.2). It is excluded
	// from in-flight usage; the caller adds its amount once.
	ExcludeIntentID *uuid.UUID

	// At is a replay seam (denial-audit replay, DST tests); production callers leave it nil, which
	// means clock_timestamp() inside the statement. It is never client input.
	At *time.Time
}

func (r UsageRequest) validate() error {
	if r.TenantID == uuid.Nil || r.PlayerAccountID == uuid.Nil {
		return errors.New("rg: usage request needs a tenant and an anchor account")
	}
	if r.Scope != LimitScopeAccount && r.Scope != LimitScopeTenantPerson {
		return fmt.Errorf("rg: usage scope %q is not account or tenant_person", r.Scope)
	}
	if r.AssetCode == "" || len(r.AssetCode) > 32 {
		return errors.New("rg: usage request needs an asset code")
	}
	return nil
}

// usageGuard refuses a session in which the measurement would silently under-count (fail closed):
// a different tenant than the request's (RLS would return zero rows = zero usage), a player-scoped
// session (RLS shows it only its own account, so a Person total would be short), or a transaction
// that is not READ COMMITTED (ADR 0115 3.1).
func usageGuard(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	var iso, tenant, player string
	if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation'),
		COALESCE(current_setting('app.tenant_id', true), ''), COALESCE(current_setting('app.player_account_id', true), '')`).Scan(&iso, &tenant, &player); err != nil {
		return fmt.Errorf("rg: usage guard: %w", err)
	}
	if iso != "read committed" {
		return fmt.Errorf("%w (isolation %q)", ErrUsageIsolation, iso)
	}
	if tenant != tenantID.String() || player != "" {
		return ErrUsageScope
	}
	return nil
}

func parseMinor(s *string) (*big.Int, error) {
	if s == nil || *s == "" {
		return new(big.Int), nil
	}
	n, ok := new(big.Int).SetString(strings.TrimSpace(*s), 10)
	if !ok {
		return nil, fmt.Errorf("rg: malformed NUMERIC minor-unit value %q", *s)
	}
	return n, nil
}

// accountSetCTE resolves the account set inside the same statement as the measurement (one snapshot,
// so a concurrent Person link cannot split the set from the totals). A tenant_person set joins by
// person_id through the tenant's player_accounts (ADR 0115 1.4), never by account id alone.
const accountSetCTE = `accts AS MATERIALIZED (
	SELECT a.id FROM player_accounts a
	 WHERE a.tenant_id = $1
	   AND CASE WHEN $8 = 'tenant_person'
	            THEN a.person_id = (SELECT x.person_id FROM player_accounts x WHERE x.id = $2 AND x.tenant_id = $1)
	            ELSE a.id = $2 END
)`

// ---- wager / loss ------------------------------------------------------------------------------

// StakeWindowUsage is the wager and loss usage of one window.
type StakeWindowUsage struct {
	Window  WindowSpec
	Start   time.Time
	Wager   Amounts
	Loss    Amounts // after the loss_may_go_negative floor
	LossRaw Amounts // before the floor (may be negative)
}

// StakeUsage is the result of MeasureStakeUsage.
type StakeUsage struct {
	AsOf       time.Time
	Scope      LimitScope
	AssetCode  string
	AccountIDs []uuid.UUID // the account set the statement summed (denial-audit replay input)
	Windows    []StakeWindowUsage
}

// AuditInputs are the replay inputs of ADR 0115 4 (denial audit): account set, asset, transaction-type
// sets, the statement instant and every window start. Contains ids and codes only.
func (u StakeUsage) AuditInputs() map[string]any {
	starts := make([]string, len(u.Windows))
	for i, w := range u.Windows {
		starts[i] = string(w.Window.Period) + "=" + w.Start.UTC().Format(time.RFC3339Nano)
	}
	ids := make([]string, len(u.AccountIDs))
	for i, id := range u.AccountIDs {
		ids[i] = id.String()
	}
	return map[string]any{
		"as_of": u.AsOf.UTC().Format(time.RFC3339Nano), "scope": string(u.Scope), "asset_code": u.AssetCode,
		"account_ids": ids, "window_starts": starts,
		"stake_types": RGStakeTransactionTypes, "loss_types": RGLossTransactionTypes, "reversal_types": RGReversalTransactionTypes,
	}
}

// stakeUsageSQL builds the one-scan wager/loss statement for n windows. Parameters:
// $1 tenant, $2 anchor, $3 asset, $4 at, $5 periods, $6 tzs, $7 week starts, $8 scope,
// $9 loss types, $10 reversal types, $11 stake types. Only integer indexes are interpolated.
func stakeUsageSQL(n int, person bool) string {
	least := make([]string, n)
	for i := range least {
		least[i] = fmt.Sprintf("s%d", i+1)
	}
	var cols []string
	for i := 1; i <= n; i++ {
		start := fmt.Sprintf("(SELECT s%d FROM win)", i)
		cols = append(cols,
			fmt.Sprintf("SUM(sg) FILTER (WHERE wager_row AND eff >= %s AND acct = $2)::text AS aw%d", start, i),
			fmt.Sprintf("SUM(sg) FILTER (WHERE eff >= %s AND acct = $2)::text AS al%d", start, i))
		if person {
			cols = append(cols,
				fmt.Sprintf("SUM(sg) FILTER (WHERE wager_row AND eff >= %s)::text AS pw%d", start, i),
				fmt.Sprintf("SUM(sg) FILTER (WHERE eff >= %s)::text AS pl%d", start, i))
		}
	}
	var final []string
	for i := 1; i <= n; i++ {
		final = append(final, fmt.Sprintf("win.s%d", i))
	}
	var gcols []string
	for i := 1; i <= n; i++ {
		gcols = append(gcols, fmt.Sprintf("g.aw%d, g.al%d", i, i))
		if person {
			gcols = append(gcols, fmt.Sprintf("g.pw%d, g.pl%d", i, i))
		}
	}
	return `WITH ` + windowsCTE(n, 4, 5, 6, 7) + `,
` + accountSetCTE + `,
ent AS MATERIALIZED (
	-- Planned on its own so the (tenant_id, player_account_id, asset_code, created_at) index of migration 0135
	-- drives the scan; the joins below only ever see the window's rows.
	SELECT le.ledger_transaction_id, le.ledger_account_id, le.tenant_id, le.player_account_id, le.direction, le.amount, le.created_at
	  FROM ledger_entries le
	 WHERE le.tenant_id = $1
	   AND le.player_account_id = ANY(ARRAY(SELECT id FROM accts))
	   AND le.asset_code = $3
	   AND le.created_at >= (SELECT least(` + strings.Join(least, ", ") + `) FROM win)
	   AND le.created_at <= (SELECT t FROM asof)
),
scan AS MATERIALIZED (
	SELECT le.player_account_id AS acct, la.account_type, le.direction, le.amount, le.created_at,
	       lt.transaction_type AS tt, (lt.transaction_type = ANY($10::text[])) AS is_rev, o.ot, o.oc
	  FROM ent le
	  JOIN ledger_transactions lt ON lt.id = le.ledger_transaction_id AND lt.tenant_id = le.tenant_id
	  JOIN ledger_accounts la ON la.id = le.ledger_account_id AND la.tenant_id = le.tenant_id
	  LEFT JOIN LATERAL (
	        SELECT u.ot, u.oc FROM (
	          SELECT o1.transaction_type AS ot, o1.created_at AS oc FROM ledger_transactions o1
	           WHERE lt.transaction_type = ANY($10::text[]) AND o1.id = lt.reverses_transaction_id AND o1.tenant_id = lt.tenant_id
	          UNION ALL
	          SELECT o2.transaction_type, o2.created_at FROM ledger_transactions o2
	           WHERE lt.transaction_type = 'sportsbook_void' AND lt.reverses_transaction_id IS NULL
	             AND o2.tenant_id = lt.tenant_id AND o2.correlation_id = lt.correlation_id AND o2.transaction_type = 'sportsbook_bet'
	        ) u ORDER BY u.oc LIMIT 1) o ON true
	 WHERE lt.transaction_type = ANY($9::text[])
),
scan2 AS (
	SELECT acct, account_type,
	       CASE WHEN direction = 'debit' THEN amount ELSE -amount END AS sg,
	       CASE WHEN is_rev THEN oc ELSE created_at END AS eff,
	       (tt = ANY($11::text[]) OR (is_rev AND ot = ANY($11::text[]))) AS wager_row,
	       (is_rev AND oc IS NULL) AS unresolved,
	       (account_type = 'player_locked_cash' AND tt IN ('casino_bet', 'casino_win', 'casino_rollback')) AS undeclared_shape
	  FROM scan
),
grouped AS (
	SELECT account_type, count(*) FILTER (WHERE unresolved) AS unresolved,
	       count(*) FILTER (WHERE undeclared_shape) AS undeclared_shape,
	       ` + strings.Join(cols, ",\n\t       ") + `
	  FROM scan2 GROUP BY account_type
)
SELECT (SELECT t FROM asof), ` + strings.Join(final, ", ") + `, ARRAY(SELECT id FROM accts), g.account_type, g.unresolved, g.undeclared_shape, ` + strings.Join(gcols, ", ") + `
  FROM win LEFT JOIN grouped g ON true`
}

// MeasureStakeUsage measures wager and loss usage for every requested window in ONE statement.
// The caller holds the L0.4 person lock (S4); this function takes no lock and writes nothing.
func MeasureStakeUsage(ctx context.Context, tx pgx.Tx, req UsageRequest) (StakeUsage, error) {
	if err := req.validate(); err != nil {
		return StakeUsage{}, err
	}
	periods, tzs, wss, err := windowArgs(req.Windows)
	if err != nil {
		return StakeUsage{}, err
	}
	if err := usageGuard(ctx, tx, req.TenantID); err != nil {
		return StakeUsage{}, err
	}
	n, person := len(req.Windows), req.Scope == LimitScopeTenantPerson
	rows, err := tx.Query(ctx, stakeUsageSQL(n, person),
		req.TenantID, req.PlayerAccountID, req.AssetCode, req.At, periods, tzs, wss, string(req.Scope),
		RGLossTransactionTypes, RGReversalTransactionTypes, RGStakeTransactionTypes)
	if err != nil {
		return StakeUsage{}, fmt.Errorf("rg: stake usage: %w", err)
	}
	defer rows.Close()

	out := StakeUsage{Scope: req.Scope, AssetCode: req.AssetCode, Windows: make([]StakeWindowUsage, n)}
	for i := range out.Windows {
		out.Windows[i] = StakeWindowUsage{Window: req.Windows[i], Wager: zeroAmounts(person), LossRaw: zeroAmounts(person)}
	}
	seen := false
	for rows.Next() {
		seen = true
		starts := make([]*time.Time, n)
		var accountType *string
		var unresolved, undeclaredShape *int64
		var asOf time.Time
		dest := []any{&asOf}
		for i := range starts {
			dest = append(dest, &starts[i])
		}
		dest = append(dest, &out.AccountIDs, &accountType, &unresolved, &undeclaredShape)
		per := 2
		if person {
			per = 4
		}
		txt := make([]*string, n*per)
		for i := range txt {
			dest = append(dest, &txt[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return StakeUsage{}, fmt.Errorf("rg: scan stake usage: %w", err)
		}
		out.AsOf = asOf
		for i, s := range starts {
			if s == nil {
				return StakeUsage{}, fmt.Errorf("rg: window %d (%s) has no start", i, req.Windows[i].Period)
			}
			out.Windows[i].Start = *s
		}
		if accountType == nil { // no ledger row at all in any window
			continue
		}
		if unresolved != nil && *unresolved > 0 {
			return StakeUsage{}, fmt.Errorf("%w: account_type=%s", ErrUnresolvedReversal, *accountType)
		}
		if undeclaredShape != nil && *undeclaredShape > 0 {
			// A casino posting that locks CASH (Dr player_cash / Cr player_locked_cash, released into player_cash on a
			// win): not producible by today's casino postBet (ADR 0039 section 16.10) and its release credit would
			// net the stake out of loss usage. The shape is undeclared, so the measurement stops (ADR 0031 section 33).
			return StakeUsage{}, fmt.Errorf("%w: account_type=%s on a casino transaction", ErrUnrecognizedUsageLeg, *accountType)
		}
		measuredWager := inSet(RGCashAccountTypes, *accountType) || (req.WagerCountsBonus && inSet(RGBonusAccountTypes, *accountType))
		measuredLoss := inSet(RGCashAccountTypes, *accountType) || (req.LossCountsBonus && inSet(RGBonusAccountTypes, *accountType))
		known := inSet(RGCashAccountTypes, *accountType) || inSet(RGBonusAccountTypes, *accountType) || inSet(RGIgnoredAccountTypes, *accountType)
		if !known {
			return StakeUsage{}, fmt.Errorf("%w: account_type=%s", ErrUnrecognizedUsageLeg, *accountType)
		}
		for i := 0; i < n; i++ {
			base := i * per
			vals := make([]*big.Int, per)
			for j := 0; j < per; j++ {
				v, err := parseMinor(txt[base+j])
				if err != nil {
					return StakeUsage{}, err
				}
				vals[j] = v
			}
			w := &out.Windows[i]
			if measuredWager {
				w.Wager.Account.Add(w.Wager.Account, vals[0])
				if person {
					w.Wager.Person.Add(w.Wager.Person, vals[2])
				}
			}
			if measuredLoss {
				w.LossRaw.Account.Add(w.LossRaw.Account, vals[1])
				if person {
					w.LossRaw.Person.Add(w.LossRaw.Person, vals[3])
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return StakeUsage{}, fmt.Errorf("rg: read stake usage: %w", err)
	}
	if !seen || len(out.AccountIDs) == 0 {
		return StakeUsage{}, ErrUsageAccountNotFound
	}
	for i := range out.Windows {
		w := &out.Windows[i]
		w.Loss = zeroAmounts(person)
		w.Loss.Account = floorLoss(w.LossRaw.Account, req.LossMayGoNegative)
		if person {
			w.Loss.Person = floorLoss(w.LossRaw.Person, req.LossMayGoNegative)
		}
	}
	return out, nil
}

// floorLoss applies the engineering floor of 0 unless loss_may_go_negative is attested true.
func floorLoss(raw *big.Int, mayGoNegative bool) *big.Int {
	if !mayGoNegative && raw.Sign() < 0 {
		return new(big.Int)
	}
	return new(big.Int).Set(raw)
}

// ---- deposit -----------------------------------------------------------------------------------

// DepositWindowUsage is the deposit usage of one window.
type DepositWindowUsage struct {
	Window   WindowSpec
	Start    time.Time
	Credited Amounts // ledger deposit credits to player_cash, at the credited amount
	InFlight Amounts // pending / ambiguous intents with no posting yet, at the requested amount
	Total    Amounts // Credited + InFlight
}

// DepositUsage is the result of MeasureDepositUsage.
type DepositUsage struct {
	AsOf       time.Time
	Scope      LimitScope
	AssetCode  string
	AccountIDs []uuid.UUID
	Windows    []DepositWindowUsage
}

// depositUsageSQL builds the one-statement deposit usage query. Parameters: $1 tenant, $2 anchor,
// $3 asset, $4 at, $5 periods, $6 tzs, $7 week starts, $8 scope, $9 excluded intent id.
func depositUsageSQL(n int, person bool) string {
	least := make([]string, n)
	for i := range least {
		least[i] = fmt.Sprintf("s%d", i+1)
	}
	var lcols, icols, final, gcols []string
	for i := 1; i <= n; i++ {
		start := fmt.Sprintf("(SELECT s%d FROM win)", i)
		lcols = append(lcols, fmt.Sprintf("SUM(amount) FILTER (WHERE direction = 'credit' AND account_type = 'player_cash' AND created_at >= %s AND acct = $2)::text AS la%d", start, i))
		icols = append(icols, fmt.Sprintf("SUM(amount) FILTER (WHERE created_at >= %s AND acct = $2)::text AS ia%d", start, i))
		final = append(final, fmt.Sprintf("win.s%d", i))
		gcols = append(gcols, fmt.Sprintf("g.la%d, infl.ia%d", i, i))
		if person {
			lcols = append(lcols, fmt.Sprintf("SUM(amount) FILTER (WHERE direction = 'credit' AND account_type = 'player_cash' AND created_at >= %s)::text AS lp%d", start, i))
			icols = append(icols, fmt.Sprintf("SUM(amount) FILTER (WHERE created_at >= %s)::text AS ip%d", start, i))
			gcols = append(gcols, fmt.Sprintf("g.lp%d, infl.ip%d", i, i))
		}
	}
	return `WITH ` + windowsCTE(n, 4, 5, 6, 7) + `,
` + accountSetCTE + `,
ent AS MATERIALIZED (
	-- Planned on its own so the migration-0135 ledger_entries index drives the scan.
	SELECT le.ledger_transaction_id, le.ledger_account_id, le.tenant_id, le.player_account_id, le.direction, le.amount, le.created_at
	  FROM ledger_entries le
	 WHERE le.tenant_id = $1
	   AND le.player_account_id = ANY(ARRAY(SELECT id FROM accts))
	   AND le.asset_code = $3
	   AND le.created_at >= (SELECT least(` + strings.Join(least, ", ") + `) FROM win)
	   AND le.created_at <= (SELECT t FROM asof)
),
led AS MATERIALIZED (
	SELECT le.player_account_id AS acct, la.account_type, le.direction, le.amount, le.created_at
	  FROM ent le
	  JOIN ledger_transactions lt ON lt.id = le.ledger_transaction_id AND lt.tenant_id = le.tenant_id
	  JOIN ledger_accounts la ON la.id = le.ledger_account_id AND la.tenant_id = le.tenant_id
	 WHERE lt.transaction_type = 'deposit'
),
ledg AS (
	SELECT account_type, ` + strings.Join(lcols, ", ") + ` FROM led GROUP BY account_type
),
inflight AS MATERIALIZED (
	SELECT di.player_account_id AS acct, di.amount, di.created_at
	  FROM deposit_intents di
	 WHERE di.tenant_id = $1
	   AND di.player_account_id = ANY(ARRAY(SELECT id FROM accts))
	   AND di.asset_code = $3
	   AND di.status IN ('pending', 'ambiguous') AND di.ledger_transaction_id IS NULL
	   AND ($9::uuid IS NULL OR di.id <> $9::uuid)
	   AND di.created_at >= (SELECT least(` + strings.Join(least, ", ") + `) FROM win)
	   AND di.created_at <= (SELECT t FROM asof)
),
infl AS (
	SELECT ` + strings.Join(icols, ", ") + ` FROM inflight
)
SELECT (SELECT t FROM asof), ` + strings.Join(final, ", ") + `, ARRAY(SELECT id FROM accts), g.account_type, ` + strings.Join(gcols, ", ") + `
  FROM win CROSS JOIN infl LEFT JOIN ledg g ON true`
}

// MeasureDepositUsage measures deposit usage for every requested window in ONE statement (ledger
// deposit credits plus in-flight intents).
func MeasureDepositUsage(ctx context.Context, tx pgx.Tx, req UsageRequest) (DepositUsage, error) {
	if err := req.validate(); err != nil {
		return DepositUsage{}, err
	}
	periods, tzs, wss, err := windowArgs(req.Windows)
	if err != nil {
		return DepositUsage{}, err
	}
	if err := usageGuard(ctx, tx, req.TenantID); err != nil {
		return DepositUsage{}, err
	}
	n, person := len(req.Windows), req.Scope == LimitScopeTenantPerson
	rows, err := tx.Query(ctx, depositUsageSQL(n, person),
		req.TenantID, req.PlayerAccountID, req.AssetCode, req.At, periods, tzs, wss, string(req.Scope), req.ExcludeIntentID)
	if err != nil {
		return DepositUsage{}, fmt.Errorf("rg: deposit usage: %w", err)
	}
	defer rows.Close()

	out := DepositUsage{Scope: req.Scope, AssetCode: req.AssetCode, Windows: make([]DepositWindowUsage, n)}
	for i := range out.Windows {
		out.Windows[i] = DepositWindowUsage{Window: req.Windows[i], Credited: zeroAmounts(person), InFlight: zeroAmounts(person), Total: zeroAmounts(person)}
	}
	seen, inflightTaken := false, false
	for rows.Next() {
		seen = true
		starts := make([]*time.Time, n)
		var accountType *string
		var asOf time.Time
		dest := []any{&asOf}
		for i := range starts {
			dest = append(dest, &starts[i])
		}
		dest = append(dest, &out.AccountIDs, &accountType)
		// per window, in the order the SQL selects them: la, ia[, lp, ip].
		per := 2
		if person {
			per = 4
		}
		txt := make([]*string, n*per)
		for i := range txt {
			dest = append(dest, &txt[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return DepositUsage{}, fmt.Errorf("rg: scan deposit usage: %w", err)
		}
		out.AsOf = asOf
		for i, s := range starts {
			if s == nil {
				return DepositUsage{}, fmt.Errorf("rg: window %d (%s) has no start", i, req.Windows[i].Period)
			}
			out.Windows[i].Start = *s
		}
		if accountType != nil && *accountType != string(ledger.AccountPlayerCash) {
			return DepositUsage{}, fmt.Errorf("%w: deposit posting leg account_type=%s", ErrUnrecognizedUsageLeg, *accountType)
		}
		for i := 0; i < n; i++ {
			base := i * per
			get := func(k int) (*big.Int, error) { return parseMinor(txt[base+k]) }
			la, err := get(0)
			if err != nil {
				return DepositUsage{}, err
			}
			ia, err := get(1)
			if err != nil {
				return DepositUsage{}, err
			}
			var lp, ip *big.Int
			if person {
				if lp, err = get(2); err != nil {
					return DepositUsage{}, err
				}
				if ip, err = get(3); err != nil {
					return DepositUsage{}, err
				}
			}
			w := &out.Windows[i]
			if accountType != nil {
				w.Credited.Account.Add(w.Credited.Account, la)
				if person {
					w.Credited.Person.Add(w.Credited.Person, lp)
				}
			}
			// The in-flight figure is repeated on every ledger group row (CROSS JOIN): take it once.
			if !inflightTaken {
				w.InFlight.Account.Set(ia)
				if person {
					w.InFlight.Person.Set(ip)
				}
			}
		}
		inflightTaken = true
	}
	if err := rows.Err(); err != nil {
		return DepositUsage{}, fmt.Errorf("rg: read deposit usage: %w", err)
	}
	if !seen || len(out.AccountIDs) == 0 {
		return DepositUsage{}, ErrUsageAccountNotFound
	}
	for i := range out.Windows {
		w := &out.Windows[i]
		w.Total.Account.Add(w.Credited.Account, w.InFlight.Account)
		if person {
			w.Total.Person.Add(w.Credited.Person, w.InFlight.Person)
		}
	}
	return out, nil
}

// AuditInputs are the replay inputs of ADR 0115 4 for a deposit denial.
func (u DepositUsage) AuditInputs() map[string]any {
	starts := make([]string, len(u.Windows))
	for i, w := range u.Windows {
		starts[i] = string(w.Window.Period) + "=" + w.Start.UTC().Format(time.RFC3339Nano)
	}
	ids := make([]string, len(u.AccountIDs))
	for i, id := range u.AccountIDs {
		ids[i] = id.String()
	}
	return map[string]any{
		"as_of": u.AsOf.UTC().Format(time.RFC3339Nano), "scope": string(u.Scope), "asset_code": u.AssetCode,
		"account_ids": ids, "window_starts": starts, "ledger_type": string(ledger.TxDeposit),
		"in_flight_statuses": []string{"pending", "ambiguous"},
	}
}

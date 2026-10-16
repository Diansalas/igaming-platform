package launchgov

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/marketgate"
	"github.com/Diansalas/igaming-platform/internal/providerkind"
)

// Check codes (ADR 0112 5.1). Each yields pass | fail | not_evaluable; not_evaluable is NOT pass.
const (
	CheckJurisdictionConfig = "R-JUR-CONFIG"
	CheckLicenceValid       = "R-LIC-VALID"
	CheckRulesets           = "R-RULESETS"
	CheckCurrencies         = "R-CURRENCIES"
	CheckPaymentMethods     = "R-PAY-METHODS"
	CheckCatalogue          = "R-CATALOGUE"
	CheckMarketConfig       = "R-MARKET-CONFIG"
	CheckProviderTier       = "R-PROVIDER-TIER"
	CheckAlerts             = "R-ALERTS"
	CheckPlayerFunds        = "R-PLAYER-FUNDS"
	// CheckRGConfig (ADR 0115 section 6, completion audit A6): the subject's players are protected by RG limit
	// enforcement that is SWITCHED ON and has a compliance floor in force for every kind and every allowed
	// currency. Without it a brand would either run with no deposit / loss / wager limits (switch off) or be
	// unable to take a deposit or a wager (switch on, no floor: rg_config_missing).
	CheckRGConfig = "R-RG-CONFIG"
	// ADR 0116 s6 / S7 (A7): the AML readiness checks. Informational like every readiness check (they prove configuration
	// presence, not a legal or regulatory position) and, like every non-passing check, they must be acknowledged by a request.
	CheckAMLQuorum     = "R-AML-QUORUM"
	CheckAMLScreening  = "R-AML-SCREENING"
	CheckAMLMonitoring = "R-AML-MONITORING"

	ResultPass         = "pass"
	ResultFail         = "fail"
	ResultNotEvaluable = "not_evaluable"

	// SnapshotVersion is bumped when the check set or the canonical form changes. 2: R-RG-CONFIG (A6, ADR 0115 S4-min). 2 (upstream numbering):
	// the three R-AML-* checks (A7). 3: R-MARKET-CONFIG (A8). 4 after this merge: the combined set. A request bound to an older
	// readiness hash is stale and must be re-requested (the hash is recomputed over the current set at decision and at execution).
	SnapshotVersion = 4
)

// Check is one readiness check: a code, a result and a closed detail code. Never free text.
type Check struct {
	Code       string `json:"code"`
	Result     string `json:"result"`
	DetailCode string `json:"detail_code"`
}

// Readiness is the computed, informational readiness of a subject for an action. Checks are the
// ONLY part that is snapshotted and hashed; Info is live display data (counts for R-PLAYER-FUNDS)
// that is neither persisted nor hashed, because it changes with every wallet movement.
type Readiness struct {
	Version     int       `json:"version"`
	ComputedAt  time.Time `json:"computed_at"`
	Environment string    `json:"environment"`
	Checks      []Check   `json:"checks"`

	Info map[string]int64 `json:"-"`
	// Balances is the per-asset sum of the subject's players' wallet-account balances
	// (credit_total - debit_total over the wallet-bound projection rows), as DECIMAL STRINGS:
	// NUMERIC(38,0) sums never pass through int64 or float (an 18-exponent asset overflows
	// both). Live display data like Info: not hashed, not persisted.
	Balances map[string]string `json:"-"`
	// Notices are the fixed texts an approver must read for this evaluation (closed set below).
	Notices []string `json:"-"`
}

// Notice texts of the funds check (ADR 0112 R-PLAYER-FUNDS; ledger-finance condition). They are
// constants, never built from data.
const (
	// NoticeFundsStranded is shown with every funds position and is the substance of the
	// acknowledgement of R-PLAYER-FUNDS: closure is terminal and no unclaimed-funds process exists.
	NoticeFundsStranded = "Closing with non-zero player balances strands those balances: closure is terminal and the unclaimed-funds / dormant-balance policy is OPEN (HD-CTF-4), so the funds stay in the ledger with no process to return them. Acknowledging R-PLAYER-FUNDS acknowledges this."
	// NoticeFundsInaccessibleOnSuspension is shown with the funds position of a SUSPENSION (slice-3
	// review b4): a suspension is reversible, so the balances are not stranded. They stay in the
	// ledger, untouched, but players cannot start new wagers, deposits or withdrawals on a
	// non-active subject until it is reactivated (four-eyes), and the unclaimed-funds policy
	// (HD-CTF-4) is not engaged. Distinct from NoticeFundsStranded, which belongs to closure only.
	NoticeFundsInaccessibleOnSuspension = "Suspending makes player balances inaccessible for new wagers, deposits and withdrawals until the subject is reactivated through a new four-eyes decision. The funds are not stranded: they stay in the ledger unchanged, and a suspension is reversible. Hold-bearing withdrawals continue to hold their funds and need the separate hold-resolution process."
	// NoticeFundsOmissions states what the check cannot see.
	NoticeFundsOmissions = "R-PLAYER-FUNDS does not cover in-flight deposits or open casino rounds (Q-GP-6: a casino round has no representable open state); only wallet balances, open sportsbook bets, hold-bearing withdrawals and non-zero withdrawal hold balances are counted."
	// NoticeFundsHoldBlocksClosure (ADR 0113 s4.3 / s8 Q1, LA025) is shown with the funds position of
	// a CLOSURE while any player withdrawal hold balance of the subject is not zero: acknowledging
	// R-PLAYER-FUNDS does not unblock it. A stranded hold (no withdrawal request still holds it, the
	// HD-CTF-4 population) refuses the closure too; suspension is the lever.
	NoticeFundsHoldBlocksClosure = "Closure is refused while any player withdrawal hold balance of this subject is not zero, even when no withdrawal request still holds it (LA025); acknowledging R-PLAYER-FUNDS does not change that. Resolve the hold, or suspend the subject instead: a stranded hold has no resolution process yet (HD-CTF-4)."
)

// Hash is the SHA-256 of the canonical snapshot over version, environment and the checks sorted
// by code. computed_at is NOT hashed: two evaluations of the same state hash identically, so the
// hash an approver saw is comparable with the hash at execution.
func (r Readiness) Hash() string {
	cs := append([]Check(nil), r.Checks...)
	sort.Slice(cs, func(i, j int) bool { return cs[i].Code < cs[j].Code })
	canon := struct {
		Version     int     `json:"version"`
		Environment string  `json:"environment"`
		Checks      []Check `json:"checks"`
	}{r.Version, r.Environment, cs}
	b, _ := json.Marshal(canon) // fixed struct field order: deterministic
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SnapshotJSON is the persisted snapshot ({version, computed_at, environment, checks}); codes only.
func (r Readiness) SnapshotJSON() ([]byte, error) {
	cs := append([]Check(nil), r.Checks...)
	sort.Slice(cs, func(i, j int) bool { return cs[i].Code < cs[j].Code })
	return json.Marshal(struct {
		Version     int       `json:"version"`
		ComputedAt  time.Time `json:"computed_at"`
		Environment string    `json:"environment"`
		Checks      []Check   `json:"checks"`
	}{r.Version, r.ComputedAt.UTC(), r.Environment, cs})
}

// NonPassing returns the codes of every check whose result is not pass, sorted.
func (r Readiness) NonPassing() []string {
	var out []string
	for _, c := range r.Checks {
		if c.Result != ResultPass {
			out = append(out, c.Code)
		}
	}
	sort.Strings(out)
	return out
}

// EvalInput is what a readiness evaluation depends on besides the subject: the action (which
// decides the check set) and, for activate / reactivate / ratify, the recorded licensing basis
// and the referenced jurisdictions. All server-side values or validated request content.
type EvalInput struct {
	Subject         Subject
	Action          string
	Licensing       *LicensingRecord
	JurisdictionIDs []uuid.UUID
}

// checkSetFor returns whether an action evaluates the configuration checks and whether it
// evaluates the funds check.
func checkSetFor(action string) (config, funds bool) {
	switch action {
	case ActionActivate, ActionReactivate, ActionRatify:
		return true, false
	case ActionClose, ActionSuspend:
		return false, true
	}
	return false, false
}

// EvaluateReadiness computes the readiness of a subject for an action. It is READ-ONLY: it runs
// in one read-only tenant-scoped transaction (the route-validated tenant, never a caller value)
// and writes nothing. A read failure is fail closed: the affected check is not_evaluable (never
// pass); the only error returned is an unusable subject or context.
func (s *Service) EvaluateReadiness(ctx context.Context, in EvalInput) (Readiness, error) {
	a, err := s.assess(ctx, in)
	if err != nil {
		return Readiness{}, fmt.Errorf("launchgov: evaluate readiness: %w", err)
	}
	return a.Readiness, nil
}

func sortChecks(cs []Check) {
	sort.Slice(cs, func(i, j int) bool { return cs[i].Code < cs[j].Code })
}

func ck(code, result, detail string) Check {
	return Check{Code: code, Result: result, DetailCode: detail}
}

// configChecks evaluates the tenant-data checks inside the caller's read-only transaction. Every
// query failure degrades its own check to not_evaluable.
func (s *Service) configChecks(ctx context.Context, tx pgx.Tx, in EvalInput) []Check {
	var out []Check
	now := s.now()
	type cfg struct {
		jurisdiction            uuid.UUID
		kyc, aml, rg            *string
		currencies, paymentMeth []byte
	}
	var cfgs []cfg
	cfgErr := func() error {
		if len(in.JurisdictionIDs) == 0 {
			return nil
		}
		rows, err := tx.Query(ctx, `
			SELECT jurisdiction_id, kyc_ruleset_id, aml_ruleset_id, rg_ruleset_id, allowed_currencies, allowed_payment_methods
			  FROM tenant_jurisdiction_configs
			 WHERE tenant_id = $1 AND jurisdiction_id = ANY($2)
			   AND effective_from <= $3 AND (effective_to IS NULL OR effective_to > $3)`,
			in.Subject.TenantID, in.JurisdictionIDs, now)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c cfg
			if err := rows.Scan(&c.jurisdiction, &c.kyc, &c.aml, &c.rg, &c.currencies, &c.paymentMeth); err != nil {
				return err
			}
			cfgs = append(cfgs, c)
		}
		return rows.Err()
	}()

	// R-JUR-CONFIG
	switch {
	case cfgErr != nil:
		out = append(out, ck(CheckJurisdictionConfig, ResultNotEvaluable, "read_error"))
	case len(in.JurisdictionIDs) == 0:
		out = append(out, ck(CheckJurisdictionConfig, ResultFail, "no_jurisdiction_referenced"))
	default:
		have := map[uuid.UUID]bool{}
		for _, c := range cfgs {
			have[c.jurisdiction] = true
		}
		missing := false
		for _, j := range in.JurisdictionIDs {
			if !have[j] {
				missing = true
			}
		}
		if missing {
			out = append(out, ck(CheckJurisdictionConfig, ResultFail, "config_missing_or_not_in_effect"))
		} else {
			out = append(out, ck(CheckJurisdictionConfig, ResultPass, "ok"))
		}
	}

	// R-LIC-VALID: only a licence-backed basis is evaluable.
	out = append(out, s.licenceCheck(ctx, tx, in, now))

	// R-RULESETS
	switch {
	case cfgErr != nil:
		out = append(out, ck(CheckRulesets, ResultNotEvaluable, "read_error"))
	case len(cfgs) == 0:
		out = append(out, ck(CheckRulesets, ResultFail, "no_jurisdiction_config"))
	default:
		okAll := true
		for _, c := range cfgs {
			if c.kyc == nil || *c.kyc == "" || c.aml == nil || *c.aml == "" || c.rg == nil || *c.rg == "" {
				okAll = false
			}
		}
		if okAll {
			out = append(out, ck(CheckRulesets, ResultPass, "ok"))
		} else {
			out = append(out, ck(CheckRulesets, ResultFail, "ruleset_unset"))
		}
	}

	// R-CURRENCIES: every allowed currency is an ACTIVE asset in the registry.
	switch {
	case cfgErr != nil:
		out = append(out, ck(CheckCurrencies, ResultNotEvaluable, "read_error"))
	case len(cfgs) == 0:
		out = append(out, ck(CheckCurrencies, ResultFail, "no_jurisdiction_config"))
	default:
		codes := map[string]bool{}
		bad := false
		for _, c := range cfgs {
			var l []string
			if err := json.Unmarshal(c.currencies, &l); err != nil {
				bad = true
				continue
			}
			for _, code := range l {
				codes[code] = true
			}
		}
		if len(codes) == 0 {
			bad = true
		}
		if !bad {
			var list []string
			for k := range codes {
				list = append(list, k)
			}
			var active int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM assets WHERE code = ANY($1) AND active`, list).Scan(&active); err != nil {
				out = append(out, ck(CheckCurrencies, ResultNotEvaluable, "read_error"))
				break
			}
			bad = active != len(list)
		}
		if bad {
			out = append(out, ck(CheckCurrencies, ResultFail, "currency_missing_or_inactive"))
		} else {
			out = append(out, ck(CheckCurrencies, ResultPass, "ok"))
		}
	}

	// R-PAY-METHODS: non-empty allowed methods and an approved (active) provider credential handle.
	switch {
	case cfgErr != nil:
		out = append(out, ck(CheckPaymentMethods, ResultNotEvaluable, "read_error"))
	case len(cfgs) == 0:
		out = append(out, ck(CheckPaymentMethods, ResultFail, "no_jurisdiction_config"))
	default:
		methods := 0
		for _, c := range cfgs {
			var l []any
			if err := json.Unmarshal(c.paymentMeth, &l); err == nil {
				methods += len(l)
			}
		}
		var handles int
		err := tx.QueryRow(ctx, `SELECT count(*) FROM provider_credential_handles
			WHERE tenant_id = $1 AND status = 'active' AND domain = 'payments' AND not_before <= $2 AND (not_after IS NULL OR not_after > $2)`,
			in.Subject.TenantID, now).Scan(&handles)
		switch {
		case err != nil:
			out = append(out, ck(CheckPaymentMethods, ResultNotEvaluable, "read_error"))
		case methods == 0:
			out = append(out, ck(CheckPaymentMethods, ResultFail, "no_payment_method"))
		case handles == 0:
			out = append(out, ck(CheckPaymentMethods, ResultFail, "no_approved_credential_handle"))
		default:
			out = append(out, ck(CheckPaymentMethods, ResultPass, "ok"))
		}
	}

	// R-RG-CONFIG
	var curRaw [][]byte
	for _, c := range cfgs {
		curRaw = append(curRaw, c.currencies)
	}
	out = append(out, s.rgConfigCheck(ctx, tx, in, cfgs2currencies(curRaw), cfgErr, now))

	// R-CATALOGUE: at least one enabled casino game for the brand (or tenant-wide). The
	// sportsbook catalogue is platform-wide, not brand-scoped, so it is not a brand derivation.
	var games int
	var err error
	if in.Subject.BrandID != nil {
		err = tx.QueryRow(ctx, `SELECT count(*) FROM casino_game_availability a JOIN casino_games g ON g.id = a.game_id
			WHERE a.tenant_id = $1 AND a.enabled AND g.status = 'active' AND (a.brand_id = $2 OR a.brand_id IS NULL)`,
			in.Subject.TenantID, *in.Subject.BrandID).Scan(&games)
	} else {
		err = tx.QueryRow(ctx, `SELECT count(*) FROM casino_game_availability a JOIN casino_games g ON g.id = a.game_id
			WHERE a.tenant_id = $1 AND a.enabled AND g.status = 'active'`, in.Subject.TenantID).Scan(&games)
	}
	switch {
	case err != nil:
		out = append(out, ck(CheckCatalogue, ResultNotEvaluable, "read_error"))
	case games == 0:
		out = append(out, ck(CheckCatalogue, ResultFail, "no_enabled_casino_game"))
	default:
		out = append(out, ck(CheckCatalogue, ResultPass, "ok"))
	}

	// R-MARKET-CONFIG (A8)
	out = append(out, s.marketConfigCheck(ctx, tx, in, now))
	out = append(out, s.amlChecks(ctx, tx, in)...)
	return out
}

// marketConfigCheck is R-MARKET-CONFIG (A8): the operating-market gate (internal/marketgate)
// refuses every registration, deposit and casino launch of a brand whose operating market is not
// configured (ADR 0045: a tenant must opt in; an unconfigured brand is not_configured, a DENY).
// Launching such a brand would open a brand that can serve nobody, so the approver must see it.
// The check passes when at least one country has an enabled tenant-rung policy AND resolves
// PERMITTED, at the evaluation time, for all three gated actions (registration; deposit; wagering
// on casino) - through operatingmarket.ResolveOperatingCountryPolicy itself (licence ceiling,
// tenant, brand and operation rungs), never a re-implementation. Detail codes: ok,
// no_market_configured (no enabled tenant-rung country), no_country_permits_all_operations
// (configured, but no country is open for all three), read_error (not_evaluable).
// It reads configuration only: no player evidence, and it cannot say which rung blocks.
func (s *Service) marketConfigCheck(ctx context.Context, tx pgx.Tx, in EvalInput, now time.Time) Check {
	// The resolver logic lives in marketgate.AnyCountryPermits, shared with the
	// enforcement points (SR8-2): one definition of "the market is open".
	configured, open, err := marketgate.AnyCountryPermits(ctx, tx, in.Subject.TenantID, in.Subject.BrandID, now,
		marketgate.OpRegistration, marketgate.OpDeposit, marketgate.OpCasinoBet)
	switch {
	case err != nil:
		return ck(CheckMarketConfig, ResultNotEvaluable, "read_error")
	case !configured:
		return ck(CheckMarketConfig, ResultFail, "no_market_configured")
	case !open:
		return ck(CheckMarketConfig, ResultFail, "no_country_permits_all_operations")
	}
	return ck(CheckMarketConfig, ResultPass, "ok")
}

// licenceCheck is R-LIC-VALID: licence-backed basis => EvaluateLicenceValidity at the evaluation time.
func (s *Service) licenceCheck(ctx context.Context, tx pgx.Tx, in EvalInput, now time.Time) Check {
	if in.Licensing == nil || in.Licensing.LicenceID == nil {
		return ck(CheckLicenceValid, ResultNotEvaluable, "not_licence_backed")
	}
	v, err := jurisdiction.EvaluateLicenceValidity(ctx, tx, *in.Licensing.LicenceID, now)
	if err != nil {
		return ck(CheckLicenceValid, ResultNotEvaluable, "read_error")
	}
	if v == jurisdiction.LicenceValid {
		return ck(CheckLicenceValid, ResultPass, "ok")
	}
	return ck(CheckLicenceValid, ResultFail, "licence_"+string(v))
}

// providerTierCheck is R-PROVIDER-TIER: the running process's registered components are
// production-eligible. It describes the PROCESS, not a per-tenant provider binding (none exists).
func (s *Service) providerTierCheck() Check {
	if len(s.cfg.Components) == 0 {
		return ck(CheckProviderTier, ResultNotEvaluable, "no_component_registry")
	}
	// The guard only inspects the environment string "production"; evaluating it for
	// "production" asks "would these components be refused in production".
	if err := providerkind.RefuseSyntheticInProduction("production", s.cfg.Components); err != nil {
		return ck(CheckProviderTier, ResultFail, "synthetic_environment")
	}
	return ck(CheckProviderTier, ResultPass, "ok")
}

// alertsCheck is R-ALERTS: P1 alert routing has a real (human-notifying) channel. Derived from the
// alert channel vocabulary of the code (ALERT-DELIVERY-1): today no channel kind is known to
// notify a person, so this is always fail.
func alertsCheck() Check {
	for _, k := range alerting.AllChannelKinds() {
		if alerting.KnownHumanChannelKind(k) {
			return ck(CheckAlerts, ResultPass, "ok")
		}
	}
	return ck(CheckAlerts, ResultFail, "no_human_notification_channel")
}

// fundsCheck is R-PLAYER-FUNDS (LF3): players with a non-zero wallet balance, open sportsbook
// bets and hold-bearing withdrawals of the subject's players. The counts go to r.Info (live
// display data, not hashed); the persisted check carries a closed detail code only. This check
// requires acknowledgement; the funds fall under ADR 0107 / HD-CTF-4 (no new policy here).
func fundsCheck(ctx context.Context, tx pgx.Tx, sub Subject, action string, r *Readiness) Check {
	brand := sub.BrandID
	var players, bets, holds int64
	if err := tx.QueryRow(ctx, `
		SELECT count(DISTINCT p.player_account_id)
		  FROM wallet_balance_projection p
		  JOIN player_accounts pa ON pa.id = p.player_account_id AND pa.tenant_id = p.tenant_id
		 WHERE p.tenant_id = $1 AND p.wallet_id IS NOT NULL AND ($2::uuid IS NULL OR pa.brand_id = $2)
		   AND p.credit_total <> p.debit_total`, sub.TenantID, brand).Scan(&players); err != nil {
		return ck(CheckPlayerFunds, ResultNotEvaluable, "read_error")
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM sportsbook_bets WHERE tenant_id = $1 AND status = 'open' AND ($2::uuid IS NULL OR brand_id = $2)`,
		sub.TenantID, brand).Scan(&bets); err != nil {
		return ck(CheckPlayerFunds, ResultNotEvaluable, "read_error")
	}
	holds, err := holdBearingWithdrawals(ctx, tx, sub)
	if err != nil {
		return ck(CheckPlayerFunds, ResultNotEvaluable, "read_error")
	}
	holdAccounts, err := nonZeroWithdrawalHolds(ctx, tx, sub)
	if err != nil {
		return ck(CheckPlayerFunds, ResultNotEvaluable, "read_error")
	}
	rows, err := tx.Query(ctx, `
		SELECT p.asset_code, SUM(p.credit_total - p.debit_total)::text
		  FROM wallet_balance_projection p
		  JOIN player_accounts pa ON pa.id = p.player_account_id AND pa.tenant_id = p.tenant_id
		 WHERE p.tenant_id = $1 AND p.wallet_id IS NOT NULL AND ($2::uuid IS NULL OR pa.brand_id = $2)
		 GROUP BY p.asset_code
		HAVING SUM(p.credit_total - p.debit_total) <> 0
		 ORDER BY p.asset_code`, sub.TenantID, brand)
	if err != nil {
		return ck(CheckPlayerFunds, ResultNotEvaluable, "read_error")
	}
	balances := map[string]string{}
	for rows.Next() {
		var asset, sum string
		if err := rows.Scan(&asset, &sum); err != nil {
			rows.Close()
			return ck(CheckPlayerFunds, ResultNotEvaluable, "read_error")
		}
		balances[asset] = sum
	}
	rows.Close()
	if rows.Err() != nil {
		return ck(CheckPlayerFunds, ResultNotEvaluable, "read_error")
	}
	r.Info["players_with_non_zero_balance"] = players
	r.Info["open_sportsbook_bets"] = bets
	r.Info["hold_bearing_withdrawals"] = holds
	r.Info["withdrawal_hold_accounts_non_zero"] = holdAccounts
	r.Balances = balances
	r.Notices = append(r.Notices, NoticeFundsOmissions)
	if players == 0 && bets == 0 && holds == 0 && holdAccounts == 0 && len(balances) == 0 {
		return ck(CheckPlayerFunds, ResultPass, "ok")
	}
	// The notice follows the action: only a closure strands funds. A suspension makes them
	// inaccessible until reactivation (reversible); it must never be shown the stranded text.
	if action == ActionClose {
		r.Notices = append(r.Notices, NoticeFundsStranded)
		if holdAccounts > 0 {
			r.Notices = append(r.Notices, NoticeFundsHoldBlocksClosure)
		}
	} else {
		r.Notices = append(r.Notices, NoticeFundsInaccessibleOnSuspension)
	}
	return ck(CheckPlayerFunds, ResultFail, "funds_or_open_positions_present")
}

// holdBearingWithdrawals counts withdrawal requests of the subject's players whose state still
// holds the player's funds in player_withdrawal_hold (the hold posts at 'requested' and is
// released only on a terminal state; docs/architecture/withdrawal-state-machine.md).
func holdBearingWithdrawals(ctx context.Context, tx pgx.Tx, sub Subject) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_requests
		WHERE tenant_id = $1 AND ($2::uuid IS NULL OR brand_id = $2)
		  AND state IN ('requested', 'pending_review', 'approved', 'submitted')`, sub.TenantID, sub.BrandID).Scan(&n)
	return n, err
}

// nonZeroWithdrawalHolds counts the subject's player_withdrawal_hold accounts whose balance is not
// zero (ADR 0113 s4.3, the Go arm of LA025, migration 0133): the tenant's every brand for a tenant
// subject, only wallets of the brand (wallets.brand_id) for a brand subject. The balance is REBUILT
// FROM ledger_entries, the same read as the database arm, never from wallet_balance_projection (which
// the runtime role can write: security review C1). The caller's transaction is the subject tenant's
// scope (RLS-bound).
func nonZeroWithdrawalHolds(ctx context.Context, tx pgx.Tx, sub Subject) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_accounts la
		WHERE la.tenant_id = $1 AND la.account_type = 'player_withdrawal_hold'
		  AND ($2::uuid IS NULL OR EXISTS (
		      SELECT 1 FROM wallets wl WHERE wl.id = la.wallet_id AND wl.tenant_id = la.tenant_id AND wl.brand_id = $2))
		  AND (SELECT COALESCE(sum(CASE e.direction WHEN 'credit' THEN e.amount ELSE -e.amount END), 0)
		         FROM ledger_entries e WHERE e.ledger_account_id = la.id AND e.tenant_id = la.tenant_id) <> 0`,
		sub.TenantID, sub.BrandID).Scan(&n)
	return n, err
}

// cfgs2currencies flattens the allowed_currencies JSON arrays of the in-effect jurisdiction configs into one
// sorted, de-duplicated list; a malformed array yields a nil list (the check then reports not_evaluable).
func cfgs2currencies(raw [][]byte) []string {
	seen := map[string]bool{}
	for _, b := range raw {
		var l []string
		if err := json.Unmarshal(b, &l); err != nil {
			return nil
		}
		for _, c := range l {
			seen[c] = true
		}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// rgConfigCheck is R-RG-CONFIG. It passes only when ALL of these hold for the subject:
//
//   - RG limit enforcement is switched ON for it (a tenant-wide activation row, or for a brand subject a row
//     of that brand): rg_enforcement_activations, migration 0158;
//   - for EVERY allowed currency of the referenced jurisdictions, a compliance floor is in force (not absent,
//     not a synthetic row outside dev/test) for the deposit, loss and wager kinds, composed for the subject's
//     tenant and brand; and
//   - no session-duration control is enabled or mandatory (session enforcement is NOT BUILT: a
//     control the platform cannot enforce must not be presented as in force), and none aggregates the Person's
//     concurrent session chains (it cannot be measured and fails closed at runtime).
//
// Detail codes are closed words. A read failure is not_evaluable, never a pass. The check reads the
// subject tenant's own scope only.
func (s *Service) rgConfigCheck(ctx context.Context, tx pgx.Tx, in EvalInput, currencies []string, cfgErr error, now time.Time) Check {
	if cfgErr != nil {
		return ck(CheckRGConfig, ResultNotEvaluable, "read_error")
	}
	// A savepoint, so a failing read (for example a schema older than migration 0158) degrades THIS check to
	// not_evaluable instead of poisoning the readiness transaction for the checks that follow and for its commit.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return ck(CheckRGConfig, ResultNotEvaluable, "read_error")
	}
	c := rgConfigChecks(ctx, sp, in, currencies, now)
	if c.Result == ResultNotEvaluable {
		_ = sp.Rollback(ctx)
		return c
	}
	if err := sp.Commit(ctx); err != nil {
		return ck(CheckRGConfig, ResultNotEvaluable, "read_error")
	}
	return c
}

func rgConfigChecks(ctx context.Context, tx pgx.Tx, in EvalInput, currencies []string, now time.Time) Check {
	if len(in.JurisdictionIDs) == 0 {
		return ck(CheckRGConfig, ResultFail, "no_jurisdiction_referenced")
	}
	if len(currencies) == 0 {
		return ck(CheckRGConfig, ResultFail, "no_currency")
	}
	var activated bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM rg_enforcement_activations
			WHERE tenant_id = $1 AND (brand_id IS NULL OR ($2::uuid IS NOT NULL AND brand_id = $2)))`,
		in.Subject.TenantID, in.Subject.BrandID).Scan(&activated); err != nil {
		return ck(CheckRGConfig, ResultNotEvaluable, "read_error")
	}
	if !activated {
		return ck(CheckRGConfig, ResultFail, "enforcement_not_enabled")
	}
	return rgFloorChecks(ctx, tx, in, currencies, now)
}

// rgFloorChecks is the part of R-RG-CONFIG that does not depend on the activation row: a compliance floor in force
// for every kind and allowed currency of every referenced jurisdiction, and no session control the platform cannot enforce.
func rgFloorChecks(ctx context.Context, tx pgx.Tx, in EvalInput, currencies []string, now time.Time) Check {
	rows, err := tx.Query(ctx, `SELECT code FROM jurisdictions WHERE id = ANY($1) ORDER BY code`, in.JurisdictionIDs)
	if err != nil {
		return ck(CheckRGConfig, ResultNotEvaluable, "read_error")
	}
	var jurs []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return ck(CheckRGConfig, ResultNotEvaluable, "read_error")
		}
		jurs = append(jurs, c)
	}
	rows.Close()
	if rows.Err() != nil || len(jurs) != len(in.JurisdictionIDs) {
		return ck(CheckRGConfig, ResultNotEvaluable, "read_error")
	}
	for _, jur := range jurs {
		for _, cur := range currencies {
			for _, kind := range []string{"deposit", "loss", "wager"} {
				var found bool
				if err := tx.QueryRow(ctx, `SELECT found FROM rg_limit_config_compose($1, $2, $3, $4, $5, $6)`,
					jur, in.Subject.TenantID, in.Subject.BrandID, kind, cur, now).Scan(&found); err != nil {
					return ck(CheckRGConfig, ResultNotEvaluable, "read_error")
				}
				if !found {
					return ck(CheckRGConfig, ResultFail, "floor_missing_"+kind)
				}
			}
		}
		var sessFound, sessEnabled, sessMandatory, agg *bool
		var sessScope *string
		if err := tx.QueryRow(ctx, `SELECT found, enabled, mandatory, concurrent_chain_aggregation, scope
				FROM rg_limit_config_compose($1, $2, $3, 'session_duration', NULL, $4)`,
			jur, in.Subject.TenantID, in.Subject.BrandID, now).Scan(&sessFound, &sessEnabled, &sessMandatory, &agg, &sessScope); err != nil {
			return ck(CheckRGConfig, ResultNotEvaluable, "read_error")
		}
		if sessFound != nil && *sessFound {
			if agg != nil && *agg && sessScope != nil && *sessScope == "tenant_person" {
				return ck(CheckRGConfig, ResultFail, "session_aggregation_unsupported")
			}
			if (sessEnabled != nil && *sessEnabled) || (sessMandatory != nil && *sessMandatory) {
				return ck(CheckRGConfig, ResultFail, "session_limit_not_enforced")
			}
		}
	}
	return ck(CheckRGConfig, ResultPass, "ok")
}

// ---- AML readiness (ADR 0116 s6 / S7, A7) ------------------------------------------------------------------------------

// amlData is what the AML readiness reader (migration 0154 aml_readiness) returned for the subject's tenant.
type amlData struct {
	OwnerScope string
	QuorumMet  bool
	// Total is the number of DISTINCT referenced jurisdictions; WithPolicy those with an effective screening policy.
	Total, WithPolicy int
}

// amlChecks evaluates R-AML-QUORUM, R-AML-SCREENING and R-AML-MONITORING inside the caller's read-only tenant transaction.
// A read failure degrades the DATABASE-derived checks to not_evaluable (never pass). R-AML-MONITORING needs no read.
func (s *Service) amlChecks(ctx context.Context, tx pgx.Tx, in EvalInput) []Check {
	var d amlData
	// A SAVEPOINT, so a failing read (the function missing, a fault) degrades only the AML items and never aborts the
	// caller's transaction (the readiness commit must survive it).
	sp, err := tx.Begin(ctx)
	if err == nil {
		err = sp.QueryRow(ctx, `SELECT o_owner_scope, o_quorum_met, o_jurisdictions_total, o_jurisdictions_with_policy FROM aml_readiness($1, $2)`,
			in.Subject.TenantID, in.JurisdictionIDs).Scan(&d.OwnerScope, &d.QuorumMet, &d.Total, &d.WithPolicy)
		if err != nil {
			_ = sp.Rollback(ctx)
		} else if cerr := sp.Commit(ctx); cerr != nil {
			err = cerr
		}
	}
	return amlChecksFrom(d, err, len(in.JurisdictionIDs), s.cfg.Components, s.cfg.Environment)
}

// AMLPlatformScopeSuffix is appended to the R-AML-QUORUM detail codes when the tenant's AML owner scope is the platform.
const AMLPlatformScopeSuffix = "_platform_scope"

// The registration the screening check looks for in the process component list (the same list the synthetic-component
// startup guard checked).
const (
	amlComponentDomain = "aml"
	amlComponentName   = "screening_provider"
)

// amlChecksFrom is the pure part of amlChecks (unit-tested without a database).
//
//   - R-AML-QUORUM: pass only when at least two distinct live officer Persons of the tenant's owner scope hold an in-force
//     aml_officer grant. The reader returns a boolean only.
//   - R-AML-SCREENING: pass only when (a) a screening provider is registered and, in production, is production-eligible (a
//     MOCK counts as configured ONLY where a mock is allowed: any non-production environment), and (b) every referenced
//     jurisdiction has an effective screening policy. Unconfigured is never a pass.
//   - R-AML-MONITORING: ALWAYS fail. Transaction monitoring (ADR 0116 S4) is NOT IMPLEMENTED; this makes the gap an explicit,
//     acknowledged item so a brand activation cannot silently ignore it. It becomes evaluable only with the S4 slice.
func amlChecksFrom(d amlData, readErr error, referenced int, comps []providerkind.Registration, env string) []Check {
	out := []Check{ck(CheckAMLMonitoring, ResultFail, "monitoring_not_implemented")}
	if readErr != nil {
		return append(out, ck(CheckAMLQuorum, ResultNotEvaluable, "read_error"), ck(CheckAMLScreening, ResultNotEvaluable, "read_error"))
	}
	// SR7-6: for a PLATFORM-owned scope the detail code carries the scope (a tenant-scoped viewer is shown "see_platform" for it,
	// see httpserver coarsenForTenant: the platform officer quorum is platform process detail, not the tenant's).
	suffix := ""
	if d.OwnerScope == "platform" {
		suffix = AMLPlatformScopeSuffix
	}
	if d.QuorumMet {
		out = append(out, ck(CheckAMLQuorum, ResultPass, "ok"+suffix))
	} else {
		out = append(out, ck(CheckAMLQuorum, ResultFail, "quorum_not_met"+suffix))
	}
	return append(out, amlScreeningCheck(d, referenced, comps, env))
}

func amlScreeningCheck(d amlData, referenced int, comps []providerkind.Registration, env string) Check {
	if len(comps) == 0 {
		return ck(CheckAMLScreening, ResultNotEvaluable, "no_component_registry")
	}
	var reg *providerkind.Registration
	for i := range comps {
		if comps[i].Domain == amlComponentDomain && comps[i].Name == amlComponentName && comps[i].Component != nil {
			reg = &comps[i]
			break
		}
	}
	if reg == nil {
		return ck(CheckAMLScreening, ResultFail, "no_screening_provider")
	}
	// The guard decides eligibility; an unmarked component is as ineligible as a synthetic one.
	eligible := providerkind.RefuseSyntheticInProduction("production", []providerkind.Registration{*reg}) == nil
	if !eligible && env == "production" {
		return ck(CheckAMLScreening, ResultFail, "synthetic_provider_in_production")
	}
	if referenced == 0 || d.Total == 0 {
		return ck(CheckAMLScreening, ResultFail, "no_jurisdiction_referenced")
	}
	if d.WithPolicy < d.Total {
		return ck(CheckAMLScreening, ResultFail, "policy_missing_for_jurisdiction")
	}
	if !eligible {
		return ck(CheckAMLScreening, ResultPass, "ok_mock_provider")
	}
	return ck(CheckAMLScreening, ResultPass, "ok")
}

// RGActivationReadiness answers whether RG limit enforcement MAY be switched on for a tenant or one brand (SR6-1 guard):
// every jurisdiction with a configuration in effect for the tenant has a compliance floor for the deposit, loss and wager
// kinds in every allowed currency, and no session control that the platform cannot enforce is enabled. It is the floor part of
// R-RG-CONFIG, without the "already switched on" test (that is what is about to be created). Read-only; the caller passes a
// tenant-scoped transaction. Result pass/fail/not_evaluable, never an error for a failing precondition.
func RGActivationReadiness(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, brandID *uuid.UUID, now time.Time) Check {
	rows, err := tx.Query(ctx, `SELECT jurisdiction_id, allowed_currencies FROM tenant_jurisdiction_configs
		 WHERE tenant_id = $1 AND effective_from <= $2 AND (effective_to IS NULL OR effective_to > $2)`, tenantID, now)
	if err != nil {
		return ck(CheckRGConfig, ResultNotEvaluable, "read_error")
	}
	var ids []uuid.UUID
	var raw [][]byte
	seen := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		var cur []byte
		if err := rows.Scan(&id, &cur); err != nil {
			rows.Close()
			return ck(CheckRGConfig, ResultNotEvaluable, "read_error")
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
		raw = append(raw, cur)
	}
	rows.Close()
	if rows.Err() != nil {
		return ck(CheckRGConfig, ResultNotEvaluable, "read_error")
	}
	if len(ids) == 0 {
		return ck(CheckRGConfig, ResultFail, "no_jurisdiction_referenced")
	}
	currencies := cfgs2currencies(raw)
	if len(currencies) == 0 {
		return ck(CheckRGConfig, ResultFail, "no_currency")
	}
	return rgFloorChecks(ctx, tx, EvalInput{Subject: Subject{TenantID: tenantID, BrandID: brandID}, JurisdictionIDs: ids}, currencies, now)
}

package launchgov

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Lic7 readiness items (licensing completion, ledger-finance review condition C1, audit G6/G7/G9). Both are
// ACKNOWLEDGEABLE like every other readiness item: a non-passing result must be acknowledged by the request, it
// never refuses by itself (ADR 0112 5.2), and neither is a hard precondition.
const (
	// CheckRiskRulesLegacy (condition C1): ACTIVE stored risk_rules visible to the tenant (its own and platform-wide)
	// that the authoring refusals of lic3 would no longer accept. See riskRulesLegacyCheck.
	CheckRiskRulesLegacy = "R-RISK-RULES-LEGACY"
	// CheckKYCPolicy (audit G9): the tenant's licensing jurisdiction has no active KYC enforcement policy, so KYC
	// thresholds are dormant. See kycPolicyCheck.
	CheckKYCPolicy = "R-KYC-POLICY"
)

// Info keys (live display data, not hashed) and Refs keys added by the two checks.
const (
	InfoRiskLegacyJurisdictionScoped = "risk_rules_legacy_jurisdiction_scoped"
	InfoRiskLegacyDepositOperation   = "risk_rules_legacy_deposit_operation"
	InfoKYCActiveDeposit             = "kyc_active_policies_deposit"
	InfoKYCActiveCasinoPlay          = "kyc_active_policies_casino_play"
	InfoKYCActiveSportsbookPlay      = "kyc_active_policies_sportsbook_play"

	RefsRiskLegacyJurisdictionScoped = "risk_rules_legacy_jurisdiction_scoped"
	RefsRiskLegacyDepositOperation   = "risk_rules_legacy_deposit_operation"

	// lic23 (SN-1): the number of jurisdictions R-KYC-POLICY evaluated (licence + approved bases), and the jurisdiction ids with no
	// active policy at all (the check fails) or with some operation dormant (ok_partial_operations_dormant).
	InfoKYCJurisdictionsEvaluated = "kyc_policy_jurisdictions_evaluated"
	RefsKYCDormantJurisdictions   = "kyc_policy_dormant_jurisdictions"
	RefsKYCPartialJurisdictions   = "kyc_policy_partial_jurisdictions"

	// MaxRefsPerKey bounds the rule ids returned per Refs key; the Info count is always the full count.
	MaxRefsPerKey = 50
)

// riskLegacyRule is one stored rule flagged by R-RISK-RULES-LEGACY (id and the two flags only).
type riskLegacyRule struct {
	ID        uuid.UUID
	Jurisdict bool
	DepositOp bool
}

// riskRulesLegacyCheck is R-RISK-RULES-LEGACY. It lists the ACTIVE (status active, not past effective_until)
// risk_rules visible in the tenant's own scope (the tenant's rows plus platform-wide rows, risk_rules RLS policy
// tenant_and_platform_read, migration 0041) that either
//
//	(a) carry a non-empty jurisdiction_code. internal/risk.Evaluate runs its missing-scope gate over EVERY effective
//	    rule visible for the operation, before matching, and returns ErrMissingJurisdiction when the request
//	    carries no jurisdiction_code. Every caller takes the code from the player-scoped jurisdiction resolver, which
//	    is unresolved(no_signal) for every player (HDR-J-1 / HDR-J-3 pending), and the withdrawal gate supplies none
//	    at all. So such a rule REFUSES EVERY evaluation of its operation (casino launch / casino bet / sportsbook
//	    bet / bonus grant, and every withdrawal request and payout dispatch for a withdrawal rule) rather than
//	    never firing. If a caller ever supplied a code that differs from the rule's, the rule would merely never
//	    match; neither outcome enforces the rule as written. The gate ignores the rule's brand, so a rule scoped to
//	    another brand of the same tenant counts too; or
//	(b) have operation = 'deposit'. The deposit path does not call risk.Evaluate, so the rule is stored and never
//	    enforced (false assurance, audit G7).
//
// Since lic3, CreateRule refuses both shapes, so these rows can only be legacy rows (or a raw insert). Disabling a
// rule is always possible (DisableRule has no new check). The check never changes a rule and never reads a player.
//
// Result: pass/ok when none; fail with a closed detail code otherwise (jurisdiction_scoped_rules_present,
// deposit_rules_present or both_present); the counts go to Info and the rule ids (ids only, no other column, capped at
// MaxRefsPerKey per key) to Refs. A read failure is not_evaluable (never pass). It runs in a savepoint so a failing
// read cannot poison the readiness transaction.
func (s *Service) riskRulesLegacyCheck(ctx context.Context, tx pgx.Tx, in EvalInput, r *Readiness, now time.Time) Check {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return ck(CheckRiskRulesLegacy, ResultNotEvaluable, "read_error")
	}
	rules, err := readLegacyRiskRules(ctx, sp, in.Subject.TenantID, now)
	if err != nil {
		_ = sp.Rollback(ctx)
		return ck(CheckRiskRulesLegacy, ResultNotEvaluable, "read_error")
	}
	if err := sp.Commit(ctx); err != nil {
		return ck(CheckRiskRulesLegacy, ResultNotEvaluable, "read_error")
	}
	return riskRulesLegacyFrom(rules, r)
}

func readLegacyRiskRules(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, now time.Time) ([]riskLegacyRule, error) {
	// The tenant predicate is explicit although risk_rules RLS already restricts the read to this tenant plus
	// platform-wide rows: the correctness of the check does not rest on RLS alone.
	rows, err := tx.Query(ctx, `
		SELECT id, (jurisdiction_code IS NOT NULL AND jurisdiction_code <> ''), (operation = 'deposit')
		  FROM risk_rules
		 WHERE status = 'active'
		   AND (effective_until IS NULL OR effective_until > $2)
		   AND (tenant_id = $1 OR tenant_id IS NULL)
		   AND ((jurisdiction_code IS NOT NULL AND jurisdiction_code <> '') OR operation = 'deposit')
		 ORDER BY id`, tenantID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []riskLegacyRule
	for rows.Next() {
		var x riskLegacyRule
		if err := rows.Scan(&x.ID, &x.Jurisdict, &x.DepositOp); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// riskRulesLegacyFrom is the pure part (unit-tested without a database).
func riskRulesLegacyFrom(rules []riskLegacyRule, r *Readiness) Check {
	var jur, dep int64
	var jurIDs, depIDs []string
	for _, x := range rules {
		if x.Jurisdict {
			jur++
			if len(jurIDs) < MaxRefsPerKey {
				jurIDs = append(jurIDs, x.ID.String())
			}
		}
		if x.DepositOp {
			dep++
			if len(depIDs) < MaxRefsPerKey {
				depIDs = append(depIDs, x.ID.String())
			}
		}
	}
	if r != nil {
		if r.Info == nil {
			r.Info = map[string]int64{}
		}
		r.Info[InfoRiskLegacyJurisdictionScoped] = jur
		r.Info[InfoRiskLegacyDepositOperation] = dep
		if len(jurIDs) > 0 || len(depIDs) > 0 {
			if r.Refs == nil {
				r.Refs = map[string][]string{}
			}
			if len(jurIDs) > 0 {
				r.Refs[RefsRiskLegacyJurisdictionScoped] = jurIDs
			}
			if len(depIDs) > 0 {
				r.Refs[RefsRiskLegacyDepositOperation] = depIDs
			}
		}
	}
	switch {
	case jur > 0 && dep > 0:
		return ck(CheckRiskRulesLegacy, ResultFail, "both_present")
	case jur > 0:
		return ck(CheckRiskRulesLegacy, ResultFail, "jurisdiction_scoped_rules_present")
	case dep > 0:
		return ck(CheckRiskRulesLegacy, ResultFail, "deposit_rules_present")
	}
	return ck(CheckRiskRulesLegacy, ResultPass, "ok")
}

// kycPolicyCheck is R-KYC-POLICY (audit G9, ADR 0096 s3.2). KYC thresholds are read from kyc_enforcement_policies
// keyed by a jurisdiction (never a player signal): lic23 (SN-1) evaluates the tenant's LICENCE jurisdiction AND the jurisdictions of
// its approved operating bases, each separately; the check fails when any of them has no active policy. With no licence bound, or with no ACTIVE policy
// for that jurisdiction, every evaluation resolves not_required (dormant): no cumulative-deposit threshold and no play
// gate applies. Only the structural rule "a withdrawal needs a passed KYC" and the market gate's verified-residence
// precondition remain. Launching an active tenant in that state is a default-allow the approver must see.
//
// The data model: cumulative_deposit rows govern the DEPOSIT operation (one per asset); play rows govern casino_play and
// sportsbook_play separately; WITHDRAWAL has no policy row (structural, compiled in), so it is never dormant and is not
// counted; edd_amount and registration_tier cannot be activated (not evaluator-wired) and are not counted. The
// per-operation counts of active policies go to Info.
//
// Result: fail/no_licence_jurisdiction (no licence jurisdiction, so no policy can apply at all), fail/no_active_policy
// (zero active policies for deposit, casino_play and sportsbook_play), pass/ok_partial_operations_dormant (at least one
// operation has a policy but not all three: see Info for which have none) or pass/ok (all three). No threshold value is
// read, invented or defaulted. A read failure is not_evaluable. It runs in a savepoint.
func (s *Service) kycPolicyCheck(ctx context.Context, tx pgx.Tx, in EvalInput, r *Readiness) Check {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return ck(CheckKYCPolicy, ResultNotEvaluable, "read_error")
	}
	// lic23 (SN-1): the set of jurisdictions whose rulebook can govern a scope of this tenant: its licence jurisdiction UNION the
	// jurisdictions of its approved, in-force operating bases (a basis-governed scope uses the basis jurisdiction's KYC policies,
	// lic14). Each one is checked.
	var perJur []kycJurisdictionCounts
	jurs, err := governedJurisdictions(ctx, sp, in.Subject.TenantID)
	for i := 0; err == nil && i < len(jurs); i++ {
		c := kycJurisdictionCounts{ID: jurs[i]}
		err = sp.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE trigger_type = 'cumulative_deposit'),
			       count(*) FILTER (WHERE trigger_type = 'play' AND play_operation = 'casino_play'),
			       count(*) FILTER (WHERE trigger_type = 'play' AND play_operation = 'sportsbook_play')
			  FROM kyc_enforcement_policies
			 WHERE licensing_jurisdiction_id = $1 AND status = 'active'`, jurs[i]).Scan(&c.Deposit, &c.Casino, &c.Sports)
		perJur = append(perJur, c)
	}
	if err != nil {
		_ = sp.Rollback(ctx)
		return ck(CheckKYCPolicy, ResultNotEvaluable, "read_error")
	}
	if err := sp.Commit(ctx); err != nil {
		return ck(CheckKYCPolicy, ResultNotEvaluable, "read_error")
	}
	check, dormant, partial := kycPolicyFromSet(perJur)
	if r != nil {
		if r.Info == nil {
			r.Info = map[string]int64{}
		}
		// The Info counts are the MINIMUM over the evaluated jurisdictions (so any dormant jurisdiction shows as 0); with one
		// jurisdiction (the licence-only tenant) they are exactly that jurisdiction's counts, as before.
		var dep, cas, spo int64
		for i, c := range perJur {
			if i == 0 || c.Deposit < dep {
				dep = c.Deposit
			}
			if i == 0 || c.Casino < cas {
				cas = c.Casino
			}
			if i == 0 || c.Sports < spo {
				spo = c.Sports
			}
		}
		r.Info[InfoKYCActiveDeposit], r.Info[InfoKYCActiveCasinoPlay], r.Info[InfoKYCActiveSportsbookPlay] = dep, cas, spo
		r.Info[InfoKYCJurisdictionsEvaluated] = int64(len(perJur))
		if len(dormant) > 0 || len(partial) > 0 {
			if r.Refs == nil {
				r.Refs = map[string][]string{}
			}
			if len(dormant) > 0 {
				r.Refs[RefsKYCDormantJurisdictions] = capIDs(dormant)
			}
			if len(partial) > 0 {
				r.Refs[RefsKYCPartialJurisdictions] = capIDs(partial)
			}
		}
	}
	return check
}

// kycJurisdictionCounts is the number of ACTIVE policies per evaluated operation for one jurisdiction.
type kycJurisdictionCounts struct {
	ID                      uuid.UUID
	Deposit, Casino, Sports int64
}

// kycPolicyFromSet is the pure aggregate (unit-tested without a database): the check fails when the set is empty or when ANY
// jurisdiction has no active policy for any evaluated operation; it passes (partial) when every jurisdiction has at least one but
// some operation is dormant somewhere; otherwise ok. dormant / partial list the jurisdiction ids (display data) per class.
func kycPolicyFromSet(perJur []kycJurisdictionCounts) (check Check, dormant, partial []string) {
	if len(perJur) == 0 {
		return kycPolicyFrom(false, 0, 0, 0), nil, nil
	}
	worst := ""
	for _, c := range perJur {
		switch kycPolicyFrom(true, c.Deposit, c.Casino, c.Sports).DetailCode {
		case "no_active_policy":
			dormant = append(dormant, c.ID.String())
			worst = "no_active_policy"
		case "ok_partial_operations_dormant":
			partial = append(partial, c.ID.String())
			if worst == "" {
				worst = "ok_partial_operations_dormant"
			}
		}
	}
	switch worst {
	case "no_active_policy":
		return ck(CheckKYCPolicy, ResultFail, "no_active_policy"), dormant, partial
	case "ok_partial_operations_dormant":
		return ck(CheckKYCPolicy, ResultPass, "ok_partial_operations_dormant"), dormant, partial
	}
	return ck(CheckKYCPolicy, ResultPass, "ok"), nil, nil
}

// governedJurisdictions is the set {the tenant's licence jurisdiction} UNION {the jurisdictions of its approved (executed change
// request), active, in-force operating bases; a licence basis resolves through its licence}. A basis whose jurisdiction cannot be
// read is an ERROR (not_evaluable), never an omission. Order: licence first, then by id.
func governedJurisdictions(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	seen := map[uuid.UUID]bool{}
	lic, err := licenceJurisdiction(ctx, tx, tenantID)
	if err != nil {
		return nil, err
	}
	if lic != nil {
		out, seen[*lic] = append(out, *lic), true
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT COALESCE(b.jurisdiction_id, bl.jurisdiction_id)
		  FROM operating_bases b LEFT JOIN licences bl ON bl.id = b.licence_id
		 WHERE b.tenant_id = $1 AND b.status = 'active' AND b.change_request_id IS NOT NULL
		   AND b.effective_from <= clock_timestamp() AND (b.effective_to IS NULL OR b.effective_to > clock_timestamp())
		 ORDER BY 1`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var j *uuid.UUID
		if err := rows.Scan(&j); err != nil {
			return nil, err
		}
		if j == nil {
			return nil, fmt.Errorf("launchgov: an operating basis has no readable jurisdiction")
		}
		if !seen[*j] {
			out, seen[*j] = append(out, *j), true
		}
	}
	return out, rows.Err()
}

// kycPolicyFrom is the pure part (unit-tested without a database).
func kycPolicyFrom(haveLicenceJurisdiction bool, deposit, casino, sports int64) Check {
	switch {
	case !haveLicenceJurisdiction:
		return ck(CheckKYCPolicy, ResultFail, "no_licence_jurisdiction")
	case deposit == 0 && casino == 0 && sports == 0:
		return ck(CheckKYCPolicy, ResultFail, "no_active_policy")
	case deposit == 0 || casino == 0 || sports == 0:
		return ck(CheckKYCPolicy, ResultPass, "ok_partial_operations_dormant")
	}
	return ck(CheckKYCPolicy, ResultPass, "ok")
}

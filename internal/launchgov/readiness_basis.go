package launchgov

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

// Lic9 readiness item (migration 0180, snapshot version 7). ACKNOWLEDGEABLE like every other readiness item: a non-passing result
// must be acknowledged by the request, it never refuses by itself (ADR 0112 5.2), and it is NOT a hard precondition (a unit test
// pins that hard.go, decide.go and execute.go never name it). It reports; it never decides a market and never states a legal position.
const (
	// CheckOperatingBasis (lic9): for the subject being launched, whether every country with an enabled tenant-rung policy
	// resolves through an EXPLICIT, approved operating basis (operating_bases) for the gated operations, and which bases are due
	// for review soon or overdue. A country that resolves through the tenant's own licence chain (no basis row) is reported, not
	// failed: that is the pre-lic9 model and stays valid.
	CheckOperatingBasis = "R-OPERATING-BASIS"
)

// Info and Refs keys (live display data, not hashed) of R-OPERATING-BASIS.
const (
	InfoBasisCountriesEnabled   = "operating_basis_countries_enabled"
	InfoBasisCountriesExplicit  = "operating_basis_countries_explicit"
	InfoBasisCountriesLicence   = "operating_basis_countries_licence_chain"
	InfoBasisNoLicenceInForce   = "operating_basis_no_licence_in_force"
	InfoBasisReviewDueSoon      = "operating_basis_review_due_soon"
	InfoBasisReviewOverdue      = "operating_basis_review_overdue"
	RefsBasisReviewDueSoon      = "operating_basis_review_due_soon"
	RefsBasisReviewOverdue      = "operating_basis_review_overdue"
	basisReviewSoonWindow       = 30 * 24 * time.Hour
	basisCoverageWagerProduct   = "casino"
	basisOpRegistration         = "registration"
	basisOpDeposit              = "deposit"
	basisOpWagering             = "wagering"
	basisDetailOK               = "ok"
	basisDetailOKLicenceChain   = "ok_some_via_licence_chain"
	basisDetailNoCountry        = "no_enabled_country"
	basisDetailReviewOverdue    = "review_overdue"
	basisDetailReviewDueSoon    = "review_due_soon"
	basisDetailReadError        = "read_error"
	basisCountriesScannedBounds = 250
)

// operatingBasisCheck is R-OPERATING-BASIS. It runs in a savepoint (a failing read degrades only this item to not_evaluable).
func (s *Service) operatingBasisCheck(ctx context.Context, tx pgx.Tx, in EvalInput, r *Readiness, now time.Time) Check {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return ck(CheckOperatingBasis, ResultNotEvaluable, basisDetailReadError)
	}
	policies, err := operatingmarket.ListCurrentOperatingCountryPolicies(ctx, sp, in.Subject.TenantID, now)
	var bases []operatingmarket.BasisRecord
	if err == nil {
		bases, err = operatingmarket.ListInForceOperatingBases(ctx, sp, in.Subject.TenantID, now)
	}
	if err != nil {
		_ = sp.Rollback(ctx)
		return ck(CheckOperatingBasis, ResultNotEvaluable, basisDetailReadError)
	}
	if err := sp.Commit(ctx); err != nil {
		return ck(CheckOperatingBasis, ResultNotEvaluable, basisDetailReadError)
	}
	var countries []string
	seen := map[string]bool{}
	for _, p := range policies {
		if p.Scope == operatingmarket.ScopeTenant && p.Status == operatingmarket.StatusActive && p.State == operatingmarket.StateEnabled &&
			!seen[p.CountryCode] && len(countries) < basisCountriesScannedBounds {
			seen[p.CountryCode] = true
			countries = append(countries, p.CountryCode)
		}
	}
	return operatingBasisFrom(countries, bases, in.Subject.BrandID, now, r)
}

// operatingBasisFrom is the pure part (unit-tested without a database).
//
// A country is "explicit" when, for each of registration, deposit and casino wagering, an ACTIVE basis row backed by an executed
// change request (change_request_id set) is in force for the subject's scope (tenant-wide, or the subject brand's own) and does
// not restrict that operation's product. Withdrawn, unapproved and owner-path rows never count. A review is overdue when
// needs_rereview is set or the review date has passed (the day after review_due_at), due soon within 30 days; both are listed
// by basis id only (never a reference, name or note).
//
// Result: pass/no_enabled_country (nothing to report; R-MARKET-CONFIG covers an unconfigured market), fail/review_overdue,
// fail/review_due_soon, pass/ok_some_via_licence_chain (at least one enabled country is not governed by an explicit basis: it
// resolves through the tenant's own licence ceiling, the pre-lic9 model), pass/ok.
func operatingBasisFrom(countries []string, bases []operatingmarket.BasisRecord, brand *uuid.UUID, now time.Time, r *Readiness) Check {
	if r != nil && r.Info == nil {
		r.Info = map[string]int64{}
	}
	today := now.UTC().Truncate(24 * time.Hour)
	covers := map[string]map[string]bool{}
	var noLicence int64
	var soon, overdue []string
	for _, b := range bases {
		if b.Status != operatingmarket.StatusActive || b.ChangeRequestID == nil {
			continue
		}
		if b.BrandID != nil && (brand == nil || *b.BrandID != *brand) {
			continue
		}
		if b.Kind == operatingmarket.BasisNoLicenceDocumented {
			noLicence++
		}
		if b.NeedsRereview || (b.ReviewDueAt != nil && today.After(*b.ReviewDueAt)) {
			overdue = append(overdue, b.ID.String())
		} else if b.ReviewDueAt != nil && b.ReviewDueAt.Sub(today) <= basisReviewSoonWindow {
			soon = append(soon, b.ID.String())
		}
		if b.ProductCode != nil && !(b.OperationCode == basisOpWagering && *b.ProductCode == basisCoverageWagerProduct) {
			continue
		}
		if b.OperationCode == basisOpWagering && containsStr(b.RestrictedProductCodes, basisCoverageWagerProduct) {
			continue
		}
		if covers[b.CountryCode] == nil {
			covers[b.CountryCode] = map[string]bool{}
		}
		covers[b.CountryCode][b.OperationCode] = true
	}
	var explicit, viaLicence int64
	for _, c := range countries {
		m := covers[c]
		if m[basisOpRegistration] && m[basisOpDeposit] && m[basisOpWagering] {
			explicit++
		} else {
			viaLicence++
		}
	}
	sort.Strings(soon)
	sort.Strings(overdue)
	if r != nil {
		r.Info[InfoBasisCountriesEnabled] = int64(len(countries))
		r.Info[InfoBasisCountriesExplicit] = explicit
		r.Info[InfoBasisCountriesLicence] = viaLicence
		r.Info[InfoBasisNoLicenceInForce] = noLicence
		r.Info[InfoBasisReviewDueSoon] = int64(len(soon))
		r.Info[InfoBasisReviewOverdue] = int64(len(overdue))
		if len(soon) > 0 || len(overdue) > 0 {
			if r.Refs == nil {
				r.Refs = map[string][]string{}
			}
			if len(soon) > 0 {
				r.Refs[RefsBasisReviewDueSoon] = capIDs(soon)
			}
			if len(overdue) > 0 {
				r.Refs[RefsBasisReviewOverdue] = capIDs(overdue)
			}
		}
	}
	switch {
	case len(overdue) > 0:
		return ck(CheckOperatingBasis, ResultFail, basisDetailReviewOverdue)
	case len(soon) > 0:
		return ck(CheckOperatingBasis, ResultFail, basisDetailReviewDueSoon)
	case len(countries) == 0:
		return ck(CheckOperatingBasis, ResultPass, basisDetailNoCountry)
	case viaLicence > 0:
		return ck(CheckOperatingBasis, ResultPass, basisDetailOKLicenceChain)
	}
	return ck(CheckOperatingBasis, ResultPass, basisDetailOK)
}

func capIDs(ids []string) []string {
	if len(ids) > MaxRefsPerKey {
		return ids[:MaxRefsPerKey]
	}
	return ids
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

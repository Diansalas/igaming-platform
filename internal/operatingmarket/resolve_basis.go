package operatingmarket

// lic9 (migration 0180): the explicit OPERATING BASIS step of the resolution algorithm (ADR 0045 §3.5, STEP 0 / STEP 1).
//
// A scope is (tenant, brand?, country, operation, product?). When operating_bases holds a row for it, the permission relies on
// THAT row's basis instead of the tenant's single licence:
//
//   - platform_licence / partner_licence: the row names the governing licence; the EXISTING licence validity and
//     licence_country_ceilings logic (applyLicenceCeiling, the pre-lic9 STEP 1 verbatim) then applies to THAT licence.
//   - no_licence_documented: an explicit, approved, reviewed determination. Permitted at this step ONLY while it is active, backed
//     by an executed change request, in its window, not past review_due_at and not flagged needs_rereview; and it is a
//     configuration_conflict (fail closed) if the tenant's own licence ALSO has a ceiling row for the country (two bases, silently
//     ambiguous).
//
// A scope with NO operating_bases row resolves exactly as before lic9 (the tenant's licence chain and ceilings). Country
// configuration alone is never proof of legality: nothing here treats the presence of a policy row as a basis.
//
// Selection: the most specific in-force row wins (brand-specific before tenant-wide, then product-specific before all-products). A
// WITHDRAWN more-specific row masks the broader one (fail closed). Two in-force rows of the same specificity are a conflict. If rows
// exist for the scope but none is in force at AsOf the scope is not permitted (not yet effective / not in force), never silently
// the legacy path.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
)

// BasisKind is the closed operating-basis vocabulary (operating_bases.basis_kind).
type BasisKind string

const (
	BasisPlatformLicence     BasisKind = "platform_licence"
	BasisPartnerLicence      BasisKind = "partner_licence"
	BasisNoLicenceDocumented BasisKind = "no_licence_documented"
)

func parseBasisKind(s string) (BasisKind, error) {
	switch s {
	case string(BasisPlatformLicence):
		return BasisPlatformLicence, nil
	case string(BasisPartnerLicence):
		return BasisPartnerLicence, nil
	case string(BasisNoLicenceDocumented):
		return BasisNoLicenceDocumented, nil
	default:
		return "", fmt.Errorf("%w: unrecognized basis_kind %q", ErrInvalidInput, s)
	}
}

// BasisReason is the closed diagnostic reason an operating basis did not permit (Explanation / ChainStep only; never on Result).
type BasisReason string

const (
	BasisReasonNone              BasisReason = ""
	BasisReasonWithdrawn         BasisReason = "basis_withdrawn"
	BasisReasonNotYetEffective   BasisReason = "basis_not_yet_effective"
	BasisReasonNotInForce        BasisReason = "basis_not_in_force"
	BasisReasonNotApproved       BasisReason = "basis_not_approved"
	BasisReasonReviewOverdue     BasisReason = "basis_review_overdue"
	BasisReasonNeedsRereview     BasisReason = "basis_needs_rereview"
	BasisReasonProductRestricted BasisReason = "basis_product_restricted"
	BasisReasonCeilingConflict   BasisReason = "basis_licence_ceiling_conflict"
)

type basisSelState int

const (
	basisSelNone     basisSelState = iota // no operating_bases row for the scope: the legacy path
	basisSelRow                           // a permitting-candidate row (kind-specific checks follow)
	basisSelBlocked                       // a row (or the lack of an in-force one) that denies, with a reason
	basisSelConflict                      // two in-force rows of the same specificity
	basisSelInvalid                       // an unparsable stored value
)

type basisSelection struct {
	state        basisSelState
	reason       BasisReason
	id           uuid.UUID
	kind         BasisKind
	licenceID    uuid.UUID
	jurisdiction uuid.UUID
	status       Status
	from         time.Time
	to           *time.Time
	reviewDue    *time.Time
}

type basisCandidate struct {
	id             uuid.UUID
	brandID        *uuid.UUID
	productCode    *string
	kind           string
	licenceID      *uuid.UUID
	jurisdictionID *uuid.UUID
	status         string
	from           time.Time
	to             *time.Time
	reviewDue      *time.Time
	rereview       bool
	approved       bool
	restricted     []string
}

func selectBasis(ctx context.Context, q ReadOnlyQuerier, p Query) (basisSelection, error) {
	rows, err := q.Query(ctx, `
		SELECT id, brand_id, product_code, basis_kind, licence_id, jurisdiction_id, status, effective_from, effective_to,
		       review_due_at, needs_rereview, change_request_id IS NOT NULL, restricted_product_codes
		  FROM operating_bases
		 WHERE tenant_id = $1 AND country_code = $2 AND operation_code = $3
		   AND (brand_id = $4 OR brand_id IS NULL)
		   AND (product_code = $5 OR product_code IS NULL)
		   AND effective_from <= $6 AND (effective_to IS NULL OR effective_to > $6)
		 ORDER BY (brand_id IS NOT NULL) DESC, (product_code IS NOT NULL) DESC, effective_from DESC`,
		p.TenantID, p.CountryCode, p.OperationCode, p.BrandID, p.ProductCode, p.AsOf)
	if err != nil {
		return basisSelection{}, fmt.Errorf("operatingmarket: read operating basis: %w", err)
	}
	var cands []basisCandidate
	for rows.Next() {
		var c basisCandidate
		if err := rows.Scan(&c.id, &c.brandID, &c.productCode, &c.kind, &c.licenceID, &c.jurisdictionID, &c.status, &c.from, &c.to,
			&c.reviewDue, &c.rereview, &c.approved, &c.restricted); err != nil {
			rows.Close()
			return basisSelection{}, fmt.Errorf("operatingmarket: scan operating basis: %w", err)
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return basisSelection{}, fmt.Errorf("operatingmarket: read operating basis: %w", err)
	}

	if len(cands) == 0 {
		return classifyBasisAbsence(ctx, q, p)
	}
	for i := 0; i < len(cands); i++ {
		for j := i + 1; j < len(cands); j++ {
			if (cands[i].brandID != nil) == (cands[j].brandID != nil) && (cands[i].productCode != nil) == (cands[j].productCode != nil) {
				return basisSelection{state: basisSelConflict}, nil
			}
		}
	}

	c := cands[0]
	sel := basisSelection{id: c.id, from: c.from, to: c.to, reviewDue: c.reviewDue}
	kind, err := parseBasisKind(c.kind)
	if err != nil {
		sel.state = basisSelInvalid
		return sel, nil
	}
	status, err := parseStatus(c.status)
	if err != nil {
		sel.state = basisSelInvalid
		return sel, nil
	}
	sel.kind, sel.status = kind, status
	if c.licenceID != nil {
		sel.licenceID = *c.licenceID
	}
	if c.jurisdictionID != nil {
		sel.jurisdiction = *c.jurisdictionID
	}
	blocked := func(r BasisReason) (basisSelection, error) {
		sel.state, sel.reason = basisSelBlocked, r
		return sel, nil
	}
	switch {
	case status == StatusWithdrawn:
		return blocked(BasisReasonWithdrawn)
	case !c.approved:
		// Only an owner-path (provisioning / fixture) row has no executed change request: never an approved basis.
		return blocked(BasisReasonNotApproved)
	case p.ProductCode != nil && containsString(c.restricted, *p.ProductCode):
		return blocked(BasisReasonProductRestricted)
	case p.ProductCode == nil && p.OperationCode == "wagering" && len(c.restricted) > 0:
		// lic14 L-4: a wagering question that names no product cannot prove it is outside the restricted products: refused
		// (fail closed). Operations that are not product-specific (registration, deposit) are not affected.
		return blocked(BasisReasonProductRestricted)
	case c.rereview:
		return blocked(BasisReasonNeedsRereview)
	case c.reviewDue != nil && p.AsOf.UTC().Truncate(24*time.Hour).After(*c.reviewDue):
		// review_due_at is the last day the review stands (a DATE); the day after it, the basis stops permitting.
		return blocked(BasisReasonReviewOverdue)
	}
	switch kind {
	case BasisNoLicenceDocumented:
		// Shape (CHECK): licence_id NULL, jurisdiction set, review date set. A stored row that breaks it is a data defect.
		if c.licenceID != nil || c.reviewDue == nil {
			sel.state = basisSelInvalid
			return sel, nil
		}
	default:
		if c.licenceID == nil {
			sel.state = basisSelInvalid
			return sel, nil
		}
	}
	sel.state = basisSelRow
	return sel, nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// classifyBasisAbsence distinguishes "no basis was ever configured for the scope" (the legacy path) from "rows exist but none is in
// force at AsOf" (denied: an explicit basis exists and must not be bypassed by the legacy path).
func classifyBasisAbsence(ctx context.Context, q ReadOnlyQuerier, p Query) (basisSelection, error) {
	rows, err := q.Query(ctx, `
		SELECT effective_from FROM operating_bases
		 WHERE tenant_id = $1 AND country_code = $2 AND operation_code = $3
		   AND (brand_id = $4 OR brand_id IS NULL) AND (product_code = $5 OR product_code IS NULL)`,
		p.TenantID, p.CountryCode, p.OperationCode, p.BrandID, p.ProductCode)
	if err != nil {
		return basisSelection{}, fmt.Errorf("operatingmarket: read operating basis history: %w", err)
	}
	defer rows.Close()
	any_, allFuture := false, true
	for rows.Next() {
		var from time.Time
		if err := rows.Scan(&from); err != nil {
			return basisSelection{}, fmt.Errorf("operatingmarket: scan operating basis history: %w", err)
		}
		any_ = true
		if !from.After(p.AsOf) {
			allFuture = false
		}
	}
	if err := rows.Err(); err != nil {
		return basisSelection{}, fmt.Errorf("operatingmarket: read operating basis history: %w", err)
	}
	switch {
	case !any_:
		return basisSelection{state: basisSelNone}, nil
	case allFuture:
		return basisSelection{state: basisSelBlocked, reason: BasisReasonNotYetEffective}, nil
	default:
		return basisSelection{state: basisSelBlocked, reason: BasisReasonNotInForce}, nil
	}
}

func basisChainStep(sel basisSelection) ChainStep {
	id := sel.id
	status := sel.status
	step := ChainStep{Scope: ScopeBasis, Present: sel.id != uuid.Nil, ReasonCode: string(sel.reason), BasisKind: sel.kind, ReviewDueAt: sel.reviewDue}
	if sel.id != uuid.Nil {
		step.VersionID = &id
		step.Status = &status
		from := sel.from
		step.EffectiveFrom = &from
		step.EffectiveTo = sel.to
	}
	return step
}

// applyBasis runs the kind-specific part of a selected, otherwise-permitting basis row. done == true means res.outcome is final.
func applyBasis(ctx context.Context, q ReadOnlyQuerier, p Query, res *resolution, sel basisSelection) (done bool, err error) {
	res.basisID, res.basisKind, res.basisReview = sel.id, sel.kind, sel.reviewDue
	switch sel.kind {
	case BasisNoLicenceDocumented:
		chain, err := jurisdiction.ResolveLicensingChain(ctx, q, p.TenantID)
		if err != nil {
			return false, fmt.Errorf("operatingmarket: read tenant licence: %w", err)
		}
		switch chain.Outcome {
		case jurisdiction.LicensingNoLicence:
			// nothing to conflict with
		case jurisdiction.LicensingResolved:
			var n int
			if err := q.QueryRow(ctx, `
				SELECT count(*) FROM licence_country_ceilings
				 WHERE licence_id = $1 AND country_code = $2
				   AND effective_from <= $3 AND (effective_to IS NULL OR effective_to > $3)`,
				chain.LicenceID, p.CountryCode, p.AsOf).Scan(&n); err != nil {
				return false, fmt.Errorf("operatingmarket: read licence ceiling for the basis conflict check: %w", err)
			}
			if n > 0 {
				res.outcome = OutcomeConfigurationConflict
				res.basisReason = BasisReasonCeilingConflict
				s := sel
				s.reason = BasisReasonCeilingConflict
				res.chain = append(res.chain, basisChainStep(s))
				return true, nil
			}
		default:
			res.outcome = OutcomeLicensingUnknown
			return true, nil
		}
		res.chain = append(res.chain, basisChainStep(sel))
		return false, nil

	case BasisPlatformLicence, BasisPartnerLicence:
		var licensee string
		err := q.QueryRow(ctx, `SELECT licensee FROM licences WHERE id = $1`, sel.licenceID).Scan(&licensee)
		if errors.Is(err, pgx.ErrNoRows) {
			res.licenceID = sel.licenceID
			res.outcome = OutcomeLicensingUnknown
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("operatingmarket: read basis licence: %w", err)
		}
		want := "platform"
		if sel.kind == BasisPartnerLicence {
			want = "tenant"
		}
		if licensee != want {
			res.outcome = OutcomeInvalidConfiguration
			return true, nil
		}
		if sel.kind == BasisPartnerLicence {
			// A partner's own licence belongs to ONE tenant. The write path refuses a second tenant; this re-checks the
			// binding the other way (tenants.licence_id) so a row that bypassed the write path still fails closed.
			var other bool
			if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE licence_id = $1 AND id <> $2)`, sel.licenceID, p.TenantID).Scan(&other); err != nil {
				return false, fmt.Errorf("operatingmarket: read basis licence binding: %w", err)
			}
			if other {
				res.outcome = OutcomeInvalidConfiguration
				return true, nil
			}
		}
		res.chain = append(res.chain, basisChainStep(sel))
		return applyLicenceCeiling(ctx, q, p, res, sel.licenceID)
	}
	return false, fmt.Errorf("%w: unrecognized basis kind %q", ErrInvalidInput, sel.kind)
}

// applyLicenceCeiling is the pre-lic9 STEP 1 (licence validity, then the licence_country_ceilings row for the country), unchanged,
// for a given governing licence. done == true means res.outcome is final; done == false means the ceiling permits.
func applyLicenceCeiling(ctx context.Context, q ReadOnlyQuerier, p Query, res *resolution, licenceID uuid.UUID) (done bool, err error) {
	res.licenceID = licenceID

	validity, err := jurisdiction.EvaluateLicenceValidity(ctx, q, licenceID, p.AsOf)
	if err != nil {
		return false, fmt.Errorf("operatingmarket: evaluate licence validity: %w", err)
	}
	res.licenceValidity = validity
	switch validity {
	case jurisdiction.LicenceNotFound, jurisdiction.LicenceNotBound:
		res.outcome = OutcomeLicensingUnknown
		return true, nil
	case jurisdiction.LicenceSuspended:
		res.outcome = OutcomeNotPermittedByLicence
		res.ceilingReason = CeilingReasonLicenceSuspended
		res.chain = append(res.chain, licenceChainStep(false, nil, res.ceilingReason))
		return true, nil
	case jurisdiction.LicenceStatusExpired, jurisdiction.LicenceDateExpired:
		res.outcome = OutcomeNotPermittedByLicence
		res.ceilingReason = CeilingReasonLicenceExpired
		res.chain = append(res.chain, licenceChainStep(false, nil, res.ceilingReason))
		return true, nil
	case jurisdiction.LicenceNotYetIssued:
		res.outcome = OutcomeNotPermittedByLicence
		res.ceilingReason = CeilingReasonLicenceNotYetIssued
		res.chain = append(res.chain, licenceChainStep(false, nil, res.ceilingReason))
		return true, nil
	case jurisdiction.LicenceValid:
		// fall through
	default:
		return false, fmt.Errorf("%w: unrecognized licence validity %q", ErrInvalidInput, validity)
	}

	ceilingRows, err := queryWindowedRows(ctx, q, `
		SELECT id, state, status, effective_from, effective_to, reason_code, COALESCE(authorization_reference, '')
		  FROM licence_country_ceilings
		 WHERE licence_id = $1 AND country_code = $2
		   AND effective_from <= $3 AND (effective_to IS NULL OR effective_to > $3)
		 ORDER BY effective_from DESC LIMIT 2`,
		licenceID, p.CountryCode, p.AsOf)
	if err != nil {
		return false, fmt.Errorf("operatingmarket: read licence ceiling: %w", err)
	}
	switch len(ceilingRows) {
	case 2:
		res.outcome = OutcomeConfigurationConflict
		return true, nil
	case 0:
		res.outcome = OutcomeNotPermittedByLicence
		res.ceilingReason = CeilingReasonNoCeilingRow
		res.chain = append(res.chain, licenceChainStep(false, nil, res.ceilingReason))
		return true, nil
	}
	ceilingRow := ceilingRows[0]
	ceilingStatus, err := parseStatus(ceilingRow.status)
	if err != nil {
		res.outcome = OutcomeInvalidConfiguration
		return true, nil
	}
	if ceilingStatus == StatusWithdrawn {
		res.outcome = OutcomeNotPermittedByLicence
		res.ceilingReason = CeilingReasonCeilingWithdrawn
		res.chain = append(res.chain, licenceChainStep(true, &ceilingRow, res.ceilingReason))
		return true, nil
	}
	ceilingState, err := parseState(ceilingRow.state)
	if err != nil {
		res.outcome = OutcomeInvalidConfiguration
		return true, nil
	}
	if ceilingState == StateDisabled {
		res.outcome = OutcomeNotPermittedByLicence
		res.ceilingReason = CeilingReasonCountryDisabled
		res.chain = append(res.chain, licenceChainStep(true, &ceilingRow, res.ceilingReason))
		return true, nil
	}
	// Ceiling permits. Nothing below may exceed it, and no lower row can ever short-circuit this step.
	res.chain = append(res.chain, licenceChainStep(true, &ceilingRow, CeilingReasonNone))
	return false, nil
}

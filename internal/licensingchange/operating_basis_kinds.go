package licensingchange

// Migration 0180 (lic9): the operating-basis kinds. Extension recipe (docs/plans/lic2-maker-checker-record.md section 6): registry
// rows + licensing_change_classify and licensing_change_row_matches branches + the governed trigger on operating_bases are in
// migration 0180; this file is the Go register() of the three kinds. The executors call the operatingmarket writers
// (operatingmarket.SetOperatingBasis, WithdrawOperatingBasis, RecordOperatingBasisReview) unchanged; the DATABASE classifies the
// request:
//
//	operating_basis_set            always WIDENING (it creates or replaces a permission): two distinct platform_admin Persons
//	operating_basis_withdraw       always NARROWING: single actor with a reason code, executed in the requester's transaction
//	operating_basis_review_record  widening iff the review date moves later or needs_rereview is cleared; otherwise narrowing
//
// Payload keys are fixed and always all present (a JSON null is an explicit "none"): a missing key is refused. Every free-text value
// is an OPAQUE reference or a bounded note: the launchgov accident lint (ADR 0112 8.8) refuses e-mail, phone, IBAN, wallet and
// card shapes, and the database refuses control characters and '@'. determined_by_role is a FUNCTION code (for example
// head_of_compliance), never a person's name.
//
// Nothing here is a legal position. A recorded basis says WHO decided WHAT on WHICH evidence; it does not make anything lawful.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

// The operating-basis kinds (migration 0180).
const (
	KindOperatingBasisSet          Kind = "operating_basis_set"
	KindOperatingBasisWithdraw     Kind = "operating_basis_withdraw"
	KindOperatingBasisReviewRecord Kind = "operating_basis_review_record"
)

// OperatingBasisSetPayload is the payload of KindOperatingBasisSet.
type OperatingBasisSetPayload struct {
	TenantID               uuid.UUID  `json:"tenant_id"`
	BrandID                *uuid.UUID `json:"brand_id"`
	CountryCode            string     `json:"country_code"`
	OperationCode          string     `json:"operation_code"`
	ProductCode            *string    `json:"product_code"`
	BasisKind              string     `json:"basis_kind"`
	LicenceID              *uuid.UUID `json:"licence_id"`
	JurisdictionID         *uuid.UUID `json:"jurisdiction_id"`
	DeterminationReference string     `json:"determination_reference"`
	DeterminedByRole       string     `json:"determined_by_role"`
	EvidenceReference      *string    `json:"basis_evidence_reference"`
	RestrictionsNote       *string    `json:"restrictions_note"`
	RestrictedProductCodes []string   `json:"restricted_product_codes"`
	ReviewDueAt            *string    `json:"review_due_at"`
}

// OperatingBasisWithdrawPayload is the payload of KindOperatingBasisWithdraw.
type OperatingBasisWithdrawPayload struct {
	TenantID      uuid.UUID  `json:"tenant_id"`
	BrandID       *uuid.UUID `json:"brand_id"`
	CountryCode   string     `json:"country_code"`
	OperationCode string     `json:"operation_code"`
	ProductCode   *string    `json:"product_code"`
}

// OperatingBasisReviewPayload is the payload of KindOperatingBasisReviewRecord.
type OperatingBasisReviewPayload struct {
	TenantID        uuid.UUID  `json:"tenant_id"`
	BrandID         *uuid.UUID `json:"brand_id"`
	CountryCode     string     `json:"country_code"`
	OperationCode   string     `json:"operation_code"`
	ProductCode     *string    `json:"product_code"`
	ReviewDueAt     string     `json:"review_due_at"`
	NeedsRereview   bool       `json:"needs_rereview"`
	ReviewReference string     `json:"review_reference"`
}

var roleCodeRE = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{1,99}$`)

// lic14 (L-5): the character classes the database CHECKs enforce (migration 0183): an opaque reference is one token, a note is
// short plain text. Neither admits '@', a '+', brackets other than parentheses, or quotes other than the apostrophe.
var (
	refCharsetRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/#-]*$`)
	noteCharsetRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 .,;:()/#_'-]*$`)
	nonAlnumRE    = regexp.MustCompile(`[^0-9A-Za-z]`)
	digitRunRE    = regexp.MustCompile(`[0-9]{9}`)
)

// lintRef normalises (trim-refusal) and lints one opaque reference / note.
func lintRef(field string, s string, max int) (string, error) {
	re := refCharsetRE
	if field == "restrictions_note" {
		re = noteCharsetRE
	}
	if !re.MatchString(s) || digitRunRE.MatchString(nonAlnumRE.ReplaceAllString(s, "")) {
		return "", fmt.Errorf("%w: %s uses characters or digit runs the stored form does not allow (a single opaque token of letters, digits and . _ : / # -; a note may also hold spaces and , ; ( ) ')", ErrInvalidInput, field)
	}
	return lintRefBase(field, s, max)
}

func lintRefBase(field string, s string, max int) (string, error) {
	if s != strings.TrimSpace(s) || s == "" || len(s) > max || strings.ContainsAny(s, "\x00\r\n\t") || strings.Contains(s, "@") {
		return "", fmt.Errorf("%w: %s must be an opaque, trimmed value of 1..%d bytes without control characters or contact details", ErrInvalidInput, field, max)
	}
	if err := launchgov.LintFreeText(field, s, max); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	return s, nil
}

func lintOptRef(field string, s *string, max int) (*string, error) {
	if s == nil {
		return nil, nil
	}
	v, err := lintRef(field, *s, max)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func basisScopeOf(tenant uuid.UUID, brand *uuid.UUID, country, op string, product *string) (operatingmarket.BasisScope, *string, error) {
	if tenant == uuid.Nil || !validation.IsISO3166Alpha2(country) {
		return operatingmarket.BasisScope{}, nil, fmt.Errorf("%w: tenant_id and a valid uppercase ISO-3166-1 alpha-2 country_code are required", ErrInvalidInput)
	}
	if brand != nil && *brand == uuid.Nil {
		return operatingmarket.BasisScope{}, nil, fmt.Errorf("%w: brand_id must be a UUID or null", ErrInvalidInput)
	}
	opV, err := optText(&op, 64)
	if err != nil {
		return operatingmarket.BasisScope{}, nil, fmt.Errorf("%w: operation_code is required", ErrInvalidInput)
	}
	prod, err := optText(product, 64)
	if err != nil {
		return operatingmarket.BasisScope{}, nil, err
	}
	return operatingmarket.BasisScope{TenantID: tenant, BrandID: brand, CountryCode: country, OperationCode: *opV, ProductCode: prod}, prod, nil
}

func basisActor(ex execContext) operatingmarket.ActorContext { return ex.marketActor() }

func init() {
	platform := func(any) auth.Permission { return "" }

	register(KindOperatingBasisSet, kindSpec{
		platformOnly: true, platformPerm: auth.PermOperatingMarketCeilingManage, tenantPerm: platform,
		decode: func(raw json.RawMessage) (any, uuid.UUID, error) {
			var p OperatingBasisSetPayload
			if err := strictDecode(raw, &p); err != nil {
				return nil, uuid.Nil, err
			}
			if err := requireKeys(raw, "tenant_id", "brand_id", "country_code", "operation_code", "product_code", "basis_kind", "licence_id",
				"jurisdiction_id", "determination_reference", "determined_by_role", "basis_evidence_reference", "restrictions_note",
				"restricted_product_codes", "review_due_at"); err != nil {
				return nil, uuid.Nil, err
			}
			scope, prod, err := basisScopeOf(p.TenantID, p.BrandID, p.CountryCode, p.OperationCode, p.ProductCode)
			if err != nil {
				return nil, uuid.Nil, err
			}
			p.OperationCode, p.ProductCode = scope.OperationCode, prod
			switch operatingmarket.BasisKind(p.BasisKind) {
			case operatingmarket.BasisNoLicenceDocumented:
				if p.LicenceID != nil || p.JurisdictionID == nil || *p.JurisdictionID == uuid.Nil || p.ReviewDueAt == nil {
					return nil, uuid.Nil, fmt.Errorf("%w: a no_licence_documented basis needs jurisdiction_id and review_due_at and carries no licence_id", ErrInvalidInput)
				}
			case operatingmarket.BasisPlatformLicence, operatingmarket.BasisPartnerLicence:
				if p.LicenceID == nil || *p.LicenceID == uuid.Nil || p.JurisdictionID != nil {
					return nil, uuid.Nil, fmt.Errorf("%w: a licence basis needs licence_id and carries no jurisdiction_id", ErrInvalidInput)
				}
			default:
				return nil, uuid.Nil, fmt.Errorf("%w: basis_kind must be one of platform_licence, partner_licence, no_licence_documented", ErrInvalidInput)
			}
			if p.DeterminationReference, err = lintRef("determination_reference", p.DeterminationReference, 200); err != nil {
				return nil, uuid.Nil, err
			}
			if !roleCodeRE.MatchString(p.DeterminedByRole) {
				return nil, uuid.Nil, fmt.Errorf("%w: determined_by_role must be a function code (lower-case letters, digits, '_', '.', ':', '-'; 2..100 characters), not a person's name", ErrInvalidInput)
			}
			if p.EvidenceReference, err = lintOptRef("basis_evidence_reference", p.EvidenceReference, 200); err != nil {
				return nil, uuid.Nil, err
			}
			if p.RestrictionsNote, err = lintOptRef("restrictions_note", p.RestrictionsNote, 1000); err != nil {
				return nil, uuid.Nil, err
			}
			if p.RestrictedProductCodes, err = normList(p.RestrictedProductCodes, "restricted_product_codes"); err != nil {
				return nil, uuid.Nil, err
			}
			if p.ReviewDueAt, err = normDate(p.ReviewDueAt, "review_due_at"); err != nil {
				return nil, uuid.Nil, err
			}
			return p, p.TenantID, nil
		},
		precheck: func(ctx context.Context, tx pgx.Tx, payload any) error {
			p := payload.(OperatingBasisSetPayload)
			// A documented no-licence basis is refused early if the tenant's own licence already has a ceiling row for the country
			// (the resolver would answer configuration_conflict: two bases, silently ambiguous). The resolver stays the decider.
			if operatingmarket.BasisKind(p.BasisKind) == operatingmarket.BasisNoLicenceDocumented {
				var clash bool
				if err := tx.QueryRow(ctx, `
					SELECT EXISTS (SELECT 1 FROM licence_country_ceilings c JOIN tenants t ON t.licence_id = c.licence_id
					                WHERE t.id = $1 AND c.country_code = $2 AND c.effective_to IS NULL)`, p.TenantID, p.CountryCode).Scan(&clash); err != nil {
					return err
				}
				if clash {
					return refuse("basis_clashes_with_licence_ceiling", fmt.Errorf("%w: the tenant's own licence already has a ceiling row for %s; a documented no-licence basis would be ambiguous (withdraw one of the two first)",
						operatingmarket.ErrInvalidInput, p.CountryCode))
				}
			}
			return nil
		},
		execute: func(ctx context.Context, tx pgx.Tx, ex execContext, payload any) (map[string]any, error) {
			p := payload.(OperatingBasisSetPayload)
			scope := operatingmarket.BasisScope{TenantID: p.TenantID, BrandID: p.BrandID, CountryCode: p.CountryCode, OperationCode: p.OperationCode, ProductCode: p.ProductCode}
			rec, err := operatingmarket.SetOperatingBasis(ctx, tx, operatingmarket.SetOperatingBasisParams{
				Scope: scope, Kind: operatingmarket.BasisKind(p.BasisKind), LicenceID: p.LicenceID, JurisdictionID: p.JurisdictionID,
				DeterminationReference: p.DeterminationReference, DeterminedByRole: p.DeterminedByRole, EvidenceReference: p.EvidenceReference,
				RestrictionsNote: p.RestrictionsNote, RestrictedProductCodes: p.RestrictedProductCodes, ReviewDueAt: p.ReviewDueAt,
				Actor: basisActor(ex),
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"operating_basis_id": rec.ID.String(), "basis_kind": string(rec.Kind)}, nil
		},
	})

	register(KindOperatingBasisWithdraw, kindSpec{
		platformOnly: true, platformPerm: auth.PermOperatingMarketCeilingManage, tenantPerm: platform,
		decode: func(raw json.RawMessage) (any, uuid.UUID, error) {
			var p OperatingBasisWithdrawPayload
			if err := strictDecode(raw, &p); err != nil {
				return nil, uuid.Nil, err
			}
			if err := requireKeys(raw, "tenant_id", "brand_id", "country_code", "operation_code", "product_code"); err != nil {
				return nil, uuid.Nil, err
			}
			scope, prod, err := basisScopeOf(p.TenantID, p.BrandID, p.CountryCode, p.OperationCode, p.ProductCode)
			if err != nil {
				return nil, uuid.Nil, err
			}
			p.OperationCode, p.ProductCode = scope.OperationCode, prod
			return p, p.TenantID, nil
		},
		execute: func(ctx context.Context, tx pgx.Tx, ex execContext, payload any) (map[string]any, error) {
			p := payload.(OperatingBasisWithdrawPayload)
			rec, err := operatingmarket.WithdrawOperatingBasis(ctx, tx, operatingmarket.WithdrawOperatingBasisParams{
				Scope: operatingmarket.BasisScope{TenantID: p.TenantID, BrandID: p.BrandID, CountryCode: p.CountryCode, OperationCode: p.OperationCode, ProductCode: p.ProductCode},
				Actor: basisActor(ex),
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"operating_basis_id": rec.ID.String(), "status": string(rec.Status)}, nil
		},
	})

	register(KindOperatingBasisReviewRecord, kindSpec{
		platformOnly: true, platformPerm: auth.PermOperatingMarketCeilingManage, tenantPerm: platform,
		decode: func(raw json.RawMessage) (any, uuid.UUID, error) {
			var p OperatingBasisReviewPayload
			if err := strictDecode(raw, &p); err != nil {
				return nil, uuid.Nil, err
			}
			if err := requireKeys(raw, "tenant_id", "brand_id", "country_code", "operation_code", "product_code", "review_due_at", "needs_rereview", "review_reference"); err != nil {
				return nil, uuid.Nil, err
			}
			scope, prod, err := basisScopeOf(p.TenantID, p.BrandID, p.CountryCode, p.OperationCode, p.ProductCode)
			if err != nil {
				return nil, uuid.Nil, err
			}
			p.OperationCode, p.ProductCode = scope.OperationCode, prod
			d := p.ReviewDueAt
			if _, err := normDate(&d, "review_due_at"); err != nil {
				return nil, uuid.Nil, err
			}
			if p.ReviewReference, err = lintRef("review_reference", p.ReviewReference, 200); err != nil {
				return nil, uuid.Nil, err
			}
			return p, p.TenantID, nil
		},
		execute: func(ctx context.Context, tx pgx.Tx, ex execContext, payload any) (map[string]any, error) {
			p := payload.(OperatingBasisReviewPayload)
			rec, err := operatingmarket.RecordOperatingBasisReview(ctx, tx, operatingmarket.RecordBasisReviewParams{
				Scope:       operatingmarket.BasisScope{TenantID: p.TenantID, BrandID: p.BrandID, CountryCode: p.CountryCode, OperationCode: p.OperationCode, ProductCode: p.ProductCode},
				ReviewDueAt: p.ReviewDueAt, NeedsRereview: p.NeedsRereview, ReviewReference: p.ReviewReference, Actor: basisActor(ex),
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"operating_basis_id": rec.ID.String(), "needs_rereview": rec.NeedsRereview}, nil
		},
	})
}

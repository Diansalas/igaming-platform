package licensingchange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

// execContext is what a kind's executor receives: the executing actor (the final approver, or the requester of a
// single-actor change), the request, and the domain actor contexts built from them.
type execContext struct {
	actor Actor
	req   Request
}

func (e execContext) jurisdictionActor() jurisdiction.ActorContext {
	return jurisdiction.ActorContext{ActorID: e.actor.StaffID, IPAddress: e.actor.IPAddress, UserAgent: e.actor.UserAgent,
		RequestID: e.actor.RequestID, ReasonCode: e.req.ReasonCode}
}

func (e execContext) marketActor() operatingmarket.ActorContext {
	return operatingmarket.ActorContext{ActorID: e.actor.StaffID, IPAddress: e.actor.IPAddress, UserAgent: e.actor.UserAgent,
		RequestID: e.actor.RequestID, ReasonCode: e.req.ReasonCode}
}

// kindSpec is one registered kind. decode parses and validates the payload's SHAPE (no database) and returns the canonical
// value (marshalled as the stored payload) and the subject tenant (uuid.Nil if none); precheck runs inside the request
// transaction and refuses early with the domain writer's own refusals; execute is the governed write in the executor's
// transaction (platform scope; a kind that writes a tenant-RLS table binds the subject tenant itself and must be the LAST
// statement of the transaction's platform-scoped work).
type kindSpec struct {
	platformOnly bool
	// platformPerm is the permission a PLATFORM requester needs beyond PermLicensingChangeRequest ("" = none).
	platformPerm auth.Permission
	// tenantPerm is the permission a TENANT requester needs for this payload ("" = tenant staff may not request it).
	tenantPerm func(payload any) auth.Permission
	decode     func(raw json.RawMessage) (payload any, subjectTenant uuid.UUID, err error)
	precheck   func(ctx context.Context, tx pgx.Tx, payload any) error
	execute    func(ctx context.Context, tx pgx.Tx, ex execContext, payload any) (map[string]any, error)
}

var kinds = map[Kind]kindSpec{}

// register adds a kind (the extension hook; called from init of the file that defines the kind). A kind registered here
// without its licensing_change_kinds row and SQL branches fails closed in the database.
func register(k Kind, s kindSpec) {
	if _, dup := kinds[k]; dup {
		panic("licensingchange: duplicate kind " + string(k))
	}
	if s.decode == nil || s.execute == nil {
		panic("licensingchange: kind " + string(k) + " needs decode and execute")
	}
	kinds[k] = s
}

// Kinds returns the registered kinds (for the HTTP layer's vocabulary and the registry pin test).
func Kinds() []Kind {
	out := make([]Kind, 0, len(kinds))
	for k := range kinds {
		out = append(out, k)
	}
	return out
}

// strictDecode decodes raw into dst refusing unknown fields and trailing data.
func strictDecode(raw json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("%w: payload is required", ErrInvalidInput)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: payload: %v", ErrInvalidInput, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: payload has trailing data", ErrInvalidInput)
	}
	return nil
}

func optText(s *string, max int) (*string, error) {
	if s == nil {
		return nil, nil
	}
	v := strings.TrimSpace(*s)
	if v == "" || len(v) > max || strings.ContainsAny(v, "\x00\r\n\t") {
		return nil, fmt.Errorf("%w: text value must be 1..%d characters without control characters", ErrInvalidInput, max)
	}
	return &v, nil
}

// ---- licence_create -------------------------------------------------------------------------------------------------

// LicenceCreatePayload is the payload of KindLicenceCreate (the POST /v1/admin/licences body minus reason_code).
type LicenceCreatePayload struct {
	JurisdictionID    uuid.UUID `json:"jurisdiction_id"`
	Licensee          string    `json:"licensee"`
	LicenceNumber     string    `json:"licence_number"`
	PermittedProducts []string  `json:"permitted_products"`
	PermittedMarkets  []string  `json:"permitted_markets"`
}

func normList(in []string, name string) ([]string, error) {
	if in == nil {
		return []string{}, nil
	}
	if len(in) > 64 {
		return nil, fmt.Errorf("%w: %s has more than 64 entries", ErrInvalidInput, name)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v2, err := optText(&v, 64)
		if err != nil {
			return nil, err
		}
		if seen[*v2] {
			return nil, fmt.Errorf("%w: %s entry %q is duplicated", ErrInvalidInput, name, *v2)
		}
		seen[*v2] = true
		out = append(out, *v2)
	}
	return out, nil
}

// NormalizeLicenceCreate validates and normalizes a licence_create payload.
func NormalizeLicenceCreate(p LicenceCreatePayload) (LicenceCreatePayload, error) {
	if p.JurisdictionID == uuid.Nil {
		return p, fmt.Errorf("%w: jurisdiction_id is required", ErrInvalidInput)
	}
	if p.Licensee != "platform" && p.Licensee != "tenant" {
		return p, fmt.Errorf("%w: licensee must be 'platform' or 'tenant'", ErrInvalidInput)
	}
	n, err := optText(&p.LicenceNumber, 200)
	if err != nil {
		return p, fmt.Errorf("%w: licence_number is required (1..200 characters)", ErrInvalidInput)
	}
	p.LicenceNumber = *n
	if p.PermittedProducts, err = normList(p.PermittedProducts, "permitted_products"); err != nil {
		return p, err
	}
	if p.PermittedMarkets, err = normList(p.PermittedMarkets, "permitted_markets"); err != nil {
		return p, err
	}
	return p, nil
}

func init() {
	register(KindLicenceCreate, kindSpec{
		platformOnly: true,
		platformPerm: auth.PermJurisdictionRegistryManage,
		tenantPerm:   func(any) auth.Permission { return "" },
		decode: func(raw json.RawMessage) (any, uuid.UUID, error) {
			var p LicenceCreatePayload
			if err := strictDecode(raw, &p); err != nil {
				return nil, uuid.Nil, err
			}
			p, err := NormalizeLicenceCreate(p)
			return p, uuid.Nil, err
		},
		precheck: func(ctx context.Context, tx pgx.Tx, payload any) error {
			p := payload.(LicenceCreatePayload)
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jurisdictions WHERE id = $1)`, p.JurisdictionID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return refuse("jurisdiction_unknown", fmt.Errorf("%w: unknown jurisdiction_id %s", jurisdiction.ErrInvalidInput, p.JurisdictionID))
			}
			return nil
		},
		execute: func(ctx context.Context, tx pgx.Tx, ex execContext, payload any) (map[string]any, error) {
			p := payload.(LicenceCreatePayload)
			l, err := jurisdiction.CreateLicence(ctx, tx, jurisdiction.CreateLicenceParams{
				JurisdictionID: p.JurisdictionID, Licensee: p.Licensee, LicenceNumber: p.LicenceNumber,
				PermittedProducts: p.PermittedProducts, PermittedMarkets: p.PermittedMarkets, Actor: ex.jurisdictionActor(),
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"licence_id": l.ID.String(), "status": l.Status}, nil
		},
	})
}

// ---- tenant_licence_bind / tenant_licence_unbind ----------------------------------------------------------------------

// TenantLicenceBindPayload is the payload of KindTenantLicenceBind.
type TenantLicenceBindPayload struct {
	TenantID  uuid.UUID `json:"tenant_id"`
	LicenceID uuid.UUID `json:"licence_id"`
}

// TenantLicenceUnbindPayload is the payload of KindTenantLicenceUnbind.
type TenantLicenceUnbindPayload struct {
	TenantID uuid.UUID `json:"tenant_id"`
}

// precheckBind repeats AssignTenantLicence's refusals (and the database's model / exclusivity constraints) at request time,
// with the same sentinels, so an operator learns before approvers spend time on it. The executor re-checks everything.
func precheckBind(ctx context.Context, tx pgx.Tx, tenantID, licenceID uuid.UUID) error {
	var model string
	var current *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT licensing_model, licence_id FROM tenants WHERE id = $1`, tenantID).Scan(&model, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse("tenant_unknown", fmt.Errorf("%w: unknown tenant_id %s", jurisdiction.ErrNotFound, tenantID))
	}
	if err != nil {
		return err
	}
	var status, licensee string
	err = tx.QueryRow(ctx, `SELECT status, licensee FROM licences WHERE id = $1`, licenceID).Scan(&status, &licensee)
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse("licence_unknown", fmt.Errorf("%w: unknown licence_id %s", jurisdiction.ErrInvalidInput, licenceID))
	}
	if err != nil {
		return err
	}
	if status != "active" {
		return refuse("licence_not_active", fmt.Errorf("%w: licence %s is not active (status=%s)", jurisdiction.ErrInvalidInput, licenceID, status))
	}
	if (model == "under_platform_licence") != (licensee == "platform") {
		return refuse("licence_licensee_mismatch", fmt.Errorf("%w: licence licensee does not match this tenant's licensing_model", jurisdiction.ErrInvalidInput))
	}
	if licensee == "tenant" {
		var other bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE licence_id = $1 AND id <> $2)`, licenceID, tenantID).Scan(&other); err != nil {
			return err
		}
		if other {
			return refuse("licence_bound_to_other_tenant", fmt.Errorf("%w: licence is already exclusively bound to another tenant", jurisdiction.ErrInvalidInput))
		}
	}
	return nil
}

func init() {
	register(KindTenantLicenceBind, kindSpec{
		platformOnly: true,
		platformPerm: auth.PermTenantLicenceAssign,
		tenantPerm:   func(any) auth.Permission { return "" },
		decode: func(raw json.RawMessage) (any, uuid.UUID, error) {
			var p TenantLicenceBindPayload
			if err := strictDecode(raw, &p); err != nil {
				return nil, uuid.Nil, err
			}
			if p.TenantID == uuid.Nil || p.LicenceID == uuid.Nil {
				return nil, uuid.Nil, fmt.Errorf("%w: tenant_id and licence_id are required", ErrInvalidInput)
			}
			return p, p.TenantID, nil
		},
		precheck: func(ctx context.Context, tx pgx.Tx, payload any) error {
			p := payload.(TenantLicenceBindPayload)
			return precheckBind(ctx, tx, p.TenantID, p.LicenceID)
		},
		execute: func(ctx context.Context, tx pgx.Tx, ex execContext, payload any) (map[string]any, error) {
			p := payload.(TenantLicenceBindPayload)
			id := p.LicenceID
			st, err := jurisdiction.AssignTenantLicence(ctx, tx, jurisdiction.AssignTenantLicenceParams{TenantID: p.TenantID, LicenceID: &id, Actor: ex.jurisdictionActor()})
			if err != nil {
				return nil, err
			}
			return map[string]any{"tenant_id": st.TenantID.String(), "licence_id": st.LicenceID.String()}, nil
		},
	})
	register(KindTenantLicenceUnbind, kindSpec{
		platformOnly: true,
		platformPerm: auth.PermTenantLicenceAssign,
		tenantPerm:   func(any) auth.Permission { return "" },
		decode: func(raw json.RawMessage) (any, uuid.UUID, error) {
			var p TenantLicenceUnbindPayload
			if err := strictDecode(raw, &p); err != nil {
				return nil, uuid.Nil, err
			}
			if p.TenantID == uuid.Nil {
				return nil, uuid.Nil, fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
			}
			return p, p.TenantID, nil
		},
		precheck: func(ctx context.Context, tx pgx.Tx, payload any) error {
			p := payload.(TenantLicenceUnbindPayload)
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1)`, p.TenantID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return refuse("tenant_unknown", fmt.Errorf("%w: unknown tenant_id %s", jurisdiction.ErrNotFound, p.TenantID))
			}
			return nil
		},
		execute: func(ctx context.Context, tx pgx.Tx, ex execContext, payload any) (map[string]any, error) {
			p := payload.(TenantLicenceUnbindPayload)
			st, err := jurisdiction.AssignTenantLicence(ctx, tx, jurisdiction.AssignTenantLicenceParams{TenantID: p.TenantID, LicenceID: nil, Actor: ex.jurisdictionActor()})
			if err != nil {
				return nil, err
			}
			return map[string]any{"tenant_id": st.TenantID.String(), "licence_id": nil}, nil
		},
	})
}

// ---- licence_ceiling_version / operating_policy_version ----------------------------------------------------------------

// LicenceCeilingPayload is the payload of KindLicenceCeilingVersion.
type LicenceCeilingPayload struct {
	LicenceID              uuid.UUID `json:"licence_id"`
	CountryCode            string    `json:"country_code"`
	State                  string    `json:"state"`
	Status                 string    `json:"status"`
	AuthorizationReference *string   `json:"authorization_reference"`
}

// OperatingPolicyPayload is the payload of KindOperatingPolicyVersion.
type OperatingPolicyPayload struct {
	TenantID               uuid.UUID  `json:"tenant_id"`
	Scope                  string     `json:"scope"`
	BrandID                *uuid.UUID `json:"brand_id"`
	OperationCode          *string    `json:"operation_code"`
	ProductCode            *string    `json:"product_code"`
	CountryCode            string     `json:"country_code"`
	State                  string     `json:"state"`
	Status                 string     `json:"status"`
	AuthorizationReference *string    `json:"authorization_reference"`
}

func checkStateStatus(state, status string, authRef *string) error {
	if state != string(operatingmarket.StateEnabled) && state != string(operatingmarket.StateDisabled) {
		return fmt.Errorf("%w: state must be 'enabled' or 'disabled'", ErrInvalidInput)
	}
	if status != string(operatingmarket.StatusActive) && status != string(operatingmarket.StatusWithdrawn) {
		return fmt.Errorf("%w: status must be 'active' or 'withdrawn'", ErrInvalidInput)
	}
	if status == string(operatingmarket.StatusActive) && state == string(operatingmarket.StateEnabled) && authRef == nil {
		return fmt.Errorf("%w: authorization_reference is required to enable a country", ErrInvalidInput)
	}
	if status == string(operatingmarket.StatusWithdrawn) && state != string(operatingmarket.StateDisabled) {
		return fmt.Errorf("%w: a withdrawal must carry state 'disabled'", ErrInvalidInput)
	}
	return nil
}

func init() {
	register(KindLicenceCeilingVersion, kindSpec{
		platformOnly: true,
		platformPerm: auth.PermOperatingMarketCeilingManage,
		tenantPerm:   func(any) auth.Permission { return "" },
		decode: func(raw json.RawMessage) (any, uuid.UUID, error) {
			var p LicenceCeilingPayload
			if err := strictDecode(raw, &p); err != nil {
				return nil, uuid.Nil, err
			}
			if p.LicenceID == uuid.Nil || !validation.IsISO3166Alpha2(p.CountryCode) {
				return nil, uuid.Nil, fmt.Errorf("%w: licence_id and a valid uppercase ISO-3166-1 alpha-2 country_code are required", ErrInvalidInput)
			}
			var err error
			if p.AuthorizationReference, err = optText(p.AuthorizationReference, 500); err != nil {
				return nil, uuid.Nil, err
			}
			return p, uuid.Nil, checkStateStatus(p.State, p.Status, p.AuthorizationReference)
		},
		precheck: func(ctx context.Context, tx pgx.Tx, payload any) error {
			p := payload.(LicenceCeilingPayload)
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM licences WHERE id = $1)`, p.LicenceID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return refuse("licence_unknown", fmt.Errorf("%w: unknown licence_id %s", operatingmarket.ErrInvalidInput, p.LicenceID))
			}
			return nil
		},
		execute: func(ctx context.Context, tx pgx.Tx, ex execContext, payload any) (map[string]any, error) {
			p := payload.(LicenceCeilingPayload)
			ref := ""
			if p.AuthorizationReference != nil {
				ref = *p.AuthorizationReference
			}
			rec, err := operatingmarket.CreateLicenceCountryCeilingVersion(ctx, tx, operatingmarket.CreateLicenceCountryCeilingVersionParams{
				LicenceID: p.LicenceID, CountryCode: p.CountryCode, State: operatingmarket.State(p.State), Status: operatingmarket.Status(p.Status),
				AuthorizationReference: ref, Actor: ex.marketActor(),
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"ceiling_version_id": rec.ID.String()}, nil
		},
	})

	register(KindOperatingPolicyVersion, kindSpec{
		platformOnly: false,
		tenantPerm: func(payload any) auth.Permission {
			if payload.(OperatingPolicyPayload).Scope == string(operatingmarket.ScopeTenant) {
				return auth.PermOperatingMarketTenantPolicyWrite
			}
			return auth.PermOperatingMarketBrandPolicyWrite
		},
		decode: func(raw json.RawMessage) (any, uuid.UUID, error) {
			var p OperatingPolicyPayload
			if err := strictDecode(raw, &p); err != nil {
				return nil, uuid.Nil, err
			}
			if p.TenantID == uuid.Nil || !validation.IsISO3166Alpha2(p.CountryCode) {
				return nil, uuid.Nil, fmt.Errorf("%w: tenant_id and a valid uppercase ISO-3166-1 alpha-2 country_code are required", ErrInvalidInput)
			}
			var err error
			if p.OperationCode, err = optText(p.OperationCode, 64); err != nil {
				return nil, uuid.Nil, err
			}
			if p.ProductCode, err = optText(p.ProductCode, 64); err != nil {
				return nil, uuid.Nil, err
			}
			if p.AuthorizationReference, err = optText(p.AuthorizationReference, 500); err != nil {
				return nil, uuid.Nil, err
			}
			switch operatingmarket.Scope(p.Scope) {
			case operatingmarket.ScopeTenant:
				if p.BrandID != nil || p.OperationCode != nil || p.ProductCode != nil {
					return nil, uuid.Nil, fmt.Errorf("%w: a tenant-scope policy carries no brand_id, operation_code or product_code", ErrInvalidInput)
				}
			case operatingmarket.ScopeBrand:
				if p.BrandID == nil || p.OperationCode != nil || p.ProductCode != nil {
					return nil, uuid.Nil, fmt.Errorf("%w: a brand-scope policy carries a brand_id and no operation_code or product_code", ErrInvalidInput)
				}
			case operatingmarket.ScopeOperation:
				if p.OperationCode == nil {
					return nil, uuid.Nil, fmt.Errorf("%w: operation_code is required for an operation-scope policy", ErrInvalidInput)
				}
			default:
				return nil, uuid.Nil, fmt.Errorf("%w: scope must be 'tenant', 'brand' or 'operation'", ErrInvalidInput)
			}
			if err := checkStateStatus(p.State, p.Status, p.AuthorizationReference); err != nil {
				return nil, uuid.Nil, err
			}
			if p.Status == string(operatingmarket.StatusWithdrawn) && p.Scope != string(operatingmarket.ScopeTenant) && p.AuthorizationReference == nil {
				return nil, uuid.Nil, fmt.Errorf("%w: authorization_reference is required to withdraw a %s-scope policy (absence there inherits, so a withdrawal widens)", ErrInvalidInput, p.Scope)
			}
			return p, p.TenantID, nil
		},
		precheck: func(ctx context.Context, tx pgx.Tx, payload any) error {
			p := payload.(OperatingPolicyPayload)
			var licence *uuid.UUID
			err := tx.QueryRow(ctx, `SELECT licence_id FROM tenants WHERE id = $1`, p.TenantID).Scan(&licence)
			if errors.Is(err, pgx.ErrNoRows) {
				return refuse("tenant_unknown", fmt.Errorf("%w: unknown tenant_id %s", operatingmarket.ErrInvalidInput, p.TenantID))
			}
			if err != nil {
				return err
			}
			// F-4 (security review): a TENANT session's brand-rung policy must name a brand of its own tenant. Checked now, under the
			// requester's RLS, so approvers never see it; a foreign or unknown brand is answered as not found. A platform requester
			// cannot read tenant brands, so only the tenant session is checked here (the composite FK refuses a foreign brand at execution).
			if p.BrandID != nil {
				var tenantSession, own bool
				if err := tx.QueryRow(ctx, `SELECT NULLIF(current_setting('app.tenant_id', true), '') IS NOT NULL`).Scan(&tenantSession); err != nil {
					return err
				}
				if tenantSession {
					if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM brands WHERE id = $1 AND tenant_id = $2)`, *p.BrandID, p.TenantID).Scan(&own); err != nil {
						return err
					}
					if !own {
						return refuse("brand_not_owned", fmt.Errorf("%w: unknown brand_id %s", jurisdiction.ErrNotFound, *p.BrandID))
					}
				}
			}
			// Enabling needs either an approved, in-force operating basis for the country (lic9: the resolver then decides per brand
			// and operation), or a bound licence whose ceiling enables the country (the 0076 trigger is authoritative at execution;
			// this is the early, diagnosable answer).
			if p.State == string(operatingmarket.StateEnabled) && p.Status == string(operatingmarket.StatusActive) {
				var basis bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM operating_bases WHERE tenant_id = $1 AND country_code = $2
					AND status = 'active' AND change_request_id IS NOT NULL AND effective_to IS NULL)`, p.TenantID, p.CountryCode).Scan(&basis); err != nil {
					return err
				}
				if basis {
					return nil
				}
				if licence == nil {
					return refuse("tenant_without_licence_or_basis", fmt.Errorf("%w: tenant has no licence bound and no operating basis for %s; no country may be enabled for it", operatingmarket.ErrCeilingExceeded, p.CountryCode))
				}
				var ok bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM licence_country_ceilings WHERE licence_id = $1 AND country_code = $2
					AND status = 'active' AND state = 'enabled' AND effective_to IS NULL)`, *licence, p.CountryCode).Scan(&ok); err != nil {
					return err
				}
				if !ok {
					return refuse("ceiling_not_enabled_for_country", fmt.Errorf("%w: country %s is not enabled by the tenant's licence ceiling", operatingmarket.ErrCeilingExceeded, p.CountryCode))
				}
			}
			return nil
		},
		execute: func(ctx context.Context, tx pgx.Tx, ex execContext, payload any) (map[string]any, error) {
			p := payload.(OperatingPolicyPayload)
			// operating_country_policies is tenant-RLS: bind the subject tenant (a server-side value, the request's own
			// payload) for the rest of the transaction. It stays bound through COMMIT so the deferred successor check
			// (ocp_inherit_rung_close_requires_successor) reads the tenant's rows.
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, p.TenantID.String()); err != nil {
				return nil, fmt.Errorf("licensingchange: bind subject tenant: %w", err)
			}
			op := ""
			if p.OperationCode != nil {
				op = *p.OperationCode
			}
			ref := ""
			if p.AuthorizationReference != nil {
				ref = *p.AuthorizationReference
			}
			rec, err := operatingmarket.CreateOperatingCountryPolicyVersion(ctx, tx, operatingmarket.CreateOperatingCountryPolicyVersionParams{
				Scope: operatingmarket.Scope(p.Scope), TenantID: p.TenantID, BrandID: p.BrandID, OperationCode: op, ProductCode: p.ProductCode,
				CountryCode: p.CountryCode, State: operatingmarket.State(p.State), Status: operatingmarket.Status(p.Status),
				AuthorizationReference: ref, Actor: ex.marketActor(),
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"policy_version_id": rec.ID.String()}, nil
		},
	})
}

// ---- tenant_jurisdiction_config_version -------------------------------------------------------------------------------

// Configuration actions.
const (
	ConfigActionSet   = "set"
	ConfigActionClose = "close"
)

// TenantJurisdictionConfigPayload is the payload of KindTenantJurisdictionConfigVersion. For "close" every content field is
// null.
type TenantJurisdictionConfigPayload struct {
	TenantID              uuid.UUID `json:"tenant_id"`
	JurisdictionID        uuid.UUID `json:"jurisdiction_id"`
	Action                string    `json:"action"`
	KYCRulesetID          *string   `json:"kyc_ruleset_id"`
	AMLRulesetID          *string   `json:"aml_ruleset_id"`
	RGRulesetID           *string   `json:"rg_ruleset_id"`
	ReportingRulesetID    *string   `json:"reporting_ruleset_id"`
	AllowedCurrencies     []string  `json:"allowed_currencies"`
	AllowedPaymentMethods []string  `json:"allowed_payment_methods"`
	GeoBlockList          []string  `json:"geo_block_list"`
}

func (p TenantJurisdictionConfigPayload) content() jurisdiction.TenantJurisdictionConfigContent {
	return jurisdiction.TenantJurisdictionConfigContent{
		KYCRulesetID: p.KYCRulesetID, AMLRulesetID: p.AMLRulesetID, RGRulesetID: p.RGRulesetID, ReportingRulesetID: p.ReportingRulesetID,
		AllowedCurrencies: p.AllowedCurrencies, AllowedPaymentMethods: p.AllowedPaymentMethods, GeoBlockList: p.GeoBlockList,
	}
}

func init() {
	register(KindTenantJurisdictionConfigVersion, kindSpec{
		platformOnly: false,
		tenantPerm:   func(any) auth.Permission { return auth.PermOperatingMarketTenantPolicyWrite },
		decode: func(raw json.RawMessage) (any, uuid.UUID, error) {
			var p TenantJurisdictionConfigPayload
			if err := strictDecode(raw, &p); err != nil {
				return nil, uuid.Nil, err
			}
			if p.TenantID == uuid.Nil || p.JurisdictionID == uuid.Nil {
				return nil, uuid.Nil, fmt.Errorf("%w: tenant_id and jurisdiction_id are required", ErrInvalidInput)
			}
			switch p.Action {
			case ConfigActionClose:
				if p.KYCRulesetID != nil || p.AMLRulesetID != nil || p.RGRulesetID != nil || p.ReportingRulesetID != nil ||
					p.AllowedCurrencies != nil || p.AllowedPaymentMethods != nil || p.GeoBlockList != nil {
					return nil, uuid.Nil, fmt.Errorf("%w: a close carries no configuration content", ErrInvalidInput)
				}
				return p, p.TenantID, nil
			case ConfigActionSet:
				c, err := p.content().Normalize()
				if err != nil {
					return nil, uuid.Nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
				}
				p.KYCRulesetID, p.AMLRulesetID, p.RGRulesetID, p.ReportingRulesetID = c.KYCRulesetID, c.AMLRulesetID, c.RGRulesetID, c.ReportingRulesetID
				p.AllowedCurrencies, p.AllowedPaymentMethods, p.GeoBlockList = c.AllowedCurrencies, c.AllowedPaymentMethods, c.GeoBlockList
				return p, p.TenantID, nil
			}
			return nil, uuid.Nil, fmt.Errorf("%w: action must be 'set' or 'close'", ErrInvalidInput)
		},
		precheck: func(ctx context.Context, tx pgx.Tx, payload any) error {
			p := payload.(TenantJurisdictionConfigPayload)
			var tenantOK, jurOK bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1), EXISTS (SELECT 1 FROM jurisdictions WHERE id = $2)`,
				p.TenantID, p.JurisdictionID).Scan(&tenantOK, &jurOK); err != nil {
				return err
			}
			if !tenantOK {
				return refuse("tenant_unknown", fmt.Errorf("%w: unknown tenant", jurisdiction.ErrInvalidInput))
			}
			if !jurOK {
				return refuse("jurisdiction_unknown", fmt.Errorf("%w: unknown jurisdiction", jurisdiction.ErrInvalidInput))
			}
			if p.Action == ConfigActionSet {
				if _, err := jurisdiction.ValidateTenantJurisdictionConfigContent(ctx, tx, p.content()); err != nil {
					// The shape was validated by decode, so the only refusal left is a currency that is not an active asset.
					if errors.Is(err, jurisdiction.ErrInactiveAsset) {
						return refuse("currency_not_active_asset", err)
					}
					return &DomainError{Err: err}
				}
			}
			return nil
		},
		execute: func(ctx context.Context, tx pgx.Tx, ex execContext, payload any) (map[string]any, error) {
			p := payload.(TenantJurisdictionConfigPayload)
			if p.Action == ConfigActionClose {
				c, err := jurisdiction.CloseTenantJurisdictionConfig(ctx, tx, p.TenantID, p.JurisdictionID, ex.jurisdictionActor(), ex.req.ID)
				if err != nil {
					return nil, err
				}
				return map[string]any{"closed_config_id": c.ID.String()}, nil
			}
			c, err := jurisdiction.SetTenantJurisdictionConfigVersion(ctx, tx, jurisdiction.SetTenantJurisdictionConfigVersionParams{
				TenantID: p.TenantID, JurisdictionID: p.JurisdictionID, Content: p.content(), Actor: ex.jurisdictionActor(), ChangeRequestID: ex.req.ID,
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"config_version_id": c.ID.String()}, nil
		},
	})
}

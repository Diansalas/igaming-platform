package marketgate

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

// maxCountriesScanned bounds the per-call resolver work of AnyCountryPermits
// (there are fewer than 250 ISO-3166-1 alpha-2 codes).
const maxCountriesScanned = 250

// AnyCountryPermits is the BRAND-LEVEL "is the market still open?" question
// (SR8-2): does the operating-market configuration currently permit AT LEAST
// ONE country for every one of ops, for this tenant (and brand when non-nil)
// at asOf? It needs no player evidence, so it can run where no request exists
// (a casino vendor callback, the launch-token bootstrap), and it resolves
// through operatingmarket.ResolveOperatingCountryPolicy itself (licence
// ceiling, tenant, brand and operation rungs) - never a re-implementation.
//
// configured reports whether any country has an enabled tenant-rung row at
// all (distinguishes "never configured" from "configured but every country is
// closed" for the readiness item's detail codes). tx MUST be a tenant-scoped,
// non-player-scoped transaction for tenantID. A non-nil error means "could not
// decide": callers must treat it as closed.
func AnyCountryPermits(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, brandID *uuid.UUID, asOf time.Time, ops ...Operation) (configured, open bool, err error) {
	if tenantID == uuid.Nil || len(ops) == 0 {
		return false, false, fmt.Errorf("%w: tenant and at least one operation are required", ErrInvalidRequest)
	}
	type spec struct {
		op      string
		product *string
	}
	specs := make([]spec, 0, len(ops))
	for _, o := range ops {
		code, product, ok := opCodes(o)
		if !ok {
			return false, false, fmt.Errorf("%w: unknown operation %q", ErrInvalidRequest, o)
		}
		specs = append(specs, spec{code, product})
	}
	policies, err := operatingmarket.ListCurrentOperatingCountryPolicies(ctx, tx, tenantID, asOf)
	if err != nil {
		return false, false, fmt.Errorf("marketgate: list current policies: %w", err)
	}
	seen := map[string]bool{}
	var countries []string
	for _, p := range policies {
		if p.Scope == operatingmarket.ScopeTenant && p.Status == operatingmarket.StatusActive &&
			p.State == operatingmarket.StateEnabled && !seen[p.CountryCode] && len(countries) < maxCountriesScanned {
			seen[p.CountryCode] = true
			countries = append(countries, p.CountryCode)
		}
	}
	if len(countries) == 0 {
		return false, false, nil
	}
	// SR8R-1: bound the resolver work. (1) One set-based query drops every
	// candidate that has no in-force, active, ENABLED licence ceiling - the
	// resolver denies those unconditionally (its step 1), so skipping them cannot
	// change an answer. (lic17 L-4: any error of the prefilter, including a tenant row
	// that is not visible, is returned - "could not decide", treated as CLOSED by every
	// caller - instead of silently continuing with the unfiltered list.) (2) The country that last permitted this (tenant, brand,
	// operations) is tried FIRST: a hint only, the resolver still decides, so a
	// stale hint costs one extra resolve and never changes a verdict.
	countries, err = prefilterByCeiling(ctx, tx, tenantID, asOf, countries)
	if err != nil {
		return true, false, fmt.Errorf("marketgate: ceiling prefilter: %w", err)
	}
	hintKey := openHintKey(tenantID, brandID, ops)
	if hint, ok := openHints.get(hintKey); ok {
		for i, cc := range countries {
			if cc == hint {
				countries[0], countries[i] = countries[i], countries[0]
				break
			}
		}
	}
	for _, cc := range countries {
		all := true
		for _, sp := range specs {
			res, err := operatingmarket.ResolveOperatingCountryPolicy(ctx, tx, operatingmarket.Query{
				TenantID: tenantID, BrandID: brandID, CountryCode: cc,
				OperationCode: sp.op, ProductCode: sp.product, AsOf: asOf,
			})
			if err != nil {
				return true, false, fmt.Errorf("marketgate: resolve %s: %w", cc, err)
			}
			if !res.Permitted() {
				all = false
				break
			}
		}
		if all {
			openHints.put(hintKey, cc)
			return true, true, nil
		}
	}
	return true, false, nil
}

// BrandOpen is AnyCountryPermits at the DATABASE clock (the clock that stamped
// every policy row), for enforcement points (casino new bets, the launch-token
// bootstrap). Reads only; writes nothing; an error means closed.
func BrandOpen(ctx context.Context, tx pgx.Tx, tenantID, brandID uuid.UUID, ops ...Operation) (bool, error) {
	if brandID == uuid.Nil {
		return false, fmt.Errorf("%w: brand is required", ErrInvalidRequest)
	}
	var asOf time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&asOf); err != nil {
		return false, fmt.Errorf("marketgate: read database clock: %w", err)
	}
	_, open, err := AnyCountryPermits(ctx, tx, tenantID, &brandID, asOf, ops...)
	return open, err
}

// AuditActionResidenceIntoOpenMarket is the alert-able audit action written when a reviewer records a
// verified residence in a country the brand currently serves for at least one gated operation (SRME-1).
// Its metadata carries ids only, never the country.
const AuditActionResidenceIntoOpenMarket = "market.residence_into_open_market"

// CountryServedForAny reports whether country is currently permitted for the brand for AT LEAST ONE of
// deposit, casino wagering or sportsbook wagering, at the database clock (a residence recorded there would
// admit the player to that operation). tx must be a tenant-scoped, non-player-scoped transaction. An
// error means "could not decide".
func CountryServedForAny(ctx context.Context, tx pgx.Tx, tenantID, brandID uuid.UUID, country string) (bool, error) {
	if tenantID == uuid.Nil || brandID == uuid.Nil {
		return false, fmt.Errorf("%w: tenant and brand are required", ErrInvalidRequest)
	}
	var asOf time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&asOf); err != nil {
		return false, fmt.Errorf("marketgate: read database clock: %w", err)
	}
	for _, op := range []Operation{OpDeposit, OpLaunch, OpSportsbookWager} {
		code, product, _ := opCodes(op)
		res, err := operatingmarket.ResolveOperatingCountryPolicy(ctx, tx, operatingmarket.Query{
			TenantID: tenantID, BrandID: &brandID, CountryCode: country, OperationCode: code, ProductCode: product, AsOf: asOf,
		})
		if err != nil {
			return false, fmt.Errorf("marketgate: resolve %s: %w", op, err)
		}
		if res.Permitted() {
			return true, nil
		}
	}
	return false, nil
}

// openHints remembers, per (tenant, brand, operation set), the country that last
// permitted it. Process-local and advisory only; bounded at openHintsMax entries
// (cleared wholesale when full - losing it only costs latency).
var openHints = &hintCache{m: map[string]string{}}

const openHintsMax = 20000

type hintCache struct {
	mu sync.Mutex
	m  map[string]string
}

func (h *hintCache) get(k string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.m[k]
	return v, ok
}

func (h *hintCache) put(k, v string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.m[k]; !ok && len(h.m) >= openHintsMax {
		h.m = map[string]string{}
	}
	h.m[k] = v
}

func openHintKey(tenantID uuid.UUID, brandID *uuid.UUID, ops []Operation) string {
	k := tenantID.String() + "|"
	if brandID != nil {
		k += brandID.String()
	}
	for _, o := range ops {
		k += "|" + string(o)
	}
	return k
}

// prefilterByCeiling keeps only the candidates that have an in-force, active,
// enabled ceiling for the tenant's licence, plus every country with an approved,
// active, in-force operating basis. A tenant with NO licence keeps the input (the
// resolver decides each country; a superset is harmless). lic17 L-4: any read error
// (including a tenant row that is not visible) is RETURNED, never answered with the
// unfiltered list (fail closed and explicit: the caller treats an error as CLOSED).
func prefilterByCeiling(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, asOf time.Time, in []string) ([]string, error) {
	// Two plain reads (the licence binding, then its ceilings) so no single statement
	// both joins tenants and names a status column (tenant/status_readers_static_test).
	var licenceID *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT licence_id FROM tenants WHERE id = $1`, tenantID).Scan(&licenceID); err != nil {
		return nil, fmt.Errorf("read the tenant licence: %w", err)
	}
	if licenceID == nil {
		return in, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT country_code
		  FROM licence_country_ceilings
		 WHERE licence_id = $1 AND state = 'enabled' AND status = 'active'
		   AND effective_from <= $2 AND (effective_to IS NULL OR effective_to > $2)`, *licenceID, asOf)
	if err != nil {
		return nil, fmt.Errorf("read the licence ceilings: %w", err)
	}
	defer rows.Close()
	ok := map[string]bool{}
	for rows.Next() {
		var cc string
		if err := rows.Scan(&cc); err != nil {
			return nil, fmt.Errorf("scan a licence ceiling: %w", err)
		}
		ok[cc] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the licence ceilings: %w", err)
	}
	rows.Close()
	// lic9: a country governed by an explicit operating basis (a licence of the basis, or a documented no-licence determination) is
	// decided by the resolver, not by the tenant's own licence ceiling: keep every country that has an active, approved, in-force
	// basis row for the tenant (a superset is harmless - the resolver still decides; only dropping a permitted country would be a bug).
	brows, err := tx.Query(ctx, `
		SELECT DISTINCT country_code
		  FROM operating_bases
		 WHERE tenant_id = $1 AND status = 'active' AND change_request_id IS NOT NULL
		   AND effective_from <= $2 AND (effective_to IS NULL OR effective_to > $2)`, tenantID, asOf)
	if err != nil {
		return nil, fmt.Errorf("read the operating bases: %w", err)
	}
	defer brows.Close()
	for brows.Next() {
		var cc string
		if err := brows.Scan(&cc); err != nil {
			return nil, fmt.Errorf("scan an operating basis: %w", err)
		}
		ok[cc] = true
	}
	if err := brows.Err(); err != nil {
		return nil, fmt.Errorf("read the operating bases: %w", err)
	}
	out := make([]string, 0, len(in))
	for _, cc := range in {
		if ok[cc] {
			out = append(out, cc)
		}
	}
	return out, nil
}

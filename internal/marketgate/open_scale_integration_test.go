//go:build integration

package marketgate

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

// SR8R-1. AnyCountryPermits runs on every new casino bet, bootstrap and (via R-MARKET-CONFIG) readiness
// evaluation, inside the transaction that holds the delivery lock, so its cost with MANY configured
// countries is bounded and tested:
//
//   - one set-based query drops candidates with no in-force enabled licence ceiling (the resolver denies
//     those unconditionally, so this cannot change an answer);
//   - the country that last permitted the same (tenant, brand, operations) is tried first (advisory);
//   - worst case (cold hint, the only open country sorts last, every other candidate passes the ceiling
//     but is closed at the brand rung): one resolver pass per candidate. DOCUMENTED BOUND: with 100
//     configured countries that is 100 resolves (~5 reads each); the test requires < coldBound.
//     The warm path (hint hit) is one resolve; the test requires < warmBound.
const (
	scaleCountries = 100
	coldBound      = 5 * time.Second
	warmBound      = 500 * time.Millisecond
)

func isoCodes(n int) []string {
	var out []string
	for a := 'A'; a <= 'Z' && len(out) < n; a++ {
		for b := 'A'; b <= 'Z' && len(out) < n; b++ {
			cc := string([]rune{a, b})
			if validation.IsISO3166Alpha2(cc) {
				out = append(out, cc)
			}
		}
	}
	return out
}

func (w world) brandDisabledFor(t *testing.T, pool *db.Pool, cc string) {
	t.Helper()
	b := w.brandID
	err := pool.WithTenant(context.Background(), w.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := operatingmarket.CreateOperatingCountryPolicyVersion(ctx, tx, operatingmarket.CreateOperatingCountryPolicyVersionParams{
			Scope: operatingmarket.ScopeBrand, TenantID: w.tenantID, BrandID: &b, CountryCode: cc,
			State: operatingmarket.StateDisabled, Status: operatingmarket.StatusActive, Actor: actor(w.admin, "scale-test"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("brand disable %s: %v", cc, err)
	}
}

// bruteForce is the reference: the resolver on EVERY tenant-enabled country, no prefilter, no hint.
func (w world) bruteForce(t *testing.T, pool *db.Pool, ops ...Operation) bool {
	t.Helper()
	open := false
	err := pool.WithTenant(context.Background(), w.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var asOf time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&asOf); err != nil {
			return err
		}
		pols, err := operatingmarket.ListCurrentOperatingCountryPolicies(ctx, tx, w.tenantID, asOf)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, p := range pols {
			if p.Scope != operatingmarket.ScopeTenant || p.State != operatingmarket.StateEnabled || p.Status != operatingmarket.StatusActive || seen[p.CountryCode] {
				continue
			}
			seen[p.CountryCode] = true
			all := true
			for _, o := range ops {
				code, product, _ := opCodes(o)
				brand := w.brandID
				res, err := operatingmarket.ResolveOperatingCountryPolicy(ctx, tx, operatingmarket.Query{
					TenantID: w.tenantID, BrandID: &brand, CountryCode: p.CountryCode, OperationCode: code, ProductCode: product, AsOf: asOf})
				if err != nil {
					return err
				}
				if !res.Permitted() {
					all = false
					break
				}
			}
			if all {
				open = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("brute force: %v", err)
	}
	return open
}

func resetHints() { openHints = &hintCache{m: map[string]string{}} }

func TestAnyCountryPermits_ManyCountries_LatencyBoundAndEquivalence(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	codes := isoCodes(scaleCountries)
	for _, cc := range codes {
		w.openMarket(t, pool, cc)
	}
	open := codes[len(codes)-1]
	for _, cc := range codes {
		if cc != open {
			w.brandDisabledFor(t, pool, cc) // ceiling + tenant row pass; closed at the brand rung
		}
	}

	resetHints()
	start := time.Now()
	got, err := w.brandOpen(t, pool, OpCasinoBet)
	cold := time.Since(start)
	if err != nil || !got {
		t.Fatalf("the one open country must open the brand, got open=%v err=%v", got, err)
	}
	if cold > coldBound {
		t.Fatalf("cold path with %d countries took %v (bound %v)", scaleCountries, cold, coldBound)
	}
	if want := w.bruteForce(t, pool, OpCasinoBet); want != got {
		t.Fatalf("BrandOpen=%v differs from the brute-force resolver pass=%v", got, want)
	}

	start = time.Now()
	got, err = w.brandOpen(t, pool, OpCasinoBet)
	warm := time.Since(start)
	if err != nil || !got {
		t.Fatalf("warm: open=%v err=%v", got, err)
	}
	if warm > warmBound {
		t.Fatalf("warm path (hint hit) took %v (bound %v); cold was %v", warm, warmBound, cold)
	}
	t.Logf("%d countries: cold %v, warm %v", scaleCountries, cold, warm)

	// A STALE hint never changes a verdict: close the hinted country and the answer follows the resolver.
	w.brandDisabledFor(t, pool, open)
	if got, _ := w.brandOpen(t, pool, OpCasinoBet); got || got != w.bruteForce(t, pool, OpCasinoBet) {
		t.Fatalf("after closing the hinted country the brand must be closed (and equal brute force), got %v", got)
	}
}

// Candidates without an enabled ceiling are never resolved, yet the verdict equals brute force.
func TestAnyCountryPermits_PrefilterDropsOnlyCeilinglessCandidates(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	w.openMarket(t, pool, "MT") // ceiling + tenant row
	// Withdraw the ceiling by disabling it: the tenant row stays enabled (orphaned opt-in).
	w.disableCeiling(t, pool, "MT")
	resetHints()
	got, err := w.brandOpen(t, pool, OpCasinoBet)
	if err != nil || got || got != w.bruteForce(t, pool, OpCasinoBet) {
		t.Fatalf("an orphaned opt-in must not open the brand: open=%v err=%v", got, err)
	}
}

func (w world) disableCeiling(t *testing.T, pool *db.Pool, cc string) {
	t.Helper()
	err := pool.WithPlatformAdmin(context.Background(), w.admin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := operatingmarket.CreateLicenceCountryCeilingVersion(ctx, tx, operatingmarket.CreateLicenceCountryCeilingVersionParams{
			LicenceID: w.licenceID, CountryCode: cc, State: operatingmarket.StateDisabled, Status: operatingmarket.StatusActive,
			Actor: actor(w.admin, "scale-test"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable ceiling: %v", err)
	}
}

var _ = uuid.Nil

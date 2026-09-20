//go:build integration

package operatingmarket

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestIsRegistrationPermitted_FailsClosedAndLeaksNoScope(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	// Not configured at all -> false, nil error.
	ccUnconfigured := "EC"
	var permitted bool
	var err error
	poolErr := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		permitted, err = IsRegistrationPermitted(ctx, tx, RegistrationQuery{
			TenantID: f.tenantID, CountryCode: ccUnconfigured, AsOf: time.Now().UTC(),
		})
		return nil
	})
	if poolErr != nil {
		t.Fatalf("pool error: %v", poolErr)
	}
	if err != nil {
		t.Fatalf("expected nil error for not_configured, got %v", err)
	}
	if permitted {
		t.Fatal("expected false for an unconfigured country")
	}

	// Ceiling disabled -> false.
	ccDisabled := "BZ"
	disableCeiling(t, pool, f.platformAdmin, f.licenceID, ccDisabled)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var innerErr error
		permitted, innerErr = IsRegistrationPermitted(ctx, tx, RegistrationQuery{
			TenantID: f.tenantID, CountryCode: ccDisabled, AsOf: time.Now().UTC(),
		})
		return innerErr
	})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if permitted {
		t.Fatal("expected false with a disabled ceiling")
	}

	// Ceiling enabled, tenant enabled, but the `registration` OPERATION
	// row itself disabled -> false.
	ccRegDisabled := "GY"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, ccRegDisabled)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, ccRegDisabled)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeOperation, TenantID: f.tenantID, OperationCode: "registration", CountryCode: ccRegDisabled,
			State: StateDisabled, Status: StatusActive, Actor: testActor(f.staffActorID, "disable-registration"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable registration operation: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var innerErr error
		permitted, innerErr = IsRegistrationPermitted(ctx, tx, RegistrationQuery{
			TenantID: f.tenantID, CountryCode: ccRegDisabled, AsOf: time.Now().UTC(),
		})
		return innerErr
	})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if permitted {
		t.Fatal("expected false when registration is specifically disabled")
	}

	// Fully permitted case -> true.
	ccPermitted := "SR"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, ccPermitted)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, ccPermitted)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var innerErr error
		permitted, innerErr = IsRegistrationPermitted(ctx, tx, RegistrationQuery{
			TenantID: f.tenantID, CountryCode: ccPermitted, AsOf: time.Now().UTC(),
		})
		return innerErr
	})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !permitted {
		t.Fatal("expected true for a fully permitted country")
	}

	// Error case: wrong transaction scope -> (false, non-nil error). Never
	// (true, err).
	err = pool.WithTenant(context.Background(), f.otherTenantID, func(ctx context.Context, tx pgx.Tx) error {
		var innerErr error
		permitted, innerErr = IsRegistrationPermitted(ctx, tx, RegistrationQuery{
			TenantID: f.tenantID, CountryCode: ccPermitted, AsOf: time.Now().UTC(),
		})
		if innerErr == nil {
			t.Fatal("expected a non-nil error for a mismatched transaction scope")
		}
		if permitted {
			t.Fatal("expected false alongside the error - NEVER (true, err)")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("pool error: %v", err)
	}
}

func TestExplain_MatchesResolveOutcomeAlways(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "VE"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	q := Query{TenantID: f.tenantID, CountryCode: cc, OperationCode: opWagering, AsOf: time.Now().UTC()}

	var resolveOutcome, explainOutcome Outcome
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		res, err := ResolveOperatingCountryPolicy(ctx, tx, q)
		if err != nil {
			return err
		}
		resolveOutcome = res.Outcome()
		exp, err := ExplainOperatingCountryPolicy(ctx, tx, q)
		if err != nil {
			return err
		}
		explainOutcome = exp.Outcome
		if len(exp.Chain) == 0 {
			t.Fatal("expected a non-empty diagnostic chain for a resolvable query")
		}
		if exp.PolicyVersion != PolicyVersion {
			t.Fatalf("expected PolicyVersion %q, got %q", PolicyVersion, exp.PolicyVersion)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("resolve/explain: %v", err)
	}
	if resolveOutcome != explainOutcome {
		t.Fatalf("ExplainOperatingCountryPolicy's Outcome (%s) must always equal ResolveOperatingCountryPolicy's Outcome (%s)", explainOutcome, resolveOutcome)
	}
	if resolveOutcome != OutcomePermitted {
		t.Fatalf("expected permitted, got %s", resolveOutcome)
	}

	// Also check a BLOCKED case names the blocking scope correctly.
	ccBlocked := "TT"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, ccBlocked)
	enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, ccBlocked)
	disableTenantPolicy(t, pool, f.tenantID, f.staffActorID, ccBlocked)
	qBlocked := Query{TenantID: f.tenantID, CountryCode: ccBlocked, OperationCode: opWagering, AsOf: time.Now().UTC()}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		res, err := ResolveOperatingCountryPolicy(ctx, tx, qBlocked)
		if err != nil {
			return err
		}
		exp, err := ExplainOperatingCountryPolicy(ctx, tx, qBlocked)
		if err != nil {
			return err
		}
		if exp.Outcome != res.Outcome() {
			t.Fatalf("mismatch: resolve=%s explain=%s", res.Outcome(), exp.Outcome)
		}
		if exp.BlockingScope != ScopeTenant {
			t.Fatalf("expected BlockingScope tenant, got %q", exp.BlockingScope)
		}
		if exp.BlockingVersionID == nil {
			t.Fatal("expected a non-nil BlockingVersionID")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("resolve/explain blocked case: %v", err)
	}
}

func TestOperatingMarketAudit_RecordsBeforeAfterAndNeverPlayerEvidence(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cc := "JM"
	ceiling := enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)

	// Ceiling audit: platform-scoped.
	var ceilingMetadata []byte
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT metadata FROM audit_log
			 WHERE action = 'operating_market.licence_ceiling_version_created' AND target_id = $1`,
			ceiling.ID.String()).Scan(&ceilingMetadata)
	})
	if err != nil {
		t.Fatalf("read ceiling audit row: %v", err)
	}
	var ceilingMeta map[string]any
	if err := json.Unmarshal(ceilingMetadata, &ceilingMeta); err != nil {
		t.Fatalf("unmarshal ceiling audit metadata: %v", err)
	}
	if ceilingMeta["scope"] != "licence" {
		t.Fatalf("expected scope=licence, got %v", ceilingMeta["scope"])
	}
	if ceilingMeta["country_code"] != cc {
		t.Fatalf("expected country_code=%s, got %v", cc, ceilingMeta["country_code"])
	}
	if ceilingMeta["state"] != "enabled" {
		t.Fatalf("expected state=enabled, got %v", ceilingMeta["state"])
	}
	after, ok := ceilingMeta["after"].(map[string]any)
	if !ok {
		t.Fatalf("expected 'after' to be a real object, got %T: %v", ceilingMeta["after"], ceilingMeta["after"])
	}
	if after["id"] != ceiling.ID.String() {
		t.Fatalf("expected after.id to be the new version's id, got %v", after["id"])
	}
	if ceilingMeta["before"] != nil {
		t.Fatalf("expected before=nil for the FIRST version, got %v", ceilingMeta["before"])
	}

	// Policy audit: tenant-scoped, and a SECOND version's audit row
	// carries a real, non-nil 'before'.
	rec := enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)
	rec2 := disableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	var policyMetadata []byte
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT metadata FROM audit_log
			 WHERE tenant_id = $1 AND action = 'operating_market.policy_version_created' AND target_id = $2`,
			f.tenantID, rec2.ID.String()).Scan(&policyMetadata)
	})
	if err != nil {
		t.Fatalf("read policy audit row: %v", err)
	}
	var policyMeta map[string]any
	if err := json.Unmarshal(policyMetadata, &policyMeta); err != nil {
		t.Fatalf("unmarshal policy audit metadata: %v", err)
	}
	before, ok := policyMeta["before"].(map[string]any)
	if !ok {
		t.Fatalf("expected 'before' to be a real object for the second version, got %T: %v", policyMeta["before"], policyMeta["before"])
	}
	if before["id"] != rec.ID.String() {
		t.Fatalf("expected before.id to be the FIRST version's id %s, got %v", rec.ID, before["id"])
	}
	afterPolicy, ok := policyMeta["after"].(map[string]any)
	if !ok {
		t.Fatalf("expected 'after' to be a real object, got %T", policyMeta["after"])
	}
	if afterPolicy["state"] != "disabled" {
		t.Fatalf("expected after.state=disabled, got %v", afterPolicy["state"])
	}

	// Never any player evidence field.
	forbidden := []string{"player_account_id", "residence", "declared_residence", "verified_residence", "location_signal", "geo", "ip_country", "evidence"}
	raw := strings.ToLower(string(policyMetadata) + string(ceilingMetadata))
	for _, f := range forbidden {
		if strings.Contains(raw, f) {
			t.Fatalf("audit metadata must NEVER contain a player-evidence field %q, found in: %s", f, raw)
		}
	}
}

// TestOperatingMarketAudit_SecondVersionRecordsPriorEffectiveTo is Fix 1
// (Phase E fix round): the "before" state used to be read BEFORE the
// close UPDATE ran, so the audit metadata's prior_effective_to was always
// JSON null even on a second-or-later version. Both writers
// (CreateOperatingCountryPolicyVersion and
// CreateLicenceCountryCeilingVersion) now use `UPDATE ... RETURNING
// effective_to` to close the prior version and fold that returned value
// into prior_effective_to. On the FIRST version for a key it is null; on
// every version after that it is non-null RFC3339Nano and equals the
// closed row's own actual effective_to.
func TestOperatingMarketAudit_SecondVersionRecordsPriorEffectiveTo(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	// --- Policy writer (CreateOperatingCountryPolicyVersion) ---
	cc := "KN"
	enableCeiling(t, pool, f.platformAdmin, f.licenceID, cc)
	rec1 := enableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	readPolicyAuditMetadata := func(targetID string) map[string]any {
		t.Helper()
		var raw []byte
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT metadata FROM audit_log WHERE tenant_id = $1 AND action = 'operating_market.policy_version_created' AND target_id = $2`,
				f.tenantID, targetID).Scan(&raw)
		})
		if err != nil {
			t.Fatalf("read audit row for %s: %v", targetID, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal audit metadata: %v", err)
		}
		return m
	}

	m1 := readPolicyAuditMetadata(rec1.ID.String())
	if v, present := m1["prior_effective_to"]; !present || v != nil {
		t.Fatalf("expected prior_effective_to to be null on the FIRST version, got %v", v)
	}

	rec2 := disableTenantPolicy(t, pool, f.tenantID, f.staffActorID, cc)

	var rec1EffectiveTo time.Time
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT effective_to FROM operating_country_policies WHERE id = $1`, rec1.ID).Scan(&rec1EffectiveTo)
	})
	if err != nil {
		t.Fatalf("read rec1's closed effective_to: %v", err)
	}

	m2 := readPolicyAuditMetadata(rec2.ID.String())
	priorRaw, ok := m2["prior_effective_to"].(string)
	if !ok {
		t.Fatalf("expected prior_effective_to to be a non-null string on the SECOND version, got %T: %v", m2["prior_effective_to"], m2["prior_effective_to"])
	}
	priorParsed, err := time.Parse(time.RFC3339Nano, priorRaw)
	if err != nil {
		t.Fatalf("parse prior_effective_to as RFC3339Nano: %v", err)
	}
	if !priorParsed.Equal(rec1EffectiveTo) {
		t.Fatalf("expected prior_effective_to (%v) to equal the closed row's actual effective_to (%v)", priorParsed, rec1EffectiveTo)
	}

	// --- Ceiling writer (CreateLicenceCountryCeilingVersion) ---
	ccCeiling := "LK"
	ceilingRec1 := enableCeiling(t, pool, f.platformAdmin, f.licenceID, ccCeiling)

	readCeilingAuditMetadata := func(targetID string) map[string]any {
		t.Helper()
		var raw []byte
		err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT metadata FROM audit_log WHERE action = 'operating_market.licence_ceiling_version_created' AND target_id = $1`,
				targetID).Scan(&raw)
		})
		if err != nil {
			t.Fatalf("read ceiling audit row for %s: %v", targetID, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal ceiling audit metadata: %v", err)
		}
		return m
	}

	cm1 := readCeilingAuditMetadata(ceilingRec1.ID.String())
	if v, present := cm1["prior_effective_to"]; !present || v != nil {
		t.Fatalf("expected prior_effective_to to be null on the FIRST ceiling version, got %v", v)
	}

	ceilingRec2 := disableCeiling(t, pool, f.platformAdmin, f.licenceID, ccCeiling)

	var ceilingRec1EffectiveTo time.Time
	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT effective_to FROM licence_country_ceilings WHERE id = $1`, ceilingRec1.ID).Scan(&ceilingRec1EffectiveTo)
	})
	if err != nil {
		t.Fatalf("read ceilingRec1's closed effective_to: %v", err)
	}

	cm2 := readCeilingAuditMetadata(ceilingRec2.ID.String())
	cPriorRaw, ok := cm2["prior_effective_to"].(string)
	if !ok {
		t.Fatalf("expected prior_effective_to to be a non-null string on the SECOND ceiling version, got %T: %v", cm2["prior_effective_to"], cm2["prior_effective_to"])
	}
	cPriorParsed, err := time.Parse(time.RFC3339Nano, cPriorRaw)
	if err != nil {
		t.Fatalf("parse ceiling prior_effective_to as RFC3339Nano: %v", err)
	}
	if !cPriorParsed.Equal(ceilingRec1EffectiveTo) {
		t.Fatalf("expected ceiling prior_effective_to (%v) to equal the closed row's actual effective_to (%v)", cPriorParsed, ceilingRec1EffectiveTo)
	}
}

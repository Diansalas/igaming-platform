package risk

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func uuidPtr(u uuid.UUID) *uuid.UUID { return &u }

func timePtr(t time.Time) *time.Time { return &t }

func parseTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return parsed
}

func TestRule_Matches_ScopeDimensions(t *testing.T) {
	tenantA, tenantB := uuid.New(), uuid.New()
	brandA := uuid.New()
	playerX := uuid.New()
	gameX := uuid.New()

	req := RiskRequest{
		TenantID: tenantA, BrandID: brandA, PlayerAccountID: playerX,
		Operation: OperationCasinoBet, ProviderID: "mock", GameID: gameX, AssetCode: "EUR",
		LicensingMode: "under_platform_licence",
	}

	cases := []struct {
		name string
		rule Rule
		want bool
	}{
		{"platform-wide rule with no scope matches anything of the same operation", Rule{Operation: OperationCasinoBet}, true},
		{"wrong operation never matches", Rule{Operation: OperationCasinoLaunch}, false},
		{"matching tenant matches", Rule{Operation: OperationCasinoBet, TenantID: &tenantA}, true},
		{"different tenant does not match", Rule{Operation: OperationCasinoBet, TenantID: &tenantB}, false},
		{"matching player matches", Rule{Operation: OperationCasinoBet, PlayerAccountID: &playerX}, true},
		{"different player does not match", Rule{Operation: OperationCasinoBet, PlayerAccountID: uuidPtr(uuid.New())}, false},
		{"matching game matches", Rule{Operation: OperationCasinoBet, GameID: &gameX}, true},
		{"different game does not match", Rule{Operation: OperationCasinoBet, GameID: uuidPtr(uuid.New())}, false},
		{"matching asset matches", Rule{Operation: OperationCasinoBet, AssetCode: "EUR"}, true},
		{"different asset does not match", Rule{Operation: OperationCasinoBet, AssetCode: "USD"}, false},
		{"matching provider matches", Rule{Operation: OperationCasinoBet, ProviderID: "mock"}, true},
		{"different provider does not match", Rule{Operation: OperationCasinoBet, ProviderID: "other"}, false},
		{"matching licensing mode matches", Rule{Operation: OperationCasinoBet, LicensingMode: "under_platform_licence"}, true},
		{"different licensing mode does not match", Rule{Operation: OperationCasinoBet, LicensingMode: "own_licence"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.rule.matches(req); got != c.want {
				t.Fatalf("matches() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestRule_Matches_GameScopedRuleNeverMatchesAGamelessRequest(t *testing.T) {
	// A request with no GameID (e.g. a deposit) must never accidentally
	// satisfy a game-scoped rule - uuid.Nil is not a wildcard.
	r := Rule{Operation: OperationDeposit, GameID: uuidPtr(uuid.New())}
	req := RiskRequest{TenantID: uuid.New(), BrandID: uuid.New(), Operation: OperationDeposit}
	if r.matches(req) {
		t.Fatal("expected a game-scoped rule to never match a request with no GameID")
	}
}

func TestRule_Specificity_Ordering(t *testing.T) {
	player := Rule{PlayerAccountID: uuidPtr(uuid.New())}
	game := Rule{GameID: uuidPtr(uuid.New())}
	provider := Rule{ProviderID: "mock"}
	brand := Rule{BrandID: uuidPtr(uuid.New())}
	tenant := Rule{TenantID: uuidPtr(uuid.New())}
	jurisdiction := Rule{JurisdictionCode: "KM-ANJ"}
	licensingMode := Rule{LicensingMode: "under_platform_licence"}
	platform := Rule{}

	ordered := []Rule{player, game, provider, brand, tenant, jurisdiction, licensingMode, platform}
	for i := 0; i < len(ordered)-1; i++ {
		if ordered[i].specificity() <= ordered[i+1].specificity() {
			t.Fatalf("expected strictly decreasing specificity at index %d: %d <= %d", i, ordered[i].specificity(), ordered[i+1].specificity())
		}
	}
}

func TestRule_IsEffective_StatusAndWindow(t *testing.T) {
	now := parseTime(t, "2026-01-15T12:00:00Z")
	cases := []struct {
		name string
		rule Rule
		want bool
	}{
		{"active, no bounds", Rule{Status: RuleActive, EffectiveFrom: parseTime(t, "2026-01-01T00:00:00Z")}, true},
		{"disabled is never effective", Rule{Status: RuleDisabled, EffectiveFrom: parseTime(t, "2026-01-01T00:00:00Z")}, false},
		{"not yet effective", Rule{Status: RuleActive, EffectiveFrom: parseTime(t, "2026-02-01T00:00:00Z")}, false},
		{"expired", Rule{Status: RuleActive, EffectiveFrom: parseTime(t, "2026-01-01T00:00:00Z"), EffectiveUntil: timePtr(parseTime(t, "2026-01-10T00:00:00Z"))}, false},
		{"within an explicit window", Rule{Status: RuleActive, EffectiveFrom: parseTime(t, "2026-01-01T00:00:00Z"), EffectiveUntil: timePtr(parseTime(t, "2026-01-20T00:00:00Z"))}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.rule.isEffective(now); got != c.want {
				t.Fatalf("isEffective() = %v, want %v", got, c.want)
			}
		})
	}
}

// --- Stage 4H-B0-R6 Workstream D: unit-level fail-closed coverage ---

func TestLimitKind_IsAmountShaped(t *testing.T) {
	for _, k := range []LimitKind{LimitMinAmount, LimitMaxAmount, LimitCumulativeAmount} {
		if !k.isAmountShaped() {
			t.Fatalf("%q must be amount-shaped: it needs an asset context and a threshold denomination", k)
		}
	}
	for _, k := range []LimitKind{LimitKind(""), LimitKind("count"), LimitKind("velocity")} {
		if k.isAmountShaped() {
			t.Fatalf("%q must not be treated as amount-shaped", k)
		}
	}
}

func TestIsKnownOperation_ClosedSet(t *testing.T) {
	for _, o := range []Operation{
		OperationCasinoLaunch, OperationCasinoBet, OperationDeposit,
		OperationWithdrawal, OperationSportsbookBet, OperationBonusGrant,
		// OperationBonusConversion joined the known set in Stage 4H-B1
		// Wave 2 Phase 4 (migration 0065 widened the CHECK constraint
		// this test's own failure message names) - ADR 0031 §16/§16a/
		// §40's long-documented, now-landed extension.
		OperationBonusConversion,
	} {
		if !IsKnownOperation(o) {
			t.Fatalf("%q is declared by this package and accepted by migration 0041 (as widened by migration 0065), so it must be known", o)
		}
	}
	// A typo, an empty value, or a proposed-but-unstorable operation
	// (ADR 0031 §26/§36) must never silently match zero rules and ALLOW.
	for _, o := range []Operation{
		Operation(""), Operation("casino_bett"), Operation("CASINO_BET"),
		Operation("sportsbook_settlement"), Operation("sportsbook_cashout"), Operation("bonus_activate"),
	} {
		if IsKnownOperation(o) {
			t.Fatalf("%q must NOT be a known operation", o)
		}
	}
}

func TestNumericToBigInt_HandlesPostgresScaleAndRefusesFractions(t *testing.T) {
	cases := []struct {
		name string
		in   pgtype.Numeric
		want string
	}{
		{"invalid is zero", pgtype.Numeric{}, "0"},
		{"plain integer", pgtype.Numeric{Int: big.NewInt(1234), Exp: 0, Valid: true}, "1234"},
		// PostgreSQL legitimately returns an exact integer SUM in
		// scientific form (1000 as 1E+3) - rejecting that was an
		// availability defect in the pre-Stage-4H-B0-R6 version.
		{"scaled integer", pgtype.Numeric{Int: big.NewInt(1), Exp: 3, Valid: true}, "1000"},
		{"negative scaled integer", pgtype.Numeric{Int: big.NewInt(-25), Exp: 2, Valid: true}, "-2500"},
		{"beyond int64", pgtype.Numeric{Int: big.NewInt(99), Exp: 18, Valid: true}, "99000000000000000000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := numericToBigInt(c.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.String() != c.want {
				t.Fatalf("got %s, want %s", got.String(), c.want)
			}
		})
	}
	// A fractional minor-unit value cannot exist in this schema, and
	// truncating one would be silent money-mangling.
	if _, err := numericToBigInt(pgtype.Numeric{Int: big.NewInt(105), Exp: -1, Valid: true}); err == nil {
		t.Fatal("expected a fractional NUMERIC to be refused rather than rounded")
	}
}

func TestRule_MissingScopeContext_EveryOptionalDimension(t *testing.T) {
	gameID := uuid.New()
	base := RiskRequest{Operation: OperationCasinoBet}
	cases := []struct {
		name string
		rule Rule
		want error
	}{
		{"jurisdiction", Rule{Operation: OperationCasinoBet, JurisdictionCode: "KM-ANJ"}, ErrMissingJurisdiction},
		{"licensing mode", Rule{Operation: OperationCasinoBet, LicensingMode: "own_licence"}, ErrMissingLicensingMode},
		{"asset", Rule{Operation: OperationCasinoBet, AssetCode: "BTC"}, ErrMissingAsset},
		{"product", Rule{Operation: OperationCasinoBet, Product: "casino"}, ErrMissingScopeContext},
		{"provider", Rule{Operation: OperationCasinoBet, ProviderID: "mock"}, ErrMissingScopeContext},
		{"payment method", Rule{Operation: OperationCasinoBet, PaymentMethod: "card"}, ErrMissingScopeContext},
		{"game", Rule{Operation: OperationCasinoBet, GameID: &gameID}, ErrMissingScopeContext},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.rule.missingScopeContext(base)
			if err == nil {
				t.Fatal("expected a rule scoping a dimension the request left empty to fail closed")
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("expected %v, got %v", c.want, err)
			}
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("every missing-scope error must remain an ErrInvalidInput for its HTTP mapping, got %v", err)
			}
		})
	}

	// A fully-populated request is never gated, and an unscoped rule
	// never demands anything.
	full := RiskRequest{
		Operation: OperationCasinoBet, JurisdictionCode: "KM-ANJ", LicensingMode: "under_platform_licence",
		AssetCode: "EUR", Product: "casino", ProviderID: "mock", PaymentMethod: "card", GameID: gameID,
	}
	for _, c := range cases {
		if err := c.rule.missingScopeContext(full); err != nil {
			t.Fatalf("%s: a fully-populated request must not be gated: %v", c.name, err)
		}
	}
	if err := (Rule{Operation: OperationCasinoBet}).missingScopeContext(base); err != nil {
		t.Fatalf("an unscoped rule must demand nothing: %v", err)
	}
}

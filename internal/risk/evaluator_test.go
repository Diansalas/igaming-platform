package risk

import (
	"testing"
	"time"

	"github.com/google/uuid"
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

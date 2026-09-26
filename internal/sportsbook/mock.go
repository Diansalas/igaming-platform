package sportsbook

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// MockSportsbookProvider is a Provider implementation with a small,
// realistic, deterministic-in-SHAPE catalogue (a handful of sports,
// competitions, events, markets, selections with plausible fixed odds) -
// this stage's ONLY provider (CLAUDE.md's "does not integrate a real
// provider without a confirmed commercial relationship" limitation).
//
// MOCK: labeled per CLAUDE.md's "No fake completion" rule - a synthetic
// double for development/testing, never a real sportsbook data feed or
// odds provider (this package never builds odds/trading/risk management
// in-house - the fixed odds below are static seed data, not a pricing
// engine).
//
// Event start times are computed RELATIVE TO time.Now() at the moment
// Catalogue() is called, never a fixed calendar date baked into a
// migration - so a SyncCatalogue run (catalogue.go) always produces
// "near-future" events regardless of how long after this code was
// written it actually runs.
type MockSportsbookProvider struct {
	now func() time.Time
}

// NewMockSportsbookProvider constructs the mock adapter.
func NewMockSportsbookProvider() *MockSportsbookProvider {
	return &MockSportsbookProvider{now: time.Now}
}

// SyntheticComponent implements providerkind.Synthetic (Stage 10.3,
// MOCK-ADAPTER-PROD-1) - a structural marker only, satisfied without this
// package importing internal/providerkind. This is the sportsbook mock
// catalogue named explicitly in 01-provider-trust-analysis.md §3: its
// catalogue is written to the database at every startup, in every
// environment, until W1b's guard exists.
func (m *MockSportsbookProvider) SyntheticComponent() {}

// Catalogue implements Provider.
func (m *MockSportsbookProvider) Catalogue() CatalogueResult {
	now := m.now().UTC()

	return CatalogueResult{
		Sports: []CatalogueSport{
			{
				ExternalRef: "mock-sport-football", Code: "football", Name: "Football",
				Competitions: []CatalogueCompetition{
					{
						ExternalRef: "mock-comp-premier-league", Name: "Premier League",
						Events: []CatalogueEvent{
							mockMatchWinnerEvent("mock-event-arsenal-chelsea", "Arsenal vs Chelsea", now.Add(48*time.Hour), 210, 100, 320, 100, 280, 100),
							mockMatchWinnerEvent("mock-event-liverpool-city", "Liverpool vs Manchester City", now.Add(72*time.Hour), 260, 100, 330, 100, 240, 100),
						},
					},
					{
						ExternalRef: "mock-comp-la-liga", Name: "La Liga",
						Events: []CatalogueEvent{
							mockMatchWinnerEvent("mock-event-madrid-barca", "Real Madrid vs Barcelona", now.Add(96*time.Hour), 220, 100, 310, 100, 300, 100),
						},
					},
				},
			},
			{
				ExternalRef: "mock-sport-basketball", Code: "basketball", Name: "Basketball",
				Competitions: []CatalogueCompetition{
					{
						ExternalRef: "mock-comp-nba", Name: "NBA",
						Events: []CatalogueEvent{
							mockTwoWayEvent("mock-event-lakers-celtics", "Los Angeles Lakers vs Boston Celtics", now.Add(30*time.Hour), 185, 100, 195, 100),
							mockTwoWayEvent("mock-event-warriors-nets", "Golden State Warriors vs Brooklyn Nets", now.Add(54*time.Hour), 150, 100, 250, 100),
						},
					},
				},
			},
			{
				ExternalRef: "mock-sport-tennis", Code: "tennis", Name: "Tennis",
				Competitions: []CatalogueCompetition{
					{
						ExternalRef: "mock-comp-atp-masters", Name: "ATP Masters 1000",
						Events: []CatalogueEvent{
							mockTwoWayEvent("mock-event-alcaraz-sinner", "Carlos Alcaraz vs Jannik Sinner", now.Add(20*time.Hour), 175, 100, 205, 100),
						},
					},
				},
			},
		},
	}
}

// mockMatchWinnerEvent builds a "Match Winner" (1X2) market: Home/Draw/Away.
func mockMatchWinnerEvent(externalRef, name string, startTime time.Time, homeNum, homeDen, drawNum, drawDen, awayNum, awayDen int64) CatalogueEvent {
	return CatalogueEvent{
		ExternalRef: externalRef, Name: name, StartTime: startTime, Status: EventScheduled,
		Markets: []CatalogueMarket{
			{
				ExternalRef: externalRef + "-match-winner", Name: "Match Winner", Status: MarketOpen,
				Selections: []CatalogueSelection{
					{ExternalRef: externalRef + "-home", Name: "Home", OddsNumerator: homeNum, OddsDenominator: homeDen},
					{ExternalRef: externalRef + "-draw", Name: "Draw", OddsNumerator: drawNum, OddsDenominator: drawDen},
					{ExternalRef: externalRef + "-away", Name: "Away", OddsNumerator: awayNum, OddsDenominator: awayDen},
				},
			},
		},
	}
}

// mockTwoWayEvent builds a "Match Winner" market with no draw (basketball/
// tennis have no drawn outcome).
func mockTwoWayEvent(externalRef, name string, startTime time.Time, homeNum, homeDen, awayNum, awayDen int64) CatalogueEvent {
	return CatalogueEvent{
		ExternalRef: externalRef, Name: name, StartTime: startTime, Status: EventScheduled,
		Markets: []CatalogueMarket{
			{
				ExternalRef: externalRef + "-match-winner", Name: "Match Winner", Status: MarketOpen,
				Selections: []CatalogueSelection{
					{ExternalRef: externalRef + "-home", Name: "Home", OddsNumerator: homeNum, OddsDenominator: homeDen},
					{ExternalRef: externalRef + "-away", Name: "Away", OddsNumerator: awayNum, OddsDenominator: awayDen},
				},
			},
		},
	}
}

// SettlementStatementLine and SettlementStatementSource are the
// reconciliation statement contract (internal/reconciliation/statement,
// a dependency-free leaf so neither package imports the other), aliased
// here for the sportsbook domain's own use.
type (
	SettlementStatementLine   = statement.SportsbookSettlementLine
	SettlementStatementSource = statement.SportsbookSettlementSource
)

// MockSettlementStatementLabel is the label every record, audit entry and
// log line of the mock statement match carries.
const MockSettlementStatementLabel = "MOCK in-house settlement statement (rendered from sportsbook_bet_settlements; real provider statement matching is PROVIDER DEPENDENT, ADR 0038 §12)"

// MockSettlementStatementSource is the ADR 0088 §8.4 statement source,
// injected into the reconciliation sweep by cmd/platform-api.
//
// MOCK: in-house mode has no provider and therefore no provider statement.
// This source renders a statement from the platform's OWN settlement
// history table (sportsbook_bet_settlements), so on uncorrupted data it
// is tautological by construction and needs no new storage. What it
// proves is the matching path: the stream compares these lines against
// the ledger-derived view of every bet, exactly as it will compare a real
// provider's statement. Real statement ingestion and matching are
// PROVIDER DEPENDENT (ADR 0038 §12) and NOT IMPLEMENTED.
//
// Rendering rule: one line per un-reversed settlement row (generation,
// outcome, payout, asset) and one line per void row (void flag, asset).
type MockSettlementStatementSource struct{}

var _ SettlementStatementSource = MockSettlementStatementSource{}

// Label implements SettlementStatementSource.
func (MockSettlementStatementSource) Label() string { return MockSettlementStatementLabel }

// SyntheticComponent implements providerkind.Synthetic (Stage 10.3,
// MOCK-ADAPTER-PROD-1) - a structural marker only, satisfied without this
// package importing internal/providerkind.
func (MockSettlementStatementSource) SyntheticComponent() {}

// StatementLines implements SettlementStatementSource. Read-only.
func (MockSettlementStatementSource) StatementLines(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]SettlementStatementLine, error) {
	rows, err := tx.Query(ctx, `
		SELECT s.bet_id, s.generation, s.outcome, s.payout_amount, s.asset_code, false
		  FROM sportsbook_bet_settlements s
		 WHERE s.tenant_id = $1 AND s.event_kind = 'settlement'
		   AND NOT EXISTS (SELECT 1 FROM sportsbook_bet_settlements r
		                    WHERE r.event_kind = 'rollback' AND r.reverses_settlement_id = s.id)
		UNION ALL
		SELECT v.bet_id, NULL, NULL, 0, v.asset_code, true
		  FROM sportsbook_bet_settlements v
		 WHERE v.tenant_id = $1 AND v.event_kind = 'void'
		 ORDER BY 1, 2 NULLS LAST`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("sportsbook: MOCK settlement statement: %w", err)
	}
	defer rows.Close()
	var out []SettlementStatementLine
	for rows.Next() {
		var l SettlementStatementLine
		var generation *int32
		var outcome *string
		var payout *int64
		if err := rows.Scan(&l.BetID, &generation, &outcome, &payout, &l.AssetCode, &l.Void); err != nil {
			return nil, fmt.Errorf("sportsbook: MOCK settlement statement: scan: %w", err)
		}
		if generation != nil {
			g := int(*generation)
			l.Generation = &g
		}
		if outcome != nil {
			l.Outcome = *outcome
		}
		if payout != nil {
			l.PayoutAmount = *payout
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

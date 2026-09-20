package sportsbook

import "time"

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

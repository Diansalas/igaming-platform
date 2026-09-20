// Package sportsbook implements Stage 6's sportsbook vertical slice: the
// canonical catalogue (Sport -> Competition -> Event -> Market ->
// Selection), a mock provider adapter, and the fixed-odds "singles" bet
// placement orchestrator (stake locked into player_locked_cash, no
// external round-trip required since the mock provider is same-process -
// see Orchestrator.PlaceBet's own doc comment for why this makes bet
// placement a single synchronous call, unlike internal/casino's launch-
// token/callback split).
//
// Scope for this stage (CLAUDE.md's stage-gate rule, this stage's own
// directive): singles bets only (no multi-leg/accumulator/bet-builder),
// no settlement/void/partial-settlement/cashout (a bet stays "open" -
// that is explicitly out of scope), no live/in-play odds updates, and a
// MOCK provider only (no real commercial relationship exists - CLAUDE.md's
// "does not integrate a real provider without a confirmed commercial
// relationship" limitation). The canonical domain model this package
// implements is a deliberately narrowed subset of
// docs/architecture/09-sportsbook-architecture.md's full design (no
// Season/Participant/MarketType-vs-Market split, no Exposure/Liability
// distinction, no in-house trading engine) - this stage's directive is
// explicit that building the complete design is NOT the goal; only what a
// single fixed-odds singles bet placement flow needs. See this package's
// own doc comments for what is deferred, not silently dropped.
//
// Odds are stored as an integer numerator/denominator pair (never a
// float), and every stake/potential-return computation is done via
// internal/money's *big.Rat/*big.Int arithmetic - CLAUDE.md's "never use
// floating-point for money" rule, extended here to the odds multiplication
// itself even though odds are not money.
package sportsbook

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// EventStatus mirrors sb_events.status.
type EventStatus string

const (
	EventScheduled EventStatus = "scheduled"
	EventLive      EventStatus = "live"
	EventFinished  EventStatus = "finished"
	EventCancelled EventStatus = "cancelled"
)

// MarketStatus mirrors sb_markets.status.
type MarketStatus string

const (
	MarketOpen      MarketStatus = "open"
	MarketSuspended MarketStatus = "suspended"
	MarketClosed    MarketStatus = "closed"
)

// SelectionStatus mirrors sb_selections.status.
type SelectionStatus string

const (
	SelectionActive    SelectionStatus = "active"
	SelectionSuspended SelectionStatus = "suspended"
)

// BetStatus mirrors sportsbook_bets.status. Per this stage's explicit
// scope boundary, only BetStatusOpen is ever WRITTEN this stage - the
// other three values exist as a closed enum for future settlement work
// (docs/decisions/0038's settlement/void transaction types) so the column
// never needs a widening migration just to become readable.
type BetStatus string

const (
	BetStatusOpen        BetStatus = "open"
	BetStatusSettledWon  BetStatus = "settled_won"
	BetStatusSettledLost BetStatus = "settled_lost"
	BetStatusVoid        BetStatus = "void"
)

// Sport is a top-level catalogue classification (football, basketball,
// ...) - a pure catalogue node, platform-wide (no tenant_id), mirroring
// internal/casino's casino_games precedent for read-mostly, platform-
// administered catalogue data.
type Sport struct {
	ID   uuid.UUID
	Code string
	Name string
}

// Competition belongs to a Sport (e.g. a league or tournament).
type Competition struct {
	ID      uuid.UUID
	SportID uuid.UUID
	Name    string
}

// Event is a single fixture within a Competition - the unit a Market
// attaches to. No Season/Participant split this stage (see this
// package's own doc comment) - Participant names are folded into Name
// (e.g. "Arsenal vs Chelsea").
type Event struct {
	ID            uuid.UUID
	CompetitionID uuid.UUID
	Name          string
	StartTime     time.Time
	Status        EventStatus
}

// Market belongs to an Event (e.g. "Match Winner"). No MarketType
// template/instance split this stage - see this package's own doc
// comment for why that is an explicitly deferred simplification.
type Market struct {
	ID      uuid.UUID
	EventID uuid.UUID
	Name    string
	Status  MarketStatus
}

// Selection is one outcome within a Market a bettor can back (e.g.
// "Home"/"Draw"/"Away"), carrying fixed decimal odds as an integer
// numerator/denominator pair - e.g. numerator=250, denominator=100 means
// decimal odds of 2.50. Never a float (CLAUDE.md).
type Selection struct {
	ID              uuid.UUID
	MarketID        uuid.UUID
	Name            string
	OddsNumerator   int64
	OddsDenominator int64
	Status          SelectionStatus
}

// EventDetail is one event with its full market/selection tree - the
// shape GET /v1/sportsbook/events/{id} returns. Kept separate from Event
// itself so the shallow list endpoints (sports/competitions/events) never
// have to carry every market/selection row.
type EventDetail struct {
	Event
	CompetitionName string
	SportName       string
	SportCode       string
	Markets         []MarketDetail
}

// MarketDetail is one market with its selections.
type MarketDetail struct {
	Market
	Selections []Selection
}

// SportCatalogue is one sport with its nested competitions/events - the
// shape GET /v1/sportsbook/sports returns (a shallow browse view, no
// market/selection data - a client drills into GET /v1/sportsbook/events/
// {id} for that). The whole catalogue is small and fixed this stage
// (a handful of sports/competitions/events), so this single nested
// response is not an over-fetch the way a genuinely large catalogue's
// would be (see this package's own doc comment for the judgment call).
type SportCatalogue struct {
	Sport
	Competitions []CompetitionCatalogue
}

// CompetitionCatalogue is one competition with its nested event
// summaries.
type CompetitionCatalogue struct {
	Competition
	Events []EventSummary
}

// EventSummary is the shallow, browse-view projection of an Event -
// enough for a catalogue listing to link into GET /v1/sportsbook/events/
// {id} for the full market/selection tree.
type EventSummary struct {
	ID        uuid.UUID
	Name      string
	StartTime time.Time
	Status    EventStatus
}

// Bet mirrors a sportsbook_bets row - the tenant-owned, RLS-protected
// open-bet record (docs/decisions/0038 §2/§3). PotentialReturn is a
// DOMAIN PROJECTION computed at acceptance, never a ledger-visible fact
// (ADR 0038 §2: "potential return is a domain fact, never a ledger
// fact") - the only ledger-visible fact for an open bet is the stake
// sitting in player_locked_cash.
type Bet struct {
	ID                  uuid.UUID
	TenantID            uuid.UUID
	BrandID             uuid.UUID
	PlayerAccountID     uuid.UUID
	WalletID            uuid.UUID
	SelectionID         uuid.UUID
	AssetCode           string
	StakeAmount         int64
	OddsNumerator       int64
	OddsDenominator     int64
	PotentialReturn     int64
	Status              BetStatus
	IdempotencyKey      string
	LedgerTransactionID uuid.UUID
	PlacedAt            time.Time
}

// Sentinel errors. Mirrors internal/casino's "specific, distinguishable
// sentinel" convention throughout this codebase.
var (
	ErrInvalidInput      = errors.New("sportsbook: invalid input")
	ErrSportNotFound     = errors.New("sportsbook: sport not found")
	ErrEventNotFound     = errors.New("sportsbook: event not found")
	ErrSelectionNotFound = errors.New("sportsbook: selection not found")
	ErrBetNotFound       = errors.New("sportsbook: bet not found")
)

// Rejection categories - PlaceBetResult.RejectionCategory's closed set,
// deliberately distinguishable machine-readable values (never collapsed
// into one generic "rejected") so the frontend/bet-slip UI can render
// each cleanly, per this stage's directive: an odds-changed rejection
// needs a "refresh and re-confirm" flow, insufficient funds needs a
// top-up prompt, and an RG/risk denial needs a compliance-appropriate
// message - three materially different UX responses to one HTTP status.
const (
	RejectionOddsChanged       = "odds_changed"
	RejectionEventNotOpen      = "event_not_open"
	RejectionInsufficientFunds = "insufficient_funds"
	RejectionRGDenied          = "rg_denied"
	RejectionRiskDenied        = "risk_denied"
)

// Provider is sportsbook's minimal, provider-neutral catalogue-sync
// adapter interface (mirrors internal/casino.CasinoProvider's
// Catalogue() method at a much smaller scope, per docs/architecture/
// 09-sportsbook-architecture.md §2/§4's provider-neutral abstraction
// discipline - applied here to only the surface this stage's vertical
// slice needs). A real provider adapter is a drop-in later: it implements
// this same interface, and SyncCatalogue (catalogue.go) upserts its
// output into the same canonical tables, with zero change to the
// orchestrator or HTTP layer. PlaceBet/Settlement/Cashout adapter methods
// are deliberately NOT part of this interface this stage - no external
// provider round-trip occurs at placement time (a mock provider is
// same-process, per this package's own doc comment), and no settlement/
// cashout code is built this stage at all.
type Provider interface {
	Catalogue() CatalogueResult
}

// CatalogueResult is Provider.Catalogue's canonical, adapter-agnostic
// output shape - no free-form passthrough field, no provider SDK type,
// mirroring internal/casino.CatalogueEntry's identical discipline.
type CatalogueResult struct {
	Sports []CatalogueSport
}

// CatalogueSport is one sport and its full nested tree, as a provider
// declares it. ExternalRef is the provider's own identifier, retained
// only as a non-authoritative external reference at sync time
// (docs/architecture/09-sportsbook-architecture.md §2.5) - the platform
// mints its own id for every row.
type CatalogueSport struct {
	ExternalRef  string
	Code         string
	Name         string
	Competitions []CatalogueCompetition
}

type CatalogueCompetition struct {
	ExternalRef string
	Name        string
	Events      []CatalogueEvent
}

type CatalogueEvent struct {
	ExternalRef string
	Name        string
	StartTime   time.Time
	Status      EventStatus
	Markets     []CatalogueMarket
}

type CatalogueMarket struct {
	ExternalRef string
	Name        string
	Status      MarketStatus
	Selections  []CatalogueSelection
}

type CatalogueSelection struct {
	ExternalRef     string
	Name            string
	OddsNumerator   int64
	OddsDenominator int64
}

package sportsbook

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/providerref"
	"github.com/Diansalas/igaming-platform/internal/txscope"
)

// ErrCatalogueFetchRefused is the sentinel FetchCatalogue wraps ONLY when it
// refuses to call the provider at all because ctx is (despite the API shape
// below) already marked as holding a pooled database transaction
// (txscope.Held) - a programming bug at the call site, never an ordinary
// provider/network failure. Scoped identically to payments'
// ErrProviderCallRefused / casino's own sentinel (ADR 0095, defence-in-depth
// behind the primary API-shape control: no function that can reach
// Provider.Catalogue takes a pgx.Tx): each covers only its own gate-level
// refusal, never the adapter's own error (code review F5 - the prior
// revision's doc comment incorrectly said this sentinel also wrapped
// ordinary provider errors). A provider error is instead returned with
// plain context, unwrapped by this sentinel - see FetchCatalogue below.
var ErrCatalogueFetchRefused = errors.New("sportsbook: provider catalogue fetch refused: tx held")

// catalogueUnavailableReason is the ONE opaque, player-facing reason every
// jurisdiction-gated catalogue annotation reports (Stage 9.2, ADR 0083
// §5.4.1: "the marker must not leak the blocking rung... One opaque
// player-facing reason only"). Unlike PlaceBetResult.RejectionCode (which
// stays internally distinguishable and is collapsed only at the HTTP
// boundary, K3-6), EventSummary/MarketDetail/Selection.UnavailableReason
// is serialized directly, so the collapse happens HERE, at the point of
// annotation - never DenialCodeJurisdictionBlocked/
// DenialCodeJurisdictionUnresolved themselves.
const catalogueUnavailableReason = "not_available_in_your_jurisdiction"

// FetchCatalogue calls provider.Catalogue and validates the result - the
// ONLY sanctioned way to invoke a sportsbook Provider's catalogue method
// (SB-CATALOGUE-IO-1, ADR 0095 amendment 2026-09-28). It runs with NO
// pooled database transaction held: it refuses outright if ctx is already
// txscope-marked (the same defence-in-depth guard payments' callProvider
// and casino's Orchestrator.LaunchGame apply to their own adapter calls),
// and it never itself opens one. This keeps a provider network round-trip
// (a real future adapter's HTTP call) off any connection/lock hold, per
// ADR 0094 INV-POOL / ADR 0095's no-provider-I/O-with-a-connection-held
// rule - the same rule PAY-POOL-1/F-POOL-2 apply to payments/casino/KYC,
// now applied here even though the sync is not itself financial.
//
// Callers upsert FetchCatalogue's result via SyncCatalogue, inside its own
// separate db.Pool.WithPlatformService transaction, opened only AFTER this
// function returns - see cmd/platform-api/main.go's call site for the
// canonical two-step sequence. A fetch error or a validation failure here
// means nothing is ever written: no transaction is opened at all in that
// case, which is a strictly stronger guarantee than "nothing written
// inside the transaction" (SyncCatalogue's own pre-split behaviour).
func FetchCatalogue(ctx context.Context, provider Provider) (CatalogueResult, error) {
	if txscope.Held(ctx) {
		return CatalogueResult{}, ErrCatalogueFetchRefused
	}
	result, err := provider.Catalogue(ctx)
	if err != nil {
		return CatalogueResult{}, fmt.Errorf("sportsbook: fetch catalogue: %w", err)
	}
	// PROVIDER-REF-BOUND-1: the whole tree is validated here, before any
	// transaction is opened at all - a single bad external_ref rejects the
	// fetch with nothing ever written.
	if err := validateCatalogueReferences(result); err != nil {
		return CatalogueResult{}, err
	}
	return result, nil
}

// SyncCatalogue upserts an already-fetched (FetchCatalogue) catalogue
// result into the platform-wide sb_sports/sb_competitions/sb_events/
// sb_markets/sb_selections tables, keyed at each level by (parent id,
// external_ref) - the platform mints its own id once, at first insert, and
// never re-derives it from a provider's own reference again, mirroring
// internal/casino.UpsertGame's identical (provider_id, provider_game_id)
// keying discipline.
//
// tx MUST come from db.Pool.WithPlatformService(ctx,
// db.ServiceSportsbookCatalogueSync, ...) - migration 0084 (ADR 0081,
// ARCH-DB-2) gave these five tables ENABLE+FORCE row-level security with
// a write policy scoped to the app.platform_service_id =
// 'sportsbook_catalogue_sync' GUC, so a plain WithoutTenant transaction
// can no longer write them. AssertPlatformServiceScope below is this
// function's own in-Go, defence-in-depth check of that requirement,
// mirroring internal/jurisdiction's assertPlatformScope - migration
// 0084's policies enforce the identical predicate independently at the
// database regardless. Read is open (FOR SELECT USING (true)), exactly
// like casino_games (migration 0035) and the assets registry (migration
// 0003), unaffected by this requirement. A real provider adapter
// implementing Provider is a drop-in replacement for MockSportsbookProvider
// here - this function, the domain model, and every downstream reader are
// unchanged by which adapter produced the CatalogueResult
// (docs/architecture/09-sportsbook-architecture.md §0's design test,
// applied at this stage's narrower scope).
//
// result is validated again here (validateCatalogueReferences is pure and
// I/O-free, so this costs nothing) as defence in depth: a caller other
// than the canonical FetchCatalogue-then-SyncCatalogue sequence (e.g. a
// test) can never get SyncCatalogue to write a half-validated tree, even
// if it skipped FetchCatalogue's own validation step.
func SyncCatalogue(ctx context.Context, tx pgx.Tx, result CatalogueResult) error {
	if err := db.AssertPlatformServiceScope(ctx, tx, db.ServiceSportsbookCatalogueSync); err != nil {
		return err
	}
	if err := validateCatalogueReferences(result); err != nil {
		return err
	}
	for _, s := range result.Sports {
		sportID, err := upsertSport(ctx, tx, s.ExternalRef, s.Code, s.Name)
		if err != nil {
			return err
		}
		for _, c := range s.Competitions {
			compID, err := upsertCompetition(ctx, tx, sportID, c.ExternalRef, c.Name)
			if err != nil {
				return err
			}
			for _, e := range c.Events {
				eventID, err := upsertEvent(ctx, tx, compID, e.ExternalRef, e.Name, e.StartTime, e.Status)
				if err != nil {
					return err
				}
				for _, mkt := range e.Markets {
					marketID, err := upsertMarket(ctx, tx, eventID, mkt.ExternalRef, mkt.Name, mkt.Status)
					if err != nil {
						return err
					}
					for _, sel := range mkt.Selections {
						if _, err := upsertSelection(ctx, tx, marketID, sel.ExternalRef, sel.Name, sel.OddsNumerator, sel.OddsDenominator); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}

// validateCatalogueReferences applies the platform provider-reference
// bound to every external_ref in a provider catalogue tree.
func validateCatalogueReferences(result CatalogueResult) error {
	check := func(field, ref string) error {
		if err := providerref.Validate(field, ref); err != nil {
			return fmt.Errorf("%w: %w", ErrProviderReferenceInvalid, err)
		}
		return nil
	}
	for _, s := range result.Sports {
		if err := check("sport.external_ref", s.ExternalRef); err != nil {
			return err
		}
		for _, c := range s.Competitions {
			if err := check("competition.external_ref", c.ExternalRef); err != nil {
				return err
			}
			for _, e := range c.Events {
				if err := check("event.external_ref", e.ExternalRef); err != nil {
					return err
				}
				for _, m := range e.Markets {
					if err := check("market.external_ref", m.ExternalRef); err != nil {
						return err
					}
					for _, sel := range m.Selections {
						if err := check("selection.external_ref", sel.ExternalRef); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}

func upsertSport(ctx context.Context, tx pgx.Tx, externalRef, code, name string) (uuid.UUID, error) {
	id := uuid.New()
	_, err := tx.Exec(ctx,
		`INSERT INTO sb_sports (id, external_ref, code, name)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (external_ref) DO UPDATE SET code = EXCLUDED.code, name = EXCLUDED.name, updated_at = now()`,
		id, externalRef, code, name,
	)
	if err != nil {
		return uuid.Nil, fmt.Errorf("sportsbook: upsert sport: %w", err)
	}
	var resolvedID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM sb_sports WHERE external_ref = $1`, externalRef).Scan(&resolvedID); err != nil {
		return uuid.Nil, fmt.Errorf("sportsbook: resolve sport id: %w", err)
	}
	return resolvedID, nil
}

func upsertCompetition(ctx context.Context, tx pgx.Tx, sportID uuid.UUID, externalRef, name string) (uuid.UUID, error) {
	id := uuid.New()
	_, err := tx.Exec(ctx,
		`INSERT INTO sb_competitions (id, sport_id, external_ref, name)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (external_ref) DO UPDATE SET sport_id = EXCLUDED.sport_id, name = EXCLUDED.name, updated_at = now()`,
		id, sportID, externalRef, name,
	)
	if err != nil {
		return uuid.Nil, fmt.Errorf("sportsbook: upsert competition: %w", err)
	}
	var resolvedID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM sb_competitions WHERE external_ref = $1`, externalRef).Scan(&resolvedID); err != nil {
		return uuid.Nil, fmt.Errorf("sportsbook: resolve competition id: %w", err)
	}
	return resolvedID, nil
}

func upsertEvent(ctx context.Context, tx pgx.Tx, competitionID uuid.UUID, externalRef, name string, startTime time.Time, status EventStatus) (uuid.UUID, error) {
	id := uuid.New()
	_, err := tx.Exec(ctx,
		`INSERT INTO sb_events (id, competition_id, external_ref, name, start_time, status)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (external_ref) DO UPDATE SET
			competition_id = EXCLUDED.competition_id, name = EXCLUDED.name,
			start_time = EXCLUDED.start_time, status = EXCLUDED.status, updated_at = now()`,
		id, competitionID, externalRef, name, startTime, status,
	)
	if err != nil {
		return uuid.Nil, fmt.Errorf("sportsbook: upsert event: %w", err)
	}
	var resolvedID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM sb_events WHERE external_ref = $1`, externalRef).Scan(&resolvedID); err != nil {
		return uuid.Nil, fmt.Errorf("sportsbook: resolve event id: %w", err)
	}
	return resolvedID, nil
}

func upsertMarket(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, externalRef, name string, status MarketStatus) (uuid.UUID, error) {
	id := uuid.New()
	_, err := tx.Exec(ctx,
		`INSERT INTO sb_markets (id, event_id, external_ref, name, status)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (external_ref) DO UPDATE SET
			event_id = EXCLUDED.event_id, name = EXCLUDED.name, status = EXCLUDED.status, updated_at = now()`,
		id, eventID, externalRef, name, status,
	)
	if err != nil {
		return uuid.Nil, fmt.Errorf("sportsbook: upsert market: %w", err)
	}
	var resolvedID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM sb_markets WHERE external_ref = $1`, externalRef).Scan(&resolvedID); err != nil {
		return uuid.Nil, fmt.Errorf("sportsbook: resolve market id: %w", err)
	}
	return resolvedID, nil
}

func upsertSelection(ctx context.Context, tx pgx.Tx, marketID uuid.UUID, externalRef, name string, oddsNumerator, oddsDenominator int64) (uuid.UUID, error) {
	id := uuid.New()
	_, err := tx.Exec(ctx,
		`INSERT INTO sb_selections (id, market_id, external_ref, name, odds_numerator, odds_denominator)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (external_ref) DO UPDATE SET
			market_id = EXCLUDED.market_id, name = EXCLUDED.name,
			odds_numerator = EXCLUDED.odds_numerator, odds_denominator = EXCLUDED.odds_denominator, updated_at = now()`,
		id, marketID, externalRef, name, oddsNumerator, oddsDenominator,
	)
	if err != nil {
		return uuid.Nil, fmt.Errorf("sportsbook: upsert selection: %w", err)
	}
	var resolvedID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM sb_selections WHERE external_ref = $1`, externalRef).Scan(&resolvedID); err != nil {
		return uuid.Nil, fmt.Errorf("sportsbook: resolve selection id: %w", err)
	}
	return resolvedID, nil
}

// --- Read paths (platform-wide, no tenant filter - open catalogue browse) ---

// ListSportsCatalogue returns the full browse tree (sports -> competitions
// -> event summaries), excluding cancelled events and only ever showing
// active markets' worth of data at the shallow level - a client drills
// into GetEventDetail for a specific event's markets/selections.
func ListSportsCatalogue(ctx context.Context, tx pgx.Tx) ([]SportCatalogue, error) {
	sportRows, err := tx.Query(ctx, `SELECT id, code, name FROM sb_sports ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("sportsbook: list sports: %w", err)
	}
	var sports []Sport
	for sportRows.Next() {
		var s Sport
		if err := sportRows.Scan(&s.ID, &s.Code, &s.Name); err != nil {
			sportRows.Close()
			return nil, fmt.Errorf("sportsbook: scan sport: %w", err)
		}
		sports = append(sports, s)
	}
	if err := sportRows.Err(); err != nil {
		return nil, err
	}
	sportRows.Close()

	out := make([]SportCatalogue, 0, len(sports))
	for _, s := range sports {
		compRows, err := tx.Query(ctx, `SELECT id, sport_id, name FROM sb_competitions WHERE sport_id = $1 ORDER BY name`, s.ID)
		if err != nil {
			return nil, fmt.Errorf("sportsbook: list competitions: %w", err)
		}
		var competitions []Competition
		for compRows.Next() {
			var c Competition
			if err := compRows.Scan(&c.ID, &c.SportID, &c.Name); err != nil {
				compRows.Close()
				return nil, fmt.Errorf("sportsbook: scan competition: %w", err)
			}
			competitions = append(competitions, c)
		}
		if err := compRows.Err(); err != nil {
			return nil, err
		}
		compRows.Close()

		sc := SportCatalogue{Sport: s}
		for _, c := range competitions {
			eventRows, err := tx.Query(ctx,
				`SELECT id, name, start_time, status FROM sb_events
				 WHERE competition_id = $1 AND status <> $2 ORDER BY start_time`,
				c.ID, EventCancelled,
			)
			if err != nil {
				return nil, fmt.Errorf("sportsbook: list events: %w", err)
			}
			var events []EventSummary
			for eventRows.Next() {
				// Available defaults true (Stage 9.2, ADR 0083 §5.4.1) - an
				// unannotated read (every read this function itself performs;
				// annotation is the caller's own separate, opt-in step via
				// AnnotateCatalogueAvailability) reports every event
				// available, exactly like today.
				e := EventSummary{Available: true}
				if err := eventRows.Scan(&e.ID, &e.Name, &e.StartTime, &e.Status); err != nil {
					eventRows.Close()
					return nil, fmt.Errorf("sportsbook: scan event: %w", err)
				}
				events = append(events, e)
			}
			if err := eventRows.Err(); err != nil {
				return nil, err
			}
			eventRows.Close()
			sc.Competitions = append(sc.Competitions, CompetitionCatalogue{Competition: c, Events: events})
		}
		out = append(out, sc)
	}
	return out, nil
}

// GetEventDetail returns eventID's full market/selection tree.
func GetEventDetail(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (EventDetail, error) {
	var d EventDetail
	err := tx.QueryRow(ctx,
		`SELECT e.id, e.competition_id, e.name, e.start_time, e.status, c.name, s.name, s.code
		 FROM sb_events e
		 JOIN sb_competitions c ON c.id = e.competition_id
		 JOIN sb_sports s ON s.id = c.sport_id
		 WHERE e.id = $1`,
		eventID,
	).Scan(&d.ID, &d.CompetitionID, &d.Name, &d.StartTime, &d.Status, &d.CompetitionName, &d.SportName, &d.SportCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return EventDetail{}, ErrEventNotFound
	}
	if err != nil {
		return EventDetail{}, fmt.Errorf("sportsbook: get event detail: %w", err)
	}

	marketRows, err := tx.Query(ctx, `SELECT id, event_id, name, status FROM sb_markets WHERE event_id = $1 ORDER BY name`, eventID)
	if err != nil {
		return EventDetail{}, fmt.Errorf("sportsbook: list markets: %w", err)
	}
	var markets []Market
	for marketRows.Next() {
		var m Market
		if err := marketRows.Scan(&m.ID, &m.EventID, &m.Name, &m.Status); err != nil {
			marketRows.Close()
			return EventDetail{}, fmt.Errorf("sportsbook: scan market: %w", err)
		}
		markets = append(markets, m)
	}
	if err := marketRows.Err(); err != nil {
		return EventDetail{}, err
	}
	marketRows.Close()

	for _, m := range markets {
		selRows, err := tx.Query(ctx,
			`SELECT id, market_id, name, odds_numerator, odds_denominator, status
			 FROM sb_selections WHERE market_id = $1 ORDER BY name`, m.ID)
		if err != nil {
			return EventDetail{}, fmt.Errorf("sportsbook: list selections: %w", err)
		}
		var selections []Selection
		for selRows.Next() {
			// Available defaults true - see ListSportsCatalogue's identical
			// doc comment.
			sel := Selection{Available: true}
			if err := selRows.Scan(&sel.ID, &sel.MarketID, &sel.Name, &sel.OddsNumerator, &sel.OddsDenominator, &sel.Status); err != nil {
				selRows.Close()
				return EventDetail{}, fmt.Errorf("sportsbook: scan selection: %w", err)
			}
			selections = append(selections, sel)
		}
		if err := selRows.Err(); err != nil {
			return EventDetail{}, err
		}
		selRows.Close()
		d.Markets = append(d.Markets, MarketDetail{Market: m, Selections: selections, Available: true})
	}
	return d, nil
}

// selectionWithContext is the join PlaceBet needs: the selection itself,
// plus its market/event status - all resolved in one query so bet
// validation never has to make three separate round trips.
type selectionWithContext struct {
	Selection
	MarketStatus MarketStatus
	EventID      uuid.UUID
	EventStatus  EventStatus
}

// getSelectionWithContext resolves a Selection plus its owning market/event
// status, for PlaceBet's structural-validation step (docs/architecture/
// 09-sportsbook-architecture.md §3.3 step 1, narrowed to this stage's
// scope). Returns ErrSelectionNotFound if selectionID does not resolve.
func getSelectionWithContext(ctx context.Context, tx pgx.Tx, selectionID uuid.UUID) (selectionWithContext, error) {
	var sc selectionWithContext
	err := tx.QueryRow(ctx,
		`SELECT sel.id, sel.market_id, sel.name, sel.odds_numerator, sel.odds_denominator, sel.status,
			m.status, e.id, e.status
		 FROM sb_selections sel
		 JOIN sb_markets m ON m.id = sel.market_id
		 JOIN sb_events e ON e.id = m.event_id
		 WHERE sel.id = $1`,
		selectionID,
	).Scan(&sc.ID, &sc.MarketID, &sc.Name, &sc.OddsNumerator, &sc.OddsDenominator, &sc.Status,
		&sc.MarketStatus, &sc.EventID, &sc.EventStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return selectionWithContext{}, ErrSelectionNotFound
	}
	if err != nil {
		return selectionWithContext{}, fmt.Errorf("sportsbook: resolve selection: %w", err)
	}
	return sc, nil
}

// --- Jurisdiction availability annotation (Stage 9.2, ADR 0083 §5.4.1) ---

// AnnotateCatalogueAvailability marks every event in cat unavailable if an
// active sb_jurisdiction_restrictions row applies to it under ac's
// resolved jurisdiction - a NO-OP for an anonymous ac (the zero value),
// so both genuinely anonymous readers of GET /v1/sportsbook/sports get
// exactly today's response. Uses the SAME ResolvePlayerJurisdiction +
// evaluateJurisdictionRestriction pair PlaceBet uses (INV-SB-JUR-1) - only
// the bulk restriction load below (loadActiveRestrictionsForScopes) is
// specific to annotating a whole tree in one query rather than PlaceBet's
// single selection, per §5.4.1's stated cost model ("one SELECT... covering
// the whole browse tree, evaluated in memory"); it contains no
// arm/deny reasoning of its own; evaluateJurisdictionRestriction remains
// the ONLY place that reasoning lives.
//
// The shallow browse tree (SportCatalogue) carries no market/selection
// nodes, so only event-level marking applies here - GetEventDetail's own
// full tree is annotated by AnnotateEventAvailability below.
func AnnotateCatalogueAvailability(ctx context.Context, tx pgx.Tx, cat []SportCatalogue, ac AvailabilityContext) error {
	if ac.IsAnonymous() {
		return nil
	}
	res, err := ResolvePlayerJurisdiction(ctx, tx, ac, jurisdiction.OperationCatalogueAvailability)
	if err != nil {
		return err
	}
	var eventIDs []uuid.UUID
	for _, sc := range cat {
		for _, cc := range sc.Competitions {
			for _, e := range cc.Events {
				eventIDs = append(eventIDs, e.ID)
			}
		}
	}
	byEvent, _, _, err := loadActiveRestrictionsForScopes(ctx, tx, eventIDs, nil, nil)
	if err != nil {
		return err
	}
	for si := range cat {
		for ci := range cat[si].Competitions {
			for ei := range cat[si].Competitions[ci].Events {
				e := &cat[si].Competitions[ci].Events[ei]
				decision, err := evaluateJurisdictionRestriction(byEvent[e.ID], res)
				if err != nil {
					return err
				}
				e.Available = decision.Available
				if !decision.Available {
					e.UnavailableReason = catalogueUnavailableReason
				}
			}
		}
	}
	return nil
}

// AnnotateEventAvailability marks d's markets/selections unavailable under
// ac's resolved jurisdiction, exactly like AnnotateCatalogueAvailability -
// a NO-OP for an anonymous ac. Restrictions flow DOWNWARD only (§5.4.1):
// an event-level restriction marks every market and selection beneath it
// unavailable; a market-level restriction marks its own selections
// unavailable; a selection-level restriction never propagates up to its
// market, and a market's restriction never propagates up to its event - a
// market with one restricted selection is still a real, bettable market.
func AnnotateEventAvailability(ctx context.Context, tx pgx.Tx, d *EventDetail, ac AvailabilityContext) error {
	if ac.IsAnonymous() {
		return nil
	}
	res, err := ResolvePlayerJurisdiction(ctx, tx, ac, jurisdiction.OperationCatalogueAvailability)
	if err != nil {
		return err
	}
	marketIDs := make([]uuid.UUID, 0, len(d.Markets))
	var selectionIDs []uuid.UUID
	for _, m := range d.Markets {
		marketIDs = append(marketIDs, m.ID)
		for _, sel := range m.Selections {
			selectionIDs = append(selectionIDs, sel.ID)
		}
	}
	byEvent, byMarket, bySelection, err := loadActiveRestrictionsForScopes(ctx, tx, []uuid.UUID{d.ID}, marketIDs, selectionIDs)
	if err != nil {
		return err
	}
	eventDecision, err := evaluateJurisdictionRestriction(byEvent[d.ID], res)
	if err != nil {
		return err
	}
	for mi := range d.Markets {
		m := &d.Markets[mi]
		switch {
		case !eventDecision.Available:
			// Downward propagation from the event - never re-evaluated
			// against the market's own (possibly empty) restriction set,
			// since the event-level denial already governs.
			m.Available = false
			m.UnavailableReason = catalogueUnavailableReason
		default:
			marketDecision, err := evaluateJurisdictionRestriction(byMarket[m.ID], res)
			if err != nil {
				return err
			}
			m.Available = marketDecision.Available
			if !marketDecision.Available {
				m.UnavailableReason = catalogueUnavailableReason
			}
		}
		for si := range m.Selections {
			sel := &m.Selections[si]
			if !m.Available {
				// Downward propagation from the event or the market.
				sel.Available = false
				sel.UnavailableReason = catalogueUnavailableReason
				continue
			}
			selDecision, err := evaluateJurisdictionRestriction(bySelection[sel.ID], res)
			if err != nil {
				return err
			}
			sel.Available = selDecision.Available
			if !selDecision.Available {
				sel.UnavailableReason = catalogueUnavailableReason
			}
		}
	}
	return nil
}

// loadActiveRestrictionsForScopes is loadActiveRestrictions' bulk sibling
// (jurisdiction.go), used ONLY by the two catalogue annotators above to
// satisfy §5.4.1's "one query covering the whole browse tree" cost model -
// PlaceBet's own call site always uses the single-scope
// loadActiveRestrictions and is never routed through this function. It
// performs no arm/deny reasoning of its own (evaluateJurisdictionRestriction
// remains the only place that logic lives) - it is purely a bulk data load,
// bucketed by scope_kind into three maps keyed by the relevant id.
func loadActiveRestrictionsForScopes(
	ctx context.Context, tx pgx.Tx, eventIDs, marketIDs, selectionIDs []uuid.UUID,
) (byEvent, byMarket, bySelection map[uuid.UUID][]string, err error) {
	byEvent = make(map[uuid.UUID][]string)
	byMarket = make(map[uuid.UUID][]string)
	bySelection = make(map[uuid.UUID][]string)
	if len(eventIDs) == 0 && len(marketIDs) == 0 && len(selectionIDs) == 0 {
		return byEvent, byMarket, bySelection, nil
	}
	rows, err := tx.Query(ctx,
		`SELECT scope_kind, event_id, market_id, selection_id, jurisdiction_code
		 FROM sb_jurisdiction_restrictions
		 WHERE status = 'active'
		   AND (event_id = ANY($1) OR market_id = ANY($2) OR selection_id = ANY($3))`,
		eventIDs, marketIDs, selectionIDs,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("sportsbook: load active jurisdiction restrictions for tree: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var scopeKind string
		var eventID, marketID, selectionID *uuid.UUID
		var code string
		if err := rows.Scan(&scopeKind, &eventID, &marketID, &selectionID, &code); err != nil {
			return nil, nil, nil, fmt.Errorf("sportsbook: scan active jurisdiction restriction for tree: %w", err)
		}
		switch scopeKind {
		case "event":
			if eventID != nil {
				byEvent[*eventID] = append(byEvent[*eventID], code)
			}
		case "market":
			if marketID != nil {
				byMarket[*marketID] = append(byMarket[*marketID], code)
			}
		case "selection":
			if selectionID != nil {
				bySelection[*selectionID] = append(bySelection[*selectionID], code)
			}
		}
	}
	return byEvent, byMarket, bySelection, rows.Err()
}

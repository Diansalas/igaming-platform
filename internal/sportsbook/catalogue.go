package sportsbook

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// SyncCatalogue upserts provider's full catalogue into the platform-wide
// sb_sports/sb_competitions/sb_events/sb_markets/sb_selections tables,
// keyed at each level by (parent id, external_ref) - the platform mints
// its own id once, at first insert, and never re-derives it from a
// provider's own reference again, mirroring internal/casino.UpsertGame's
// identical (provider_id, provider_game_id) keying discipline.
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
func SyncCatalogue(ctx context.Context, tx pgx.Tx, provider Provider) error {
	if err := db.AssertPlatformServiceScope(ctx, tx, db.ServiceSportsbookCatalogueSync); err != nil {
		return err
	}
	result := provider.Catalogue()
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
				var e EventSummary
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
			var sel Selection
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
		d.Markets = append(d.Markets, MarketDetail{Market: m, Selections: selections})
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

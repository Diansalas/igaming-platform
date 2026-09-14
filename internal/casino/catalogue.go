package casino

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Game mirrors a casino_games row - the platform-wide catalogue entry
// (ADR 0025 §2). No tenant_id: game content is shared across every
// tenant that can reach the provider, never tenant-owned.
type Game struct {
	ID                    uuid.UUID
	ProviderID            string
	ProviderGameID        string
	Name                  string
	GameType              string
	RTPVariant            string
	Volatility            string
	FeatureFlags          []string
	SupportedAssets       []string
	MobileSupported       bool
	DemoSupported         bool
	JurisdictionBlocklist []string
	Status                GameStatus
}

const gameColumns = `id, provider_id, provider_game_id, name, game_type, rtp_variant, volatility,
	feature_flags, supported_assets, mobile_supported, demo_supported, jurisdiction_blocklist, status`

func scanGame(row rowScanner) (Game, error) {
	var g Game
	var rtpVariant, volatility *string
	err := row.Scan(
		&g.ID, &g.ProviderID, &g.ProviderGameID, &g.Name, &g.GameType, &rtpVariant, &volatility,
		&g.FeatureFlags, &g.SupportedAssets, &g.MobileSupported, &g.DemoSupported, &g.JurisdictionBlocklist, &g.Status,
	)
	if rtpVariant != nil {
		g.RTPVariant = *rtpVariant
	}
	if volatility != nil {
		g.Volatility = *volatility
	}
	return g, err
}

// rowScanner is satisfied by both pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// GetGameByID looks up a Game by its platform id. casino_games carries no
// RLS (platform-wide, like the assets registry) so tx may come from
// either WithTenant or WithoutTenant - this function applies no tenant
// filtering itself.
func GetGameByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Game, error) {
	row := tx.QueryRow(ctx, `SELECT `+gameColumns+` FROM casino_games WHERE id = $1`, id)
	g, err := scanGame(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Game{}, ErrGameNotFound
	}
	if err != nil {
		return Game{}, fmt.Errorf("casino: get game by id: %w", err)
	}
	return g, nil
}

// GetGameByProviderRef looks up a Game by its (provider_id,
// provider_game_id) pair - the identifier a launch/callback path actually
// has to hand, per ADR 0025 §2's "provider game identifier... never
// promoted to platform identity".
func GetGameByProviderRef(ctx context.Context, tx pgx.Tx, providerID, providerGameID string) (Game, error) {
	row := tx.QueryRow(ctx, `SELECT `+gameColumns+` FROM casino_games WHERE provider_id = $1 AND provider_game_id = $2`, providerID, providerGameID)
	g, err := scanGame(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Game{}, ErrGameNotFound
	}
	if err != nil {
		return Game{}, fmt.Errorf("casino: get game by provider ref: %w", err)
	}
	return g, nil
}

// UpsertGameInput is UpsertGame's input - a platform-administrative
// action (registering/updating a title in the catalogue), never
// tenant-scoped. Mirrors CatalogueEntry's own fields plus the
// platform-only ones (JurisdictionBlocklist, Status) a provider's own
// Catalogue() call never declares.
type UpsertGameInput struct {
	ProviderID            string
	ProviderGameID        string
	Name                  string
	GameType              string
	RTPVariant            string
	Volatility            string
	FeatureFlags          []string
	SupportedAssets       []string
	MobileSupported       bool
	DemoSupported         bool
	JurisdictionBlocklist []string
	Status                GameStatus
}

// UpsertGame registers a new title or updates an existing one, keyed on
// (provider_id, provider_game_id) - the platform's own id is assigned
// once, at first insert, and never changes on a later update (ADR 0025
// §2). A platform-administrative action, audited by the caller per
// CLAUDE.md, exactly like internal/payments.WriteCapability leaves the
// audit record to its own caller.
func UpsertGame(ctx context.Context, tx pgx.Tx, in UpsertGameInput) (Game, error) {
	if in.ProviderID == "" || in.ProviderGameID == "" || in.Name == "" || in.GameType == "" {
		return Game{}, fmt.Errorf("%w: provider_id, provider_game_id, name, and game_type are required", ErrInvalidInput)
	}
	status := in.Status
	if status == "" {
		status = GameStatusActive
	}
	id := uuid.New()
	_, err := tx.Exec(ctx,
		`INSERT INTO casino_games
			(id, provider_id, provider_game_id, name, game_type, rtp_variant, volatility,
			 feature_flags, supported_assets, mobile_supported, demo_supported, jurisdiction_blocklist, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		 ON CONFLICT (provider_id, provider_game_id) DO UPDATE SET
			name = EXCLUDED.name, game_type = EXCLUDED.game_type, rtp_variant = EXCLUDED.rtp_variant,
			volatility = EXCLUDED.volatility, feature_flags = EXCLUDED.feature_flags,
			supported_assets = EXCLUDED.supported_assets, mobile_supported = EXCLUDED.mobile_supported,
			demo_supported = EXCLUDED.demo_supported, jurisdiction_blocklist = EXCLUDED.jurisdiction_blocklist,
			status = EXCLUDED.status, updated_at = now()`,
		id, in.ProviderID, in.ProviderGameID, in.Name, in.GameType, nonEmptyPtr(in.RTPVariant), nonEmptyPtr(in.Volatility),
		nonNilStrings(in.FeatureFlags), nonNilStrings(in.SupportedAssets), in.MobileSupported, in.DemoSupported,
		nonNilStrings(in.JurisdictionBlocklist), status,
	)
	if err != nil {
		return Game{}, fmt.Errorf("casino: upsert game: %w", err)
	}
	return GetGameByProviderRef(ctx, tx, in.ProviderID, in.ProviderGameID)
}

func nonEmptyPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// GameAvailability mirrors a casino_game_availability row - the tenant/
// brand opt-in layer (ADR 0025 §2).
type GameAvailability struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	BrandID  *uuid.UUID
	GameID   uuid.UUID
	Enabled  bool
}

// SetGameAvailability enables/disables gameID for (tenantID, brandID) -
// brandID nil writes the tenant-wide row. tx must already be tenant-
// scoped. A narrowing-only rule does not apply here the way it does for
// provider capabilities (ADR 0025 §4): availability is a boolean
// opt-in/opt-out, not a set that could be "widened" beyond an adapter's
// declaration - the platform-level casino_games.status and jurisdiction
// checks (ResolveLaunchEligibility) are what actually bound what a tenant
// may ever launch, independent of this flag.
func SetGameAvailability(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, brandID *uuid.UUID, gameID uuid.UUID, enabled bool) (GameAvailability, error) {
	id := uuid.New()
	var err error
	if brandID == nil {
		_, err = tx.Exec(ctx,
			`INSERT INTO casino_game_availability (id, tenant_id, brand_id, game_id, enabled)
			 VALUES ($1, $2, NULL, $3, $4)
			 ON CONFLICT (tenant_id, game_id) WHERE brand_id IS NULL DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = now()`,
			id, tenantID, gameID, enabled,
		)
	} else {
		_, err = tx.Exec(ctx,
			`INSERT INTO casino_game_availability (id, tenant_id, brand_id, game_id, enabled)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (tenant_id, brand_id, game_id) WHERE brand_id IS NOT NULL DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = now()`,
			id, tenantID, *brandID, gameID, enabled,
		)
	}
	if err != nil {
		return GameAvailability{}, fmt.Errorf("casino: set game availability: %w", err)
	}
	return GameAvailability{TenantID: tenantID, BrandID: brandID, GameID: gameID, Enabled: enabled}, nil
}

// IsGameAvailable resolves whole-row, most-specific-wins availability
// for (tenantID, brandID, gameID) - a brand-specific row replaces a
// tenant-wide row entirely, exactly like docs/decisions/0022 §3's
// resolution rule for payment capabilities. No row at all means "not
// available" (fail closed - a tenant/brand must explicitly opt a game
// in, never implicitly inherit platform-catalogue membership).
func IsGameAvailable(ctx context.Context, tx pgx.Tx, tenantID, brandID, gameID uuid.UUID) (bool, error) {
	var enabled bool
	err := tx.QueryRow(ctx,
		`SELECT enabled FROM casino_game_availability
		 WHERE tenant_id = $1 AND game_id = $2 AND (brand_id = $3 OR brand_id IS NULL)
		 ORDER BY (brand_id IS NULL) ASC
		 LIMIT 1`,
		tenantID, gameID, brandID,
	).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("casino: resolve game availability: %w", err)
	}
	return enabled, nil
}

// ListAvailableGames returns every casino_games row enabled for
// (tenantID, brandID) - the player-facing catalogue view, joining the
// platform catalogue against the tenant's own opt-in layer and excluding
// any platform-disabled title. tx must already be tenant-scoped.
//
// Resolution is most-specific-row-wins FIRST (the inner DISTINCT ON,
// exactly like IsGameAvailable), THEN filtered by that winning row's own
// `enabled` value - never the other order. A brand-specific row with
// enabled=false must be the one that decides a brand's view of a game
// that is enabled tenant-wide; filtering `enabled = true` in the join's
// WHERE clause before resolving most-specific-wins (a bug this fix
// closes - specialist review finding) would instead let the tenant-wide
// row win by simply discarding the brand's own opt-out before selection.
func ListAvailableGames(ctx context.Context, tx pgx.Tx, tenantID, brandID uuid.UUID) ([]Game, error) {
	rows, err := tx.Query(ctx,
		`SELECT g.id, g.provider_id, g.provider_game_id, g.name, g.game_type, g.rtp_variant, g.volatility,
			g.feature_flags, g.supported_assets, g.mobile_supported, g.demo_supported, g.jurisdiction_blocklist, g.status
		 FROM casino_games g
		 JOIN (
			SELECT DISTINCT ON (game_id) game_id, enabled
			FROM casino_game_availability
			WHERE tenant_id = $1 AND (brand_id = $2 OR brand_id IS NULL)
			ORDER BY game_id, (brand_id IS NULL) ASC
		 ) a ON a.game_id = g.id
		 WHERE g.status = 'active' AND a.enabled = true`,
		tenantID, brandID,
	)
	if err != nil {
		return nil, fmt.Errorf("casino: list available games: %w", err)
	}
	defer rows.Close()

	var games []Game
	for rows.Next() {
		g, err := scanGame(rows)
		if err != nil {
			return nil, fmt.Errorf("casino: scan game: %w", err)
		}
		games = append(games, g)
	}
	return games, rows.Err()
}

package casino

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// LoadCapability resolves the effective ProviderCapability for
// (tenantID, brandID, providerID), applying ADR 0025 §4's most-specific-
// row-wins resolution - a brand-specific row replaces a tenant-wide row
// for the same provider entirely (whole-row replacement, mirroring
// docs/decisions/0022 §3 exactly). Returns found=false, not an error,
// when no row of either shape exists. tx must already be tenant-scoped.
func LoadCapability(ctx context.Context, tx pgx.Tx, tenantID, brandID uuid.UUID, providerID string) (ProviderCapability, bool, error) {
	row := tx.QueryRow(ctx,
		`SELECT id, tenant_id, brand_id, provider_id,
			supports_catalogue, supports_launch, supports_balance, supports_bet, supports_win, supports_rollback,
			supported_assets, supported_game_types, callback_capabilities, priority, status
		 FROM casino_provider_capabilities
		 WHERE tenant_id = $1 AND provider_id = $2 AND (brand_id = $3 OR brand_id IS NULL)
		 ORDER BY (brand_id IS NULL) ASC
		 LIMIT 1`,
		tenantID, providerID, brandID,
	)
	pc, err := scanCapabilityRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProviderCapability{}, false, nil
	}
	if err != nil {
		return ProviderCapability{}, false, fmt.Errorf("casino: load capability: %w", err)
	}
	return pc, true, nil
}

// ListRoutingCandidates returns the effective (most-specific-row-wins)
// ProviderCapability for every provider_id configured for (tenantID,
// brandID), one row per provider_id.
func ListRoutingCandidates(ctx context.Context, tx pgx.Tx, tenantID, brandID uuid.UUID) ([]ProviderCapability, error) {
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT ON (provider_id)
			id, tenant_id, brand_id, provider_id,
			supports_catalogue, supports_launch, supports_balance, supports_bet, supports_win, supports_rollback,
			supported_assets, supported_game_types, callback_capabilities, priority, status
		 FROM casino_provider_capabilities
		 WHERE tenant_id = $1 AND (brand_id = $2 OR brand_id IS NULL)
		 ORDER BY provider_id, (brand_id IS NULL) ASC`,
		tenantID, brandID,
	)
	if err != nil {
		return nil, fmt.Errorf("casino: list routing candidates: %w", err)
	}
	defer rows.Close()

	var caps []ProviderCapability
	for rows.Next() {
		pc, err := scanCapabilityRow(rows)
		if err != nil {
			return nil, fmt.Errorf("casino: scan routing candidate: %w", err)
		}
		caps = append(caps, pc)
	}
	return caps, rows.Err()
}

func scanCapabilityRow(row rowScanner) (ProviderCapability, error) {
	var pc ProviderCapability
	err := row.Scan(
		&pc.ID, &pc.TenantID, &pc.BrandID, &pc.ProviderID,
		&pc.SupportsCatalogue, &pc.SupportsLaunch, &pc.SupportsBalance, &pc.SupportsBet, &pc.SupportsWin, &pc.SupportsRollback,
		&pc.SupportedAssets, &pc.SupportedGameTypes, &pc.CallbackCapabilities, &pc.Priority, &pc.Status,
	)
	return pc, err
}

// CapabilityConfig is the operator-editable "layer (b)" subset of a
// ProviderCapability (ADR 0025 §4): tenant-config narrowing of what the
// adapter itself declares, plus priority/status. Deliberately excludes
// ProviderID/CallbackCapabilities - those are adapter-declared facts
// (layer (a)) WriteCapability always takes from the adapter's own
// Capabilities() call, never from caller input.
type CapabilityConfig struct {
	SupportsCatalogue  bool
	SupportsLaunch     bool
	SupportsBalance    bool
	SupportsBet        bool
	SupportsWin        bool
	SupportsRollback   bool
	SupportedAssets    []string
	SupportedGameTypes []string
	Priority           int
	Status             CapabilityStatus
}

// WriteCapability registers or updates the effective ProviderCapability
// for (tenantID, brandID) against provider's own declared capability.
// Platform/admin-level operation; audited by the caller, not this
// function (mirrors internal/payments.WriteCapability's identical
// division of responsibility). tx must already be tenant-scoped. brandID
// nil writes/replaces the tenant-wide row; non-nil writes/replaces that
// brand's own row - whole-row replacement, never a per-field merge.
func WriteCapability(ctx context.Context, tx pgx.Tx, provider CasinoProvider, tenantID uuid.UUID, brandID *uuid.UUID, cfg CapabilityConfig) (uuid.UUID, error) {
	declared := provider.Capabilities()
	if err := validateNarrowing(declared, cfg); err != nil {
		return uuid.Nil, err
	}

	assets := nonNilStrings(cfg.SupportedAssets)
	gameTypes := nonNilStrings(cfg.SupportedGameTypes)

	id := uuid.New()
	var err error
	if brandID == nil {
		_, err = tx.Exec(ctx,
			`INSERT INTO casino_provider_capabilities
				(id, tenant_id, brand_id, provider_id,
				 supports_catalogue, supports_launch, supports_balance, supports_bet, supports_win, supports_rollback,
				 supported_assets, supported_game_types, callback_capabilities, priority, status)
			 VALUES ($1, $2, NULL, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			 ON CONFLICT (tenant_id, provider_id) WHERE brand_id IS NULL DO UPDATE SET
				supports_catalogue = EXCLUDED.supports_catalogue, supports_launch = EXCLUDED.supports_launch,
				supports_balance = EXCLUDED.supports_balance, supports_bet = EXCLUDED.supports_bet,
				supports_win = EXCLUDED.supports_win, supports_rollback = EXCLUDED.supports_rollback,
				supported_assets = EXCLUDED.supported_assets, supported_game_types = EXCLUDED.supported_game_types,
				callback_capabilities = EXCLUDED.callback_capabilities, priority = EXCLUDED.priority,
				status = EXCLUDED.status, updated_at = now()`,
			id, tenantID, declared.ProviderID,
			cfg.SupportsCatalogue, cfg.SupportsLaunch, cfg.SupportsBalance, cfg.SupportsBet, cfg.SupportsWin, cfg.SupportsRollback,
			assets, gameTypes, declared.CallbackCapabilities, cfg.Priority, cfg.Status,
		)
	} else {
		_, err = tx.Exec(ctx,
			`INSERT INTO casino_provider_capabilities
				(id, tenant_id, brand_id, provider_id,
				 supports_catalogue, supports_launch, supports_balance, supports_bet, supports_win, supports_rollback,
				 supported_assets, supported_game_types, callback_capabilities, priority, status)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
			 ON CONFLICT (tenant_id, brand_id, provider_id) WHERE brand_id IS NOT NULL DO UPDATE SET
				supports_catalogue = EXCLUDED.supports_catalogue, supports_launch = EXCLUDED.supports_launch,
				supports_balance = EXCLUDED.supports_balance, supports_bet = EXCLUDED.supports_bet,
				supports_win = EXCLUDED.supports_win, supports_rollback = EXCLUDED.supports_rollback,
				supported_assets = EXCLUDED.supported_assets, supported_game_types = EXCLUDED.supported_game_types,
				callback_capabilities = EXCLUDED.callback_capabilities, priority = EXCLUDED.priority,
				status = EXCLUDED.status, updated_at = now()`,
			id, tenantID, *brandID, declared.ProviderID,
			cfg.SupportsCatalogue, cfg.SupportsLaunch, cfg.SupportsBalance, cfg.SupportsBet, cfg.SupportsWin, cfg.SupportsRollback,
			assets, gameTypes, declared.CallbackCapabilities, cfg.Priority, cfg.Status,
		)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("casino: write capability: %w", err)
	}

	written, found, err := LoadCapability(ctx, tx, tenantID, derefOrNilBrand(brandID), declared.ProviderID)
	if err != nil {
		return uuid.Nil, err
	}
	if !found {
		return uuid.Nil, fmt.Errorf("casino: write capability: row not found immediately after write")
	}
	return written.ID, nil
}

func derefOrNilBrand(brandID *uuid.UUID) uuid.UUID {
	if brandID == nil {
		return uuid.Nil
	}
	return *brandID
}

// validateNarrowing enforces ADR 0025 §4: a tenant configuration may
// only narrow what the adapter itself declares, never widen it, and may
// never assert an operation the adapter does not implement.
func validateNarrowing(declared AdapterCapability, cfg CapabilityConfig) error {
	if cfg.SupportsCatalogue && !declared.SupportsCatalogue {
		return fmt.Errorf("%w: adapter %s does not support catalogue", ErrCapabilityWidensAdapter, declared.ProviderID)
	}
	if cfg.SupportsLaunch && !declared.SupportsLaunch {
		return fmt.Errorf("%w: adapter %s does not support launch", ErrCapabilityWidensAdapter, declared.ProviderID)
	}
	if cfg.SupportsBalance && !declared.SupportsBalance {
		return fmt.Errorf("%w: adapter %s does not support balance", ErrCapabilityWidensAdapter, declared.ProviderID)
	}
	if cfg.SupportsBet && !declared.SupportsBet {
		return fmt.Errorf("%w: adapter %s does not support bet", ErrCapabilityWidensAdapter, declared.ProviderID)
	}
	if cfg.SupportsWin && !declared.SupportsWin {
		return fmt.Errorf("%w: adapter %s does not support win", ErrCapabilityWidensAdapter, declared.ProviderID)
	}
	if cfg.SupportsRollback && !declared.SupportsRollback {
		return fmt.Errorf("%w: adapter %s does not support rollback", ErrCapabilityWidensAdapter, declared.ProviderID)
	}
	if !isSubset(cfg.SupportedAssets, declared.SupportedAssets) {
		return fmt.Errorf("%w: supported_assets exceeds adapter %s's declared set", ErrCapabilityWidensAdapter, declared.ProviderID)
	}
	if !isSubset(cfg.SupportedGameTypes, declared.SupportedGameTypes) {
		return fmt.Errorf("%w: supported_game_types exceeds adapter %s's declared set", ErrCapabilityWidensAdapter, declared.ProviderID)
	}
	return nil
}

func isSubset(subset, superset []string) bool {
	if len(subset) == 0 {
		return true
	}
	allowed := make(map[string]struct{}, len(superset))
	for _, s := range superset {
		allowed[s] = struct{}{}
	}
	for _, s := range subset {
		if _, ok := allowed[s]; !ok {
			return false
		}
	}
	return true
}

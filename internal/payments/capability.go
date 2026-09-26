package payments

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// LoadCapability resolves the effective ProviderCapability for
// (tenantID, brandID, providerID), applying docs/decisions/0022 §3's
// most-specific-row-wins resolution: a brand-specific row (brand_id =
// brandID) replaces a tenant-wide row (brand_id IS NULL) for the same
// provider entirely - whole-row replacement, never a per-field merge.
// Returns found=false, not an error, when no row of either shape exists.
// tx must already be tenant-scoped (db.Pool.WithTenant) - this is a
// tenant-owned, RLS-protected table with no platform-global fallback
// (docs/decisions/0022 §2.2).
func LoadCapability(ctx context.Context, tx pgx.Tx, tenantID, brandID uuid.UUID, providerID string) (ProviderCapability, bool, error) {
	row := tx.QueryRow(ctx,
		`SELECT id, tenant_id, brand_id, provider_id, provider_kind,
			supported_fiat_currencies, supported_crypto_assets, supported_payment_methods, supported_countries,
			supports_deposit, supports_withdrawal, supports_refund_reversal,
			settlement_behavior, callback_capabilities, priority, status
		 FROM provider_capabilities
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
		return ProviderCapability{}, false, fmt.Errorf("payments: load capability: %w", err)
	}

	limits, err := loadAmountLimits(ctx, tx, []uuid.UUID{pc.ID})
	if err != nil {
		return ProviderCapability{}, false, err
	}
	pc.AmountLimits = limits[pc.ID]
	return pc, true, nil
}

// ListRoutingCandidates returns the effective (most-specific-row-wins)
// ProviderCapability for every provider_id configured for
// (tenantID, brandID), one row per provider_id - the full candidate set
// RouteProvider filters down. Includes disabled rows; callers filter on
// Status themselves (kept this way so a capability-management UI can show
// disabled providers too, rather than this function silently hiding them).
func ListRoutingCandidates(ctx context.Context, tx pgx.Tx, tenantID, brandID uuid.UUID) ([]ProviderCapability, error) {
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT ON (provider_id)
			id, tenant_id, brand_id, provider_id, provider_kind,
			supported_fiat_currencies, supported_crypto_assets, supported_payment_methods, supported_countries,
			supports_deposit, supports_withdrawal, supports_refund_reversal,
			settlement_behavior, callback_capabilities, priority, status
		 FROM provider_capabilities
		 WHERE tenant_id = $1 AND (brand_id = $2 OR brand_id IS NULL)
		 ORDER BY provider_id, (brand_id IS NULL) ASC`,
		tenantID, brandID,
	)
	if err != nil {
		return nil, fmt.Errorf("payments: list routing candidates: %w", err)
	}
	defer rows.Close()

	var caps []ProviderCapability
	var ids []uuid.UUID
	for rows.Next() {
		pc, err := scanCapabilityRow(rows)
		if err != nil {
			return nil, fmt.Errorf("payments: scan routing candidate: %w", err)
		}
		caps = append(caps, pc)
		ids = append(ids, pc.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("payments: read routing candidates: %w", err)
	}

	limits, err := loadAmountLimits(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	for i := range caps {
		caps[i].AmountLimits = limits[caps[i].ID]
	}
	return caps, nil
}

// ProviderAcceptsWebhook reports whether providerID has a capability row
// configured for tenantID whose callback_capabilities is 'webhook' or
// 'both' (PAY-WH-TENANT-1 step (b), docs/decisions/0022 §3 amendment). This
// is an explicit tenant predicate layered ON TOP of RLS (tx must already
// be tenant-scoped via db.Pool.WithTenant) - defense in depth, not the
// sole isolation mechanism. Deliberately does NOT filter on brand_id (a
// row for ANY brand under the tenant counts - PAYWH-BRAND-1, a registered,
// deferred limitation: a callback carries no brand identity today) and
// deliberately does NOT filter on status: 'disabled' is a ROUTING
// kill-switch only (I4/ruling 7) - rejecting callbacks for a disabled
// provider would strand money already in flight. A compromised provider is
// revoked by removing its credential from the WebhookCredentialResolver,
// not by disabling the capability row. This is a read-only EXISTS with no
// FOR UPDATE - the only statement PAY-WH-TENANT-1's verification order
// permits before signature verification succeeds (invariant I1).
func ProviderAcceptsWebhook(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM provider_capabilities
			WHERE tenant_id = $1 AND provider_id = $2
			  AND callback_capabilities IN ('webhook', 'both')
		 )`,
		tenantID, providerID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("payments: check provider webhook acceptance: %w", err)
	}
	return exists, nil
}

// rowScanner is the subset of pgx.Row/pgx.Rows this package needs to scan
// a capability row from either QueryRow or Query.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanCapabilityRow(row rowScanner) (ProviderCapability, error) {
	var pc ProviderCapability
	err := row.Scan(
		&pc.ID, &pc.TenantID, &pc.BrandID, &pc.ProviderID, &pc.ProviderKind,
		&pc.SupportedFiatCurrencies, &pc.SupportedCryptoAssets, &pc.SupportedPaymentMethods, &pc.SupportedCountries,
		&pc.SupportsDeposit, &pc.SupportsWithdrawal, &pc.SupportsRefundReversal,
		&pc.SettlementBehavior, &pc.CallbackCapabilities, &pc.Priority, &pc.Status,
	)
	return pc, err
}

func loadAmountLimits(ctx context.Context, tx pgx.Tx, capabilityIDs []uuid.UUID) (map[uuid.UUID][]AmountLimit, error) {
	result := make(map[uuid.UUID][]AmountLimit, len(capabilityIDs))
	if len(capabilityIDs) == 0 {
		return result, nil
	}
	rows, err := tx.Query(ctx,
		`SELECT provider_capability_id, asset_code, min_amount, max_amount
		 FROM provider_capability_amount_limits
		 WHERE provider_capability_id = ANY($1)`,
		capabilityIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("payments: load amount limits: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var capID uuid.UUID
		var lim AmountLimit
		if err := rows.Scan(&capID, &lim.AssetCode, &lim.MinAmount, &lim.MaxAmount); err != nil {
			return nil, fmt.Errorf("payments: scan amount limit: %w", err)
		}
		result[capID] = append(result[capID], lim)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("payments: read amount limits: %w", err)
	}
	return result, nil
}

// CapabilityConfig is the operator-editable "layer (b)" subset of a
// ProviderCapability (docs/decisions/0022 §2): tenant-config narrowing of
// what the adapter itself declares, plus priority/status. It deliberately
// excludes ProviderID/ProviderKind/SettlementBehavior/CallbackCapabilities
// - those are adapter-declared facts (layer (a)) that WriteCapability
// always takes from the adapter's own Capabilities() call, never from
// caller input (docs/decisions/0022 §2.1: "a partner-console operator may
// enable/disable, re-rank, or narrow; they may never change provider_kind,
// register a provider_id the platform has not integrated, or assert a
// capability the adapter does not declare").
type CapabilityConfig struct {
	SupportedFiatCurrencies []string
	SupportedCryptoAssets   []string
	SupportedPaymentMethods []string
	SupportedCountries      []string
	SupportsDeposit         bool
	SupportsWithdrawal      bool
	SupportsRefundReversal  bool
	AmountLimits            []AmountLimit
	Priority                int
	Status                  CapabilityStatus
}

// WriteCapability registers or updates the effective ProviderCapability
// for (tenantID, brandID) against provider's own declared capability. This
// is a platform/admin-level operation, audited by the caller per CLAUDE.md
// (registering a provider_id or changing what it may do is an
// administrative action - docs/decisions/0022 §2.1) - this function itself
// performs only the write and the narrowing check, not the audit record,
// consistent with every other package in this codebase (e.g.
// internal/ledger.Post) leaving audit.Record to the caller's own
// transaction.
//
// tx must already be tenant-scoped. brandID nil writes/replaces the
// tenant-wide row for this provider; non-nil writes/replaces that brand's
// own row - whole-row replacement (docs/decisions/0022 §3), never a
// per-field merge with any existing row.
func WriteCapability(ctx context.Context, tx pgx.Tx, provider PaymentProvider, tenantID uuid.UUID, brandID *uuid.UUID, cfg CapabilityConfig) (uuid.UUID, error) {
	declared := provider.Capabilities()
	if err := validateNarrowing(declared, cfg); err != nil {
		return uuid.Nil, err
	}

	// pgx encodes a nil []string as SQL NULL, not an empty array, and
	// every one of these four columns is NOT NULL (migration 0024) - an
	// operator who narrows a list down to "none" still needs to store
	// '{}', never NULL.
	fiatCurrencies := nonNilStrings(cfg.SupportedFiatCurrencies)
	cryptoAssets := nonNilStrings(cfg.SupportedCryptoAssets)
	paymentMethods := nonNilStrings(cfg.SupportedPaymentMethods)
	countries := nonNilStrings(cfg.SupportedCountries)

	id := uuid.New()
	var err error
	if brandID == nil {
		_, err = tx.Exec(ctx,
			`INSERT INTO provider_capabilities
				(id, tenant_id, brand_id, provider_id, provider_kind,
				 supported_fiat_currencies, supported_crypto_assets, supported_payment_methods, supported_countries,
				 supports_deposit, supports_withdrawal, supports_refund_reversal,
				 settlement_behavior, callback_capabilities, priority, status)
			 VALUES ($1, $2, NULL, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
			 ON CONFLICT (tenant_id, provider_id) WHERE brand_id IS NULL DO UPDATE SET
				provider_kind = EXCLUDED.provider_kind,
				supported_fiat_currencies = EXCLUDED.supported_fiat_currencies,
				supported_crypto_assets = EXCLUDED.supported_crypto_assets,
				supported_payment_methods = EXCLUDED.supported_payment_methods,
				supported_countries = EXCLUDED.supported_countries,
				supports_deposit = EXCLUDED.supports_deposit,
				supports_withdrawal = EXCLUDED.supports_withdrawal,
				supports_refund_reversal = EXCLUDED.supports_refund_reversal,
				settlement_behavior = EXCLUDED.settlement_behavior,
				callback_capabilities = EXCLUDED.callback_capabilities,
				priority = EXCLUDED.priority,
				status = EXCLUDED.status,
				updated_at = now()`,
			id, tenantID, declared.ProviderID, declared.ProviderKind,
			fiatCurrencies, cryptoAssets, paymentMethods, countries,
			cfg.SupportsDeposit, cfg.SupportsWithdrawal, cfg.SupportsRefundReversal,
			declared.SettlementBehavior, declared.CallbackCapabilities, cfg.Priority, cfg.Status,
		)
	} else {
		_, err = tx.Exec(ctx,
			`INSERT INTO provider_capabilities
				(id, tenant_id, brand_id, provider_id, provider_kind,
				 supported_fiat_currencies, supported_crypto_assets, supported_payment_methods, supported_countries,
				 supports_deposit, supports_withdrawal, supports_refund_reversal,
				 settlement_behavior, callback_capabilities, priority, status)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
			 ON CONFLICT (tenant_id, brand_id, provider_id) WHERE brand_id IS NOT NULL DO UPDATE SET
				provider_kind = EXCLUDED.provider_kind,
				supported_fiat_currencies = EXCLUDED.supported_fiat_currencies,
				supported_crypto_assets = EXCLUDED.supported_crypto_assets,
				supported_payment_methods = EXCLUDED.supported_payment_methods,
				supported_countries = EXCLUDED.supported_countries,
				supports_deposit = EXCLUDED.supports_deposit,
				supports_withdrawal = EXCLUDED.supports_withdrawal,
				supports_refund_reversal = EXCLUDED.supports_refund_reversal,
				settlement_behavior = EXCLUDED.settlement_behavior,
				callback_capabilities = EXCLUDED.callback_capabilities,
				priority = EXCLUDED.priority,
				status = EXCLUDED.status,
				updated_at = now()`,
			id, tenantID, *brandID, declared.ProviderID, declared.ProviderKind,
			fiatCurrencies, cryptoAssets, paymentMethods, countries,
			cfg.SupportsDeposit, cfg.SupportsWithdrawal, cfg.SupportsRefundReversal,
			declared.SettlementBehavior, declared.CallbackCapabilities, cfg.Priority, cfg.Status,
		)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("payments: write capability: %w", err)
	}

	// Re-fetch the row's actual id (the INSERT..ON CONFLICT above may have
	// updated an existing row whose id differs from the freshly generated
	// one above).
	written, found, err := LoadCapability(ctx, tx, tenantID, derefOrNilBrand(brandID), declared.ProviderID)
	if err != nil {
		return uuid.Nil, err
	}
	if !found {
		return uuid.Nil, fmt.Errorf("payments: write capability: row not found immediately after write")
	}

	if _, err := tx.Exec(ctx, `DELETE FROM provider_capability_amount_limits WHERE provider_capability_id = $1`, written.ID); err != nil {
		return uuid.Nil, fmt.Errorf("payments: clear amount limits: %w", err)
	}
	for _, lim := range cfg.AmountLimits {
		if _, err := tx.Exec(ctx,
			`INSERT INTO provider_capability_amount_limits (provider_capability_id, tenant_id, asset_code, min_amount, max_amount)
			 VALUES ($1, $2, $3, $4, $5)`,
			written.ID, tenantID, lim.AssetCode, lim.MinAmount, lim.MaxAmount,
		); err != nil {
			return uuid.Nil, fmt.Errorf("payments: write amount limit for %s: %w", lim.AssetCode, err)
		}
	}
	return written.ID, nil
}

func derefOrNilBrand(brandID *uuid.UUID) uuid.UUID {
	if brandID == nil {
		return uuid.Nil
	}
	return *brandID
}

// validateNarrowing enforces docs/decisions/0022 §2.1: a tenant
// configuration may only narrow what the adapter itself declares, never
// widen it, and may never reassign provider_kind (WriteCapability never
// even accepts provider_kind/provider_id from cfg - they are always taken
// from declared - so this function's job is only the narrowing checks).
func validateNarrowing(declared AdapterCapability, cfg CapabilityConfig) error {
	if !isSubset(cfg.SupportedFiatCurrencies, declared.SupportedFiatCurrencies) {
		return fmt.Errorf("%w: supported_fiat_currencies exceeds adapter %s's declared set", ErrCapabilityWidensAdapter, declared.ProviderID)
	}
	if !isSubset(cfg.SupportedCryptoAssets, declared.SupportedCryptoAssets) {
		return fmt.Errorf("%w: supported_crypto_assets exceeds adapter %s's declared set", ErrCapabilityWidensAdapter, declared.ProviderID)
	}
	if !isSubset(cfg.SupportedPaymentMethods, declared.SupportedPaymentMethods) {
		return fmt.Errorf("%w: supported_payment_methods exceeds adapter %s's declared set", ErrCapabilityWidensAdapter, declared.ProviderID)
	}
	// Countries: empty means "not restricted" (docs/decisions/0022 §2). A
	// declared, non-empty restriction may never be widened to "not
	// restricted" (empty) or to a country outside the declared set.
	if len(declared.SupportedCountries) > 0 {
		if len(cfg.SupportedCountries) == 0 {
			return fmt.Errorf("%w: adapter %s restricts countries but configuration asserts unrestricted access", ErrCapabilityWidensAdapter, declared.ProviderID)
		}
		if !isSubset(cfg.SupportedCountries, declared.SupportedCountries) {
			return fmt.Errorf("%w: supported_countries exceeds adapter %s's declared set", ErrCapabilityWidensAdapter, declared.ProviderID)
		}
	}
	if cfg.SupportsDeposit && !declared.SupportsDeposit {
		return fmt.Errorf("%w: adapter %s does not support deposits", ErrCapabilityWidensAdapter, declared.ProviderID)
	}
	if cfg.SupportsWithdrawal && !declared.SupportsWithdrawal {
		return fmt.Errorf("%w: adapter %s does not support withdrawals", ErrCapabilityWidensAdapter, declared.ProviderID)
	}
	if cfg.SupportsRefundReversal && !declared.SupportsRefundReversal {
		return fmt.Errorf("%w: adapter %s does not support refund/reversal", ErrCapabilityWidensAdapter, declared.ProviderID)
	}

	declaredLimits := make(map[string]AmountLimit, len(declared.AmountLimits))
	for _, dl := range declared.AmountLimits {
		declaredLimits[dl.AssetCode] = dl
	}
	for _, cl := range cfg.AmountLimits {
		dl, ok := declaredLimits[cl.AssetCode]
		if !ok {
			return fmt.Errorf("%w: adapter %s declares no amount limits for asset %s", ErrCapabilityWidensAdapter, declared.ProviderID, cl.AssetCode)
		}
		if cl.MinAmount < dl.MinAmount || cl.MaxAmount > dl.MaxAmount {
			return fmt.Errorf("%w: amount limits for %s (%d-%d) exceed adapter %s's declared range (%d-%d)",
				ErrCapabilityWidensAdapter, cl.AssetCode, cl.MinAmount, cl.MaxAmount, declared.ProviderID, dl.MinAmount, dl.MaxAmount)
		}
	}
	return nil
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
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

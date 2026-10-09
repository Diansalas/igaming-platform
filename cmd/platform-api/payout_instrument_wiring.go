package main

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
)

// payoutInstrumentKeys builds the two B13 key families from configuration, or
// (nil, nil) when neither is configured. config.Load already validated them;
// this repeats the parse so a Config built by hand fails closed too.
func payoutInstrumentKeys(cfg config.Config) (*payoutinstrument.Keys, error) {
	if !cfg.PayoutInstrumentKeys.IsSet() && !cfg.PayoutInstrumentFPKeys.IsSet() {
		return nil, nil
	}
	master, err := actorproof.ParseKeySet(cfg.PayoutInstrumentKeys.Reveal())
	if err != nil {
		return nil, fmt.Errorf("payout instrument keys: %w", err)
	}
	fp, err := actorproof.ParseKeySet(cfg.PayoutInstrumentFPKeys.Reveal())
	if err != nil {
		return nil, fmt.Errorf("payout instrument fingerprint keys: %w", err)
	}
	return payoutinstrument.NewKeys(cfg.PayoutInstrumentActiveKID, master, cfg.PayoutInstrumentFPActiveKID, fp)
}

// buildPayoutInstrumentService applies the ADR 0111 2.2 startup gate and
// builds the service. The gate uses cfg.GuardEnvironment() (a missing APP_ENV
// counts as production) and fires whenever ANY non-Synthetic payout adapter or
// instrument verifier is registered, in any environment. With no keys and no
// reason to require them the service is nil (the routes answer 503): there is
// no random-key fallback in this binary.
func buildPayoutInstrumentService(cfg config.Config, b providerBundle) (*payoutinstrument.Service, error) {
	keys, err := payoutInstrumentKeys(cfg)
	if err != nil {
		return nil, err
	}
	var verifiers []payoutinstrument.PayoutInstrumentVerifier
	if b.PayoutVerifier != nil {
		verifiers = append(verifiers, b.PayoutVerifier)
	}
	var adapters []any
	for _, a := range b.paymentsAdapters() {
		adapters = append(adapters, a)
	}
	if err := payoutinstrument.VerifyStartup(cfg.GuardEnvironment(), keys, payoutinstrument.Registrations{PaymentAdapters: adapters, Verifiers: verifiers}); err != nil {
		return nil, err
	}
	if keys == nil {
		return nil, nil
	}
	return payoutinstrument.NewService(keys, payoutinstrument.DefaultKinds(), verifiers...)
}

// legacyBindingChecker counts non-terminal withdrawals carrying a provider id
// outside the MOCK set (the Go repeat of migration 0123's up-time assertion,
// A-11). The tables are FORCE RLS, so it iterates tenants.
func legacyBindingChecker(pool *db.Pool) payoutinstrument.LegacyBindingChecker {
	return func(ctx context.Context, mockIDs []string) (int64, error) {
		var tenants []uuid.UUID
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT id FROM tenants ORDER BY id`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id uuid.UUID
				if err := rows.Scan(&id); err != nil {
					return err
				}
				tenants = append(tenants, id)
			}
			return rows.Err()
		}); err != nil {
			return 0, err
		}
		var total int64
		for _, tid := range tenants {
			if err := pool.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
				var n int64
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_requests
					WHERE tenant_id = $1 AND state IN ('requested','pending_review','approved','submitted')
					  AND provider_id IS NOT NULL AND payout_instrument_id IS NULL AND NOT (provider_id = ANY($2))`, tid, mockIDs).Scan(&n); err != nil {
					return err
				}
				total += n
				return nil
			}); err != nil {
				return 0, err
			}
		}
		return total, nil
	}
}

// bindingGuardChecker reads the source of the binding guard function (catalog read; no tenant data).
func bindingGuardChecker(pool *db.Pool) payoutinstrument.BindingGuardChecker {
	return func(ctx context.Context) (string, error) {
		var src string
		err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT prosrc FROM pg_proc WHERE proname = 'withdrawal_requests_payout_binding_guard' AND pronamespace = 'public'::regnamespace`).Scan(&src)
		})
		return src, err
	}
}

// payoutRegistrations is the set the startup gates inspect.
func payoutRegistrations(b providerBundle) payoutinstrument.Registrations {
	var adapters []any
	for _, a := range b.paymentsAdapters() {
		adapters = append(adapters, a)
	}
	return payoutinstrument.Registrations{PaymentAdapters: adapters}
}

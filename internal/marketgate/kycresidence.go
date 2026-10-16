package marketgate

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/geolocation"
	"github.com/Diansalas/igaming-platform/internal/kyc"
)

// KYCResidenceLocator is the platform's CURRENT, production-eligible
// market-evidence source (owner decision 3; docs/plans/
// market-evidence-kyc-record.md). It implements geolocation.Locator, but it
// consults NO network or vendor: the "signal" is the country of the player's
// KYC-VERIFIED residence (kyc.ReadMarketResidenceEvidence), read through a
// tenant-scoped READ ONLY transaction (RLS on kyc_verifications).
//
//   - Resolved only for a player account whose latest decided verification is
//     approved and unexpired and that carries a reviewer-recorded residence
//     determination. Everything else - no player account (registration),
//     unbound pool, no verification, unverified / pending / review / rejected /
//     expired, no determination - is SignalInconclusive, which the gate
//     refuses.
//   - A database error is returned as an error (also refused by the gate).
//   - The tenant and player account come from the SignalRequest, which every
//     caller fills from the authenticated token; the request IP is ignored and
//     never read, stored or logged. The country is never taken from the client.
//
// A future vendor geolocation adapter replaces or complements this by being
// wired as the gate's Locator instead; no caller changes.
type KYCResidenceLocator struct {
	reader atomic.Pointer[tenantReader]
}

// tenantReader is the one database capability the locator needs. *db.Pool
// satisfies it; tests inject a failing one.
type tenantReader interface {
	WithTenantReadOnly(ctx context.Context, tenantID uuid.UUID, fn db.TxFunc) error
}

var _ geolocation.Locator = (*KYCResidenceLocator)(nil)

// NewKYCResidenceLocator returns a locator over pool. A nil pool is legal: the
// locator is then unbound and reports no evidence (the gate refuses) until Bind.
func NewKYCResidenceLocator(pool *db.Pool) *KYCResidenceLocator {
	l := &KYCResidenceLocator{}
	if pool != nil {
		_ = l.Bind(pool)
	}
	return l
}

// Bind attaches the database pool (cmd/platform-api builds the provider
// bundle before the pool exists, so the production startup guard can run
// first). A nil pool is an error.
func (l *KYCResidenceLocator) Bind(pool *db.Pool) error {
	if pool == nil {
		return errors.New("marketgate: KYC residence locator needs a database pool")
	}
	return l.bindReader(pool)
}

func (l *KYCResidenceLocator) bindReader(r tenantReader) error {
	l.reader.Store(&r)
	return nil
}

// Bound reports whether a database pool is attached.
func (l *KYCResidenceLocator) Bound() bool { return l != nil && l.reader.Load() != nil }

// recheck implements txRechecker: the same read as GetSignal, on the caller's gated transaction.
func (l *KYCResidenceLocator) recheck(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID uuid.UUID) (string, bool, error) {
	if tenantID == uuid.Nil || playerAccountID == uuid.Nil {
		return "", false, nil
	}
	return kyc.ReadMarketResidenceEvidence(ctx, tx, tenantID, playerAccountID)
}

// MarkProductionEligible implements providerkind.ProductionEligible
// (structural marker): this is a platform-owned read of platform-owned KYC
// data, not a mock. It is NOT Synthetic, so the MOCK-ADAPTER-PROD-1 guard accepts it.
func (l *KYCResidenceLocator) MarkProductionEligible() {}

// Reasons carried in SignalResult.Reason (machine-readable, never a country).
const (
	reasonUnbound     = "kyc_residence_unbound"
	reasonNoPlayer    = "kyc_residence_no_player_account"
	reasonNoEvidence  = "kyc_residence_not_verified"
	reasonKYCVerified = "kyc_verified_residence"
)

// GetSignal implements geolocation.Locator.
func (l *KYCResidenceLocator) GetSignal(ctx context.Context, req geolocation.SignalRequest) (geolocation.SignalResult, error) {
	if l == nil {
		return geolocation.SignalResult{Outcome: geolocation.SignalInconclusive, Reason: reasonUnbound}, nil
	}
	rp := l.reader.Load()
	if rp == nil {
		return geolocation.SignalResult{Outcome: geolocation.SignalInconclusive, Reason: reasonUnbound}, nil
	}
	if req.TenantID == uuid.Nil || req.PlayerAccountID == uuid.Nil {
		return geolocation.SignalResult{Outcome: geolocation.SignalInconclusive, Reason: reasonNoPlayer}, nil
	}
	var country string
	var found bool
	err := (*rp).WithTenantReadOnly(ctx, req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, ok, err := kyc.ReadMarketResidenceEvidence(ctx, tx, req.TenantID, req.PlayerAccountID)
		country, found = c, ok
		return err
	})
	if err != nil {
		return geolocation.SignalResult{}, err
	}
	if !found {
		return geolocation.SignalResult{Outcome: geolocation.SignalInconclusive, Reason: reasonNoEvidence}, nil
	}
	return geolocation.SignalResult{
		Outcome: geolocation.SignalResolved, CountryCode: country,
		ProviderReference: reasonKYCVerified, Reason: reasonKYCVerified,
	}, nil
}

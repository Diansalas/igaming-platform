package tenant

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrNotActiveForPaymentInitiation is returned by
// RequireActiveForPaymentInitiation when the tenant or the brand is not
// 'active', or either row is missing/unreadable. Callers translate it into a
// deterministic, non-retryable-until-reactivation refusal; nothing may be
// created after it (no intent, attempt, request, hold or provider call).
var ErrNotActiveForPaymentInitiation = errors.New("tenant: tenant or brand is not active; new payment initiation is refused")

// RequireActiveForPaymentInitiation is the ONE shared check behind owner/
// security rulings H-SEC-5 (HTTP deposit initiation) and H-SEC-11 (HTTP
// withdrawal/payout initiation): a NEW deposit intent/attempt, withdrawal
// request, or payout claim is refused unless BOTH the tenant and the brand
// are 'active' at initiation time. It must be called inside the SAME
// transaction that creates the row, before the first insert, and (where the
// path locks a parent row) after that lock.
//
// Tenant half: GameplayStatus - takes the per-tenant status advisory lock
// SHARED (migration 0118), then reads tenants.status in a fresh statement,
// so a concurrent status change either committed first (and is seen) or
// waits until this transaction ends. Brand half: the brand row of THIS tenant
// is read FOR SHARE, so a concurrent brand status UPDATE waits for this
// transaction (the brand UPDATE policy is satisfied by the tenant-scoped
// connection; a row the locking read cannot see is treated as missing).
//
// Fail closed: a missing row, a brand that does not belong to the tenant, a
// status other than 'active', or any read error refuses (a read error is
// returned wrapped, never swallowed as "active").
func RequireActiveForPaymentInitiation(ctx context.Context, tx pgx.Tx, tenantID, brandID uuid.UUID) error {
	status, err := GameplayStatus(ctx, tx, tenantID)
	if err != nil {
		if errors.Is(err, ErrNotActiveForGameplay) {
			return fmt.Errorf("%w: %w", ErrNotActiveForPaymentInitiation, err)
		}
		return err
	}
	if status != "active" {
		return fmt.Errorf("%w: tenant status=%s", ErrNotActiveForPaymentInitiation, status)
	}
	return RequireBrandActive(ctx, tx, tenantID, brandID)
}

// ErrBrandNotActive is the BRAND-only refusal (H(8), ADR 0095 section 44
// decisions 19-23): the brand row of the tenant is not 'active', is missing /
// not visible, or no brand id was supplied. It is separate from the tenant
// policy check on purpose (decision 23): RequireBrandActive never reads the
// tenant, and the tenant half never reads the brand.
var ErrBrandNotActive = errors.New("tenant: brand is not active")

// RequireBrandActive is the brand half of RequireActiveForPaymentInitiation as
// a standalone, brand-only policy check. The brand row of THIS tenant is read
// FOR SHARE, so a concurrent brand status UPDATE waits for the caller's
// transaction (no check/use race). A refusal wraps BOTH ErrBrandNotActive and
// ErrNotActiveForPaymentInitiation (so existing HTTP callers keep matching);
// fail closed: missing row, foreign tenant's brand, nil id or non-'active'
// refuses; a read error is returned wrapped, never read as "active".
func RequireBrandActive(ctx context.Context, tx pgx.Tx, tenantID, brandID uuid.UUID) error {
	if brandID == uuid.Nil {
		return fmt.Errorf("%w: %w: no brand id", ErrNotActiveForPaymentInitiation, ErrBrandNotActive)
	}
	var brandStatus string
	err := tx.QueryRow(ctx,
		`SELECT status FROM public.brands WHERE id = $1 AND tenant_id = $2 FOR SHARE`,
		brandID, tenantID,
	).Scan(&brandStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %w: brand row not visible", ErrNotActiveForPaymentInitiation, ErrBrandNotActive)
	}
	if err != nil {
		return fmt.Errorf("tenant: read brand status: %w", err)
	}
	if brandStatus != "active" {
		return fmt.Errorf("%w: %w: brand status=%s", ErrNotActiveForPaymentInitiation, ErrBrandNotActive, brandStatus)
	}
	return nil
}

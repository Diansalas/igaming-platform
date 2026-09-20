//go:build integration

// Real-PostgreSQL tests for EvaluateLicenceValidity (ADR 0045 §2.5,
// SEC-4I-F10's technical predicate).
package jurisdiction

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestEvaluateLicenceValidity_SuspendedExpiredBoundaryAndNotBound(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	asOf := time.Now().UTC()

	t.Run("not bound - uuid.Nil", func(t *testing.T) {
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			got, err := EvaluateLicenceValidity(ctx, tx, uuid.Nil, asOf)
			if err != nil {
				return err
			}
			if got != LicenceNotBound {
				t.Fatalf("expected LicenceNotBound, got %q", got)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("EvaluateLicenceValidity: %v", err)
		}
	})

	t.Run("not found - unknown licence id", func(t *testing.T) {
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			got, err := EvaluateLicenceValidity(ctx, tx, uuid.New(), asOf)
			if err != nil {
				return err
			}
			if got != LicenceNotFound {
				t.Fatalf("expected LicenceNotFound, got %q", got)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("EvaluateLicenceValidity: %v", err)
		}
	})

	t.Run("active, no expiry - valid", func(t *testing.T) {
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			got, err := EvaluateLicenceValidity(ctx, tx, f.licenceID, asOf)
			if err != nil {
				return err
			}
			if got != LicenceValid {
				t.Fatalf("expected LicenceValid, got %q", got)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("EvaluateLicenceValidity: %v", err)
		}
	})

	t.Run("suspended", func(t *testing.T) {
		licenceID := uuid.New()
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number, status) VALUES ($1, $2, 'platform', 'LIC-SUSP', 'suspended')`,
				licenceID, f.jurisdictionID)
			return err
		})
		if err != nil {
			t.Fatalf("seed suspended licence: %v", err)
		}
		err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			got, err := EvaluateLicenceValidity(ctx, tx, licenceID, asOf)
			if err != nil {
				return err
			}
			if got != LicenceSuspended {
				t.Fatalf("expected LicenceSuspended, got %q", got)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("EvaluateLicenceValidity: %v", err)
		}
	})

	t.Run("status expired", func(t *testing.T) {
		licenceID := uuid.New()
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number, status) VALUES ($1, $2, 'platform', 'LIC-EXP', 'expired')`,
				licenceID, f.jurisdictionID)
			return err
		})
		if err != nil {
			t.Fatalf("seed expired licence: %v", err)
		}
		err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			got, err := EvaluateLicenceValidity(ctx, tx, licenceID, asOf)
			if err != nil {
				return err
			}
			if got != LicenceStatusExpired {
				t.Fatalf("expected LicenceStatusExpired, got %q", got)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("EvaluateLicenceValidity: %v", err)
		}
	})

	t.Run("date expiry boundary is strict - invalid ON the expiry date itself", func(t *testing.T) {
		licenceID := uuid.New()
		today := time.Now().UTC().Truncate(24 * time.Hour)
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number, status, expires_at) VALUES ($1, $2, 'platform', 'LIC-DATE', 'active', $3)`,
				licenceID, f.jurisdictionID, today)
			return err
		})
		if err != nil {
			t.Fatalf("seed date-bound licence: %v", err)
		}

		// AsOf strictly BEFORE the expiry date: valid.
		err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			got, err := EvaluateLicenceValidity(ctx, tx, licenceID, today.Add(-24*time.Hour))
			if err != nil {
				return err
			}
			if got != LicenceValid {
				t.Fatalf("expected LicenceValid the day before expiry, got %q", got)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("EvaluateLicenceValidity: %v", err)
		}

		// AsOf ON the expiry date itself: INVALID (strict boundary).
		err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			got, err := EvaluateLicenceValidity(ctx, tx, licenceID, today)
			if err != nil {
				return err
			}
			if got != LicenceDateExpired {
				t.Fatalf("expected LicenceDateExpired ON the expiry date itself (strict boundary), got %q", got)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("EvaluateLicenceValidity: %v", err)
		}

		// AsOf AFTER the expiry date: also invalid.
		err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			got, err := EvaluateLicenceValidity(ctx, tx, licenceID, today.Add(24*time.Hour))
			if err != nil {
				return err
			}
			if got != LicenceDateExpired {
				t.Fatalf("expected LicenceDateExpired after the expiry date, got %q", got)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("EvaluateLicenceValidity: %v", err)
		}
	})
}

// TestEvaluateLicenceValidity_NotYetIssuedLicenceIsInvalid is ADR 0045
// §18's finding F4: a licence whose issued_at is strictly in the future
// at AsOf must be reported LicenceNotYetIssued, not LicenceValid - the
// original predicate's fail-open, since it read only status/expires_at.
func TestEvaluateLicenceValidity_NotYetIssuedLicenceIsInvalid(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	licenceID := uuid.New()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	futureIssue := today.Add(24 * time.Hour)

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number, status, issued_at) VALUES ($1, $2, 'platform', 'LIC-FUTURE', 'active', $3)`,
			licenceID, f.jurisdictionID, futureIssue)
		return err
	})
	if err != nil {
		t.Fatalf("seed future-issued licence: %v", err)
	}

	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		got, err := EvaluateLicenceValidity(ctx, tx, licenceID, today)
		if err != nil {
			return err
		}
		if got != LicenceNotYetIssued {
			t.Fatalf("expected LicenceNotYetIssued, got %q", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("EvaluateLicenceValidity: %v", err)
	}

	// Once AsOf reaches the issue date, the licence is valid again (no
	// expiry set).
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		got, err := EvaluateLicenceValidity(ctx, tx, licenceID, futureIssue)
		if err != nil {
			return err
		}
		if got != LicenceValid {
			t.Fatalf("expected LicenceValid once AsOf reaches issued_at, got %q", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("EvaluateLicenceValidity: %v", err)
	}
}

// TestEvaluateLicenceValidity_LicenceIsValidOnItsOwnIssueDate pins the
// INCLUSIVE lower boundary: AsOf == issued_at (the same UTC date) is
// valid, not "not yet issued" - the interval is half-open
// [issued_at, expires_at).
func TestEvaluateLicenceValidity_LicenceIsValidOnItsOwnIssueDate(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	licenceID := uuid.New()
	today := time.Now().UTC().Truncate(24 * time.Hour)

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number, status, issued_at) VALUES ($1, $2, 'platform', 'LIC-TODAY', 'active', $3)`,
			licenceID, f.jurisdictionID, today)
		return err
	})
	if err != nil {
		t.Fatalf("seed today-issued licence: %v", err)
	}

	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		got, err := EvaluateLicenceValidity(ctx, tx, licenceID, today)
		if err != nil {
			return err
		}
		if got != LicenceValid {
			t.Fatalf("expected LicenceValid ON the licence's own issue date (inclusive lower boundary), got %q", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("EvaluateLicenceValidity: %v", err)
	}
}

// TestEvaluateLicenceValidity_NullIssuedAtIsNotTreatedAsNotYetIssued
// confirms a NULL issued_at (every existing licence row, since no Go code
// writes this column today) is treated as "no issue date asserted" and
// falls through to LicenceValid (given no expiry), NOT as
// LicenceNotYetIssued - the latter would fail-close every existing row.
func TestEvaluateLicenceValidity_NullIssuedAtIsNotTreatedAsNotYetIssued(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	asOf := time.Now().UTC()

	// f.licenceID (seedFixture's own licence) has issued_at NULL by
	// construction - no INSERT anywhere names the column.
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		got, err := EvaluateLicenceValidity(ctx, tx, f.licenceID, asOf)
		if err != nil {
			return err
		}
		if got != LicenceValid {
			t.Fatalf("expected LicenceValid for a NULL issued_at (no issue date asserted), got %q", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("EvaluateLicenceValidity: %v", err)
	}
}

// TestEvaluateLicenceValidity_SuspendedStatusOutranksNotYetIssued pins the
// ordering: the status switch runs BEFORE the issued_at check, so a
// suspended licence reports LicenceSuspended even if its issued_at is
// also in the future.
func TestEvaluateLicenceValidity_SuspendedStatusOutranksNotYetIssued(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	licenceID := uuid.New()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	futureIssue := today.Add(24 * time.Hour)

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number, status, issued_at) VALUES ($1, $2, 'platform', 'LIC-SUSP-FUTURE', 'suspended', $3)`,
			licenceID, f.jurisdictionID, futureIssue)
		return err
	})
	if err != nil {
		t.Fatalf("seed suspended future-issued licence: %v", err)
	}

	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		got, err := EvaluateLicenceValidity(ctx, tx, licenceID, today)
		if err != nil {
			return err
		}
		if got != LicenceSuspended {
			t.Fatalf("expected LicenceSuspended (status is checked before issued_at), got %q", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("EvaluateLicenceValidity: %v", err)
	}
}

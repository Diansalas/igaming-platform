package withdrawal

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// Migration 0131 defence in depth: the database's WD010 / WD011 refusals are the H-SEC-11 refusal.
func TestTranslateInsertError(t *testing.T) {
	for _, code := range []string{"WD010", "WD011"} {
		err := translateInsertError(&pgconn.PgError{Code: code, Message: "m"})
		if !errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
			t.Errorf("%s must be the not-active refusal: %v", code, err)
		}
	}
	for _, code := range []string{"23505", "PI046", "WD012", "42501"} {
		err := translateInsertError(&pgconn.PgError{Code: code})
		if errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
			t.Errorf("%s must NOT be the not-active refusal: %v", code, err)
		}
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != code {
			t.Errorf("%s must stay inspectable: %v", code, err)
		}
	}
	if err := translateInsertError(errors.New("boom")); errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
		t.Error("an unrelated error must not be translated")
	}
}

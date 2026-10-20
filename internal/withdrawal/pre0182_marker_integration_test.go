//go:build integration

package withdrawal

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// markPre0182Schema is the TEST-SIDE probe (the f4AllowDispatch pattern): a migration up / down / up test drives a scratch
// database whose schema predates migration 0182; on such a database (NEITHER the policy table NOR its set-version function
// exists) the returned context carries the test-only marker, so the restriction evaluation answers the pre-0182 behaviour.
// On any 0182+ schema the context is returned unchanged and the production path runs in full.
func markPre0182Schema(ctx context.Context, tx pgx.Tx) (context.Context, error) {
	var fn, table bool
	if err := tx.QueryRow(ctx, `SELECT to_regprocedure('public.withdrawal_restriction_set_version(uuid)') IS NOT NULL,
		to_regclass('public.withdrawal_restriction_policies') IS NOT NULL`).Scan(&fn, &table); err != nil {
		return ctx, err
	}
	if fn || table {
		return ctx, nil
	}
	return context.WithValue(ctx, pre0182SchemaKey{}, true), nil
}

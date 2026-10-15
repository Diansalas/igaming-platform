//go:build integration

// Package adjlinkpost (integration builds ONLY) is adjlinkfix.Without around one ledger.Post: the owner-only fixture
// exemption from migration 0153's manual_adjustment link fence (MANUAL-ADJ-LINK-1, ADR 0100 section 21) for a
// fixture that funds a wallet (or posts an arbitrary balanced shape) with an UNGOVERNED manual_adjustment. It is a
// separate package only because internal/ledger's own tests import adjlinkfix and cannot import a package that
// imports internal/ledger. Every rule of adjlinkfix applies (owner transaction only, fixture setup only, never in a
// race, every caller file listed in internal/adjustment's static inventory).
package adjlinkpost

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/testsupport/adjlinkfix"
)

// Post is ledger.Post(ctx, tx, in) with the two 0153 link triggers disabled for exactly this call, inside tx.
func Post(ctx context.Context, tx pgx.Tx, in ledger.TransactionInput) (ledger.PostResult, error) {
	var res ledger.PostResult
	err := adjlinkfix.Without(ctx, tx, func() error {
		var err error
		res, err = ledger.Post(ctx, tx, in)
		return err
	})
	return res, err
}

package db

import (
	"context"
	"fmt"

	"github.com/Diansalas/igaming-platform/internal/txscope"
)

// ADR 0119 section 13.1 item 6: the two pre-tenant lookup helpers for staff
// login challenges and staff enrolment tokens. A presented challenge or
// enrolment token carries no tenant hint, exactly like a refresh token or a
// credential token, so these mirror WithSessionLookup / WithCredentialTokenLookup
// (migration 0018 / 0040 / ADR 0016): a platform-scoped transaction (no
// app.tenant_id) with one lookup GUC set to the lowercase hex of the token's
// KEYED MAC. Migration 0137's staff_challenge_select_by_mac /
// staff_enrolment_select_by_mac policies grant visibility to at most the one
// row whose token_mac equals that value, SELECT only. Any mutation (including
// FOR UPDATE) must use the two-phase pattern: learn the staff member's scope
// here, then operate in the resolved tenant or platform scope.

// staffLookupMACHexLen is the hex length of a 32-byte HMAC-SHA256.
const staffLookupMACHexLen = 64

func validStaffLookupMACHex(s string) bool {
	if len(s) != staffLookupMACHexLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// WithStaffChallengeLookup runs fn in a platform-scoped transaction with
// "app.staff_challenge_lookup_mac" set to macHex (64 lowercase hex characters:
// the keyed MAC of the presented challenge token, never the token itself).
// Anything else is refused before a transaction is opened.
func (p *Pool) WithStaffChallengeLookup(ctx context.Context, macHex string, fn TxFunc) error {
	return p.withStaffLookup(ctx, "app.staff_challenge_lookup_mac", macHex, fn)
}

// WithStaffEnrolmentLookup is WithStaffChallengeLookup for staff_enrolment_tokens
// ("app.staff_enrolment_lookup_mac").
func (p *Pool) WithStaffEnrolmentLookup(ctx context.Context, macHex string, fn TxFunc) error {
	return p.withStaffLookup(ctx, "app.staff_enrolment_lookup_mac", macHex, fn)
}

func (p *Pool) withStaffLookup(ctx context.Context, guc, macHex string, fn TxFunc) error {
	if !validStaffLookupMACHex(macHex) {
		return fmt.Errorf("db: staff lookup called with a malformed MAC")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// guc is one of the two constants above, never caller input.
	if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, guc, macHex); err != nil {
		return fmt.Errorf("db: set staff lookup context: %w", err)
	}
	if err := fn(txscope.Mark(ctx), tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit tx: %w", err)
	}
	return nil
}

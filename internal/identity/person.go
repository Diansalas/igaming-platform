package identity

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Person is the platform-wide human behind one or more PlayerAccounts.
// See docs/architecture/05-identity-architecture.md. Deliberately a
// separate type from PlayerAccount, even though Stage 2 creates exactly
// one Person per registration (no cross-brand resolution logic exists
// yet - that is Stage 4's KYC-driven hash matching).
type Person struct {
	ID     uuid.UUID
	Status string
}

// CreatePerson inserts a new, unverified Person. tx can be any
// transaction - persons carries no tenant_id and has no RLS (it is
// platform-wide by design), so this works whether tx came from
// db.WithTenant or db.WithoutTenant. Callers creating a player account
// call this within the SAME transaction as the player_accounts insert,
// so a person is never left orphaned by a failed registration.
func CreatePerson(ctx context.Context, tx pgx.Tx) (Person, error) {
	p := Person{ID: uuid.New(), Status: "unverified"}
	_, err := tx.Exec(ctx, `INSERT INTO persons (id, status) VALUES ($1, $2)`, p.ID, p.Status)
	if err != nil {
		return Person{}, fmt.Errorf("identity: create person: %w", err)
	}
	return p, nil
}

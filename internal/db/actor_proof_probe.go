package db

import (
	"context"
	"fmt"
)

// ActorProofKeyActive reports whether kid is an ACTIVE verification key in the
// owner-only actor_proof_keys table (migration 0120), through the SECURITY
// DEFINER probe actor_proof_key_active - the runtime role cannot SELECT the
// table itself and the probe discloses no key material. It satisfies
// actorproof.KeyChecker for the production startup gate
// (actorproof.VerifyConfiguredInProduction).
func (p *Pool) ActorProofKeyActive(ctx context.Context, kid string) (bool, error) {
	var ok bool
	if err := p.pool.QueryRow(ctx, `SELECT actor_proof_key_active($1)`, kid).Scan(&ok); err != nil {
		return false, fmt.Errorf("db: actor proof key probe: %w", err)
	}
	return ok, nil
}

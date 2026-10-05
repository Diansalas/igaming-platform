//go:build integration

package kyc

// PRH-2 E1 (ADR 0106 test 15, mutants M1/M2): the (id, claim_token)
// compare-and-set is enforced by EACH statement on its own. The layers are
// redundant by design (the lock precedes every CAS), so this test drives each
// statement directly instead of through the worker, where one layer would mask
// the other.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestOutbox_15b_EachStatementRequiresTheClaimToken_M1_M2(t *testing.T) {
	r := newRig(t)
	v := r.create()
	claimed, err := r.w.claimNext(context.Background(), nil)
	if err != nil || claimed == nil || claimed.VerificationID != v.ID {
		t.Fatalf("setup: claim failed: %+v %v", claimed, err)
	}
	wrong := *claimed
	wrong.ClaimToken = uuid.New()

	err = r.pool.WithTenant(context.Background(), r.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockClaim(ctx, tx, *claimed); err != nil {
			t.Errorf("positive control: the real token must lock the row, got %v", err)
		}
		if err := lockClaim(ctx, tx, wrong); !errors.Is(err, errLostClaim) {
			t.Errorf("lockClaim with a wrong token must report a lost claim (M2), got %v", err)
		}
		cases := []struct {
			name string
			sql  string
			args []any
		}{
			{"sent", sqlOutboxSent, []any{wrong.ID, wrong.ClaimToken}},
			{"cancel", sqlOutboxCancel, []any{wrong.ID, wrong.ClaimToken, string(CancelSuperseded)}},
			{"retry", sqlOutboxRetry, []any{wrong.ID, wrong.ClaimToken, string(ClassNotSent), float64(60)}},
			{"terminal", sqlOutboxTerminal, []any{wrong.ID, wrong.ClaimToken, string(ClassNotSent)}},
		}
		for _, c := range cases {
			if err := casOne(ctx, tx, c.sql, c.args...); !errors.Is(err, errLostClaim) {
				t.Errorf("the %s transition with a wrong token must report a lost claim (M1), got %v", c.name, err)
			}
		}
		return errProbeRollback
	})
	if err != nil && !errors.Is(err, errProbeRollback) {
		t.Fatal(err)
	}
	if row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate); row.State != OutboxClaimed {
		t.Fatalf("a wrong token must leave the row claimed, got %s", row.State)
	}
}

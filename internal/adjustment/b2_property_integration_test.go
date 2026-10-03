//go:build integration

package adjustment

import (
	"math/rand"
	"testing"
)

// B-2 (PROP): a seeded random sequence of submissions, approvals,
// rejections and cancellations - credits and debits, across tenant and
// acting actors, including debits beyond the balance (refusals) - keeps the
// player's balance equal to a simple model and every financial invariant
// (SUM(D) = SUM(C), projection = recomputed) true after every step.
func TestB2_RandomSequencesKeepInvariants(t *testing.T) {
	for _, seed := range []int64{20260928, 7, 424242} {
		runB2(t, seed)
	}
}

func runB2(t *testing.T, seed int64) {
	w := newWorld(t, worldOpts{base: 1})
	rng := rand.New(rand.NewSource(seed))
	model := int64(0)
	initiators := []staffMember{w.F1, w.F2, w.Acting}
	deciders := []staffMember{w.F1, w.F2, w.F3, w.Acting, w.Acting2}
	executed, refused, rejected, cancelled := 0, 0, 0, 0

	for step := 0; step < 40; step++ {
		init := initiators[rng.Intn(len(initiators))]
		amount := int64(1 + rng.Intn(2_000))
		var in SubmitInput
		if rng.Intn(2) == 0 {
			in = w.credit(amount, []ReasonCode{ReasonOperationalErrorCorrection, ReasonExternalInstruction}[rng.Intn(2)])
		} else {
			// Debits straddle the balance so both outcomes occur.
			amount = int64(1 + rng.Intn(int(model)+1_500))
			in = w.debit(amount, ReasonOperationalErrorCorrection)
		}
		r, err := w.submit(init, in)
		if err != nil {
			t.Fatalf("seed %d step %d submit: %v", seed, step, err)
		}
		switch rng.Intn(6) {
		case 0:
			if _, err := w.svc.Cancel(ctxFor(init), w.target(), r.ID, Meta{}); err != nil {
				t.Fatalf("seed %d step %d cancel: %v", seed, step, err)
			}
			cancelled++
			continue
		case 1:
			var dec staffMember
			for {
				dec = deciders[rng.Intn(len(deciders))]
				if dec.ID != init.ID {
					break
				}
			}
			if _, err := w.decide(dec, r, DecisionReject); err != nil {
				t.Fatalf("seed %d step %d reject: %v", seed, step, err)
			}
			rejected++
			continue
		}
		var dec staffMember
		for {
			dec = deciders[rng.Intn(len(deciders))]
			if dec.ID != init.ID {
				break
			}
		}
		out, err := w.decide(dec, r, DecisionApprove)
		if err != nil {
			t.Fatalf("seed %d step %d approve: %v", seed, step, err)
		}
		switch {
		case out.Executed && in.Direction == DirectionCreditPlayer:
			model += amount
			executed++
		case out.Executed:
			if amount > model {
				t.Fatalf("seed %d step %d: a debit beyond the balance executed", seed, step)
			}
			model -= amount
			executed++
		case out.Request.State == StateRefusedInsufficientFunds:
			if in.Direction != DirectionDebitPlayer || amount <= model {
				t.Fatalf("seed %d step %d: wrongly refused for funds: %+v", seed, step, out.Request)
			}
			refused++
		default:
			t.Fatalf("seed %d step %d: unexpected outcome %+v", seed, step, out)
		}
		if got := w.playerCash(); got != model {
			t.Fatalf("seed %d step %d: balance %d != model %d", seed, step, got, model)
		}
		if got := w.playerCash(); got < 0 {
			t.Fatalf("INV-ADJ-4 broken: negative player_cash %d", got)
		}
	}
	w.assertInvariants()
	t.Logf("seed %d: executed=%d refused_insufficient=%d rejected=%d cancelled=%d final=%d", seed, executed, refused, rejected, cancelled, model)
	if executed == 0 || refused == 0 {
		t.Fatalf("sequence did not exercise both executions and refusals (executed=%d refused=%d)", executed, refused)
	}
}

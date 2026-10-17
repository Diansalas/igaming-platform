package payoutinstrument

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// F2 / SRF2-1 (migration 0168): the payment_statement run seal.

func runSealFixture() statement.RunSealInput {
	t0 := time.Date(2026, 10, 17, 1, 0, 0, 123000, time.UTC)
	return statement.RunSealInput{RunID: uuid.New(), TenantID: uuid.New(), ProviderID: "mock-payments", Stream: "payment_statement",
		ImportID: uuid.New(), CoverageStart: t0, CoverageEnd: t0.Add(time.Hour), WindowTruncated: true, RunAt: t0.Add(61 * time.Minute)}
}

func TestRunSeal_RoundTrip_TamperRefused(t *testing.T) {
	k := newTestKeys(t)
	in := runSealFixture()
	seal, kid, err := k.SealPaymentRun(in)
	if err != nil || len(seal) != 64 || kid == "" {
		t.Fatalf("seal: %q %q %v", seal, kid, err)
	}
	if err := k.VerifyPaymentRun(in, kid, seal); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	tampered := []func(*statement.RunSealInput){
		func(x *statement.RunSealInput) { x.RunID = uuid.New() },
		func(x *statement.RunSealInput) { x.TenantID = uuid.New() },
		func(x *statement.RunSealInput) { x.ProviderID = "other" },
		func(x *statement.RunSealInput) { x.ImportID = uuid.New() },
		func(x *statement.RunSealInput) { x.CoverageEnd = x.CoverageEnd.Add(time.Microsecond) },
		func(x *statement.RunSealInput) { x.WindowTruncated = false },
		func(x *statement.RunSealInput) { x.RunAt = time.Now() },
	}
	for i, m := range tampered {
		x := in
		m(&x)
		if err := k.VerifyPaymentRun(x, kid, seal); !errors.Is(err, ErrRunSealInvalid) {
			t.Fatalf("tamper %d verified: %v", i, err)
		}
	}
	if err := k.VerifyPaymentRun(in, "unknown", seal); !errors.Is(err, ErrRunSealInvalid) {
		t.Fatalf("unknown kid: %v", err)
	}
	if err := k.VerifyPaymentRun(in, kid, ""); !errors.Is(err, ErrRunSealInvalid) {
		t.Fatalf("empty seal: %v", err)
	}
	var nilKeys *Keys
	if err := nilKeys.VerifyPaymentRun(in, kid, seal); !errors.Is(err, ErrRunSealInvalid) {
		t.Fatalf("nil keys: %v", err)
	}
	if _, _, err := nilKeys.SealPaymentRun(in); err == nil {
		t.Fatal("nil keys sealed")
	}
	// Other keys never verify.
	if err := newTestKeys(t).VerifyPaymentRun(in, kid, seal); !errors.Is(err, ErrRunSealInvalid) {
		t.Fatalf("other keys: %v", err)
	}
}

// The run subkey is independent of the import and instrument subkeys (distinct HKDF labels).
func TestRunSeal_KeySeparation(t *testing.T) {
	k := newTestKeys(t)
	in := runSealFixture()
	seal, kid, _ := k.SealPaymentRun(in)
	if macHex(k.imp[kid], statement.RunCanon(in)) == seal {
		t.Fatal("run and import seals share a subkey")
	}
	if instr, _ := k.sealMAC(kid, statement.RunCanon(in)); instr == seal {
		t.Fatal("run and instrument seals share a subkey")
	}
}

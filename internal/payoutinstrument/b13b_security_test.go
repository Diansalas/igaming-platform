package payoutinstrument

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Security L-4: a non-Synthetic payout adapter requires the NULL-arm-closing migration (the guard source names PI046).
func TestVerifyBindingGuardApplied(t *testing.T) {
	ctx := context.Background()
	guard := func(src string, err error) BindingGuardChecker {
		return func(context.Context) (string, error) { return src, err }
	}
	cases := []struct {
		name    string
		regs    Registrations
		check   BindingGuardChecker
		wantErr bool
	}{
		{"synthetic only: not checked even with the old guard", Registrations{PaymentAdapters: []any{syntheticAdapter{}}}, guard("RETURN NEW;", nil), false},
		{"nothing registered", Registrations{}, guard("", errors.New("must not be called")), false},
		{"non-Synthetic + old guard (0123 only)", Registrations{PaymentAdapters: []any{realAdapter{}}}, guard("IF NEW.payout_instrument_id IS NULL THEN RETURN NEW;", nil), true},
		{"non-Synthetic + 0126 guard", Registrations{PaymentAdapters: []any{syntheticAdapter{}, realAdapter{}}}, guard("... USING ERRCODE = 'PI046'; ...", nil), false},
		{"non-Synthetic + unreadable guard", Registrations{PaymentAdapters: []any{realAdapter{}}}, guard("", errors.New("db down")), true},
		{"non-Synthetic verifier only is not an adapter", Registrations{Verifiers: []PayoutInstrumentVerifier{realVerifier{src: SourcePSPAccount}}}, guard("old", nil), false},
	}
	for _, c := range cases {
		err := VerifyBindingGuardApplied(ctx, c.regs, c.check)
		if (err != nil) != c.wantErr || (err != nil && !errors.Is(err, ErrStartupGate)) {
			t.Errorf("%s: err=%v wantErr=%v", c.name, err, c.wantErr)
		}
	}
}

// Info finding: GateResult carries the decrypted destination and must redact under every rendering path.
func TestGateResultRedactsDetail(t *testing.T) {
	secret := "GB82WEST12345698765432"
	g := GateResult{Instrument: Instrument{ID: uuid.New(), State: StateVerified}, Detail: []byte(`{"iban":"` + secret + `"}`)}
	renders := []string{g.String(), g.GoString(), fmt.Sprint(g), fmt.Sprintf("%v %+v %#v %s %q", g, g, g, g, g)}
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	renders = append(renders, string(b))
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "gate", g)
	renders = append(renders, buf.String())
	for _, r := range renders {
		if strings.Contains(r, secret) || strings.Contains(r, "iban") {
			t.Fatalf("GateResult leaked the detail: %s", r)
		}
	}
	if !strings.Contains(g.String(), "REDACTED") {
		t.Fatal("expected a redaction marker")
	}
}

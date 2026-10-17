package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// F2 / LF-A11-2: a declared settle lag outside [0, statement.MaxStatementSettleLag] is a startup refusal, in every
// environment (all-or-nothing, like the other registry refusals).
type laggedReal struct {
	prodReal
	lag time.Duration
}

func (l laggedReal) StatementSettleLag() time.Duration { return l.lag }

func TestF2_Registry_RefusesAnInvalidDeclaredSettleLag(t *testing.T) {
	for _, env := range []string{"development", "production"} {
		ok := laggedReal{prodReal{attestedReal{realStmt{id: "psp-lag"}}}, 30 * time.Minute}
		if _, err := newPaymentStatementRegistry(env, []statement.PaymentStatementSource{ok}); err != nil {
			t.Fatalf("%s: a valid declared lag must be admitted: %v", env, err)
		}
		for _, lag := range []time.Duration{-time.Nanosecond, statement.MaxStatementSettleLag + time.Nanosecond} {
			bad := laggedReal{prodReal{attestedReal{realStmt{id: "psp-lag"}}}, lag}
			if _, err := newPaymentStatementRegistry(env, []statement.PaymentStatementSource{bad}); err == nil || !strings.Contains(err.Error(), "settle lag") {
				t.Fatalf("%s: lag %s must be refused at startup: %v", env, lag, err)
			}
		}
	}
}

// F2 / SR11-10: run() refuses a production start whose registered statement sources could not have their cursor
// verified (no import keys), right after the import sealer is installed. Static pin of the call and its order.
func TestF2_Run_VerifiesCursorSealingAfterInstallingTheSealer(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	seal := strings.Index(body, "reconciliation.SetDefaultImportSealer(payoutInstruments.Keys())")
	gate := strings.Index(body, `reconciliation.VerifyPaymentCursorSealing(cfg.GuardEnvironment() == "production", len(payRegistry.Providers()))`)
	loop := strings.Index(body, "reconciliation.RunSchedulerLoopScoped(")
	if seal < 0 || gate < 0 || loop < 0 || seal >= gate || gate >= loop {
		t.Fatalf("run() must install the sealer, then VerifyPaymentCursorSealing, before the scoped loop starts (seal=%d gate=%d loop=%d)", seal, gate, loop)
	}
}

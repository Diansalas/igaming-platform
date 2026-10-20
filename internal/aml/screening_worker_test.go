package aml

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestPlanFinish_OnlyACompletedClearIsAClear(t *testing.T) {
	cfg := DefaultScreeningWorkerConfig()
	if p := planFinish(OutcomeClear, nil, "", 1, cfg); p.Result != "clear" || p.Reason != "" {
		t.Fatalf("clear: %+v", p)
	}
	// Every non-clear, non-match value (an error, an empty value, an unknown value) is a failure: never "clear".
	for _, o := range []ScreeningOutcome{OutcomeError, "", "ok", "CLEAR", "approved", "unknown"} {
		p := planFinish(o, nil, "", 1, cfg)
		if p.Result == "clear" || p.Result == "potential_match" || p.Result == "confirmed" {
			t.Errorf("outcome %q must not become %q", o, p.Result)
		}
		if p.Result != "retry" || p.ErrorCode != ErrCodeUnknown {
			t.Errorf("outcome %q: %+v, want a retry with the closed unknown code", o, p)
		}
	}
	// A match is never cleared and always needs an officer (with an alert), with its lists.
	for _, o := range []ScreeningOutcome{OutcomePotentialMatch, OutcomeConfirmed} {
		p := planFinish(o, []ListType{ListSanctions}, "", 1, cfg)
		if p.Result != string(o) || p.Reason != "match_review" || len(p.MatchLists) != 1 {
			t.Errorf("%s: %+v", o, p)
		}
	}
}

func TestPlanFinish_RetryThenTerminalWithAlert(t *testing.T) {
	cfg := DefaultScreeningWorkerConfig()
	cfg.MaxAttempts = 3
	for attempts := 1; attempts < 3; attempts++ {
		p := planFinish(OutcomeError, nil, ErrCodeTimeout, attempts, cfg)
		if p.Result != "retry" || p.ErrorCode != ErrCodeTimeout || p.Backoff <= 0 || p.Reason != "" {
			t.Errorf("attempt %d: %+v", attempts, p)
		}
	}
	p := planFinish(OutcomeError, nil, ErrCodeUnavailable, 3, cfg)
	if p.Result != "terminal" || p.Reason != "failed_terminal" || p.ErrorCode != ErrCodeUnavailable {
		t.Errorf("budget exhausted: %+v", p)
	}
}

func TestBackoff_ExponentialAndCapped(t *testing.T) {
	cfg := ScreeningWorkerConfig{BackoffBase: 10 * time.Second, BackoffCap: time.Minute}
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute}
	for i, w := range want {
		if got := backoffFor(i+1, cfg); got != w {
			t.Errorf("attempt %d: %v, want %v", i+1, got, w)
		}
	}
	if got := backoffFor(0, cfg); got != 10*time.Second {
		t.Errorf("attempt 0: %v", got)
	}
	if got := backoffFor(10_000, cfg); got != time.Minute {
		t.Errorf("a huge attempt count must be capped, got %v", got)
	}
}

func TestScreeningWorker_ValidateForLoop(t *testing.T) {
	good := func() *ScreeningWorker {
		return &ScreeningWorker{Pool: nil, Provider: NewMockScreeningProvider(), Keys: MockSubjectKeys{}, Config: DefaultScreeningWorkerConfig()}
	}
	w := good()
	if err := w.ValidateForLoop(); err == nil || !strings.Contains(err.Error(), "no pool") {
		t.Fatalf("a worker with no pool must not start: %v", err)
	}
	var nilW *ScreeningWorker
	if nilW.ValidateForLoop() == nil {
		t.Fatal("a nil worker must not start")
	}
	// The default configuration is valid apart from the pool (checked via the config rules directly).
	cfg := DefaultScreeningWorkerConfig()
	if cfg.MaxAttempts < 1 || cfg.Lease < 2*(2*cfg.PhaseTimeout+cfg.CallTimeout) {
		t.Fatalf("the default lease must cover the phases: %+v", cfg)
	}
}

// Static pins of the worker's boundaries (the behaviour is proven by the integration tests).
func TestScreeningWorker_Static_Boundaries(t *testing.T) {
	b, err := os.ReadFile("screening_worker.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	code := regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(src, "")
	// The worker never places or lifts a hold, never records an officer result and never calls the officer orchestrator store.
	for _, bad := range []string{"aml_hold_place", "aml_hold_lift", "aml_screening_record", "RecordScreening", "PlaceHold", "OpenCase", "aml_screening_results", "aml_holds"} {
		if strings.Contains(code, bad) {
			t.Errorf("the worker must not touch %q: it cannot place a hold or read a result", bad)
		}
	}
	// aml may not import kyc (kyc imports aml); the vendor subject reference is a tenant-predicated SQL read.
	if strings.Contains(src, "internal/kyc") {
		t.Error("the worker must not import internal/kyc")
	}
	if !strings.Contains(code, "WHERE tenant_id = $1 AND person_id = $2") {
		t.Error("the vendor subject reference read must carry the tenant predicate")
	}
	// The provider is called outside any transaction: the call sits between the phase A and phase C transactions, on the pass
	// context. It must not be inside a WithTenant / InTx closure.
	call := strings.Index(code, "o.callProvider(ctx, req)")
	phaseA := strings.Index(code, "// Phase A")
	_ = phaseA
	a := strings.LastIndex(code[:call], "w.Pool.WithTenant(")
	endA := strings.Index(code[a:], "\n\t})\n")
	if call < 0 || a < 0 || endA < 0 || a+endA > call {
		t.Error("the provider call must come after the phase A transaction closure has ended")
	}
	c := strings.Index(code, "alerting.InTx(")
	if c < call {
		t.Error("phase C (the finish transaction) must come after the provider call")
	}
	// No identifier reaches a log line: log calls carry only fixed text.
	for _, l := range regexp.MustCompile(`w\.logErr\([^)]*\)`).FindAllString(code, -1) {
		if !regexp.MustCompile(`^w\.logErr\("[a-zA-Z0-9 ]+"\)$`).MatchString(l) {
			t.Errorf("a log call with a non-literal argument: %s", l)
		}
	}
	// The attention alert carries the request id, a closed reason and the closed trigger only.
	if strings.Contains(code, "person") && strings.Contains(code[strings.Index(code, "KindAMLScreeningAttention"):], "PersonID.String()") {
		t.Error("the alert must not carry the person id")
	}
}

func TestSetTriggerPolicyInput_Validate(t *testing.T) {
	base := SetTriggerPolicyInput{JurisdictionID: [16]byte{1}, Events: []TriggerEvent{EventRegistration}, GateWithdrawal: true,
		EffectiveFrom: time.Now(), LegalReviewReference: "REF-1", ReasonCode: "policy_review"}
	if ev, err := base.validate(); err != nil || len(ev) != 1 {
		t.Fatalf("valid input refused: %v", err)
	}
	dup := base
	dup.Events = []TriggerEvent{EventScheduled, EventRegistration, EventScheduled}
	if ev, err := dup.validate(); err != nil || strings.Join(ev, ",") != "registration,scheduled" {
		t.Fatalf("events must be a sorted set: %v %v", ev, err)
	}
	bad := map[string]func(i *SetTriggerPolicyInput){
		"no jurisdiction":                    func(i *SetTriggerPolicyInput) { i.JurisdictionID = [16]byte{} },
		"unknown event":                      func(i *SetTriggerPolicyInput) { i.Events = []TriggerEvent{"first_deposit"} },
		"withdrawal_request is not an event": func(i *SetTriggerPolicyInput) { i.Events = []TriggerEvent{"withdrawal_request"} },
		"gate with no events":                func(i *SetTriggerPolicyInput) { i.Events = nil },
		"no effective time":                  func(i *SetTriggerPolicyInput) { i.EffectiveFrom = time.Time{} },
		"no legal reference":                 func(i *SetTriggerPolicyInput) { i.LegalReviewReference = "" },
		"padded legal reference":             func(i *SetTriggerPolicyInput) { i.LegalReviewReference = " x " },
		"control char":                       func(i *SetTriggerPolicyInput) { i.LegalReviewReference = "a\x00" },
		"bad reason":                         func(i *SetTriggerPolicyInput) { i.ReasonCode = "Not A Code" },
	}
	for name, mut := range bad {
		in := base
		mut(&in)
		if _, err := in.validate(); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	// No events and no gate is a legal explicit "nothing triggers" version.
	none := base
	none.Events, none.GateWithdrawal = nil, false
	if _, err := none.validate(); err != nil {
		t.Errorf("an explicit empty, ungated policy must be valid: %v", err)
	}
}

package actorproof

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/stepup"
)

// ADR 0119 S4b section 9.1: the signer backstop.

func TestStepUpOperations_RegistryPinned(t *testing.T) {
	want := map[string]stepup.Class{
		"ledger_adjustment:initiate": stepup.Financial, "ledger_adjustment:approve": stepup.Financial,
		"payment_force_resolve:request": stepup.Financial, "payment_force_resolve:approve": stepup.Financial,
		"withdrawal_hold_resolution:request": stepup.Financial, "withdrawal_hold_resolution:approve": stepup.Financial,
		"capability_grant:request": stepup.Critical, "capability_grant:approve": stepup.Critical, "capability_grant:revoke": stepup.Critical,
		"financial_policy_change:propose": stepup.Critical, "financial_policy_change:approve": stepup.Critical,
		"launch_authorisation:request": stepup.Critical, "launch_authorisation:approve": stepup.Critical,
		"rg_limit_config:request": stepup.Critical, "rg_limit_config:approve": stepup.Critical, "rg_limit_config:apply": stepup.Critical,
		// ADR 0119 S5 + S6 (S4b-R11 closed).
		"staff_lifecycle:request": stepup.Critical, "staff_lifecycle:approve": stepup.Critical,
		// ADR 0116 A7: AML hold lift and the proposal / approval halves of an AML four-eyes action.
		"aml_hold:lift": stepup.Critical, "aml_case:propose": stepup.Critical, "aml_case:approve": stepup.Critical,
		"aml_screening_trigger_policy:set": stepup.Critical,
		// ADR 0110 T8 (migration 0165): each half of a four-eyes payout reversal.
		"withdrawal_payout_reversal:decide": stepup.Critical,
		// Migration 0177 (MKT-DUAL-1): filing, approving and executing a licensing / jurisdiction change.
		"licensing_change:request": stepup.Critical, "licensing_change:approve": stepup.Critical, "licensing_change:execute": stepup.Critical,
		// lic10 (migration 0181): the two approval halves of the residence determination maker-checker (the policy filing is protective).
		"residence_policy:approve": stepup.Critical, "residence_determination:approve": stepup.Critical,
		// lic11 (migration 0182): filing, approving and executing a withdrawal restriction policy change.
		"withdrawal_restriction_policy:request": stepup.Critical, "withdrawal_restriction_policy:approve": stepup.Critical,
		"withdrawal_restriction_policy:execute": stepup.Critical,
	}
	if len(StepUpOperations) != len(want) {
		t.Fatalf("registry has %d operations, ADR 0119 section 9.1 pins %d", len(StepUpOperations), len(want))
	}
	for op, c := range want {
		if StepUpOperations[op] != c {
			t.Errorf("%s: class %q, want %q", op, StepUpOperations[op], c)
		}
	}
	// The protective operations are never in the registry (section 8.1 / 9.1 exemptions).
	for _, op := range []string{OpAdjustmentReject, OpAdjustmentCancel, OpResolutionReject, OpResolutionCancel, OpHoldResolutionReject,
		OpHoldResolutionCancel, OpGrantReject, OpGrantCancel, OpPolicyReject, OpPolicyCancel, OpLaunchSuspend, OpLaunchCancel,
		OpLaunchReject, OpRGConfigReject, OpRGConfigCancel, OpStaffLifecycleReject, OpStaffLifecycleCancel,
		OpLicensingChangeReject, OpLicensingChangeCancel, OpResidencePolicySet, OpWithdrawalRestrictionReject, OpWithdrawalRestrictionCancel} {
		if _, ok := StepUpOperations[op]; ok {
			t.Errorf("%s is protective and must not require step-up", op)
		}
	}
}

func TestCheckStepUp_Matrix(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	actor := uuid.New()
	with := func(subject uuid.UUID, age time.Duration) context.Context {
		return stepup.ContextWith(context.Background(), stepup.Assertion{Subject: subject, SessionID: uuid.New(), MFAAt: now.Add(-age), Class: stepup.Critical})
	}
	crit := Claims{Actor: actor, Scope: ScopePlatform, Operation: OpGrantApprove, Target: uuid.NewString(), PayloadHash: strings.Repeat("a", 64)}
	fin := Claims{Actor: actor, Scope: ScopeTenant, Tenant: uuid.New(), Operation: OpAdjustmentApprove, Target: uuid.NewString(), PayloadHash: strings.Repeat("a", 64)}
	for _, c := range []struct {
		name string
		ctx  context.Context
		cl   Claims
		ok   bool
	}{
		{"critical, no assertion", context.Background(), crit, false},
		{"critical, fresh", with(actor, time.Minute), crit, true},
		{"critical, at the 5 min bound + grace", with(actor, 5*time.Minute+StepUpInFlightGrace), crit, true},
		{"critical, just past bound + grace", with(actor, 5*time.Minute+StepUpInFlightGrace+time.Second), crit, false},
		{"critical, 7 min (a financial window)", with(actor, 7*time.Minute), crit, false},
		{"critical, another subject", with(uuid.New(), 0), crit, false},
		{"critical, future beyond skew", with(actor, -time.Minute), crit, false},
		{"financial, 10 min + grace", with(actor, 10*time.Minute+StepUpInFlightGrace), fin, true},
		{"financial, past", with(actor, 10*time.Minute+StepUpInFlightGrace+time.Second), fin, false},
		{"reject needs nothing", context.Background(), Claims{Actor: actor, Scope: ScopeTenant, Tenant: uuid.New(), Operation: OpAdjustmentReject}, true},
		{"launch suspend needs nothing", context.Background(), Claims{Actor: actor, Scope: ScopeTenant, Tenant: uuid.New(), Operation: OpLaunchSuspend}, true},
		{"tenant-scope policy propose (tighten-only) exempt", context.Background(), Claims{Actor: actor, Scope: ScopeTenant, Tenant: uuid.New(), Operation: OpPolicyPropose}, true},
		{"platform-scope policy propose (author) required", context.Background(), Claims{Actor: actor, Scope: ScopePlatform, Operation: OpPolicyPropose}, false},
		{"staff-marked request, no assertion", stepup.WithStaffRequest(context.Background()), crit, false},
	} {
		err := checkStepUp(c.ctx, c.cl, now)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrStepUpMissing)) {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

// Attach refuses BEFORE touching the transaction (a nil tx would panic if it were reached); Sign (no context, tests
// only) is unaffected.
func TestAttach_BackstopRefusesBeforeAnyWrite(t *testing.T) {
	i := newTestIssuer(t)
	c := Claims{Actor: uuid.New(), Scope: ScopePlatform, Operation: OpGrantApprove, Target: uuid.NewString(), PayloadHash: strings.Repeat("a", 64)}
	if err := i.Attach(context.Background(), nil, c); !errors.Is(err, ErrStepUpMissing) {
		t.Fatalf("Attach without a step-up: %v", err)
	}
	if _, err := i.Sign(c); err != nil {
		t.Fatalf("Sign is the context-free primitive: %v", err)
	}
	var nilIssuer *Issuer
	if err := nilIssuer.Attach(context.Background(), nil, c); !errors.Is(err, ErrNoIssuer) {
		t.Fatalf("nil issuer: %v", err)
	}
	bad := c
	bad.PayloadHash = "x"
	if err := i.Attach(stepup.ContextWith(context.Background(), stepup.Assertion{Subject: c.Actor, SessionID: uuid.New(), MFAAt: time.Now()}), nil, bad); !errors.Is(err, ErrInvalidClaims) {
		t.Fatalf("invalid claims: %v", err)
	}
	if testStepUpFallbackOn() {
		t.Fatal("the integration-test fallback must be off in a plain (non-integration) test binary")
	}
}

// Static pins: (1) production code signs ONLY through Attach (the backstop): no non-test file outside this package
// calls Sign on an issuer; (2) the integration-only fallback is set ONLY by testsupport_integration.go (build tag
// integration) and turned on ONLY by prooftest; (3) stepup.ContextWith is called only by internal/auth (the gate's
// re-export) and prooftest.
func TestStatic_StepUpBackstopPins(t *testing.T) {
	root := filepath.Join("..", "..")
	signCall := regexp.MustCompile(`\b(iss|issuer|i|p|proofs|Proofs|Default\(\))\.Sign\(`)
	var signers, fallbackSetters, fallbackStores, contextWith []string
	err := filepath.Walk(filepath.Join(root, "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(b)
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		test := strings.HasSuffix(path, "_test.go")
		if !test && !strings.HasPrefix(rel, "internal/actorproof/") && strings.Contains(src, "actorproof") && signCall.MatchString(src) {
			signers = append(signers, rel)
		}
		if !test && strings.Contains(src, "SetTestStepUpFallback(") {
			fallbackSetters = append(fallbackSetters, rel)
		}
		if !test && strings.Contains(src, "testStepUpFallback.Store(") {
			fallbackStores = append(fallbackStores, rel)
		}
		if !test && strings.Contains(src, "stepup.ContextWith(") {
			contextWith = append(contextWith, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(signers) != 0 {
		t.Errorf("production code signing outside Attach (no backstop): %v", signers)
	}
	sort.Strings(fallbackSetters)
	if strings.Join(fallbackSetters, ",") != "internal/actorproof/prooftest/stepup.go,internal/actorproof/testsupport_integration.go" {
		t.Errorf("SetTestStepUpFallback references: %v", fallbackSetters)
	}
	if strings.Join(fallbackStores, ",") != "internal/actorproof/testsupport_integration.go" {
		t.Errorf("testStepUpFallback.Store references: %v", fallbackStores)
	}
	for _, f := range []string{"internal/actorproof/testsupport_integration.go", "internal/actorproof/prooftest/stepup.go"} {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil || !strings.HasPrefix(string(b), "//go:build integration\n") {
			t.Errorf("%s must carry the integration build tag", f)
		}
	}
	// The production half of the split: !integration, and a constant false (no variable to flip).
	prod, err := os.ReadFile(filepath.Join(root, "internal", "actorproof", "stepup_fallback_prod.go"))
	if err != nil || !strings.HasPrefix(string(prod), "//go:build !integration\n") ||
		!strings.Contains(string(prod), "func testStepUpFallbackOn() bool { return false }") || strings.Contains(string(prod), "var ") {
		t.Errorf("stepup_fallback_prod.go must be !integration and return the constant false (err %v)", err)
	}
	sort.Strings(contextWith)
	if strings.Join(contextWith, ",") != "internal/actorproof/prooftest/prooftest.go,internal/auth/stepup.go" {
		t.Errorf("stepup.ContextWith callers: %v", contextWith)
	}
}

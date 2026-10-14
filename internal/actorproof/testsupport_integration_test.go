//go:build integration

package actorproof

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/stepup"
)

// ADR 0119 S4b: the integration-only fallback applies to a context that never passed the HTTP staff chain, and NEVER
// to a staff HTTP request (marked by auth.Middleware): those always meet the real backstop.
func TestTestStepUpFallback_NeverAppliesToAStaffRequest(t *testing.T) {
	SetTestStepUpFallback(true)
	defer SetTestStepUpFallback(false)
	c := Claims{Actor: uuid.New(), Scope: ScopePlatform, Operation: OpGrantApprove, Target: uuid.NewString(), PayloadHash: strings.Repeat("a", 64)}
	now := time.Now()
	if err := checkStepUp(context.Background(), c, now); err != nil {
		t.Fatalf("a direct domain call (no HTTP chain) under the fallback: %v", err)
	}
	if err := checkStepUp(stepup.WithStaffRequest(context.Background()), c, now); !errors.Is(err, ErrStepUpMissing) {
		t.Fatalf("a staff HTTP request must meet the real backstop: %v", err)
	}
	// An assertion that is present but wrong is never rescued by the fallback.
	wrong := stepup.ContextWith(context.Background(), stepup.Assertion{Subject: uuid.New(), SessionID: uuid.New(), MFAAt: now})
	if err := checkStepUp(wrong, c, now); !errors.Is(err, ErrStepUpMissing) {
		t.Fatalf("a wrong assertion: %v", err)
	}
}

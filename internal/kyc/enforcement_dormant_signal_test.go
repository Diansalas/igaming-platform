package kyc

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// G9: a default-allow (no licence / no active policy) is signalled once per
// process per (tenant, operation, reason), and never changes the decision.
func TestSignalKYCDormant_LogsOncePerKey(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	tenant := uuid.New()
	signalKYCDormant(tenant, EnforcementDeposit, "no_licence_bound")
	signalKYCDormant(tenant, EnforcementDeposit, "no_licence_bound")
	signalKYCDormant(tenant, EnforcementDeposit, "no_active_policy")
	if n := strings.Count(buf.String(), "kyc_enforcement_dormant"); n != 2 {
		t.Fatalf("want 2 signals (one per distinct reason), got %d:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), tenant.String()) {
		t.Fatal("signal must name the tenant")
	}
}

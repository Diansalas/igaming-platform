package payments

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/txscope"
)

// PRH-I5 (ADR 0095 §12.4): the MOCK payment statement source.
//
// MOCK: labeled per CLAUDE.md's "No fake completion" rule. It renders the
// MockProvider's OWN per-tenant records - what the synthetic provider
// itself believes happened - and never reads the platform database. That
// makes it genuinely non-tautological against the platform: a lost
// callback (the mock resolved a success the platform never heard about), a
// provider-confirmed amount that differs (SetConfirmedAmount), or a
// platform posting with no provider record all diverge. It is still a
// synthetic double: in-process, single-replica (a second replica has its
// own MockProvider and its own records), reset on restart (the coverage
// window starts at the MockProvider's construction time), and it lists no
// reversals (the mock never originates one; reversal callbacks are
// test-built). It implements providerkind.Synthetic, so the startup guard
// refuses it in production.
//
// Records are tenant-tagged only when the call came through the payments
// provider-call gate (gate.go), which carries the committed attempt's
// tenant. A record from a direct, gate-less call (the legacy deposit path
// before the deposit cutover) is untagged and appears on no tenant's
// statement: it is never attributed to a tenant by guesswork.

// MockStatementSourceLabel is the MOCK source's label (recorded on every
// import, run audit and log line).
const MockStatementSourceLabel = "MOCK in-process payment provider statement - renders the MockProvider's own records, not the platform DB; single-process; non-production"

// ErrStatementFetchUnderTx is returned when Fetch is called while a
// pooled database transaction is held (INV-IO-1).
var ErrStatementFetchUnderTx = errors.New("payments: statement fetch refused: a database transaction is held")

// MockStatementSource implements statement.PaymentStatementSource over one
// MockProvider.
type MockStatementSource struct {
	provider *MockProvider
	resolver OutboundCredentialResolver
	// maxLines is the streaming line cap (0 = statement.MaxPaymentStatementLines).
	maxLines int
}

// WithMaxLines returns a copy of s with a smaller streaming line cap (tests
// only; the default is statement.MaxPaymentStatementLines).
func (s *MockStatementSource) WithMaxLines(n int) *MockStatementSource {
	c := *s
	c.maxLines = n
	return &c
}

// NewMockStatementSource binds the source to provider (the same instance
// the payments orchestrator calls) and to the outbound credential resolver
// the fetch goes through (MockCredentialResolver for the MOCK).
func NewMockStatementSource(provider *MockProvider, resolver OutboundCredentialResolver) *MockStatementSource {
	return &MockStatementSource{provider: provider, resolver: resolver}
}

// SyntheticComponent implements providerkind.Synthetic (MOCK-ADAPTER-PROD-1).
func (s *MockStatementSource) SyntheticComponent() {}

// Label implements statement.PaymentStatementSource.
func (s *MockStatementSource) Label() string { return MockStatementSourceLabel }

// ProviderID implements statement.PaymentStatementSource.
func (s *MockStatementSource) ProviderID() string {
	if s == nil || s.provider == nil {
		return ""
	}
	return s.provider.providerID
}

// Fetch implements statement.PaymentStatementSource. It refuses under a
// held transaction and goes through the provider-call gate as a read-only
// call, so the tenant's outbound credential is resolved and its binding
// checked exactly as for QueryStatus (ADR 0095 §3.2, §11, S95-C11).
func (s *MockStatementSource) Fetch(ctx context.Context, req statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
	if txscope.Held(ctx) {
		return statement.PaymentStatement{}, ErrStatementFetchUnderTx
	}
	if s == nil || s.provider == nil || s.resolver == nil {
		return statement.PaymentStatement{}, errors.New("payments: mock statement source is not configured")
	}
	if req.ProviderID != s.provider.providerID {
		return statement.PaymentStatement{}, fmt.Errorf("payments: mock statement source for %q asked for provider %q", s.provider.providerID, req.ProviderID)
	}
	// capErr keeps a line-cap sentinel intact: the gate redacts adapter
	// errors, and the stream must see ErrPaymentStatementTooManyLines.
	var capErr error
	gr := callProvider(ctx, s.resolver, callProviderInput{
		TenantID: req.TenantID, ProviderID: req.ProviderID, ReadOnly: true, Domain: "payments",
	}, func(_ context.Context, cc CallContext) (statement.PaymentStatement, ErrorClass, error) {
		out, err := s.provider.statementFor(cc, s.maxLines)
		capErr = err
		return out, ErrorClassSucceeded, nil
	})
	if gr.Err != nil {
		return statement.PaymentStatement{}, gr.Err
	}
	if capErr != nil {
		return statement.PaymentStatement{}, capErr
	}
	return gr.Value, nil
}

// statementFor renders the provider's own records for cc's tenant,
// sorted by provider reference (deterministic), through a
// statement.PaymentLineCollector (security C1: the same streaming line cap
// a real source must apply; the MOCK has no wire body to byte-limit).
func (m *MockProvider) statementFor(cc CallContext, maxLines int) (statement.PaymentStatement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := statement.PaymentStatement{CoverageStart: m.createdAt, CoverageEnd: time.Now().UTC()}
	if !out.CoverageEnd.After(out.CoverageStart) {
		out.CoverageEnd = out.CoverageStart.Add(time.Microsecond)
	}
	if cc.ProviderID != m.providerID {
		return out, nil
	}
	lines := statement.NewPaymentLineCollector(maxLines)
	for ref, a := range m.attempts {
		if !a.tenantTagged || a.tenantID != cc.TenantID {
			continue
		}
		kind := statement.PaymentLineDeposit
		if a.kind == "withdraw" {
			kind = statement.PaymentLinePayout
		}
		status := statement.PaymentStatusPending
		switch a.outcome {
		case OutcomeSucceeded:
			status = statement.PaymentStatusSucceeded
		case OutcomeDeclined:
			status = statement.PaymentStatusDeclined
		}
		if err := lines.Add(statement.PaymentStatementLine{
			ProviderID: m.providerID, ProviderReference: ref, MerchantReference: a.merchantReference,
			Kind: kind, Status: status, Amount: a.amount, AssetCode: a.assetCode, OccurredAt: a.createdAt,
		}); err != nil {
			return statement.PaymentStatement{}, err
		}
	}
	out.Lines = lines.Lines()
	sort.Slice(out.Lines, func(i, j int) bool { return out.Lines[i].ProviderReference < out.Lines[j].ProviderReference })
	return out, nil
}

type callContextKey struct{}

// withCallContext / callContextFrom carry the gate's CallContext on ctx
// (gate.go's safeCall) so the MockProvider can tag its records with the
// calling tenant.
func withCallContext(ctx context.Context, cc CallContext) context.Context {
	return context.WithValue(ctx, callContextKey{}, cc)
}

func callContextFrom(ctx context.Context) (CallContext, bool) {
	if ctx == nil {
		return CallContext{}, false
	}
	cc, ok := ctx.Value(callContextKey{}).(CallContext)
	return cc, ok
}

package payments

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// d7FixtureContract is a TEST FIXTURE, not a provider: a fully declared contract that exists only so
// the checklist can be shown to PASS when everything is declared and to FAIL when exactly one item is
// removed. It models no vendor and is not registered anywhere (PROVIDER DEPENDENT: no real adapter
// exists; ADR 0111 section 25).
type d7FixtureContract struct {
	id, label   string
	synthetic   bool
	merchant    M4Declaration
	mapping     []M4StatusMapping
	unknown     M4UnknownStatusPolicy
	channel     M4Channel
	echo        M4DestinationEcho
	final       M4FinalStatus
	amountAsset M4AmountAsset
	reference   M4Reference
	complete    M4Completeness
	timeSem     M4TimeSemantics
}

func (c d7FixtureContract) ProviderID() string                          { return c.id }
func (c d7FixtureContract) Label() string                               { return c.label }
func (c d7FixtureContract) Synthetic() bool                             { return c.synthetic }
func (c d7FixtureContract) MerchantReferenceDeclaration() M4Declaration { return c.merchant }
func (c d7FixtureContract) StatusMapping() []M4StatusMapping            { return c.mapping }
func (c d7FixtureContract) UnknownStatusPolicy() M4UnknownStatusPolicy  { return c.unknown }
func (c d7FixtureContract) Channel() M4Channel                          { return c.channel }
func (c d7FixtureContract) DestinationEcho() M4DestinationEcho          { return c.echo }
func (c d7FixtureContract) FinalStatus() M4FinalStatus                  { return c.final }
func (c d7FixtureContract) AmountAsset() M4AmountAsset                  { return c.amountAsset }
func (c d7FixtureContract) Reference() M4Reference                      { return c.reference }
func (c d7FixtureContract) Completeness() M4Completeness                { return c.complete }
func (c d7FixtureContract) TimeSemantics() M4TimeSemantics              { return c.timeSem }

// d7MarkedFixture carries the providerkind.Synthetic marker while DECLARING Synthetic() == false.
type d7MarkedFixture struct{ d7FixtureContract }

func (d7MarkedFixture) SyntheticComponent() {}

func d7Conformant() d7FixtureContract {
	return d7FixtureContract{
		id: "fixture-psp", label: "TEST FIXTURE contract (no provider)",
		merchant: M4Declaration{Declared: true, Value: true},
		mapping: []M4StatusMapping{
			{"PAID", M4EventPaidOut, statement.PaymentStatusSucceeded},
			{"QUEUED", M4EventPending, statement.PaymentStatusPending},
			{"REFUSED", M4EventRejected, statement.PaymentStatusDeclined},
			{"BANK_RETURN", M4EventReturned, statement.PaymentStatusReversed},
			{"REVERSED", M4EventReversed, statement.PaymentStatusReversed},
			{"CHARGEBACK", M4EventChargebck, statement.PaymentStatusReversed},
		},
		unknown:     M4UnknownRejectImport,
		channel:     M4Channel{Authenticated: true, EndpointFromGovernedConfig: true, PSPOffersContentSignature: true, ContentSignatureVerified: true},
		echo:        M4DestinationEcho{Kind: M4EchoFingerprint, OnStatementLines: true},
		final:       M4FinalStatus{TerminalPaid: []string{"PAID"}, TerminalDeclined: []string{"REFUSED"}, PaidCanBeReversed: true, ReversalWindow: 30 * 24 * time.Hour, ReversalReportedOnStatement: true},
		amountAsset: M4AmountAsset{MinorUnits: true, ExponentFromAssetRegistry: true, AssetCodePerLine: true, AmountIsPayoutPrincipal: true},
		reference:   M4Reference{PSPIssued: true, StablePerPayout: true},
		complete:    M4Completeness{CoverageAuthoritative: true, PaginationComplete: true},
		timeSem:     M4TimeSemantics{OccurredAtIsProviderEventTime: true, LowerBound: M4LowerBoundFirstSend},
	}
}

func d7HasFinding(r M4SourceReport, item string, sev M4FindingSeverity) bool {
	for _, f := range r.Findings {
		if f.Item == item && f.Severity == sev {
			return true
		}
	}
	return false
}

// The checklist passes a fully declared contract with no Fail finding (and no
// finding at all), and the one thing it still cannot admit is M4 paid: the
// platform's statement-line model has no destination echo field.
func TestD7_SourceContract_FullDeclarationConforms_ButPaidStaysInadmissible(t *testing.T) {
	r := CheckM4SourceContract(d7Conformant())
	if len(r.Findings) != 0 || !r.Conformant || !r.NotPaidAdmissible {
		t.Fatalf("a fully declared contract must conform with no finding: %+v", r)
	}
	if r.PaidAdmissible {
		t.Fatal("M4 paid must stay inadmissible while the statement-line model cannot carry a destination echo")
	}
	if CheckM4SourceContract(nil).Conformant {
		t.Fatal("a nil contract conformed")
	}
}

// Each checklist item is effective: removing exactly one declaration yields exactly
// that item's finding, with the stated severity, and a Fail disqualifies the source.
func TestD7_SourceContract_EachItemIsEffective(t *testing.T) {
	type tc struct {
		name   string
		mutate func(*d7FixtureContract)
		item   string
		sev    M4FindingSeverity
	}
	drop := func(ev M4Event) func(*d7FixtureContract) {
		return func(c *d7FixtureContract) {
			var m []M4StatusMapping
			for _, e := range c.mapping {
				if e.Event != ev {
					m = append(m, e)
				}
			}
			c.mapping = m
			// keep the terminal declarations consistent so only the mapping item fires
			if ev == M4EventPaidOut {
				c.final.TerminalPaid = []string{"PAID"}
			}
		}
	}
	cases := []tc{
		{"no provider id", func(c *d7FixtureContract) { c.id = "" }, M4ItemIdentity, M4Fail},
		{"synthetic source", func(c *d7FixtureContract) { c.synthetic = true }, M4ItemIdentity, M4Fail},
		{"merchant reference undeclared", func(c *d7FixtureContract) { c.merchant = M4Declaration{} }, M4ItemMerchantRef, M4Fail},
		{"payout lines do not carry the merchant reference", func(c *d7FixtureContract) { c.merchant = M4Declaration{Declared: true} }, M4ItemMerchantRef, M4Fail},
		{"return of a payout unmapped", drop(M4EventReturned), M4ItemStatusMap, M4Fail},
		{"reversal of a payout unmapped", drop(M4EventReversed), M4ItemStatusMap, M4Fail},
		{"chargeback of a payout unmapped", drop(M4EventChargebck), M4ItemStatusMap, M4Fail},
		{"a return mapped as succeeded", func(c *d7FixtureContract) { c.mapping[3].Class = statement.PaymentStatusSucceeded }, M4ItemStatusMap, M4Fail},
		{"a chargeback mapped as declined", func(c *d7FixtureContract) { c.mapping[5].Class = statement.PaymentStatusDeclined }, M4ItemStatusMap, M4Fail},
		{"unknown class", func(c *d7FixtureContract) { c.mapping[0].Class = "paid" }, M4ItemStatusMap, M4Fail},
		{"duplicate provider status", func(c *d7FixtureContract) { c.mapping[1].ProviderStatus = "PAID" }, M4ItemStatusMap, M4Fail},
		{"unknown status treated as success", func(c *d7FixtureContract) { c.unknown = M4UnknownAsSucceeded }, M4ItemUnknown, M4Fail},
		{"unknown status policy undeclared", func(c *d7FixtureContract) { c.unknown = "" }, M4ItemUnknown, M4Fail},
		{"channel not authenticated", func(c *d7FixtureContract) { c.channel.Authenticated = false }, M4ItemChannel, M4Fail},
		{"endpoint not from governed config", func(c *d7FixtureContract) { c.channel.EndpointFromGovernedConfig = false }, M4ItemChannel, M4Fail},
		{"PSP signs, adapter does not verify", func(c *d7FixtureContract) { c.channel.ContentSignatureVerified = false }, M4ItemChannel, M4Fail},
		{"destination echo undeclared", func(c *d7FixtureContract) { c.echo = M4DestinationEcho{} }, M4ItemDestination, M4Fail},
		{"destination echo unknown kind", func(c *d7FixtureContract) { c.echo.Kind = "sometimes" }, M4ItemDestination, M4Fail},
		{"destination echo none", func(c *d7FixtureContract) { c.echo = M4DestinationEcho{Kind: M4EchoNone} }, M4ItemDestination, M4Limit},
		{"destination echo not on statement lines", func(c *d7FixtureContract) { c.echo.OnStatementLines = false }, M4ItemDestination, M4Limit},
		{"no terminal-paid status", func(c *d7FixtureContract) { c.final.TerminalPaid = nil }, M4ItemFinalStatus, M4Fail},
		{"terminal-paid not mapped to succeeded", func(c *d7FixtureContract) { c.final.TerminalPaid = []string{"QUEUED"} }, M4ItemFinalStatus, M4Fail},
		{"a succeeded status that is not terminal", func(c *d7FixtureContract) {
			c.mapping = append(c.mapping, M4StatusMapping{"SENT", M4EventPaidOut, statement.PaymentStatusSucceeded})
		}, M4ItemFinalStatus, M4Fail},
		{"reversible without a window", func(c *d7FixtureContract) { c.final.ReversalWindow = 0 }, M4ItemFinalStatus, M4Fail},
		{"late reversal not on the statement", func(c *d7FixtureContract) { c.final.ReversalReportedOnStatement = false }, M4ItemFinalStatus, M4Fail},
		{"amount not minor units", func(c *d7FixtureContract) { c.amountAsset.MinorUnits = false }, M4ItemAmountAsset, M4Fail},
		{"exponent not from the registry", func(c *d7FixtureContract) { c.amountAsset.ExponentFromAssetRegistry = false }, M4ItemAmountAsset, M4Fail},
		{"asset not per line", func(c *d7FixtureContract) { c.amountAsset.AssetCodePerLine = false }, M4ItemAmountAsset, M4Fail},
		{"amount net of fees", func(c *d7FixtureContract) { c.amountAsset.AmountIsPayoutPrincipal = false }, M4ItemAmountAsset, M4Fail},
		{"reference not PSP issued", func(c *d7FixtureContract) { c.reference.PSPIssued = false }, M4ItemReference, M4Fail},
		{"reference not stable", func(c *d7FixtureContract) { c.reference.StablePerPayout = false }, M4ItemReference, M4Fail},
		{"reference may echo the merchant reference", func(c *d7FixtureContract) { c.reference.EchoesMerchantReference = true }, M4ItemReference, M4Fail},
		{"occurred_at is not the provider event time", func(c *d7FixtureContract) { c.timeSem.OccurredAtIsProviderEventTime = false }, M4ItemTime, M4Fail},
		{"lower bound is not the first send", func(c *d7FixtureContract) { c.timeSem.LowerBound = "created_at" }, M4ItemTime, M4Fail},
		{"negative skew", func(c *d7FixtureContract) { c.timeSem.LowerBoundSkew = -time.Second }, M4ItemTime, M4Fail},
		{"skew above the bound", func(c *d7FixtureContract) { c.timeSem.LowerBoundSkew = M4MaxLowerBoundSkew + time.Second }, M4ItemTime, M4Fail},
		{"coverage not authoritative", func(c *d7FixtureContract) { c.complete.CoverageAuthoritative = false }, M4ItemCompleteness, M4Fail},
		{"pagination completeness not proven", func(c *d7FixtureContract) { c.complete.PaginationComplete = false }, M4ItemCompleteness, M4Fail},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := d7Conformant()
			f.mapping = append([]M4StatusMapping(nil), f.mapping...)
			c.mutate(&f)
			r := CheckM4SourceContract(f)
			if !d7HasFinding(r, c.item, c.sev) {
				t.Fatalf("want a %s finding on %s, got %+v", c.sev, c.item, r.Findings)
			}
			if c.sev == M4Fail && (r.Conformant || r.NotPaidAdmissible || r.PaidAdmissible) {
				t.Fatalf("a Fail finding must disqualify the source: %+v", r)
			}
			if c.sev == M4Limit && (!r.Conformant || r.PaidAdmissible) {
				t.Fatalf("a Limit finding keeps the source conformant but never admits paid: %+v", r)
			}
			for _, f := range r.Findings {
				if len(f.D7) == 0 || f.Detail == "" {
					t.Fatalf("every finding names the D-7 properties it protects and says why: %+v", f)
				}
			}
		})
	}
}

// The statement-line model cannot carry a destination echo today, so the checklist can never admit M4 paid.
// This pins m4StatementLineCarriesDestinationEcho to the struct: adding a destination field to
// statement.PaymentStatementLine without deliberately flipping the constant (and the SQL) fails here.
func TestD7_StatementLineHasNoDestinationField_PinsTheConstant(t *testing.T) {
	typ := reflect.TypeOf(statement.PaymentStatementLine{})
	has := false
	for i := 0; i < typ.NumField(); i++ {
		n := strings.ToLower(typ.Field(i).Name)
		if strings.Contains(n, "destination") || strings.Contains(n, "instrument") || strings.Contains(n, "beneficiary") || strings.Contains(n, "iban") {
			has = true
		}
	}
	if has != m4StatementLineCarriesDestinationEcho {
		t.Fatalf("statement line destination field present=%v but m4StatementLineCarriesDestinationEcho=%v: flip them together, with the SQL clause and the ADR 0111 section 25 matrix", has, m4StatementLineCarriesDestinationEcho)
	}
}

// The MOCK source is not an M4SourceContract and never can pass for non-MOCK: no value in the
// payments package implements the contract except the test fixture.
func TestD7_NoProductionTypeImplementsTheSourceContract(t *testing.T) {
	var ms any = &MockStatementSource{}
	if _, ok := ms.(M4SourceContract); ok {
		t.Fatal("the MOCK statement source implements M4SourceContract: MOCK must never be eligible for non-MOCK M4")
	}
}

// The Synthetic() declaration is not trusted alone: a contract object that carries the
// providerkind.Synthetic marker is refused even when it declares itself non-synthetic.
func TestD7_SourceContract_SyntheticMarkerDisqualifiesDespiteTheDeclaration(t *testing.T) {
	c := d7MarkedFixture{d7Conformant()}
	if c.Synthetic() {
		t.Fatal("setup: the fixture must declare Synthetic() == false")
	}
	r := CheckM4SourceContract(c)
	if r.Conformant || !d7HasFinding(r, M4ItemIdentity, M4Fail) {
		t.Fatalf("a marker-carrying contract conformed: %+v", r)
	}
	// A bounded skew is conformant.
	f := d7Conformant()
	f.timeSem.LowerBoundSkew = 2 * time.Minute
	if r := CheckM4SourceContract(f); !r.Conformant {
		t.Fatalf("a bounded skew must conform: %+v", r.Findings)
	}
}

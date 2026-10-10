// D-7 ADAPTER CONTRACT for a future non-MOCK payout statement source (ADR 0111
// section 25; owner decision 6 / D-7, ADR 0095 section 48).
//
// Status: CONTRACT AND CHECKLIST ONLY. PROVIDER DEPENDENT / gated. No provider
// implements M4SourceContract and none may be written until a commercial
// relationship exists. Non-MOCK M4 stays BLOCKED (ADR 0111 10.3): even a source
// that passes this checklist does not enable it - m4EligibilityRefusal refuses every
// M4 whose evidence rests on a non-MOCK import, and lifting that is a reviewed
// code change that also needs the T10 security implementation review and the S-3
// review of the real channel.
//
// What it is for. The database's verdict (payout_m4_evidence, migration 0125) is
// provider-independent: it reads persisted statement lines. Everything that
// decides whether a line MEANS what the verdict assumes it means is provider
// specific and lives in the adapter that builds the lines:
//
//   - the merchant-reference declaration (S-3): the verdict attributes a line to
//     an attempt by the platform-issued merchant reference, and may only call a
//     payout "not paid" from a source that declares it carries one;
//   - the status vocabulary: which provider status is pending, final-paid,
//     declined, or a return / reversal / chargeback of a payout;
//   - the authenticated channel and the PSP content signature (S-3);
//   - destination echo semantics (owner decision 5);
//   - final-status semantics and the post-paid reversal window;
//   - amount and asset fields;
//   - that the provider reference is PSP-issued, not an echo.
//
// A passing checklist is NECESSARY, NOT SUFFICIENT: C5 and C10 are self-declared and only the
// vendor-specific tests, the S-3 review and the T10 seal review establish them.
//
// A source's own tests must call CheckM4SourceContract on its declaration and
// require zero Fail findings. The checklist is deliberately about declarations
// the adapter must make explicitly, so that an adapter can never "pretend" a
// capability by omission (owner decision 5); the mapping of the declaration onto
// the real wire format is what the vendor-specific tests prove.
package payments

import (
	"fmt"
	"time"

	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// M4Event is the provider-independent meaning of a provider payout status.
type M4Event string

const (
	M4EventPaidOut   M4Event = "paid_out"   // the money left the PSP to the beneficiary: terminal paid
	M4EventPending   M4Event = "pending"    // accepted, not final
	M4EventRejected  M4Event = "rejected"   // refused before any money left: terminal declined
	M4EventReturned  M4Event = "returned"   // money came back after a paid-out (beneficiary bank return)
	M4EventReversed  M4Event = "reversed"   // the PSP reversed a paid-out
	M4EventChargebck M4Event = "chargeback" // a chargeback or recall of a payout
)

// M4StatusMapping maps one provider status string onto an event and the
// platform's statement status class (statement.PaymentStatus*).
type M4StatusMapping struct {
	ProviderStatus string
	Event          M4Event
	Class          string
}

// M4UnknownStatusPolicy is what the adapter does with a provider status it has
// no mapping for. There is no "treat as succeeded".
type M4UnknownStatusPolicy string

const (
	M4UnknownRejectImport M4UnknownStatusPolicy = "reject_import"
	M4UnknownAsPending    M4UnknownStatusPolicy = "treat_as_pending"
	M4UnknownAsSucceeded  M4UnknownStatusPolicy = "treat_as_succeeded" // never conformant
)

// M4Declaration is an explicit, tri-state declaration (absence is not "false").
type M4Declaration struct {
	Declared bool
	Value    bool
}

// M4Channel is the S-3 channel declaration.
type M4Channel struct {
	// Authenticated: the statement is fetched over a channel authenticated to the PSP
	// (TLS with the PSP identity verified, or mTLS), never an unauthenticated URL.
	Authenticated bool
	// EndpointFromGovernedConfig: endpoint and credentials come from the secret store or
	// governed configuration, never from statement content or a request.
	EndpointFromGovernedConfig bool
	// PSPOffersContentSignature: the PSP signs the statement body (or each line).
	PSPOffersContentSignature bool
	// ContentSignatureVerified: the adapter verifies that signature before building lines.
	ContentSignatureVerified bool
}

// M4DestinationEchoKind is the destination-echo capability (owner decision 5).
type M4DestinationEchoKind string

const (
	M4EchoUndeclared   M4DestinationEchoKind = ""
	M4EchoNone         M4DestinationEchoKind = "none"
	M4EchoFingerprint  M4DestinationEchoKind = "fingerprint"
	M4EchoFullInstrmnt M4DestinationEchoKind = "full_instrument"
)

// M4DestinationEcho declares the echo semantics of the statement.
type M4DestinationEcho struct {
	Kind M4DestinationEchoKind
	// OnStatementLines: the echo is present on the statement line of a payout (not only in
	// callbacks). Without it a statement line cannot establish the destination.
	OnStatementLines bool
}

// M4FinalStatus is the final-status semantics of the provider.
type M4FinalStatus struct {
	TerminalPaid      []string // provider statuses that are final-paid
	TerminalDeclined  []string // provider statuses that are final-declined
	PaidCanBeReversed bool
	// ReversalWindow bounds how long after a paid-out a return/reversal/chargeback can
	// still appear; required when PaidCanBeReversed.
	ReversalWindow time.Duration
	// ReversalReportedOnStatement: a late reversal appears as a statement line (so
	// reconciliation R-1 can raise it), not only by callback.
	ReversalReportedOnStatement bool
}

// M4AmountAsset is the amount/asset field semantics.
type M4AmountAsset struct {
	MinorUnits                bool // amounts are integer minor units (or converted exactly, never floating point)
	ExponentFromAssetRegistry bool // the per-asset exponent comes from the Asset registry
	AssetCodePerLine          bool // every line names its asset explicitly (never inferred from the account)
	AmountIsPayoutPrincipal   bool // the line amount is the payout amount, not net of fees or FX
}

// M4Reference is the provider-reference semantics.
type M4Reference struct {
	PSPIssued               bool // the reference is issued by the PSP for this payout
	StablePerPayout         bool // the same payout always reports the same reference on every status/line
	EchoesMerchantReference bool // true if the reference may equal the platform merchant reference (never conformant)
}

// M4Completeness is the T10 residual: authenticity/completeness of the window.
type M4Completeness struct {
	CoverageAuthoritative bool // the coverage window is vouched for by the PSP
	PaginationComplete    bool // the adapter proves it read every page of the window
}

// M4SourceContract is what a statement source must declare before it can be
// considered for non-MOCK M4. PROVIDER DEPENDENT: no implementation exists.
type M4SourceContract interface {
	ProviderID() string
	Label() string
	// Synthetic is true for a MOCK/double; a synthetic source is never eligible.
	Synthetic() bool
	MerchantReferenceDeclaration() M4Declaration
	StatusMapping() []M4StatusMapping
	UnknownStatusPolicy() M4UnknownStatusPolicy
	Channel() M4Channel
	DestinationEcho() M4DestinationEcho
	FinalStatus() M4FinalStatus
	AmountAsset() M4AmountAsset
	Reference() M4Reference
	Completeness() M4Completeness
	TimeSemantics() M4TimeSemantics
}

// M4MaxLowerBoundSkew bounds any per-source clock-skew tolerance a contract may declare.
const M4MaxLowerBoundSkew = 5 * time.Minute

// M4TimeSemantics declares how a line's occurred_at is read (ADR 0111 25.4 G-TIME).
type M4TimeSemantics struct {
	// OccurredAtIsProviderEventTime: occurred_at is the time the PSP paid/declined, not a
	// statement-generation or settlement-batch time.
	OccurredAtIsProviderEventTime bool
	// LowerBound must be M4LowerBoundFirstSend: a succeeded line must not predate the
	// attempt's first send (first_submitted_at, else last_sent_at). A line dated before
	// last_sent_at is NOT refused (an earlier send of a resent attempt may have paid).
	LowerBound string
	// LowerBoundSkew is a clock-skew tolerance applied ONLY to the lower bound: 0 (the
	// default, and the MOCK value) up to M4MaxLowerBoundSkew, ledger-finance reviewed per
	// source. Never applied to the upper bound.
	LowerBoundSkew time.Duration
}

// M4LowerBoundFirstSend is the only conformant lower bound.
const M4LowerBoundFirstSend = "first_send"

// M4FindingSeverity: a Fail disqualifies the source from every M4; a Limit
// removes one direction (M4 paid) but is not a defect.
type M4FindingSeverity string

const (
	M4Fail  M4FindingSeverity = "fail"
	M4Limit M4FindingSeverity = "limit"
)

// M4Finding is one checklist result. D7 names the D-7 properties it protects
// (ADR 0111 section 25 numbering 1-9).
type M4Finding struct {
	Item     string
	Severity M4FindingSeverity
	D7       []int
	Detail   string
}

// M4SourceReport is the outcome of the checklist.
type M4SourceReport struct {
	Findings []M4Finding
	// Conformant: no Fail finding. It is necessary and NOT sufficient for non-MOCK M4.
	Conformant bool
	// PaidAdmissible: conformant, the destination echo is on the statement lines AND the
	// platform's statement-line model can carry and compare it (it cannot today).
	PaidAdmissible bool
	// NotPaidAdmissible: conformant. Still gated by m4EligibilityRefusal.
	NotPaidAdmissible bool
}

// The checklist item identifiers (stable; tests and the ADR name them).
const (
	M4ItemIdentity     = "C1-identity"
	M4ItemMerchantRef  = "C2-merchant-reference-declaration"
	M4ItemStatusMap    = "C3-status-vocabulary"
	M4ItemUnknown      = "C4-unknown-status-policy"
	M4ItemChannel      = "C5-authenticated-channel"
	M4ItemDestination  = "C6-destination-echo"
	M4ItemFinalStatus  = "C7-final-status"
	M4ItemAmountAsset  = "C8-amount-asset"
	M4ItemReference    = "C9-provider-reference"
	M4ItemCompleteness = "C10-completeness"
	M4ItemTime         = "C11-time-semantics"
)

// m4StatementLineCarriesDestinationEcho is true only when statement.PaymentStatementLine
// has a destination-echo field AND payout_m4_evidence compares it to the attempt's
// destination snapshot. Neither exists (a migration is needed); a test pins this
// constant to the line struct, so it flips deliberately.
const m4StatementLineCarriesDestinationEcho = false

var m4ValidClasses = map[string]bool{
	statement.PaymentStatusPending: true, statement.PaymentStatusSucceeded: true,
	statement.PaymentStatusDeclined: true, statement.PaymentStatusReversed: true,
}

// m4EventClass is the only class each event may map to.
var m4EventClass = map[M4Event]string{
	M4EventPaidOut: statement.PaymentStatusSucceeded, M4EventPending: statement.PaymentStatusPending,
	M4EventRejected: statement.PaymentStatusDeclined, M4EventReturned: statement.PaymentStatusReversed,
	M4EventReversed: statement.PaymentStatusReversed, M4EventChargebck: statement.PaymentStatusReversed,
}

// CheckM4SourceContract runs the D-7 conformance checklist over a declaration.
// A nil contract is not conformant.
func CheckM4SourceContract(c M4SourceContract) M4SourceReport {
	var r M4SourceReport
	add := func(item string, sev M4FindingSeverity, d7 []int, format string, a ...any) {
		r.Findings = append(r.Findings, M4Finding{Item: item, Severity: sev, D7: d7, Detail: fmt.Sprintf(format, a...)})
	}
	if c == nil {
		add(M4ItemIdentity, M4Fail, []int{2, 3}, "no contract")
		return r
	}
	// C1: identity. A synthetic source is MOCK only.
	if c.ProviderID() == "" || c.Label() == "" {
		add(M4ItemIdentity, M4Fail, []int{3, 4}, "provider id and label are required (every line must carry the provider id)")
	}
	// The declaration is not trusted alone: the providerkind.Synthetic marker of the live
	// object (the tiering predicate's source of truth, ADR 0111 2.5) also disqualifies.
	if c.Synthetic() || payoutinstrument.IsSyntheticComponent(c) {
		add(M4ItemIdentity, M4Fail, []int{9}, "a synthetic source is MOCK only and is never eligible for non-MOCK M4")
	}
	// C2: the S-3 merchant-reference declaration.
	if d := c.MerchantReferenceDeclaration(); !d.Declared {
		add(M4ItemMerchantRef, M4Fail, []int{2, 9}, "payout_lines_carry_merchant_reference is undeclared (absence is not false)")
	} else if !d.Value {
		add(M4ItemMerchantRef, M4Fail, []int{2, 9}, "payout lines do not carry the merchant reference: no line can be attributed to an attempt")
	}
	// C3: the status vocabulary.
	m := c.StatusMapping()
	seen := map[string]bool{}
	events := map[M4Event]bool{}
	classOf := map[string]string{}
	for _, e := range m {
		switch {
		case e.ProviderStatus == "":
			add(M4ItemStatusMap, M4Fail, []int{8}, "a mapping has an empty provider status")
		case seen[e.ProviderStatus]:
			add(M4ItemStatusMap, M4Fail, []int{8}, "provider status %q is mapped twice", e.ProviderStatus)
		}
		seen[e.ProviderStatus] = true
		want, known := m4EventClass[e.Event]
		switch {
		case !known:
			add(M4ItemStatusMap, M4Fail, []int{8}, "provider status %q has unknown event %q", e.ProviderStatus, e.Event)
		case !m4ValidClasses[e.Class]:
			add(M4ItemStatusMap, M4Fail, []int{8}, "provider status %q maps to unknown class %q", e.ProviderStatus, e.Class)
		case e.Class != want:
			add(M4ItemStatusMap, M4Fail, []int{8}, "provider status %q: event %s must map to class %s, not %s", e.ProviderStatus, e.Event, want, e.Class)
		}
		events[e.Event] = true
		classOf[e.ProviderStatus] = e.Class
	}
	for _, ev := range []M4Event{M4EventPaidOut, M4EventPending, M4EventRejected, M4EventReturned, M4EventReversed, M4EventChargebck} {
		if !events[ev] {
			add(M4ItemStatusMap, M4Fail, []int{8}, "event %s has no provider status (a return, reversal or chargeback of a payout must never be left unmapped)", ev)
		}
	}
	// C4: an unknown status is never success.
	if p := c.UnknownStatusPolicy(); p != M4UnknownRejectImport && p != M4UnknownAsPending {
		add(M4ItemUnknown, M4Fail, []int{8}, "unknown-status policy %q: only reject_import or treat_as_pending are conformant", p)
	}
	// C5: the S-3 channel.
	ch := c.Channel()
	if !ch.Authenticated {
		add(M4ItemChannel, M4Fail, []int{3, 4, 9}, "the statement channel is not authenticated to the PSP")
	}
	if !ch.EndpointFromGovernedConfig {
		add(M4ItemChannel, M4Fail, []int{3, 4, 9}, "the endpoint/credentials are not from the secret store or governed configuration")
	}
	if ch.PSPOffersContentSignature && !ch.ContentSignatureVerified {
		add(M4ItemChannel, M4Fail, []int{3, 9}, "the PSP signs its statement and the adapter does not verify the signature")
	}
	// C6: destination echo (owner decision 5): explicit; a source that cannot echo cannot back M4 paid.
	de := c.DestinationEcho()
	switch de.Kind {
	case M4EchoUndeclared:
		add(M4ItemDestination, M4Fail, []int{7}, "destination-echo capability is undeclared (an adapter must not pretend by omission)")
	case M4EchoNone:
		add(M4ItemDestination, M4Limit, []int{7}, "no destination echo: M4 paid is not admissible (the destination cannot be positively evidenced)")
	case M4EchoFingerprint, M4EchoFullInstrmnt:
		if !de.OnStatementLines {
			add(M4ItemDestination, M4Limit, []int{7}, "the echo is not on the statement lines: M4 paid is not admissible")
		}
	default:
		add(M4ItemDestination, M4Fail, []int{7}, "unknown destination-echo kind %q", de.Kind)
	}
	// C7: final status.
	fs := c.FinalStatus()
	if len(fs.TerminalPaid) == 0 || len(fs.TerminalDeclined) == 0 {
		add(M4ItemFinalStatus, M4Fail, []int{8}, "terminal-paid and terminal-declined statuses must both be declared")
	}
	terminal := map[string]string{}
	for _, s := range fs.TerminalPaid {
		if classOf[s] != statement.PaymentStatusSucceeded {
			add(M4ItemFinalStatus, M4Fail, []int{8}, "terminal-paid status %q does not map to succeeded", s)
		}
		terminal[s] = "paid"
	}
	for _, s := range fs.TerminalDeclined {
		if classOf[s] != statement.PaymentStatusDeclined {
			add(M4ItemFinalStatus, M4Fail, []int{8}, "terminal-declined status %q does not map to declined", s)
		}
		if terminal[s] == "paid" {
			add(M4ItemFinalStatus, M4Fail, []int{8}, "status %q is declared both terminal-paid and terminal-declined", s)
		}
	}
	for s, cl := range classOf {
		if cl == statement.PaymentStatusSucceeded && terminal[s] != "paid" {
			add(M4ItemFinalStatus, M4Fail, []int{8}, "status %q maps to succeeded but is not declared terminal-paid (a non-final success)", s)
		}
	}
	if fs.PaidCanBeReversed && fs.ReversalWindow <= 0 {
		add(M4ItemFinalStatus, M4Fail, []int{8}, "a paid payout can be reversed but no reversal window is declared")
	}
	if fs.PaidCanBeReversed && !fs.ReversalReportedOnStatement {
		add(M4ItemFinalStatus, M4Fail, []int{8}, "a late reversal is not reported on the statement, so reconciliation R-1 could not raise it")
	}
	// C8: amount and asset.
	aa := c.AmountAsset()
	if !aa.MinorUnits || !aa.ExponentFromAssetRegistry || !aa.AssetCodePerLine || !aa.AmountIsPayoutPrincipal {
		add(M4ItemAmountAsset, M4Fail, []int{5, 6}, "amount/asset semantics incomplete: minor_units=%v exponent_from_registry=%v asset_per_line=%v principal_amount=%v",
			aa.MinorUnits, aa.ExponentFromAssetRegistry, aa.AssetCodePerLine, aa.AmountIsPayoutPrincipal)
	}
	// C9: the provider reference.
	ref := c.Reference()
	if !ref.PSPIssued || !ref.StablePerPayout {
		add(M4ItemReference, M4Fail, []int{3}, "the provider reference must be PSP-issued and stable per payout (psp_issued=%v stable=%v)", ref.PSPIssued, ref.StablePerPayout)
	}
	if ref.EchoesMerchantReference {
		add(M4ItemReference, M4Fail, []int{2, 3}, "the provider reference may echo the merchant reference: one field would be both the causal link and the reference")
	}
	// C10: completeness / authenticity of the window (T10 residuals).
	cp := c.Completeness()
	if !cp.CoverageAuthoritative || !cp.PaginationComplete {
		add(M4ItemCompleteness, M4Fail, []int{9}, "coverage not authoritative or pagination completeness not proven (coverage_authoritative=%v pagination_complete=%v)", cp.CoverageAuthoritative, cp.PaginationComplete)
	}
	// C11: time semantics.
	ts := c.TimeSemantics()
	if !ts.OccurredAtIsProviderEventTime || ts.LowerBound != M4LowerBoundFirstSend {
		add(M4ItemTime, M4Fail, []int{9}, "occurred_at must be the provider event time and the lower bound the first send (event_time=%v lower_bound=%q)", ts.OccurredAtIsProviderEventTime, ts.LowerBound)
	}
	if ts.LowerBoundSkew < 0 || ts.LowerBoundSkew > M4MaxLowerBoundSkew {
		add(M4ItemTime, M4Fail, []int{9}, "lower-bound clock-skew tolerance %s is outside [0, %s]", ts.LowerBoundSkew, M4MaxLowerBoundSkew)
	}

	r.Conformant = true
	paidLimit := false
	for _, f := range r.Findings {
		if f.Severity == M4Fail {
			r.Conformant = false
		}
		if f.Item == M4ItemDestination && f.Severity == M4Limit {
			paidLimit = true
		}
	}
	r.NotPaidAdmissible = r.Conformant
	r.PaidAdmissible = r.Conformant && !paidLimit && m4StatementLineCarriesDestinationEcho
	return r
}

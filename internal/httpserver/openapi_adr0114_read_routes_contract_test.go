package httpserver

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
)

// ADR 0114 slices S2 and S4 (R1, R1b, R2): DB-free contract and drift tests. The behavioural
// proof (authz matrix, tenant isolation, redaction, pagination) is in the integration tests
// (staff_read_routes_integration_test.go); these pin the documentation, the closed vocabularies
// and the pure functions.

var adr0114ReadFiles = []string{"payment_attempts_read_routes.go", "staff_reconciliation_routes.go"}

func TestOpenAPI_ADR0114Read_RoutesMountedAndDocumentedBothWays(t *testing.T) {
	spec := readSpec(t)
	mounted := mountedRoutes(t, adr0114ReadFiles...)
	want := []string{
		"GET /v1/admin/tenants/{tenantID}/payment-attempts",
		"GET /v1/admin/tenants/{tenantID}/payment-attempts/{attemptID}",
		"GET /v1/admin/tenants/{tenantID}/reconciliation/runs",
		"GET /v1/admin/tenants/{tenantID}/reconciliation/mismatches",
	}
	sort.Strings(mounted)
	sort.Strings(want)
	if !reflect.DeepEqual(mounted, want) {
		t.Fatalf("mounted ADR 0114 read routes = %v, want exactly %v (GET only: no write path)", mounted, want)
	}
	doc := documentedOps(spec, "/v1/admin/tenants/{tenantID}/payment-attempts", "/v1/admin/tenants/{tenantID}/reconciliation/")
	sort.Strings(doc)
	if !reflect.DeepEqual(doc, want) {
		t.Fatalf("documented operations = %v, want exactly %v", doc, want)
	}
	for _, r := range want {
		block := openAPIPathBlock(t, spec, strings.SplitN(r, " ", 2)[1])
		if strings.Contains(block, "tenant_id") {
			t.Errorf("%s documents a tenant_id parameter: the tenant is the path value only", r)
		}
		for _, code := range []string{`"400"`, `"401"`, `"403"`, `"404"`, `"500"`} {
			if !strings.Contains(block, code) {
				t.Errorf("%s does not document %s", r, code)
			}
		}
	}
	// R1b is documented on the withdrawal detail.
	if !strings.Contains(govSchemaBlock(t, spec, "WithdrawalAdminDetail"), "payout_attempt") {
		t.Error("WithdrawalAdminDetail does not document payout_attempt (R1b)")
	}
}

func TestOpenAPI_ADR0114Read_DTOsMatchSchemas(t *testing.T) {
	spec := readSpec(t)
	for _, c := range []struct {
		schema string
		typ    reflect.Type
	}{
		{"PaymentAttempt", reflect.TypeOf(paymentAttemptDTO{})},
		{"PaymentAttemptPage", reflect.TypeOf(paymentAttemptListResponse{})},
		{"PaymentAttemptSubjectRef", reflect.TypeOf(paymentAttemptSubjectRef{})},
		{"PayoutAttemptSummary", reflect.TypeOf(payoutAttemptSummaryDTO{})},
		{"StaffReconciliationRun", reflect.TypeOf(staffReconciliationRunDTO{})},
		{"StaffReconciliationMismatch", reflect.TypeOf(staffReconciliationMismatchDTO{})},
	} {
		got, want := jsonFieldNames(c.typ), govSchemaProps(t, spec, c.schema)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: DTO fields %v != schema properties %v", c.schema, got, want)
		}
	}
	// The embedded detail response gains exactly payout_attempt.
	if !strings.Contains(strings.Join(jsonFieldNames(reflect.TypeOf(staffWithdrawalDetailResponse{})), ","), "payout_attempt") {
		t.Error("staffWithdrawalDetailResponse lacks payout_attempt")
	}
}

// schemaEnum returns the enum values of components.schemas.<name> (inline `[a, b]` or a list).
func schemaEnum(t *testing.T, spec, name string) []string {
	t.Helper()
	block := govSchemaBlock(t, spec, name)
	i := strings.Index(block, "enum:")
	if i < 0 {
		t.Fatalf("schema %s has no enum", name)
	}
	rest := block[i+len("enum:"):]
	var out []string
	if strings.HasPrefix(strings.TrimLeft(rest, " "), "[") {
		inner := rest[strings.Index(rest, "[")+1 : strings.Index(rest, "]")]
		for _, v := range strings.Split(inner, ",") {
			out = append(out, strings.Trim(strings.TrimSpace(v), `"`))
		}
	} else {
		for _, line := range strings.Split(rest, "\n")[1:] {
			s := strings.TrimSpace(line)
			if !strings.HasPrefix(s, "- ") {
				break
			}
			out = append(out, strings.Trim(strings.TrimPrefix(s, "- "), `"`))
		}
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestOpenAPI_ADR0114Read_ClosedVocabulariesMatchCode(t *testing.T) {
	spec := readSpec(t)
	if got, want := schemaEnum(t, spec, "PaymentAttemptTerminalReason"), sortedKeys(paymentAttemptTerminalReasons); !reflect.DeepEqual(got, want) {
		t.Errorf("terminal reason enum %v != catalogue %v", got, want)
	}
	if got, want := schemaEnum(t, spec, "PaymentAttemptState"), sortedKeys(paymentAttemptStates); !reflect.DeepEqual(got, want) {
		t.Errorf("state enum %v != %v", got, want)
	}
	var streams []string
	for s := range staffStreamPermission {
		streams = append(streams, string(s))
	}
	sort.Strings(streams)
	if got := schemaEnum(t, spec, "StaffReconciliationStream"); !reflect.DeepEqual(got, streams) {
		t.Errorf("stream enum %v != %v", got, streams)
	}
	var kinds []string
	for _, s := range reconciliation.StaffViewStreams {
		ks, ok := reconciliation.StaffViewMismatchKinds(s)
		if !ok {
			t.Fatalf("no kind vocabulary for staff-view stream %s", s)
		}
		for _, k := range ks {
			kinds = append(kinds, string(k))
		}
	}
	sort.Strings(kinds)
	if got := schemaEnum(t, spec, "StaffReconciliationKind"); !reflect.DeepEqual(got, kinds) {
		t.Errorf("kind enum %v != %v", got, kinds)
	}
	if len(reconciliation.StaffViewStreams) != len(staffStreamPermission) {
		t.Errorf("StaffViewStreams %v and staffStreamPermission disagree", reconciliation.StaffViewStreams)
	}
}

// The wire shapes never carry a forbidden field, whatever the schema says: no provider reference
// text, merchant reference, idempotency key, player or wallet id, destination, staff id.
func TestADR0114Read_NoForbiddenFieldOnAnyDTO(t *testing.T) {
	forbidden := regexp.MustCompile(`provider_reference|merchant|idempotency|player|wallet|destination|ciphertext|fingerprint|email|_by$|staff|person|reconciliation_key|expected_value|actual_value`)
	allowed := map[string]bool{"has_provider_reference": true}
	for _, typ := range []reflect.Type{
		reflect.TypeOf(paymentAttemptDTO{}), reflect.TypeOf(paymentAttemptSubjectRef{}), reflect.TypeOf(payoutAttemptSummaryDTO{}),
		reflect.TypeOf(staffReconciliationRunDTO{}), reflect.TypeOf(staffReconciliationMismatchDTO{}), reflect.TypeOf(reconciliationSubjectRef{}),
	} {
		for _, f := range jsonFieldNames(typ) {
			if forbidden.MatchString(f) && !allowed[f] {
				t.Errorf("%s has forbidden JSON field %q", typ.Name(), f)
			}
		}
	}
}

func TestTerminalReasonOut_ClosedCatalogue(t *testing.T) {
	for k := range payments.PayoutDisputeReasons() {
		if got := terminalReasonOut(&k); got == nil || *got != k {
			t.Errorf("payout dispute reason %q is not in the output catalogue (got %v)", k, got)
		}
	}
	for _, k := range payments.DepositDisputeTerminalReasons() {
		if strings.HasSuffix(k, ":") {
			continue
		}
		if got := terminalReasonOut(&k); got == nil || *got != k {
			t.Errorf("deposit dispute reason %q is not in the output catalogue (got %v)", k, got)
		}
	}
	for _, bad := range []string{"", "something_new", "invalid_provider_reference:future", "kyc_denied\n", "a provider ref ABC123", "Other"} {
		s := bad
		if got := terminalReasonOut(&s); got == nil || *got != "other" {
			t.Errorf("unknown reason %q must be reported as other, got %v", bad, got)
		}
	}
	if terminalReasonOut(nil) != nil {
		t.Error("nil reason must stay nil")
	}
}

func str(s string) *string { return &s }

// admissible_kinds: the table over the terminal reasons (invariant 7.3). The M4 columns come from
// the database predicate in production; here they are the Go restatement that a payments parity
// test already pins to the database, so this table checks the composition and ordering.
func TestAdmissibleKinds_Table(t *testing.T) {
	m4 := func(kind payments.ResolutionKind, a attemptRow) bool {
		return payments.M4ResolvableDispute(kind, payments.AttemptState(a.state), a.terminalReason, refPtr(a.hasRef))
	}
	withM4 := func(a attemptRow) attemptRow {
		a.m4PaidScope = m4(payments.ResolutionM4EvidencePaid, a)
		a.m4NotScope = m4(payments.ResolutionM4EvidenceNotPaid, a)
		return a
	}
	submitted := str("submitted")
	cases := []struct {
		name string
		row  attemptRow
		want []string
	}{
		{"disputed deposit -> M1", attemptRow{operation: "deposit", state: "disputed", terminalReason: str("reversal_tombstone_precedes_success")}, []string{"m1_deposit_evidence"}},
		{"succeeded deposit -> none", attemptRow{operation: "deposit", state: "succeeded"}, []string{}},
		{"ambiguous payout with ref -> M2 both", attemptRow{operation: "payout", state: "ambiguous", providerID: str("mock"), hasRef: true, withdrawalState: submitted}, []string{"m2_declare_paid", "m2_declare_not_paid"}},
		{"ambiguous payout no ref -> M2 not paid only", attemptRow{operation: "payout", state: "ambiguous", providerID: str("mock"), withdrawalState: submitted}, []string{"m2_declare_not_paid"}},
		{"ambiguous payout, withdrawal not submitted -> none", attemptRow{operation: "payout", state: "ambiguous", providerID: str("mock"), hasRef: true, withdrawalState: str("completed")}, []string{}},
		{"ambiguous payout, no provider -> none", attemptRow{operation: "payout", state: "ambiguous", hasRef: true, withdrawalState: submitted}, []string{}},
		{"disputed provider_reference_mismatch -> M2", attemptRow{operation: "payout", state: "disputed", terminalReason: str("provider_reference_mismatch"), providerID: str("mock"), hasRef: true, withdrawalState: submitted}, []string{"m2_declare_paid", "m2_declare_not_paid"}},
		{"disputed success_for_never_sent_attempt -> M2", attemptRow{operation: "payout", state: "disputed", terminalReason: str("success_for_never_sent_attempt"), providerID: str("mock"), hasRef: true, withdrawalState: submitted}, []string{"m2_declare_paid", "m2_declare_not_paid"}},
		{"disputed amount_asset_mismatch -> none (GAP-AAM)", attemptRow{operation: "payout", state: "disputed", terminalReason: str("amount_asset_mismatch"), providerID: str("mock"), hasRef: true, withdrawalState: submitted}, []string{}},
		{"disputed invalid_provider_reference, no ref -> M4 both", attemptRow{operation: "payout", state: "disputed", terminalReason: str("invalid_provider_reference:empty"), providerID: str("mock"), withdrawalState: submitted}, []string{"m4_evidence_paid", "m4_evidence_not_paid"}},
		{"disputed provider_reference_conflict, no ref -> M4 both", attemptRow{operation: "payout", state: "disputed", terminalReason: str("provider_reference_conflict"), providerID: str("mock"), withdrawalState: submitted}, []string{"m4_evidence_paid", "m4_evidence_not_paid"}},
		{"disputed provider_reference_conflict, ref held -> none", attemptRow{operation: "payout", state: "disputed", terminalReason: str("provider_reference_conflict"), providerID: str("mock"), hasRef: true, withdrawalState: submitted}, []string{}},
		{"disputed destination_mismatch -> M4 not paid only", attemptRow{operation: "payout", state: "disputed", terminalReason: str("destination_mismatch"), providerID: str("mock"), hasRef: true, withdrawalState: submitted}, []string{"m4_evidence_not_paid"}},
		{"disputed destination_integrity_failure -> M4 not paid only", attemptRow{operation: "payout", state: "disputed", terminalReason: str("destination_integrity_failure"), providerID: str("mock"), withdrawalState: submitted}, []string{"m4_evidence_not_paid"}},
		{"disputed late_success_after_terminal -> none", attemptRow{operation: "payout", state: "disputed", terminalReason: str("late_success_after_terminal"), providerID: str("mock"), hasRef: true, withdrawalState: submitted}, []string{}},
		{"disputed reversal_tombstone_precedes_success payout -> none", attemptRow{operation: "payout", state: "disputed", terminalReason: str("reversal_tombstone_precedes_success"), providerID: str("mock"), hasRef: true, withdrawalState: submitted}, []string{}},
		{"unknown reason -> none", attemptRow{operation: "payout", state: "disputed", terminalReason: str("brand_new_reason"), providerID: str("mock"), withdrawalState: submitted}, []string{}},
		{"open resolution -> none", attemptRow{operation: "payout", state: "ambiguous", providerID: str("mock"), hasRef: true, withdrawalState: submitted, openResolutionID: func() *uuid.UUID { u := uuid.New(); return &u }()}, []string{}},
	}
	for _, c := range cases {
		got := admissibleKinds(withM4(c.row))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: admissible_kinds = %v, want %v", c.name, got, c.want)
		}
		if got == nil {
			t.Errorf("%s: admissible_kinds must be a non-nil slice (never null on the wire)", c.name)
		}
	}
}

func refPtr(has bool) *string {
	if !has {
		return nil
	}
	return str("held")
}

func TestPaymentAttemptCursor_RoundTripAndStrictness(t *testing.T) {
	id := uuid.New()
	c := paymentAttemptCursor{CreatedAt: time.Date(2026, 10, 12, 10, 11, 12, 123456000, time.UTC), ID: id}
	tok := encodePaymentAttemptCursor(c)
	got, err := decodePaymentAttemptCursor(tok)
	if err != nil || !got.CreatedAt.Equal(c.CreatedAt) || got.ID != id {
		t.Fatalf("round trip: %v %v", got, err)
	}
	for _, bad := range []string{"", "!!!", tok + "x", strings.Repeat("A", 200), "djEuMS4x" /* v1.1.1 */} {
		if _, err := decodePaymentAttemptCursor(bad); err == nil {
			t.Errorf("cursor %q must be refused", bad)
		}
	}
}

func TestReconciliationSubject_Parser(t *testing.T) {
	a, b := uuid.NewString(), uuid.NewString()
	cases := []struct {
		stream reconciliation.Stream
		kind   reconciliation.MismatchKind
		key    string
		want   string // type:id or ""
	}{
		{reconciliation.StreamLedgerVsProjection, reconciliation.MismatchKindBalanceMismatch, a, "ledger_account:" + a},
		{reconciliation.StreamLedgerVsProjection, reconciliation.MismatchKindMissingProjection, a, "ledger_account:" + a},
		{reconciliation.StreamLedgerVsProjection, reconciliation.MismatchKindLedgerUnlinkedManualAdjustment, a, "ledger_transaction:" + a},
		{reconciliation.StreamLedgerVsProjection, reconciliation.MismatchKindBalanceMismatch, "not-a-uuid", ""},
		{reconciliation.StreamPaymentStatement, reconciliation.MismatchKindPayAmountMismatch, "provider=mock provider_reference=SECRET-REF kind=deposit attempt=" + a + " check=amount", "payment_attempt:" + a},
		{reconciliation.StreamPaymentStatement, reconciliation.MismatchKindPayMissingPlatformRecord, "provider=mock provider_reference=SECRET-REF kind=deposit", ""},
		{reconciliation.StreamPaymentStatement, reconciliation.MismatchKindPayDuplicate, "provider=mock deposit_intent=" + a + " check=duplicate_success", "deposit_intent:" + a},
		{reconciliation.StreamPaymentStatement, reconciliation.MismatchKindPayUnresolved, "provider=mock receipt=" + a + " provider_reference=R check=deferred_receipt", "payment_receipt:" + a},
		{reconciliation.StreamPaymentStatement, reconciliation.MismatchKindPayAmountMismatch, "provider=mock attempt=" + strings.ToUpper(a) + " check=amount", ""},
		{reconciliation.StreamPaymentStatement, reconciliation.MismatchKindPayAmountMismatch, "provider=mock xattempt=" + a, ""},
		{reconciliation.StreamSportsbookSettlement, reconciliation.MismatchKindSBBetNet, "bet=" + a + " check=end_state status=won", "bet:" + a},
		{reconciliation.StreamSportsbookSettlement, reconciliation.MismatchKindSBLocked, "wallet=" + a + " asset=USD account_type=player_locked_cash", "wallet:" + a},
		{reconciliation.StreamSportsbookSettlement, reconciliation.MismatchKindSBOrphanHistory, "history=" + a + " bet=" + b + " check=reverses", "bet:" + b},
		{reconciliation.StreamSportsbookSettlement, reconciliation.MismatchKindSBOrphanLedger, "ledger_transaction=" + a, "ledger_transaction:" + a},
		{reconciliation.StreamSportsbookSettlement, reconciliation.MismatchKindSBMockStatement, "statement: some-key", ""},
	}
	for _, c := range cases {
		got := ""
		if s := reconciliationSubject(c.stream, c.kind, c.key); s != nil {
			got = s.Type + ":" + s.ID
		}
		if got != c.want {
			t.Errorf("key %q -> %q, want %q", c.key, got, c.want)
		}
	}
}

func TestReconciliationMinorUnitsAndAsset_Parsers(t *testing.T) {
	nums := map[string]string{
		"open_bet_stakes=500":                                     "500",
		"ledger_locked_net=-30":                                   "-30",
		"sportsbook_locked_bonus_net=0":                           "0",
		"attempt=x op=payout state=pending amount=1250 asset=USD": "1250",
		"line=1 kind=deposit status=ok amount=77 asset=USD":       "77",
		"ledger: amount=9":                                        "9",
	}
	for in, want := range nums {
		if got := reconciliationMinorUnits(in); got == nil || *got != want {
			t.Errorf("minor units of %q = %v, want %s", in, got, want)
		}
	}
	for _, in := range []string{
		"", "asset=USD type=player_cash debit=5 credit=7", "cash=1 locked=2 house=3", "amount=1 amount=2",
		"amount=1.5", "amount=abc", "x=1 y=2", "amount=" + strings.Repeat("9", 41), "provider_reference=abc123", "ledger: amount=1 amount=1",
	} {
		if got := reconciliationMinorUnits(in); got != nil {
			t.Errorf("minor units of %q = %q, want nil", in, *got)
		}
	}
	if got := reconciliationAsset("wallet=x asset=USD account_type=a", "asset=usd debit=1", "none"); got == nil || !strings.EqualFold(*got, "USD") {
		t.Errorf("asset = %v", got)
	}
	for _, in := range [][]string{
		{"asset=USD", "asset=EUR"}, {"no asset"}, {"asset="}, {"asset=bad!"}, {"asset=" + strings.Repeat("A", 17)}, {"xasset=USD"},
	} {
		if got := reconciliationAsset(in...); got != nil {
			t.Errorf("asset of %v = %q, want nil", in, *got)
		}
	}
}

// Section 3 / invariant 7.1 at the unit level: the stream -> permission table is exactly the ADR's.
func TestStaffStreamPermission_IsTheADRTable(t *testing.T) {
	want := map[reconciliation.Stream]auth.Permission{
		reconciliation.StreamPaymentStatement:     auth.PermPaymentForceResolveRead,
		reconciliation.StreamLedgerVsProjection:   auth.PermLedgerAdjustmentRead,
		reconciliation.StreamSportsbookSettlement: auth.PermSportsbookReconciliationRead,
	}
	if !reflect.DeepEqual(staffStreamPermission, want) {
		t.Fatalf("staffStreamPermission = %v, want %v", staffStreamPermission, want)
	}
}

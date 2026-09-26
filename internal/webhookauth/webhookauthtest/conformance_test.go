package webhookauthtest_test

import (
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
	"github.com/Diansalas/igaming-platform/internal/webhookauth/webhookauthtest"
)

// --- The suite against the platform MOCK schemes (SC13 cross-domain; SC7
// exempt only because they are synthetic). --------------------------------

func mustMockFixture(t *testing.T, s webhookauth.Scheme) webhookauthtest.Fixture {
	t.Helper()
	f, ok := webhookauthtest.MockSchemeFixture(s.VerificationScheme())
	if !ok {
		t.Fatal("MockSchemeFixture refused a platform MOCK scheme")
	}
	return f
}

func TestSchemeConformance_PlatformMockPayments(t *testing.T) {
	webhookauthtest.RunSchemeConformance(t, mustMockFixture(t, webhookauth.PaymentsScheme()))
}

func TestSchemeConformance_PlatformMockKYC(t *testing.T) {
	webhookauthtest.RunSchemeConformance(t, mustMockFixture(t, webhookauth.KYCScheme()))
}

func TestSchemeConformance_PlatformMockCasino(t *testing.T) {
	webhookauthtest.RunSchemeConformance(t, mustMockFixture(t, webhookauth.CasinoScheme()))
}

// TestSchemeConformance_PlatformMockPayments_KnownAnswer pins the payments
// MOCK bytes with an independent vector (optional for a synthetic scheme,
// supplied anyway):
//
//	printf 'igaming.payments.webhook.v1\x0011111111-2222-3333-4444-555555555555\x00mock-psp\x00mock-v1\x00{"kav":1}' \
//	  | openssl dgst -sha256 -hmac 'payments-mock-kav-test-key-0123456789'
func TestSchemeConformance_PlatformMockPayments_KnownAnswer(t *testing.T) {
	f := mustMockFixture(t, webhookauth.PaymentsScheme())
	key := []byte("payments-mock-kav-test-key-0123456789")
	tenant := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	h := http.Header{}
	h.Set("X-Payments-Signature", "v1=892a48c4f827b4a43dafcb2d61b827a3f75b40301c19b3f8e9e3bfc3943110cc")
	h.Set("X-Payments-Key-Id", "mock-v1")
	f.KnownAnswers = []webhookauthtest.KnownAnswer{{
		Provenance: "platform MOCK payments scheme; HMAC-SHA256 computed independently with openssl dgst on 2026-09-26",
		Credential: webhookauth.Credential{TenantID: tenant, ProviderID: "mock-psp", KeyID: "mock-v1", Secret: key},
		Inbound:    webhookauth.Inbound{TenantID: tenant, ProviderID: "mock-psp", Header: h, Body: []byte(`{"kav":1}`)},
		WantValid:  true,
	}}
	rep := webhookauthtest.Evaluate(f)
	if msgs := rep[webhookauthtest.CaseKAV]; len(msgs) != 0 {
		t.Fatalf("payments MOCK known-answer vector failed: %v", msgs)
	}
}

// --- The suite against the test-only timestamped reference scheme (the SC7
// path; 04-review-qa.md W1a (b)). -----------------------------------------

func TestSchemeConformance_ReferenceTimestampedScheme(t *testing.T) {
	for _, s := range []refScheme{baseRef(), accountRef(), implicitRef()} {
		t.Run(s.name, func(t *testing.T) {
			webhookauthtest.RunSchemeConformance(t, newRefFixture(s))
		})
	}
}

// --- Self-test (ruling J1; security C9 point 2): one deliberately broken
// scheme per mandatory case, each caught by EXACTLY that case and no other.

type brokenCase struct {
	name   string
	scheme refScheme
	want   string
	// alsoRed lists other cases that legitimately catch the same defect
	// through a different input (empty for every scheme but one; see
	// TimestampBeforeMAC). The red set must still match EXACTLY.
	alsoRed []string
}

func brokenSchemes() []brokenCase {
	with := func(s refScheme, mutate func(*refBugs)) refScheme {
		mutate(&s.bugs)
		return s
	}
	return []brokenCase{
		{"PrefixCompareAcceptsPrefix", with(baseRef(), func(b *refBugs) { b.prefixCompare = true }), webhookauthtest.CaseSC2, nil},
		{"IgnoresTenant", with(baseRef(), func(b *refBugs) { b.ignoreTenant = true }), webhookauthtest.CaseSC3, nil},
		{"IgnoresProvider", with(baseRef(), func(b *refBugs) { b.ignoreProvider = true }), webhookauthtest.CaseSC4, nil},
		{"PanicsOnMalformedHeader", with(baseRef(), func(b *refBugs) { b.panicOnLongHeader = true }), webhookauthtest.CaseSC5, nil},
		{"MultiKeyTrialOnAbsentKeyID", with(baseRef(), func(b *refBugs) { b.trialOnAbsentKeyID = true }), webhookauthtest.CaseSC6, nil},
		{"IgnoresTimestamp", with(baseRef(), func(b *refBugs) { b.ignoreTimestamp = true }), webhookauthtest.CaseSC7, nil},
		// Security S-2 (06-gate-w1 F2): within SC7, ONLY the stale-AND-
		// tampered assertion catches this scheme (its MAC and window are
		// each individually correct, so every other SC7 check passes - see
		// TestSchemeConformanceSelfTest_TimestampBeforeMAC_OnlySC7StaleAndTampered).
		// SC2 also goes red, and cannot be avoided: SC2 tampers the declared
		// timestamp HEADER, and any tampered value outside the window hits
		// the same window-before-MAC defect. The exact red-set match still
		// makes SC7's assertion load-bearing: deleting it turns the set into
		// [SC2] and fails this self-test.
		{"TimestampBeforeMAC", with(baseRef(), func(b *refBugs) { b.timestampBeforeMAC = true }), webhookauthtest.CaseSC7, []string{webhookauthtest.CaseSC2}},
		{"IgnoresBoundAccount", with(accountRef(), func(b *refBugs) { b.ignoreAccount = true }), webhookauthtest.CaseSC8, nil},
		{"HonoursPreviousAfterNotAfter", with(implicitRef(), func(b *refBugs) { b.ignoreNotAfter = true }), webhookauthtest.CaseSC9, nil},
		{"AcceptsEmptySecret", with(baseRef(), func(b *refBugs) { b.acceptShortSecret = true }), webhookauthtest.CaseSC10, nil},
		{"SecretInErrorText", with(baseRef(), func(b *refBugs) { b.secretInError = true }), webhookauthtest.CaseSC11, nil},
		{"SignAndVerifyShareABug", with(baseRef(), func(b *refBugs) { b.swappedOrderBoth = true }), webhookauthtest.CaseKAV, nil},
	}
}

func TestSchemeConformanceSelfTest_EachBrokenSchemeGoesRedOnlyInItsCase(t *testing.T) {
	for _, bc := range brokenSchemes() {
		t.Run(bc.name+"_GoesRed", func(t *testing.T) {
			rep := webhookauthtest.Evaluate(newRefFixture(bc.scheme))
			want := append([]string{bc.want}, bc.alsoRed...)
			sort.Strings(want)
			if got := rep.Failed(); !reflect.DeepEqual(got, want) {
				t.Fatalf("broken scheme %s: want exactly %v red, got %v\nfailures: %v", bc.name, want, got, rep)
			}
		})
	}
}

// TestSchemeConformanceSelfTest_TimestampBeforeMAC_OnlySC7StaleAndTampered
// pins security S-2 precisely: for the window-before-MAC scheme, SC7's ONLY
// failure is its stale-AND-tampered assertion (C10), and SC2's only
// failures come from tampering the timestamp header - never from a body
// flip or another header.
func TestSchemeConformanceSelfTest_TimestampBeforeMAC_OnlySC7StaleAndTampered(t *testing.T) {
	s := baseRef()
	s.bugs.timestampBeforeMAC = true
	rep := webhookauthtest.Evaluate(newRefFixture(s))
	sc7 := rep[webhookauthtest.CaseSC7]
	if len(sc7) != 1 || !strings.Contains(sc7[0], "stale AND tampered") {
		t.Fatalf("SC7 must fail ONLY on its stale-and-tampered assertion, got %v", sc7)
	}
	sc2 := rep[webhookauthtest.CaseSC2]
	if len(sc2) == 0 {
		t.Fatal("expected SC2's timestamp-header tampering to catch the defect too")
	}
	for _, msg := range sc2 {
		if !strings.Contains(msg, "auth header "+refTSHeader+" ") {
			t.Fatalf("SC2 must fail only through the timestamp header, got %q", msg)
		}
	}
}

// TestSchemeConformanceSelfTest_MandatoryCasesAllCovered proves the broken-
// scheme set covers every mandatory case (the three named by the QA plan
// plus security C9's full list and the C3 case).
func TestSchemeConformanceSelfTest_MandatoryCasesAllCovered(t *testing.T) {
	covered := map[string]bool{}
	for _, bc := range brokenSchemes() {
		covered[bc.want] = true
	}
	for _, id := range []string{
		webhookauthtest.CaseSC2, webhookauthtest.CaseSC3, webhookauthtest.CaseSC4, webhookauthtest.CaseSC5,
		webhookauthtest.CaseSC6, webhookauthtest.CaseSC7, webhookauthtest.CaseSC8, webhookauthtest.CaseSC9,
		webhookauthtest.CaseSC10, webhookauthtest.CaseSC11, webhookauthtest.CaseKAV,
	} {
		if !covered[id] {
			t.Errorf("mandatory case %s has no killing broken scheme in the self-test", id)
		}
	}
}

// The QA plan's three named self-tests (04-review-qa.md W1a), as explicit
// top-level tests.
func TestSchemeConformanceSelfTest_IgnoresTenant_GoesRed(t *testing.T) {
	assertOnlyRed(t, "IgnoresTenant")
}

func TestSchemeConformanceSelfTest_IgnoresTimestamp_GoesRed(t *testing.T) {
	assertOnlyRed(t, "IgnoresTimestamp")
}

func TestSchemeConformanceSelfTest_PrefixCompareAcceptsPrefix_GoesRed(t *testing.T) {
	assertOnlyRed(t, "PrefixCompareAcceptsPrefix")
}

func assertOnlyRed(t *testing.T, name string) {
	t.Helper()
	for _, bc := range brokenSchemes() {
		if bc.name != name {
			continue
		}
		rep := webhookauthtest.Evaluate(newRefFixture(bc.scheme))
		if got := rep.Failed(); !reflect.DeepEqual(got, []string{bc.want}) {
			t.Fatalf("want exactly [%s] red, got %v", bc.want, got)
		}
		return
	}
	t.Fatalf("no broken scheme named %s", name)
}

// TestSchemeConformance_NonSyntheticWithoutKAVFails: the KAV hook is
// required for non-synthetic schemes.
func TestSchemeConformance_NonSyntheticWithoutKAVFails(t *testing.T) {
	f := newRefFixture(baseRef())
	f.KnownAnswers = nil
	if got := webhookauthtest.Evaluate(f).Failed(); !reflect.DeepEqual(got, []string{webhookauthtest.CaseKAV}) {
		t.Fatalf("want exactly [%s] red for a non-synthetic scheme with no known-answer vector, got %v", webhookauthtest.CaseKAV, got)
	}
}

// TestSchemeConformance_NonSyntheticWithoutTimestampFails: SC7 is
// fail-not-skip for a real scheme (and P0 refuses the declaration).
func TestSchemeConformance_NonSyntheticWithoutTimestampFails(t *testing.T) {
	f := newRefFixture(baseRef())
	f.Scheme = noTimestampDeclaration{refScheme: baseRef()}
	failed := webhookauthtest.Evaluate(f).Failed()
	joined := strings.Join(failed, ",")
	if !strings.Contains(joined, webhookauthtest.CaseSC7) || !strings.Contains(joined, webhookauthtest.CaseP0) {
		t.Fatalf("a non-synthetic scheme declaring no signed timestamp must fail SC7 and P0, got %v", failed)
	}
}

type noTimestampDeclaration struct{ refScheme }

func (n noTimestampDeclaration) Properties() webhookauth.SchemeProperties {
	p := n.refScheme.Properties()
	p.SignedTimestamp, p.MaxSkew, p.Replay = false, 0, webhookauth.ReplayIdempotencyOnly
	return p
}

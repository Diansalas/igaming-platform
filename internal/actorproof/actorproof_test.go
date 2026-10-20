package actorproof

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testKey(b byte) []byte { return []byte(strings.Repeat(string([]byte{b}), 32)) }

func newTestIssuer(t *testing.T) *Issuer {
	t.Helper()
	i, err := NewIssuer("k1", map[string][]byte{"k1": testKey('a'), "k0": testKey('b')})
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func claims() Claims {
	return Claims{Actor: uuid.New(), Scope: ScopeTenant, Tenant: uuid.New(), Operation: OpAdjustmentApprove,
		Target: uuid.NewString(), PayloadHash: strings.Repeat("ab", 32)}
}

func TestSign_WireFormatAndMAC(t *testing.T) {
	i := newTestIssuer(t)
	i.now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	c := claims()
	tok, err := i.Sign(c)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, "|")
	if len(parts) != 12 || parts[0] != "v1" || parts[1] != "k1" {
		t.Fatalf("unexpected token shape: %q", tok)
	}
	want := []string{c.Actor.String(), c.Scope, c.Tenant.String(), c.Operation, c.Target, c.PayloadHash}
	for n, w := range want {
		if parts[2+n] != w {
			t.Fatalf("field %d = %q, want %q", 2+n, parts[2+n], w)
		}
	}
	if parts[8] != "1800000000" || parts[9] != "1800000030" {
		t.Fatalf("iat/exp = %s/%s", parts[8], parts[9])
	}
	m := hmac.New(sha256.New, testKey('a'))
	m.Write([]byte(strings.Join(parts[:11], "|")))
	if hex.EncodeToString(m.Sum(nil)) != parts[11] {
		t.Fatal("MAC is not HMAC-SHA256 over fields 1..11 joined by '|'")
	}
	if Lifetime > 60*time.Second {
		t.Fatalf("lifetime %v exceeds the database's 60 s cap", Lifetime)
	}
	tok2, _ := i.Sign(c)
	if tok == tok2 {
		t.Fatal("two proofs for the same claims must carry different nonces")
	}
}

func TestSign_RejectsBadClaims_FailsClosed(t *testing.T) {
	i := newTestIssuer(t)
	bad := []Claims{}
	c := claims()
	c.Actor = uuid.Nil
	bad = append(bad, c)
	c = claims()
	c.Tenant = uuid.Nil
	bad = append(bad, c)
	c = claims()
	c.Scope = "platform"
	bad = append(bad, c)
	c = claims()
	c.Operation = "x|y:z"
	bad = append(bad, c)
	c = claims()
	c.Target = "123|456"
	bad = append(bad, c)
	c = claims()
	c.PayloadHash = "short"
	bad = append(bad, c)
	for n, c := range bad {
		if _, err := i.Sign(c); !errors.Is(err, ErrInvalidClaims) {
			t.Fatalf("case %d: want ErrInvalidClaims, got %v", n, err)
		}
	}
	var nilIssuer *Issuer
	if _, err := nilIssuer.Sign(claims()); !errors.Is(err, ErrNoIssuer) {
		t.Fatalf("nil issuer must fail closed with ErrNoIssuer, got %v", err)
	}
	c = claims()
	c.Target = TargetNew
	if _, err := i.Sign(c); err != nil {
		t.Fatalf("target 'new' is valid: %v", err)
	}
}

func TestNewIssuer_KeyRules(t *testing.T) {
	if _, err := NewIssuer("k1", map[string][]byte{"k1": []byte("short")}); !errors.Is(err, ErrKeyConfig) {
		t.Fatalf("short key must be refused, got %v", err)
	}
	if _, err := NewIssuer("k2", map[string][]byte{"k1": testKey('a')}); !errors.Is(err, ErrKeyConfig) {
		t.Fatalf("active kid without a key must be refused, got %v", err)
	}
	if _, err := NewIssuer("bad kid", map[string][]byte{"bad kid": testKey('a')}); !errors.Is(err, ErrKeyConfig) {
		t.Fatalf("malformed kid must be refused, got %v", err)
	}
	i := newTestIssuer(t)
	if strings.Contains(fmt.Sprintf("%v %+v %#v %s", i, i, i, i), "aaaa") {
		t.Fatal("an Issuer printed key material")
	}
}

func TestParseKeySet(t *testing.T) {
	k1 := base64.StdEncoding.EncodeToString(testKey('a'))
	k2 := base64.URLEncoding.EncodeToString(testKey('b'))
	m, err := ParseKeySet("k1:" + k1 + ", k2:" + k2)
	if err != nil || len(m) != 2 || string(m["k2"]) != string(testKey('b')) {
		t.Fatalf("two active keys (rotation): %v %v", m, err)
	}
	for _, bad := range []string{"", "k1", "k1:!!!", "k1:" + base64.StdEncoding.EncodeToString([]byte("tiny")),
		"k1:" + k1 + ",k1:" + k1, "bad kid:" + k1} {
		if _, err := ParseKeySet(bad); !errors.Is(err, ErrKeyConfig) {
			t.Fatalf("%q: want ErrKeyConfig, got %v", bad, err)
		}
	}
	_, err = ParseKeySet("k1:" + base64.StdEncoding.EncodeToString([]byte("tiny")))
	if err != nil && strings.Contains(err.Error(), base64.StdEncoding.EncodeToString([]byte("tiny"))) {
		t.Fatal("a parse error echoed key material")
	}
	if i, err := NewIssuerFromConfig("", ""); i != nil || err != nil {
		t.Fatalf("unset configuration is (nil, nil), got %v %v", i, err)
	}
	if _, err := NewIssuerFromConfig("k1", ""); err == nil {
		t.Fatal("a kid without keys must be refused")
	}
	if i, err := NewIssuerFromConfig("k2", "k1:"+k1+",k2:"+k2); err != nil || i.KID() != "k2" {
		t.Fatalf("rotation: active kid k2: %v %v", i, err)
	}
}

// Digest must equal SQL k2_sha256_hex(k2_canonical(...)): each field as
// "<octet length>:<value>", NULL as "~", joined by ",". The vector below is the
// value PostgreSQL computes (pinned by the integration tests through the
// trigger as well).
func TestDigest_CanonicalEncoding(t *testing.T) {
	sum := sha256.Sum256([]byte("1:a,~,2:bc,0:"))
	if got := Digest(S("a"), nil, S("bc"), S("")); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest mismatch: %s", got)
	}
	if SP("") != nil || *SP("x") != "x" {
		t.Fatal("SP: empty means NULL")
	}
	u := uuid.New()
	if *UUIDP(&u) != u.String() || UUIDP(nil) != nil {
		t.Fatal("UUIDP")
	}
	// Multi-byte: octet length, not rune count.
	sum = sha256.Sum256([]byte("2:é"))
	if Digest(S("é")) != hex.EncodeToString(sum[:]) {
		t.Fatal("digest must use the octet length")
	}
}

type fakeChecker struct {
	active bool
	err    error
	asked  string
}

func (f *fakeChecker) ActorProofKeyActive(_ context.Context, kid string) (bool, error) {
	f.asked = kid
	return f.active, f.err
}

func TestVerifyConfiguredInProduction(t *testing.T) {
	ctx := context.Background()
	iss := newTestIssuer(t)
	// Not production: never gates.
	for _, env := range []string{"development", "staging", ""} {
		if err := VerifyConfiguredInProduction(ctx, env, nil, &fakeChecker{}); err != nil {
			t.Fatalf("%q must not gate: %v", env, err)
		}
	}
	// Production without a key: refuses to start.
	if err := VerifyConfiguredInProduction(ctx, "production", nil, &fakeChecker{active: true}); err == nil || !strings.Contains(err.Error(), "ACTOR_PROOF_KEYS") {
		t.Fatalf("production without a configured key must refuse, got %v", err)
	}
	// Production, key configured but the database does not hold the kid active.
	fc := &fakeChecker{active: false}
	if err := VerifyConfiguredInProduction(ctx, "production", iss, fc); err == nil || fc.asked != "k1" {
		t.Fatalf("production with an unprovisioned kid must refuse (asked %q): %v", fc.asked, err)
	}
	if err := VerifyConfiguredInProduction(ctx, "production", iss, &fakeChecker{err: errors.New("db down")}); err == nil {
		t.Fatal("a failing probe must refuse (fail closed)")
	}
	if err := VerifyConfiguredInProduction(ctx, "production", iss, &fakeChecker{active: true}); err != nil {
		t.Fatalf("provisioned key must pass: %v", err)
	}
}

// staticOffenders scans the non-test Go files under root and reports, as paths
// relative to root: (1) files outside the allowed four-eyes owners that import
// internal/actorproof (the single server-side issuer); (2) ANY non-test file
// other than prooftest itself that imports internal/actorproof/prooftest (its
// AttachForSession signs for whoever the session GUCs name, so it must never be
// reachable from production code); (3) any non-test file other than
// internal/capability and prooftest that mentions TestSessionProofHook.
func staticOffenders(t *testing.T, root string, allowed map[string]bool) []string {
	t.Helper()
	const imp = `"github.com/Diansalas/igaming-platform/internal/actorproof"`
	const impTest = `"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"`
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "backoffice", "b2c", ".claude":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		src := string(b)
		dir := filepath.Dir(rel)
		inProoftest := dir == filepath.Join("internal", "actorproof", "prooftest")
		if strings.Contains(src, impTest) && !inProoftest && !prooftestImporters[dir] {
			offenders = append(offenders, rel+" (imports prooftest)")
		}
		// Pinned to exact code-line counts (comments excluded), not to whole directories:
		// the hook is declared and read ONLY in internal/capability/proof.go (declaration,
		// nil check, call = 3) and assigned ONLY in prooftest (1); WithProofActor is
		// declared in proof.go (1) and CALLED only by the K1 route handler
		// internal/httpserver/capability_routes.go (2); PolicyCall{...} is constructed only
		// in internal/adjustment/policy.go (1).
		for _, pin := range []struct {
			needle string
			allow  map[string]int
		}{
			{"TestSessionProofHook", map[string]int{
				filepath.Join("internal", "capability", "proof.go"):                  3,
				filepath.Join("internal", "actorproof", "prooftest", "prooftest.go"): 1}},
			{"WithProofActor(", map[string]int{
				filepath.Join("internal", "capability", "proof.go"):             1,
				filepath.Join("internal", "httpserver", "capability_routes.go"): 2}},
			{"PolicyCall{", map[string]int{
				filepath.Join("internal", "adjustment", "policy.go"): 1}},
			// F4 residual R1: the key-provisioning hook of the dispatch-gate proof is declared in actorproof.go (1), read
			// ONLY in internal/withdrawal/dispatchproof.go (1: the nil-checked load) and assigned ONLY in prooftest (1).
			{"TestEnsureKeyHook", map[string]int{
				filepath.Join("internal", "actorproof", "actorproof.go"):                1,
				filepath.Join("internal", "withdrawal", "dispatchproof.go"):             1,
				filepath.Join("internal", "payments", "deposit_tombstone_proof.go"):     1, // migration 0175
				filepath.Join("internal", "actorproof", "prooftest", "dispatchhook.go"): 1}},
			// Closure security review F-3: the dispatch-gate system claims (operation, actor, scope, the operation's
			// literal and the actor's uuid) are built ONLY in actorproof.go (declaration / validation) and
			// internal/withdrawal/dispatchproof.go (the one signer); the decision table is written ONLY by
			// internal/withdrawal/dispatchgate.go (RecordDispatchGateDecision, itself pinned to four call sites).
			{"OpDispatchGateAllow", map[string]int{
				filepath.Join("internal", "actorproof", "actorproof.go"):    3,
				filepath.Join("internal", "withdrawal", "dispatchproof.go"): 1}},
			{"SystemActorDispatchGate", map[string]int{
				filepath.Join("internal", "actorproof", "actorproof.go"):    1,
				filepath.Join("internal", "withdrawal", "dispatchproof.go"): 1}},
			{"withdrawal_dispatch_gate:", map[string]int{
				filepath.Join("internal", "actorproof", "actorproof.go"): 2}},
			{"00000f4d1500", map[string]int{
				filepath.Join("internal", "actorproof", "actorproof.go"): 1}},
			{"INSERT INTO withdrawal_gate_decisions", map[string]int{
				filepath.Join("internal", "withdrawal", "dispatchgate.go"): 1}},
			// Closure security review F-1 (migration 0175): the deposit-reversal-tombstone system claims are built ONLY in
			// actorproof.go and internal/payments/deposit_tombstone_proof.go; the scope system ONLY there and in
			// dispatchproof.go. The signing helper is called ONLY by postDepositReversalTombstone (orchestrator.go), whose
			// callers are the tombstone branch of the verified reversal receipt (receipt.go) and the integration-only
			// fixture (testsupport_tombstone_integration.go, build-tag pinned below).
			{"OpDepositReversalTombstone", map[string]int{
				filepath.Join("internal", "actorproof", "actorproof.go"):            3,
				filepath.Join("internal", "payments", "deposit_tombstone_proof.go"): 1}},
			{"SystemActorDepositReversal", map[string]int{
				filepath.Join("internal", "actorproof", "actorproof.go"):            1,
				filepath.Join("internal", "payments", "deposit_tombstone_proof.go"): 1}},
			{"deposit_reversal_tombstone:", map[string]int{
				filepath.Join("internal", "actorproof", "actorproof.go"): 2}},
			{"0000000de175", map[string]int{
				filepath.Join("internal", "actorproof", "actorproof.go"): 1}},
			{"ScopeSystem", map[string]int{
				filepath.Join("internal", "actorproof", "actorproof.go"):            4,
				filepath.Join("internal", "withdrawal", "dispatchproof.go"):         1,
				filepath.Join("internal", "payments", "deposit_tombstone_proof.go"): 1}},
			{"attachDepositTombstoneProof(", map[string]int{
				filepath.Join("internal", "payments", "deposit_tombstone_proof.go"): 1,
				filepath.Join("internal", "payments", "orchestrator.go"):            1}},
			{"postDepositReversalTombstone(", map[string]int{
				filepath.Join("internal", "payments", "orchestrator.go"):                      1,
				filepath.Join("internal", "payments", "receipt.go"):                           1,
				filepath.Join("internal", "payments", "testsupport_tombstone_integration.go"): 1}},
		} {
			if n := codeLines(src, pin.needle); n != pin.allow[rel] && n > 0 {
				offenders = append(offenders, fmt.Sprintf("%s (%d code occurrence(s) of %s, allowed %d)", rel, n, pin.needle, pin.allow[rel]))
			}
		}
		if strings.Contains(src, imp) && !allowed[dir] && !staticAllowedFiles[filepath.ToSlash(rel)] && !inProoftest {
			offenders = append(offenders, rel+" (imports actorproof)")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return offenders
}

// codeLines counts the lines of src that contain needle and are not comment lines.
func codeLines(src, needle string) int {
	n := 0
	for _, l := range strings.Split(src, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "//") {
			continue
		}
		if strings.Contains(t, needle) {
			n++
		}
	}
	return n
}

// prooftestImporters are the integration-tag-only test-support packages that sign proofs for
// raw-SQL fixtures (ADR 0112 slice 3: every launch fixture write now needs a proof). They are
// behind the `integration` build tag and a separate static test pins that no non-test code
// imports them (internal/tenant/launch_fixture_pin_test.go).
var prooftestImporters = map[string]bool{
	filepath.Join("internal", "testsupport", "launchfix"):   true,
	filepath.Join("internal", "testsupport", "launchdrive"): true,
	// F4 residual R1: wdpath installs the dispatch-gate key hook (its E7 step records a signed fixture ALLOW).
	filepath.Join("internal", "testsupport", "wdpath"): true,
	// ADR 0115 S1: rgfix files the floor four-eyes flow (request / approve / apply proofs) for
	// SYNTHETIC-NOT-LEGAL fixture floors; integration-tagged, pinned by the same fixture pin.
	filepath.Join("internal", "testsupport", "rgfix"): true,
}

// staticAllowedFiles are the FILE-granular signer owners (the rest of their package may not import the issuer). F4
// residual R1 (migration 0171): the WD-RG-1 dispatch gate ALLOW is a signed SYSTEM attestation; only this one file of
// internal/withdrawal signs it (TestStatic_WithdrawalSignsOnlyInDispatchProof pins that it signs exactly once, and
// RecordDispatchGateDecision, its only caller, is pinned to four production call sites). Security review mandatory.
var staticAllowedFiles = map[string]bool{
	"internal/withdrawal/dispatchproof.go": true,
}

var staticAllowed = map[string]bool{
	filepath.Join("internal", "testsupport", "rgfix"): true, // ADR 0115 S1 fixture (see prooftestImporters)
	// ADR 0115 S2: internal/rg signs the floor four-eyes proofs (request / approve / reject / cancel /
	// apply) for the authenticated platform principal (security review mandatory).
	filepath.Join("internal", "rg"): true,
	// ADR 0112 slice 3: the launch authorisation service signs request / cancel / approve /
	// reject / suspend proofs for the authenticated principal (security review mandatory).
	filepath.Join("internal", "launchgov"): true,
	// Migration 0177 (MKT-DUAL-1): the licensing change service signs request / approve / reject / cancel / execute proofs
	// for the authenticated principal (security review mandatory).
	filepath.Join("internal", "licensingchange"): true,
	// ADR 0116 S3 (migrations 0140-0142): the AML officer service signs hold / case / four-eyes proofs for the authenticated
	// officer (security review mandatory). The fixture package builds officers through the K1 flow and signs nothing itself.
	filepath.Join("internal", "aml"): true,
	// Migration 0181 (lic10, G8/Q21): the residence maker-checker service signs the policy set / approve and the determination
	// approve proofs for the authenticated tenant or platform_acting staff member (security review mandatory).
	filepath.Join("internal", "residenceapproval"): true,
	// ADR 0119 S5 + S6 (section 10.4): the staff lifecycle service signs the platform-staff four-eyes proofs (request /
	// approve / reject / cancel) for the authenticated platform principal (security review mandatory).
	filepath.Join("internal", "stafflifecycle"):             true,
	filepath.Join("internal", "testsupport", "launchfix"):   true,
	filepath.Join("internal", "testsupport", "launchdrive"): true,
	filepath.Join("internal", "adjustment"):                 true,
	filepath.Join("internal", "payments"):                   true,
	filepath.Join("internal", "capability"):                 true,
	filepath.Join("internal", "actorproof"):                 true,
	filepath.Join("cmd", "platform-api"):                    true, // SetDefault wiring only
	filepath.Join("internal", "config"):                     true, // ParseKeySet validation only
	// canon.go uses only actorproof.TS (timestamp text encoding) for the
	// payout-instrument seal input; it never signs (B13-A, ADR 0111 s15).
	filepath.Join("internal", "payoutinstrument"): true,
	// Owner decision 5 (backup exclusion): the restore-check tool builds an Issuer from a RANDOM in-memory key only to run the
	// platform's own production startup gate (VerifyConfiguredInProduction) and to present an unsigned/invalid token to the
	// verifier inside a rolled-back transaction; it holds no real key, never signs a proof that is accepted and is not part of
	// platform-api (security review: docs/plans/backup-exclude-actor-secret-record.md, restorecheck).
	filepath.Join("deploy", "backup", "restorecheck"): true,
}

// The signing capability may be reached only from the packages that own
// four-eyes and K1 and from process wiring; prooftest must be unreachable from
// production code (SIGNED-ACTOR-PROOF: a single server-side issuer).
func TestStatic_OnlyFourEyesPackagesSign(t *testing.T) {
	if off := staticOffenders(t, filepath.Join("..", ".."), staticAllowed); len(off) > 0 {
		t.Fatalf("production code reaches the signer or the test signer outside the allowed owners: %v", off)
	}
}

// TestStatic_WithdrawalSignsOnlyInDispatchProof: dispatchproof.go signs exactly once (one Attach, no Sign), with the
// system dispatch-gate claims only.
func TestStatic_WithdrawalSignsOnlyInDispatchProof(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "withdrawal", "dispatchproof.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if n := codeLines(src, ".Attach("); n != 1 {
		t.Fatalf("dispatchproof.go must contain exactly one Attach call, found %d", n)
	}
	if n := codeLines(src, ".Sign("); n != 0 {
		t.Fatalf("dispatchproof.go must not call Sign directly, found %d", n)
	}
	for _, want := range []string{"actorproof.ScopeSystem", "actorproof.OpDispatchGateAllow", "actorproof.SystemActorDispatchGate()"} {
		if !strings.Contains(src, want) {
			t.Fatalf("dispatchproof.go must sign the system dispatch-gate claims (missing %s)", want)
		}
	}
	// Every other non-test file of internal/withdrawal stays free of the issuer.
	ents, err := os.ReadDir(filepath.Join("..", "..", "internal", "withdrawal"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") || n == "dispatchproof.go" {
			continue
		}
		fb, err := os.ReadFile(filepath.Join("..", "..", "internal", "withdrawal", n))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(fb), `"github.com/Diansalas/igaming-platform/internal/actorproof`) {
			t.Fatalf("internal/withdrawal/%s must not import the issuer", n)
		}
	}
}

// TestStatic_DepositTombstoneSignedOnlyHere (migration 0175): deposit_tombstone_proof.go signs exactly once (one Attach, no
// Sign), with the deposit-reversal-tombstone system claims only; the integration-only fixture carries the build tag (no
// production binary contains it) and signs nothing itself.
func TestStatic_DepositTombstoneSignedOnlyHere(t *testing.T) {
	read := func(rel ...string) string {
		b, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, rel...)...))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	src := read("internal", "payments", "deposit_tombstone_proof.go")
	if n := codeLines(src, ".Attach("); n != 1 {
		t.Fatalf("deposit_tombstone_proof.go must contain exactly one Attach call, found %d", n)
	}
	if n := codeLines(src, ".Sign("); n != 0 {
		t.Fatalf("deposit_tombstone_proof.go must not call Sign directly, found %d", n)
	}
	for _, want := range []string{"actorproof.ScopeSystem", "actorproof.OpDepositReversalTombstone", "actorproof.SystemActorDepositReversal()",
		"Target: receiptID.String()"} {
		if !strings.Contains(src, want) {
			t.Fatalf("deposit_tombstone_proof.go must sign the deposit tombstone system claims bound to the receipt (missing %s)", want)
		}
	}
	fix := read("internal", "payments", "testsupport_tombstone_integration.go")
	if !strings.HasPrefix(fix, "//go:build integration\n") {
		t.Fatal("testsupport_tombstone_integration.go must carry the integration build tag on its first line")
	}
	for _, banned := range []string{".Attach(", ".Sign(", "actorproof"} {
		if codeLines(fix, banned) != 0 {
			t.Fatalf("the fixture must sign only through postDepositReversalTombstone (found %s)", banned)
		}
	}
	// The verifier mirror: each system operation is admitted only for its own actor.
	for _, c := range []struct {
		op    string
		actor uuid.UUID
		ok    bool
	}{
		{OpDepositReversalTombstone, SystemActorDepositReversal(), true},
		{OpDispatchGateAllow, SystemActorDispatchGate(), true},
		{OpDepositReversalTombstone, SystemActorDispatchGate(), false},
		{OpDispatchGateAllow, SystemActorDepositReversal(), false},
		{"deposit_reversal_tombstone:other", SystemActorDepositReversal(), false},
		{OpPayoutReversalDecide, SystemActorDepositReversal(), false},
	} {
		err := Claims{Actor: c.actor, Scope: ScopeSystem, Tenant: uuid.New(), Operation: c.op, Target: uuid.NewString(),
			PayloadHash: strings.Repeat("a", 64)}.validate()
		if (err == nil) != c.ok {
			t.Fatalf("system claims %s by %s: want ok=%v, got %v", c.op, c.actor, c.ok, err)
		}
	}
	if err := (Claims{Actor: SystemActorDepositReversal(), Scope: ScopeTenant, Tenant: uuid.New(), Operation: OpDepositReversalTombstone,
		Target: uuid.NewString(), PayloadHash: strings.Repeat("a", 64)}).validate(); err == nil {
		t.Fatal("the deposit tombstone operation must not be signable for a staff scope")
	}
}

// Non-vacuity: the scan flags a planted offender of each kind.
func TestStatic_ScanIsNotVacuous(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/httpserver/x.go", "package x\nimport \"github.com/Diansalas/igaming-platform/internal/actorproof\"\n")
	write("internal/wallet/y.go", "package y\nimport _ \"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest\"\n")
	write("internal/risk/z.go", "package z\nvar _ = TestSessionProofHook\n")
	// A fourth use of the hook INSIDE internal/capability, a second caller of WithProofActor
	// in the SAME package as the allowed one, and a second PolicyCall construction.
	write("internal/capability/extra.go", "package capability\nvar _ = TestSessionProofHook\n")
	write("internal/httpserver/evil.go", "package x\nvar _ = WithProofActor(nil, 0)\n")
	write("internal/adjustment/evil.go", "package a\nvar _ = PolicyCall{}\n")
	// F4 residual R1: a SECOND file of internal/withdrawal importing the issuer, and a second reader of the
	// key-provisioning hook, are flagged; the pinned file is not.
	write("internal/withdrawal/other.go", "package withdrawal\nimport \"github.com/Diansalas/igaming-platform/internal/actorproof\"\n")
	write("internal/payments/evilhook.go", "package payments\nvar _ = actorproof.TestEnsureKeyHook\n")
	// Closure security review F-1 / F-3: a second builder of either system claim set, a second writer of the decision
	// table, a raw operation literal and a second caller of the deposit tombstone writer are flagged.
	write("internal/payments/evilclaim.go", "package payments\nvar _ = actorproof.OpDepositReversalTombstone\n")
	write("internal/httpserver/evilgate.go", "package x\nvar _ = actorproof.SystemActorDispatchGate()\n")
	write("internal/withdrawal/evilinsert.go", "package withdrawal\nconst q = `INSERT INTO withdrawal_gate_decisions (x) VALUES (1)`\n")
	write("internal/risk/evilop.go", "package risk\nconst op = \"withdrawal_dispatch_gate:allow\"\n")
	write("internal/payments/evilcaller.go", "package payments\nfunc f() { _, _ = postDepositReversalTombstone(nil, nil, x, p, r, id) }\n")
	// The legitimate shapes must NOT be flagged.
	write("internal/capability/proof.go", "package capability\n// TestSessionProofHook comment\nvar TestSessionProofHook func()\nfunc f() {\n\tif TestSessionProofHook != nil {\n\t\treturn TestSessionProofHook()\n\t}\n}\nfunc WithProofActor(ctx int) {}\n")
	write("internal/httpserver/capability_routes.go", "package x\nfunc g() {\n\t_ = capability.WithProofActor(a)\n\t_ = capability.WithProofActor(b)\n}\n")
	write("internal/adjustment/policy.go", "package a\nvar _ = PolicyCall{}\n")
	write("internal/withdrawal/dispatchproof.go", "package withdrawal\nimport \"github.com/Diansalas/igaming-platform/internal/actorproof\"\nvar _ = actorproof.TestEnsureKeyHook\nvar _ = actorproof.Claims{Actor: actorproof.SystemActorDispatchGate(), Operation: actorproof.OpDispatchGateAllow}\n")
	write("internal/withdrawal/dispatchgate.go", "package withdrawal\nconst q = `INSERT INTO withdrawal_gate_decisions (x) VALUES (1)`\n")
	write("internal/payments/deposit_tombstone_proof.go", "package payments\nvar _ = actorproof.Claims{Actor: actorproof.SystemActorDepositReversal(), Operation: actorproof.OpDepositReversalTombstone}\n")
	write("internal/adjustment/ok.go", "package ok\nimport \"github.com/Diansalas/igaming-platform/internal/actorproof\"\n")
	write("internal/wallet/y_test.go", "package y\nimport _ \"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest\"\n")
	off := staticOffenders(t, root, staticAllowed)
	if len(off) != 13 {
		t.Fatalf("want exactly the 13 planted production offenders flagged (and no legitimate shape), got %v", off)
	}
}

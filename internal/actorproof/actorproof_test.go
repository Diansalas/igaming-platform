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

// The signing capability may be reached only from the packages that own
// four-eyes and from process wiring (SIGNED-ACTOR-PROOF: a single server-side
// issuer). A new caller anywhere else fails this test and must go through
// review (ADR 0110).
func TestStatic_OnlyFourEyesPackagesSign(t *testing.T) {
	allowed := map[string]bool{
		filepath.Join("internal", "adjustment"):  true,
		filepath.Join("internal", "payments"):    true,
		filepath.Join("internal", "actorproof"):  true,
		filepath.Join("cmd", "platform-api"):     true, // SetDefault wiring only
		filepath.Join("internal", "config"):      true, // ParseKeySet validation only
		filepath.Join("internal", "testsupport"): true,
	}
	root := filepath.Join("..", "..")
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
		if !strings.Contains(src, `"github.com/Diansalas/igaming-platform/internal/actorproof"`) {
			return nil
		}
		if allowed[filepath.Dir(rel)] || strings.HasPrefix(rel, filepath.Join("internal", "actorproof")+string(filepath.Separator)) {
			return nil
		}
		offenders = append(offenders, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("non-test packages importing internal/actorproof outside the four-eyes owners: %v", offenders)
	}
}

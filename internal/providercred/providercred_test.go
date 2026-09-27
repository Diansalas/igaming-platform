package providercred

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/providers/httpclient"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
	"github.com/Diansalas/igaming-platform/internal/secretstore/memstore"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// TestFingerprint_KnownAnswerVector pins the fp1: construction to an
// independently computed vector (Python hmac/hashlib):
// HMAC-SHA256(key = 0x00..0x1f, "igaming/provider-credential-fingerprint/v1"
// || 0x00 || "w2a-known-answer-vector"). The key and input are public test
// vectors, not secrets.
func TestFingerprint_KnownAnswerVector(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	k, err := NewFingerprintKey(key)
	if err != nil {
		t.Fatal(err)
	}
	const want = "fp1:badbebfe2b132924e93ad934e8bfe6b7d587e227ae8954ef3a662029bdc77ee9"
	if got := k.Fingerprint([]byte("w2a-known-answer-vector")); got != want {
		t.Fatalf("fingerprint = %s, want %s", got, want)
	}
	if !ValidFingerprint(want) {
		t.Fatal("the stored shape must validate")
	}
	if _, err := NewFingerprintKey(key[:31]); err == nil {
		t.Fatal("a 31-byte key must be refused")
	}
}

// TestFingerprint_InputHasNoTenant: the fingerprint is a function of the
// key and the secret only (ADR 0093 §2) - the same secret registered by
// two tenants has the same fingerprint, which is what lets the GLOBAL
// unique key refuse the second binding.
func TestFingerprint_InputHasNoTenant(t *testing.T) {
	k, _ := NewFingerprintKey(randomBytes(t, 32))
	secret := randomBytes(t, 32)
	if k.Fingerprint(secret) != k.Fingerprint(append([]byte(nil), secret...)) {
		t.Fatal("the fingerprint must be deterministic")
	}
	typ := reflect.TypeOf(k.Fingerprint)
	if typ.NumIn() != 1 || typ.In(0) != reflect.TypeOf([]byte(nil)) {
		t.Fatalf("Fingerprint must take only the secret bytes, signature %s", typ)
	}
	other, _ := NewFingerprintKey(randomBytes(t, 32))
	if k.Fingerprint(secret) == other.Fingerprint(secret) {
		t.Fatal("the fingerprint must be keyed")
	}
}

func TestConfirmation_ConstantTimeShapeAndMatch(t *testing.T) {
	secret := randomBytes(t, 32)
	sum := sha256.Sum256(secret)
	good := "sha256:" + hex.EncodeToString(sum[:])
	if !ConfirmationMatches(secret, good) {
		t.Fatal("the unkeyed sha256 confirmation must match")
	}
	for _, bad := range []string{"", "sha256:", good[:len(good)-1], strings.ToUpper(good), "sha1:" + hex.EncodeToString(sum[:20]),
		"sha256:" + hex.EncodeToString(randomBytes(t, 32))} {
		if ConfirmationMatches(secret, bad) {
			t.Fatalf("%q must not match", bad)
		}
	}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// TestSecretTypes_Redaction (security review §6, C15): %v, %+v, %#v, %s,
// %x, slog and json.Marshal never render the secret of a CredentialSet, an
// OutboundCredential, a store result, the fingerprint key or a per-call
// Authenticator.
func TestSecretTypes_Redaction(t *testing.T) {
	raw := randomBytes(t, 32)
	cred := webhookauth.Credential{TenantID: uuid.New(), ProviderID: "p", KeyID: "k", Secret: raw, Fingerprint: "fp1:x"}
	prev := cred
	key, _ := NewFingerprintKey(raw)
	values := map[string]any{
		"Credential":          cred,
		"CredentialSet":       webhookauth.CredentialSet{Active: cred, Previous: &prev},
		"*CredentialSet":      &webhookauth.CredentialSet{Active: cred},
		"OutboundCredential":  OutboundCredential{TenantID: uuid.New(), secret: secretstore.NewSecret(raw)},
		"secretstore.Secret":  secretstore.NewSecret(raw),
		"FingerprintKey":      key,
		"HeaderAuthenticator": httpclient.NewHeaderAuthenticator("X-Api-Key", hex.EncodeToString(raw)),
		"config.SecretValue":  config.NewSecretValue(hex.EncodeToString(raw)),
	}
	forms := []string{string(raw), hex.EncodeToString(raw), strings.ToUpper(hex.EncodeToString(raw)), fmt.Sprintf("%v", raw)}
	for name, v := range values {
		var outs []string
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%q"} {
			outs = append(outs, fmt.Sprintf(verb, v))
		}
		var sb strings.Builder
		slog.New(slog.NewJSONHandler(&sb, nil)).Info("m", "v", v)
		slog.New(slog.NewTextHandler(&sb, nil)).Info("m", "v", v)
		outs = append(outs, sb.String())
		j, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		outs = append(outs, string(j))
		for _, out := range outs {
			for _, form := range forms {
				if strings.Contains(out, form) {
					t.Fatalf("%s rendered its secret in %q", name, out)
				}
			}
		}
	}
}

// TestOutbound_NoCredentialOnLongLivedTypes and its credentialBearing
// helper moved to outbound_credential_bearing_test.go (package
// providercred_test) so this file can stop importing internal/casino:
// casino.LaunchGame now resolves its own outbound credential via
// providercred.TenantTxRunner/OutboundCredentialResolver (ADR 0095 §15.1,
// PRH-I2), so internal/casino imports internal/providercred and an
// in-package (package providercred) test file can no longer import casino
// without an import cycle. Go's external test package convention
// (foo_test importing foo AND anything foo imports) is the standard way to
// break exactly this shape of cycle, and is used only for that one test -
// every other test here still needs package providercred's own unexported
// fields (e.g. OutboundCredential.secret) and stays in-package.

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// TestOutbound_APIKeyEnvVarRemoved (security review §2): no Go code names
// APIKeyEnvVar / ResolveAPIKey any more, and internal/providers' non-test
// files read no process environment.
func TestOutbound_APIKeyEnvVarRemoved(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		inProviders := strings.HasPrefix(rel, filepath.Join("internal", "providers")+string(filepath.Separator)) && !strings.HasSuffix(path, "_test.go")
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if x.Name == "APIKeyEnvVar" || x.Name == "ResolveAPIKey" {
					t.Errorf("%s references %s", rel, x.Name)
				}
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && id.Name == "os" && inProviders &&
					(x.Sel.Name == "Getenv" || x.Sel.Name == "LookupEnv" || x.Sel.Name == "Environ") {
					t.Errorf("%s reads the process environment (os.%s)", rel, x.Sel.Name)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTransitionReasons_ClosedEnums(t *testing.T) {
	for action, ok := range map[string][]string{
		ActionVerifyOnly: {TransitionReasonRotation},
		ActionShorten:    {ShortenReasonOverlapShortened, ShortenReasonSuspectedCompromise, ShortenReasonVendorInstruction},
		ActionRevoke: {RevokeReasonRotationComplete, RevokeReasonSuspectedCompromise, RevokeReasonConfirmedCompromise,
			RevokeReasonVendorOffboarded, RevokeReasonMisregistration, RevokeReasonTenantRequest},
	} {
		for _, r := range ok {
			if !ValidTransitionReason(action, r) {
				t.Fatalf("%s/%s must be valid", action, r)
			}
		}
		if ValidTransitionReason(action, "") || ValidTransitionReason(action, "because") {
			t.Fatalf("%s: empty/unknown reasons must be invalid", action)
		}
	}
	if ValidTransitionReason("reactivate", TransitionReasonRotation) {
		t.Fatal("unknown actions must be invalid")
	}
}

// TestNew_AbsentKeyOrBackendIsNotConstructed: no fingerprint key, or no
// backend, yields a nil subsystem (fail closed), and every nil-safe
// accessor fails closed.
func TestNew_AbsentKeyOrBackendIsNotConstructed(t *testing.T) {
	router, _ := memstore.NewRouter(memstore.New())
	empty, _ := memstore.NewRouter()
	withKey := config.Config{ProviderCredentialFingerprintKey: config.NewSecretValue(hex.EncodeToString(randomBytes(t, 32)))}
	for name, tc := range map[string]struct {
		cfg    config.Config
		router *secretstore.Router
	}{
		"no key":     {config.Config{}, router},
		"no backend": {withKey, empty},
		"nil router": {withKey, nil},
	} {
		s, err := New(tc.cfg, tc.router)
		if err != nil || s != nil {
			t.Fatalf("%s: want (nil, nil), got (%v, %v)", name, s, err)
		}
		if s.Resolver("casino") != nil || s.Outbound("casino") != nil {
			t.Fatalf("%s: a nil subsystem must expose no resolver", name)
		}
	}
	s, err := New(withKey, router)
	if err != nil || s == nil || s.Resolver("casino") == nil || s.Resolver("sportsbook") != nil {
		t.Fatalf("with key and backend: %v %v", s, err)
	}
	_ = time.Second
}

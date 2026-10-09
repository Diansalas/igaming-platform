package payments

// ADR 0111 24: the destination-echo declaration is mandatory, explicit and has no default. No database.

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
)

func TestEchoDeclaration_UnsetIsRefusedForEveryWithdrawalCapableAdapter(t *testing.T) {
	// Synthetic (MOCK) with an unset declaration: refused (the field is universally mandatory).
	mock := NewMockProvider("mock-echo-unset", "EUR")
	c := mock.Capabilities()
	c.Manifest.DestinationEchoSemantics = payoutinstrument.DestinationEchoUnset
	if err := validateManifest(mock, c); !errors.Is(err, ErrManifestRegistrationRefused) {
		t.Fatalf("synthetic + unset: got %v, want ErrManifestRegistrationRefused", err)
	}
	// Out-of-range value: refused too.
	c.Manifest.DestinationEchoSemantics = payoutinstrument.DestinationEchoSemantics(99)
	if err := validateManifest(mock, c); !errors.Is(err, ErrManifestRegistrationRefused) {
		t.Fatalf("synthetic + out of range: got %v", err)
	}
	// Non-Synthetic with an unset declaration (everything else satisfied): refused.
	real := AdapterCapability{ProviderID: "real-psp-echo", SupportsWithdrawal: true,
		Manifest: OperationManifest{CallbackEchoesMerchantReference: true}}
	if err := validateManifest(nonSyntheticFakeProvider{capability: real}, real); !errors.Is(err, ErrManifestRegistrationRefused) {
		t.Fatalf("non-synthetic + unset: got %v", err)
	}
	// Both explicit states register.
	for _, s := range []payoutinstrument.DestinationEchoSemantics{payoutinstrument.DestinationEchoSupported, payoutinstrument.DestinationEchoUnsupported} {
		real.Manifest.DestinationEchoSemantics = s
		if err := validateManifest(nonSyntheticFakeProvider{capability: real}, real); err != nil {
			t.Fatalf("non-synthetic + %s: %v", s, err)
		}
	}
	// A deposit-only adapter makes no payout and needs no payout declaration.
	dep := AdapterCapability{ProviderID: "real-psp-dep", SupportsDeposit: true,
		Manifest: OperationManifest{SupportsDeposit: true, CallbackEchoesMerchantReference: true}}
	if err := validateManifest(nonSyntheticFakeProvider{capability: dep}, dep); err != nil {
		t.Fatalf("deposit-only adapter: %v", err)
	}
}

// The shipped MOCK declares explicitly (Unsupported) and SetManifest cannot erase it by leaving it unset.
func TestEchoDeclaration_MockDeclaresExplicitly(t *testing.T) {
	m := NewMockProvider("mock-echo-decl", "EUR")
	if got := m.Capabilities().Manifest.DestinationEchoSemantics; got != payoutinstrument.DestinationEchoUnsupported {
		t.Fatalf("mock declaration = %s", got)
	}
	m.SetManifest(OperationManifest{SupportsDeposit: true})
	if !m.Capabilities().Manifest.DestinationEchoSemantics.Valid() {
		t.Fatal("SetManifest erased the mock's declaration")
	}
	m.SetManifest(OperationManifest{DestinationEchoSemantics: payoutinstrument.DestinationEchoSupported})
	if m.Capabilities().Manifest.DestinationEchoSemantics != payoutinstrument.DestinationEchoSupported {
		t.Fatal("an explicit declaration must be kept")
	}
}

// Static pin: the boolean is gone, and the payments evidence code never calls CompareEcho directly: every echo verdict goes
// through EvaluateEcho, which applies the declaration (an Unsupported adapter's echo can never be a match).
func TestEchoDeclaration_Static_NoBooleanNoDirectCompare(t *testing.T) {
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range entries {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		if strings.Contains(string(src), "EchoesDestinationFingerprint") && !strings.HasSuffix(f, "contract.go") {
			t.Errorf("%s still references the retired EchoesDestinationFingerprint boolean", f)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "CompareEcho" {
				t.Errorf("%s calls CompareEcho directly; use payoutinstrument.EvaluateEcho (it applies the declaration)", f)
			}
			return true
		})
	}
}

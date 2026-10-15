package httpserver

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/marketgate"
)

// A8 static pins. The gate is a mandatory dependency of exactly three
// handlers; these tests fail if a future edit drops the nil-check (which
// makes an unwired deployment fail closed) or stops handing the guard to the
// orchestrator that owns the in-transaction enforcement point.

func funcDecl(t *testing.T, file, name string) *ast.FuncDecl {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name {
			return fd
		}
	}
	t.Fatalf("%s not found in %s", name, file)
	return nil
}

// usesDepsMarketGateNilCheck: the handler contains `deps.MarketGate == nil`.
func usesDepsMarketGateNilCheck(fd *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fd, func(n ast.Node) bool {
		be, ok := n.(*ast.BinaryExpr)
		if !ok || be.Op != token.EQL {
			return true
		}
		sel, ok := be.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "MarketGate" {
			return true
		}
		if id, ok := be.Y.(*ast.Ident); ok && id.Name == "nil" {
			found = true
		}
		return true
	})
	return found
}

func setsCompositeKey(fd *ast.FuncDecl, key string) bool {
	found := false
	ast.Inspect(fd, func(n ast.Node) bool {
		if kv, ok := n.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == key {
				found = true
			}
		}
		return true
	})
	return found
}

func TestMarketGate_ThreeHandlersFailClosedWithoutAGate(t *testing.T) {
	for _, c := range []struct{ file, fn string }{
		{"auth_routes.go", "newRegisterHandler"},
		{"deposit_handlers.go", "newInitiateDepositHandler"},
		{"casino_handlers.go", "newLaunchCasinoGameHandler"},
	} {
		if !usesDepsMarketGateNilCheck(funcDecl(t, c.file, c.fn)) {
			t.Errorf("%s must refuse (503) when deps.MarketGate == nil", c.fn)
		}
	}
}

func TestMarketGate_DepositAndLaunchHandBackTheGuardToTheOrchestrator(t *testing.T) {
	if !setsCompositeKey(funcDecl(t, "deposit_handlers.go", "newInitiateDepositHandler"), "MarketGuard") {
		t.Error("newInitiateDepositHandler must set InitiateDepositParams.MarketGuard (the in-phase-A enforcement point)")
	}
	if !setsCompositeKey(funcDecl(t, "casino_handlers.go", "newLaunchCasinoGameHandler"), "MarketGuard") {
		t.Error("newLaunchCasinoGameHandler must set LaunchGameParams.MarketGuard (the in-phase-A enforcement point)")
	}
}

// Both refusal paths of the launch handler (its own earlier evaluation and
// LaunchGame's Denied result) must answer byte-for-byte alike.
func TestMarketGate_LaunchDenialMapsToTheGenericRefusal(t *testing.T) {
	rec := httptest.NewRecorder()
	writeCasinoLaunchDenial(rec, "req-1", casino.LaunchGameResult{Denied: true, DenialCode: casino.DenialCodeMarketNotAvailable})
	direct := httptest.NewRecorder()
	apierror.Write(direct, "req-1", apierror.CodeMarketNotAvailable, marketgate.PlayerMessage)
	if rec.Code != http.StatusForbidden || rec.Body.String() != direct.Body.String() {
		t.Fatalf("launch denial must equal the handler-level refusal:\n got  %d %s\n want %d %s", rec.Code, rec.Body, direct.Code, direct.Body)
	}
	var e apierror.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Code != apierror.CodeMarketNotAvailable || e.Message != marketgate.PlayerMessage {
		t.Fatalf("unexpected body %s", rec.Body)
	}
}

func TestMarketGate_ApierrorCodeIsA403(t *testing.T) {
	rec := httptest.NewRecorder()
	apierror.Write(rec, "r", apierror.CodeMarketNotAvailable, marketgate.PlayerMessage)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("MARKET_NOT_AVAILABLE must be a 403, got %d", rec.Code)
	}
	if string(apierror.CodeMarketNotAvailable) != "MARKET_NOT_AVAILABLE" {
		t.Fatalf("the wire code is a public contract, got %q", apierror.CodeMarketNotAvailable)
	}
}

package httpserver

import (
	"go/ast"
	"go/token"
	"testing"
)

// SR8-5: the registration handler pre-checks the market gate BEFORE the Argon2id hash, so a refused
// registration costs no hash. The authoritative check inside the account-creation transaction stays.
func TestRegisterHandler_MarketPreCheckPrecedesThePasswordHash(t *testing.T) {
	fd := funcDecl(t, "auth_routes.go", "newRegisterHandler")
	var hashPos, firstGuardPos, guardCalls = token.NoPos, token.NoPos, 0
	ast.Inspect(fd, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if x, ok := fn.X.(*ast.Ident); ok && x.Name == "auth" && fn.Sel.Name == "HashPassword" {
				hashPos = call.Pos()
			}
		case *ast.Ident:
			if fn.Name == "marketGuard" {
				guardCalls++
				if firstGuardPos == token.NoPos || call.Pos() < firstGuardPos {
					firstGuardPos = call.Pos()
				}
			}
		}
		return true
	})
	if hashPos == token.NoPos || firstGuardPos == token.NoPos {
		t.Fatal("could not find auth.HashPassword and the marketGuard calls in newRegisterHandler")
	}
	if firstGuardPos > hashPos {
		t.Error("the market pre-check must run before auth.HashPassword (SR8-5)")
	}
	// ... and a refusal there must RETURN before the hash: an `if preDenied { ...; return }` precedes it.
	refusesEarly := false
	ast.Inspect(fd, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || ifs.Pos() > hashPos {
			return true
		}
		if id, ok := ifs.Cond.(*ast.Ident); ok && id.Name == "preDenied" {
			for _, st := range ifs.Body.List {
				if _, isRet := st.(*ast.ReturnStmt); isRet {
					refusesEarly = true
				}
			}
		}
		return true
	})
	if !refusesEarly {
		t.Error("a pre-check refusal must return before auth.HashPassword (an `if preDenied { ...; return }` block)")
	}
	// SR8R-3: the authoritative check sits in the SAME transaction function literal as
	// RegisterPlayerWithResolution, and runs before it.
	inTx := false
	ast.Inspect(fd, func(n ast.Node) bool {
		lit, ok := n.(*ast.FuncLit)
		if !ok {
			return true
		}
		var guardPos, registerPos = token.NoPos, token.NoPos
		ast.Inspect(lit.Body, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				if fn.Name == "marketGuard" && guardPos == token.NoPos {
					guardPos = call.Pos()
				}
			case *ast.SelectorExpr:
				if fn.Sel.Name == "RegisterPlayerWithResolution" {
					registerPos = call.Pos()
				}
			}
			return true
		})
		if registerPos != token.NoPos && guardPos != token.NoPos && guardPos < registerPos {
			inTx = true
		}
		return true
	})
	if !inTx {
		t.Error("a marketGuard call must precede RegisterPlayerWithResolution inside the same transaction function (SR8R-3)")
	}
	if guardCalls < 2 {
		t.Errorf("the guard must run twice (pre-check and the authoritative in-transaction check), found %d calls", guardCalls)
	}
}

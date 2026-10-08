package payments

import (
	"go/ast"
	"go/token"
	"testing"
)

// H(8) AST guard: pins the call sites of the BRAND gate (ADR 0095 section 45) so a refactor
// cannot silently drop one. Position checks use token offsets in the same function body.
func h8FuncCalls(t *testing.T, file, fn string) map[string][]token.Pos {
	t.Helper()
	_, f := parseSrc(t, file, "")
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn {
			continue
		}
		out := map[string][]token.Pos{}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				out[calleeName(c)] = append(out[calleeName(c)], c.Pos())
			}
			return true
		})
		return out
	}
	t.Fatalf("%s: func %s not found (vacuity guard)", file, fn)
	return nil
}

func TestH8_BrandGateCallSites_Pinned(t *testing.T) {
	// Decision 19: both creation sites call the brand skip, before the child insert.
	for _, c := range []struct{ file, fn string }{
		{"drive.go", "applyDepositCallResult"},
		{"sweeper.go", "applyStatusEvidence"},
	} {
		calls := h8FuncCalls(t, c.file, c.fn)
		skip, ins := calls["skipCascadeChildForBrand"], calls["insertCascadeAttemptIfEligible"]
		if len(skip) != 1 || len(ins) != 1 || skip[0] > ins[0] {
			t.Errorf("%s.%s must call skipCascadeChildForBrand exactly once, before insertCascadeAttemptIfEligible (skip=%d insert=%d)", c.file, c.fn, len(skip), len(ins))
		}
	}
	// Decision 20: the sweeper claim tx reads the brand before the claim CAS.
	calls := h8FuncCalls(t, "drive.go", "driveCreatedAttempt")
	rb, claim := calls["RequireBrandActive"], calls["ClaimCreatedForSubmission"]
	if len(rb) != 1 || len(claim) != 1 || rb[0] > claim[0] {
		t.Errorf("driveCreatedAttempt must call tenant.RequireBrandActive once, before ClaimCreatedForSubmission (brand=%d claim=%d)", len(rb), len(claim))
	}
	// Pre-read (early skip) and the creation helper's own primitive.
	if len(h8FuncCalls(t, "sweeper_resolution_only.go", "deferIfResolutionOnly")["brandResolutionOnly"]) != 1 {
		t.Error("deferIfResolutionOnly must call brandResolutionOnly exactly once")
	}
	if len(h8FuncCalls(t, "sweeper_resolution_only.go", "skipCascadeChildForBrand")["RequireBrandActive"]) != 1 {
		t.Error("skipCascadeChildForBrand must call tenant.RequireBrandActive exactly once")
	}
	// Decision 23: the brand helpers never touch the tenant helpers.
	for _, fn := range []string{"skipCascadeChildForBrand", "brandResolutionOnly"} {
		if len(h8FuncCalls(t, "sweeper_resolution_only.go", fn)["tenantResolutionOnly"]) != 0 {
			t.Errorf("%s must not call tenantResolutionOnly (tenant and brand are separate checks)", fn)
		}
	}
}

// R17 (ADR 0095 section 47): the receipt-site call is pinned. The cascade block in
// applyResolvedReceiptEvidence reaches insertCascadeAttemptIfEligible only after
// gateReceiptCascadeChild (once, before the insert); the gate loads the REAL intent
// (GetDepositIntentByID) before the tenant helper and the brand helper, in that order, and passes
// that loaded value (never a DepositIntent literal / the receipt-site stub) to the brand helper.
func TestH8_ReceiptSiteGate_Pinned(t *testing.T) {
	calls := h8FuncCalls(t, "receipt.go", "applyResolvedReceiptEvidence")
	gate, ins := calls["gateReceiptCascadeChild"], calls["insertCascadeAttemptIfEligible"]
	if len(gate) != 1 || len(ins) != 1 || gate[0] > ins[0] {
		t.Errorf("applyResolvedReceiptEvidence must call gateReceiptCascadeChild exactly once, before insertCascadeAttemptIfEligible (gate=%d insert=%d)", len(gate), len(ins))
	}
	g := h8FuncCalls(t, "sweeper_resolution_only.go", "gateReceiptCascadeChild")
	load, ten, br := g["GetDepositIntentByID"], g["skipCascadeChildForResolutionOnly"], g["skipCascadeChildForBrand"]
	if len(load) != 1 || len(ten) != 1 || len(br) != 1 || !(load[0] < ten[0] && ten[0] < br[0]) {
		t.Errorf("gateReceiptCascadeChild must load the intent, then the tenant helper, then the brand helper (load=%d tenant=%d brand=%d)", len(load), len(ten), len(br))
	}
	// The brand helper's intent argument must be the loaded variable, not a composite literal.
	_, f := parseSrc(t, "sweeper_resolution_only.go", "")
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "gateReceiptCascadeChild" {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok && calleeName(c) == "skipCascadeChildForBrand" {
				if id, ok := c.Args[len(c.Args)-1].(*ast.Ident); !ok || id.Name != "intent" {
					t.Error("skipCascadeChildForBrand must receive the loaded `intent` variable, not a stub/literal")
				}
			}
			return true
		})
	}
	// Neither helper may touch the other's primitive (decision 23).
	if len(h8FuncCalls(t, "sweeper_resolution_only.go", "skipCascadeChildForResolutionOnly")["RequireBrandActive"]) != 0 {
		t.Error("skipCascadeChildForResolutionOnly must not read the brand")
	}
}

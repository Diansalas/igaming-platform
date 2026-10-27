package risk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func baseAuthoringParams() CreateRuleParams {
	exp := int16(2)
	return CreateRuleParams{
		Operation: OperationCasinoBet, LimitKind: LimitMaxAmount, TimeWindow: WindowTransaction,
		Threshold: 100, ThresholdExponent: &exp, CreatedByActorID: uuid.New(),
	}
}

// G6/G7 (licensing enforcement slice 3): rules that would never be evaluated
// (deposit) or that would refuse every evaluation (jurisdiction-scoped, while no
// player-scoped jurisdiction can be resolved) are refused at authoring time,
// before any database access (nil tx proves nothing was attempted).
func TestCreateRule_RefusesUnevaluatedOrUnresolvableScopesAtAuthoring(t *testing.T) {
	for _, op := range []Operation{OperationCasinoLaunch, OperationCasinoBet, OperationWithdrawal, OperationSportsbookBet, OperationBonusGrant, OperationBonusConversion} {
		p := baseAuthoringParams()
		p.Operation = op
		p.JurisdictionCode = "KM-ANJ"
		_, err := CreateRule(context.Background(), nil, p)
		if !errors.Is(err, ErrJurisdictionScopeUnsupported) || !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s jurisdiction-scoped: want ErrJurisdictionScopeUnsupported (an ErrInvalidInput), got %v", op, err)
		}
		if !strings.Contains(err.Error(), "jurisdiction_scope_unsupported") {
			t.Fatalf("error must carry the stable code, got %q", err)
		}
	}
	p := baseAuthoringParams()
	p.Operation = OperationDeposit
	_, err := CreateRule(context.Background(), nil, p)
	if !errors.Is(err, ErrOperationNotEvaluated) || !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "operation_not_evaluated") {
		t.Fatalf("deposit: want ErrOperationNotEvaluated, got %v", err)
	}
}

// The bypass exists only so tests can still seed the legacy fixtures on
// purpose; no non-test source file may set it.
func TestAuthoringBypassIsNeverSetByProductionCode(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		if strings.Contains(filepath.ToSlash(path), "/.claude/") || strings.HasSuffix(path, "policy_service.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(b), "TestOnlyBypassAuthoringRefusals") {
			t.Errorf("%s references TestOnlyBypassAuthoringRefusals; production code must never bypass the authoring refusals", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

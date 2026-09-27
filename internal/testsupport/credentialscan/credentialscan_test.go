package credentialscan

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
)

func TestScan_CatchesOutboundCredentialField(t *testing.T) {
	type badAdapter struct {
		cred providercred.OutboundCredential
	}
	v := &badAdapter{cred: providercred.NewMockOutboundCredential(uuid.Nil, "payments", "p")}
	if got := Scan(v); len(got) == 0 {
		t.Fatal("expected a violation for a field holding providercred.OutboundCredential, got none")
	}
}

func TestScan_CatchesSecretField(t *testing.T) {
	type badAdapter struct {
		secret secretstore.Secret
	}
	v := &badAdapter{}
	if got := Scan(v); len(got) == 0 {
		t.Fatal("expected a violation for a field holding secretstore.Secret, got none")
	}
	_ = v.secret // the field is deliberately never read - its mere TYPE is what Scan must catch
}

func TestScan_CatchesNestedCredentialThroughPointerAndSlice(t *testing.T) {
	type inner struct {
		cred providercred.OutboundCredential
	}
	type badAdapter struct {
		items []*inner
	}
	v := &badAdapter{items: []*inner{{cred: providercred.NewMockOutboundCredential(uuid.Nil, "casino", "p")}}}
	if got := Scan(v); len(got) == 0 {
		t.Fatal("expected a violation for a credential nested through a pointer inside a slice, got none")
	}
}

func TestScan_CatchesNonNilFuncField(t *testing.T) {
	type badAdapter struct {
		fn func()
	}
	v := &badAdapter{fn: func() {}}
	if got := Scan(v); len(got) == 0 {
		t.Fatal("expected a violation for a non-nil, non-allow-listed func field, got none")
	}
}

func TestScan_CleanValueProducesNoViolations(t *testing.T) {
	type cleanAdapter struct {
		ID     string
		Amount int64
		Tags   []string
	}
	v := &cleanAdapter{ID: "p", Amount: 1, Tags: []string{"a", "b"}}
	if got := Scan(v); len(got) != 0 {
		t.Fatalf("expected no violations for a clean value, got %v", got)
	}
}

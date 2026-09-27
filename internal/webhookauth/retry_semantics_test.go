package webhookauth

import "testing"

type retrySemanticsSynthetic struct{}

func (retrySemanticsSynthetic) SyntheticComponent() {}

type retrySemanticsUndeclared struct{}

type retrySemanticsDeclared struct{ sem WebhookRetrySemantics }

func (r retrySemanticsDeclared) WebhookRetrySemantics() (WebhookRetrySemantics, bool) {
	return r.sem, true
}

type retrySemanticsFalseDeclared struct{}

func (retrySemanticsFalseDeclared) WebhookRetrySemantics() (WebhookRetrySemantics, bool) {
	return WebhookRetrySemantics{}, false
}

// TestRequireRetrySemantics_SyntheticExempt: a Synthetic (MOCK) adapter
// needs no declaration (ADR 0097 §6.3 "MOCK adapters: 429/503 as in
// §6.1").
func TestRequireRetrySemantics_SyntheticExempt(t *testing.T) {
	adapters := map[string]any{"mock": retrySemanticsSynthetic{}}
	if err := RequireRetrySemantics("payments", adapters); err != nil {
		t.Fatalf("a Synthetic adapter must not require a declaration: %v", err)
	}
}

// TestRequireRetrySemantics_UndeclaredNonSyntheticFailsClosed is T14's
// registration half: an undeclared non-MOCK adapter fails registration.
func TestRequireRetrySemantics_UndeclaredNonSyntheticFailsClosed(t *testing.T) {
	adapters := map[string]any{"real": retrySemanticsUndeclared{}}
	if err := RequireRetrySemantics("payments", adapters); err == nil {
		t.Fatal("an undeclared non-Synthetic adapter must fail registration (fail closed)")
	}
}

// TestRequireRetrySemantics_DeclaredFalseFailsClosed: implementing the
// interface but returning ok=false must still fail closed - a real
// vendor's absent declaration is not a permissive default.
func TestRequireRetrySemantics_DeclaredFalseFailsClosed(t *testing.T) {
	adapters := map[string]any{"real": retrySemanticsFalseDeclared{}}
	if err := RequireRetrySemantics("payments", adapters); err == nil {
		t.Fatal("ok=false from RetrySemanticsSource must fail registration")
	}
}

// TestRequireRetrySemantics_DeclaredAccepted: a non-MOCK adapter that
// properly declares its semantics registers successfully.
func TestRequireRetrySemantics_DeclaredAccepted(t *testing.T) {
	adapters := map[string]any{
		"real": retrySemanticsDeclared{sem: WebhookRetrySemantics{Retries429: true, Retries503: true, HonorsRetryAfter: true}},
	}
	if err := RequireRetrySemantics("payments", adapters); err != nil {
		t.Fatalf("a properly declared adapter must register: %v", err)
	}
}

// TestMustRequireRetrySemantics_Panics proves the Must wrapper panics
// (startup refusal) rather than silently continuing.
func TestMustRequireRetrySemantics_Panics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustRequireRetrySemantics must panic on an undeclared non-Synthetic adapter")
		}
	}()
	MustRequireRetrySemantics("payments", map[string]any{"real": retrySemanticsUndeclared{}})
}

// TestNilAdapterSkipped: a nil registration (a domain with no adapter
// wired) is not "undeclared" - it is skipped, mirroring
// NewAdapterSchemeSet's identical nil handling.
func TestNilAdapterSkipped(t *testing.T) {
	adapters := map[string]any{"none": nil}
	if err := RequireRetrySemantics("payments", adapters); err != nil {
		t.Fatalf("a nil adapter must be skipped, not treated as undeclared: %v", err)
	}
}

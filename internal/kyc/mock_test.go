package kyc

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

func TestMockMalwareScanner_CleanContentReportsClean(t *testing.T) {
	s := NewMockMalwareScanner()
	clean, err := s.Scan(context.Background(), tinyPNGBytes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !clean {
		t.Error("expected ordinary content to report clean")
	}
}

func TestMockMalwareScanner_EICARStringReportsInfected(t *testing.T) {
	s := NewMockMalwareScanner()
	content := append([]byte{}, tinyPNGBytes...)
	content = append(content, []byte(eicarTestString)...)
	clean, err := s.Scan(context.Background(), content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if clean {
		t.Error("expected EICAR-containing content to report infected")
	}
}

func TestMockDocumentStorageProvider_StoreThenRetrieveRoundTrips(t *testing.T) {
	s := NewMockDocumentStorageProvider()
	obj, err := s.Store(context.Background(), "image/png", tinyPNGBytes)
	if err != nil {
		t.Fatalf("unexpected error storing: %v", err)
	}
	contentType, content, err := s.Retrieve(context.Background(), obj.Reference)
	if err != nil {
		t.Fatalf("unexpected error retrieving: %v", err)
	}
	if contentType != "image/png" {
		t.Errorf("expected image/png, got %q", contentType)
	}
	if string(content) != string(tinyPNGBytes) {
		t.Error("expected retrieved content to match stored content exactly")
	}
}

func TestMockDocumentStorageProvider_UnknownReferenceReturnsNotFound(t *testing.T) {
	s := NewMockDocumentStorageProvider()
	_, _, err := s.Retrieve(context.Background(), "does-not-exist")
	if err != ErrObjectNotFound {
		t.Fatalf("expected ErrObjectNotFound, got %v", err)
	}
}

func TestMockKYCProvider_CreateVerification_ReturnsUniqueReferences(t *testing.T) {
	p := NewMockKYCProvider()
	seen := make(map[string]bool)
	for i := 0; i < 20; i++ {
		result, err := p.CreateVerification(context.Background(), CreateVerificationInput{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if seen[result.ProviderReference] {
			t.Fatalf("duplicate provider reference generated: %s", result.ProviderReference)
		}
		seen[result.ProviderReference] = true
	}
}

func TestMockKYCProvider_HandleCallback_RejectsInvalidSignature(t *testing.T) {
	p := NewMockKYCProvider()
	tenantID := uuid.New()
	in := p.CallbackPayload(tenantID, "ref-1", ProviderApproved, "ok")
	in.Header.Set(webhookauth.KYCSignatureHeader, "v1="+"00000000000000000000000000000000000000000000000000000000000000")
	resolver := NewMockWebhookCredentials(p)
	cred, err := resolver.Resolve(context.Background(), tenantID, p.ID(), webhookauth.MockKeyID)
	if err != nil {
		t.Fatalf("unexpected resolver error: %v", err)
	}
	if _, err := p.HandleCallback(context.Background(), in, cred); err == nil {
		t.Fatal("expected an error for an invalid signature")
	}
}

func TestMockKYCProvider_HandleCallback_AcceptsValidSignature(t *testing.T) {
	p := NewMockKYCProvider()
	tenantID := uuid.New()
	in := p.CallbackPayload(tenantID, "ref-1", ProviderApproved, "ok")
	resolver := NewMockWebhookCredentials(p)
	cred, err := resolver.Resolve(context.Background(), tenantID, p.ID(), webhookauth.MockKeyID)
	if err != nil {
		t.Fatalf("unexpected resolver error: %v", err)
	}
	result, err := p.HandleCallback(context.Background(), in, cred)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ProviderReference != "ref-1" || result.Outcome != ProviderApproved {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestMockKYCProvider_SetUnavailable(t *testing.T) {
	p := NewMockKYCProvider()
	p.SetUnavailable(true)
	if err := p.HealthStatus(context.Background()); err == nil {
		t.Fatal("expected HealthStatus to report an error while unavailable")
	}
	if _, err := p.CreateVerification(context.Background(), CreateVerificationInput{}); err == nil {
		t.Fatal("expected CreateVerification to fail while unavailable")
	}
}

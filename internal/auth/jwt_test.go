package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const testSecret = "test-signing-secret-at-least-32-characters"

func TestIssueAndVerify_RoundTrip(t *testing.T) {
	issuer := NewIssuer(testSecret, "platform-api-test")
	tenantID := uuid.New()

	token, err := issuer.Issue("player-123", tenantID, RolePlayer, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error issuing token: %v", err)
	}

	claims, err := issuer.Verify(token)
	if err != nil {
		t.Fatalf("unexpected error verifying token: %v", err)
	}
	if claims.TenantID != tenantID {
		t.Errorf("expected tenant id %s, got %s", tenantID, claims.TenantID)
	}
	if claims.Subject != "player-123" {
		t.Errorf("expected subject 'player-123', got %q", claims.Subject)
	}
	if claims.Role != RolePlayer {
		t.Errorf("expected role %q, got %q", RolePlayer, claims.Role)
	}
}

func TestIssue_RejectsNilTenant(t *testing.T) {
	issuer := NewIssuer(testSecret, "platform-api-test")
	if _, err := issuer.Issue("player-123", uuid.Nil, RolePlayer, time.Hour); err == nil {
		t.Fatal("expected error issuing token with nil tenant id, got nil")
	}
}

func TestIssue_RejectsEmptySubject(t *testing.T) {
	issuer := NewIssuer(testSecret, "platform-api-test")
	if _, err := issuer.Issue("", uuid.New(), RolePlayer, time.Hour); err == nil {
		t.Fatal("expected error issuing token with empty subject, got nil")
	}
}

func TestVerify_RejectsExpiredToken(t *testing.T) {
	issuer := NewIssuer(testSecret, "platform-api-test")
	token, err := issuer.Issue("player-123", uuid.New(), RolePlayer, -time.Hour)
	if err != nil {
		t.Fatalf("unexpected error issuing token: %v", err)
	}

	if _, err := issuer.Verify(token); err == nil {
		t.Fatal("expected error verifying expired token, got nil")
	}
}

func TestVerify_RejectsWrongSecret(t *testing.T) {
	issuer := NewIssuer(testSecret, "platform-api-test")
	token, err := issuer.Issue("player-123", uuid.New(), RolePlayer, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error issuing token: %v", err)
	}

	otherIssuer := NewIssuer("a-completely-different-secret-32-characters", "platform-api-test")
	if _, err := otherIssuer.Verify(token); err == nil {
		t.Fatal("expected error verifying token signed with a different secret, got nil")
	}
}

// TestVerify_RejectsAlgorithmConfusion proves the verifier refuses a
// token whose header claims a non-HMAC algorithm, which is the classic
// "alg: none" / algorithm-confusion attack against naive JWT verifiers
// that trust the token's own header to pick the verification method.
func TestVerify_RejectsAlgorithmConfusion(t *testing.T) {
	issuer := NewIssuer(testSecret, "platform-api-test")

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: "player-123",
			Issuer:  "platform-api-test",
		},
		TenantID: uuid.New(),
		Role:     RolePlayer,
	}
	unsigned := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	tokenString, err := unsigned.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("failed to build test token: %v", err)
	}

	if _, err := issuer.Verify(tokenString); err == nil {
		t.Fatal("expected error verifying an 'alg: none' token, got nil")
	}
}

// TestVerify_RejectsMissingExpiry proves a token with no exp claim at all
// (as opposed to one that has already expired) is rejected too - without
// jwt.WithExpirationRequired(), the underlying library treats a missing
// exp as "never expires", which would make a token from a future issuer
// that forgets to set it permanently valid and unrevocable.
func TestVerify_RejectsMissingExpiry(t *testing.T) {
	issuer := NewIssuer(testSecret, "platform-api-test")

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: "player-123",
			Issuer:  "platform-api-test",
			// ExpiresAt intentionally omitted.
		},
		TenantID: uuid.New(),
		Role:     RolePlayer,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("failed to build test token: %v", err)
	}

	if _, err := issuer.Verify(tokenString); err == nil {
		t.Fatal("expected error verifying a token with no exp claim, got nil")
	}
}

func TestVerify_RejectsMissingTenant(t *testing.T) {
	issuer := NewIssuer(testSecret, "platform-api-test")

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: "player-123",
			Issuer:  "platform-api-test",
		},
		// TenantID intentionally left as uuid.Nil to simulate a
		// malformed/forged token.
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("failed to build test token: %v", err)
	}

	if _, err := issuer.Verify(tokenString); err == nil {
		t.Fatal("expected error verifying a token with no tenant_id, got nil")
	}
}

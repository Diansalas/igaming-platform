package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const testSecret = "test-signing-secret-at-least-32-characters"
const testPreviousSecret = "test-previous-secret-at-least-32-characters"

func testIssuer(t *testing.T) *Issuer {
	t.Helper()
	keys, err := NewKeyRegistry("k1", map[string]string{"k1": testSecret})
	if err != nil {
		t.Fatalf("failed to build key registry: %v", err)
	}
	return NewIssuer(keys, "platform-api-test", "platform-api")
}

func TestIssueAndVerify_RoundTrip(t *testing.T) {
	issuer := testIssuer(t)
	tenantID := uuid.New()

	token, err := issuer.Issue("player-123", tenantID, RolePlayer, PrincipalPlayer, time.Hour)
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
	if claims.PrincipalType != PrincipalPlayer {
		t.Errorf("expected principal type %q, got %q", PrincipalPlayer, claims.PrincipalType)
	}
	if claims.ID == "" {
		t.Error("expected a non-empty jti")
	}
}

func TestIssue_AllowsNilTenant_ForPlatformScopedPrincipal(t *testing.T) {
	issuer := testIssuer(t)

	token, err := issuer.Issue("admin-1", uuid.Nil, RolePlatformAdmin, PrincipalStaff, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error issuing a platform-scoped token: %v", err)
	}
	claims, err := issuer.Verify(token)
	if err != nil {
		t.Fatalf("unexpected error verifying a platform-scoped token: %v", err)
	}
	if claims.TenantID != uuid.Nil {
		t.Errorf("expected nil tenant id for a platform-scoped token, got %s", claims.TenantID)
	}
}

func TestIssue_RejectsEmptySubject(t *testing.T) {
	issuer := testIssuer(t)
	if _, err := issuer.Issue("", uuid.New(), RolePlayer, PrincipalPlayer, time.Hour); err == nil {
		t.Fatal("expected error issuing token with empty subject, got nil")
	}
}

func TestVerify_RejectsExpiredToken(t *testing.T) {
	issuer := testIssuer(t)
	token, err := issuer.Issue("player-123", uuid.New(), RolePlayer, PrincipalPlayer, -time.Hour)
	if err != nil {
		t.Fatalf("unexpected error issuing token: %v", err)
	}

	if _, err := issuer.Verify(token); err == nil {
		t.Fatal("expected error verifying expired token, got nil")
	}
}

func TestVerify_RejectsUnknownKeyID(t *testing.T) {
	issuer := testIssuer(t)
	token, err := issuer.Issue("player-123", uuid.New(), RolePlayer, PrincipalPlayer, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error issuing token: %v", err)
	}

	otherKeys, err := NewKeyRegistry("k1", map[string]string{"k1": "a-completely-different-secret-32-characters"})
	if err != nil {
		t.Fatalf("failed to build key registry: %v", err)
	}
	otherIssuer := NewIssuer(otherKeys, "platform-api-test", "platform-api")
	if _, err := otherIssuer.Verify(token); err == nil {
		t.Fatal("expected error verifying token signed with a different secret under the same kid, got nil")
	}
}

// TestVerify_KeyRotation_PreviousKeyStillVerifies proves the rotation
// mechanism: a token signed with a key that is no longer active still
// verifies as long as that key remains registered (as "previous"),
// while new tokens are signed with the new active key.
func TestVerify_KeyRotation_PreviousKeyStillVerifies(t *testing.T) {
	oldKeys, err := NewKeyRegistry("k1", map[string]string{"k1": testSecret})
	if err != nil {
		t.Fatalf("failed to build old key registry: %v", err)
	}
	oldIssuer := NewIssuer(oldKeys, "platform-api-test", "platform-api")

	oldToken, err := oldIssuer.Issue("player-123", uuid.New(), RolePlayer, PrincipalPlayer, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error issuing token with old key: %v", err)
	}

	// Rotate: k2 becomes active, k1 stays registered for verification only.
	rotatedKeys, err := NewKeyRegistry("k2", map[string]string{
		"k1": testSecret,
		"k2": testPreviousSecret,
	})
	if err != nil {
		t.Fatalf("failed to build rotated key registry: %v", err)
	}
	rotatedIssuer := NewIssuer(rotatedKeys, "platform-api-test", "platform-api")

	if _, err := rotatedIssuer.Verify(oldToken); err != nil {
		t.Fatalf("expected old token to still verify after rotation, got error: %v", err)
	}

	newToken, err := rotatedIssuer.Issue("player-456", uuid.New(), RolePlayer, PrincipalPlayer, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error issuing token with rotated key: %v", err)
	}
	if _, err := oldIssuer.Verify(newToken); err == nil {
		t.Fatal("expected the pre-rotation issuer (which doesn't know k2) to reject the new token, got nil")
	}
}

func TestVerify_RejectsWrongAudience(t *testing.T) {
	keys, err := NewKeyRegistry("k1", map[string]string{"k1": testSecret})
	if err != nil {
		t.Fatalf("failed to build key registry: %v", err)
	}
	issuer := NewIssuer(keys, "platform-api-test", "platform-api")
	otherAudienceIssuer := NewIssuer(keys, "platform-api-test", "some-other-api")

	token, err := issuer.Issue("player-123", uuid.New(), RolePlayer, PrincipalPlayer, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error issuing token: %v", err)
	}
	if _, err := otherAudienceIssuer.Verify(token); err == nil {
		t.Fatal("expected error verifying a token issued for a different audience, got nil")
	}
}

// TestVerify_RejectsAlgorithmConfusion proves the verifier refuses a
// token whose header claims a non-HMAC algorithm, which is the classic
// "alg: none" / algorithm-confusion attack against naive JWT verifiers
// that trust the token's own header to pick the verification method.
func TestVerify_RejectsAlgorithmConfusion(t *testing.T) {
	issuer := testIssuer(t)

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
	issuer := testIssuer(t)

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:  "player-123",
			Issuer:   "platform-api-test",
			Audience: jwt.ClaimStrings{"platform-api"},
			// ExpiresAt intentionally omitted.
		},
		TenantID: uuid.New(),
		Role:     RolePlayer,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["kid"] = "k1"
	tokenString, err := token.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("failed to build test token: %v", err)
	}

	if _, err := issuer.Verify(tokenString); err == nil {
		t.Fatal("expected error verifying a token with no exp claim, got nil")
	}
}

func TestVerify_RejectsMissingSubject(t *testing.T) {
	issuer := testIssuer(t)

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "platform-api-test",
			Audience:  jwt.ClaimStrings{"platform-api"},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		TenantID: uuid.New(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["kid"] = "k1"
	tokenString, err := token.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("failed to build test token: %v", err)
	}

	if _, err := issuer.Verify(tokenString); err == nil {
		t.Fatal("expected error verifying a token with no subject, got nil")
	}
}

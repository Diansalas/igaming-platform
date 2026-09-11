// Package auth implements the platform's authentication mechanisms:
// password hashing (password.go), signing-key management (keys.go), JWT
// issuance/verification (this file), refresh-token sessions
// (session.go), and permission-based authorization (permission.go).
//
// STATUS: FOUNDATION / PROVIDER DEPENDENT for the JWT signing mechanism
// itself. This issues and verifies HMAC-signed JWTs with process-managed
// keys, which is sufficient to establish and test key rotation, tenant-
// context, and RBAC correctly. It is explicitly NOT necessarily the final
// production authentication design - see docs/security/security-
// architecture.md's "Production authentication evaluation" section for
// the documented, evaluated alternative (asymmetric signing via a
// KMS-managed key or a real identity provider) and why HMAC was kept for
// this stage. Everything built on top of it (key rotation, aud/exp/kid
// validation, refresh rotation with reuse detection) is real,
// production-oriented design regardless of which signing mechanism
// ultimately backs it - swapping HS256-with-managed-keys for an
// asymmetric scheme changes Issue/Verify's internals, not the
// tenant-context, session, or permission model built on top.
package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Role is a coarse RBAC role. See permission.go for the permission sets
// each role maps to.
type Role string

const (
	RolePlatformAdmin Role = "platform_admin"
	RoleTenantAdmin   Role = "tenant_admin"
	RoleSupport       Role = "support"
	RoleCompliance    Role = "compliance"
	RoleFinance       Role = "finance"
	RolePlayer        Role = "player"
)

// PrincipalType distinguishes what kind of subject a token identifies -
// mirrors sessions.principal_type and audit.ActorType.
type PrincipalType string

const (
	PrincipalPlayer  PrincipalType = "player"
	PrincipalStaff   PrincipalType = "staff"
	PrincipalService PrincipalType = "service"
)

// Claims is the platform's JWT claim set.
//
// TenantID may legitimately be uuid.Nil - it represents a platform-scoped
// principal (currently: a platform_admin staff user), not a missing or
// invalid claim. See docs/decisions/0011-platform-scoped-identity-
// tokens.md: token verification does not reject a nil tenant; every
// tenant-scoped operation enforces non-nil tenant_id at the point of use
// instead (internal/auth.RequireTenantScope).
type Claims struct {
	jwt.RegisteredClaims
	TenantID      uuid.UUID     `json:"tenant_id"`
	Role          Role          `json:"role"`
	PrincipalType PrincipalType `json:"principal_type"`
}

var (
	ErrInvalidToken   = errors.New("auth: invalid token")
	ErrMissingSubject = errors.New("auth: token missing subject")
)

// Issuer signs and verifies platform JWTs using a KeyRegistry.
type Issuer struct {
	keys     *KeyRegistry
	issuer   string
	audience string
}

func NewIssuer(keys *KeyRegistry, issuerName, audience string) *Issuer {
	return &Issuer{keys: keys, issuer: issuerName, audience: audience}
}

// Issue mints a token for subject (a player, staff, or service principal
// id) with the given role, principal type, and ttl. tenantID is uuid.Nil
// for a platform-scoped principal. Every token gets a fresh jti (JWT ID)
// for audit/correlation purposes, even though Stage 2 does not implement
// access-token-level revocation by jti - revocation happens at the
// session/refresh-token layer (session.go), which is why access tokens
// are kept short-lived.
func (i *Issuer) Issue(subject string, tenantID uuid.UUID, role Role, principalType PrincipalType, ttl time.Duration) (string, error) {
	if subject == "" {
		return "", ErrMissingSubject
	}
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			Issuer:    i.issuer,
			Audience:  jwt.ClaimStrings{i.audience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			ID:        uuid.NewString(),
		},
		TenantID:      tenantID,
		Role:          role,
		PrincipalType: principalType,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["kid"] = i.keys.ActiveKID()
	return token.SignedString(i.keys.ActiveSecret())
}

// Verify parses and validates a bearer token, returning its claims. It
// rejects: any algorithm other than HS256 (preventing "alg: none" /
// algorithm-confusion attacks), a "kid" header naming a key not present
// in the registry, a wrong issuer or audience, a missing or empty
// subject, and a missing, expired, or not-yet-valid exp/nbf. It does NOT
// reject a nil tenant_id - see the Claims doc comment.
func (i *Issuer) Verify(tokenString string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("%w: unexpected signing method %v", ErrInvalidToken, t.Header["alg"])
		}
		kid, _ := t.Header["kid"].(string)
		secret, ok := i.keys.Lookup(kid)
		if !ok {
			return nil, fmt.Errorf("%w: unknown key id %q", ErrInvalidToken, kid)
		}
		return secret, nil
	},
		jwt.WithIssuer(i.issuer),
		jwt.WithAudience(i.audience),
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if !token.Valid {
		return nil, ErrInvalidToken
	}
	if claims.Subject == "" {
		return nil, ErrMissingSubject
	}
	return claims, nil
}

// Package auth implements the Stage 1 authentication foundation.
//
// STATUS: FOUNDATION / PROVIDER DEPENDENT. This issues and verifies
// HMAC-signed JWTs with a shared secret, which is sufficient to establish
// and test the tenant-context/RBAC pattern the rest of the platform
// builds on. It is explicitly NOT the production authentication design:
// production is expected to move to a real identity provider or
// KMS-managed asymmetric signing (see docs/architecture/04-api-
// architecture.md and docs/security/security-architecture.md) before any
// real player or staff credential is issued against it. Nothing in this
// package should be mistaken for that later work being done.
package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Role is a coarse RBAC role. This is the Stage 1 skeleton for the
// three-tier model in docs/architecture/12-audit-reporting-architecture.md
// (platform admin / partner admin / brand operator), plus "player" for
// the brand-frontend session case. Fine-grained permissions-per-role are
// deferred to Stage 2/6 when back office/partner console are built.
type Role string

const (
	RolePlatformAdmin Role = "platform_admin"
	RolePartnerAdmin  Role = "partner_admin"
	RoleBrandOperator Role = "brand_operator"
	RolePlayer        Role = "player"
)

// Claims is the platform's JWT claim set. TenantID is always present and
// always server-issued - a token is never accepted from, or trusted
// because of, client input claiming a tenant.
type Claims struct {
	jwt.RegisteredClaims
	TenantID uuid.UUID `json:"tenant_id"`
	Role     Role      `json:"role"`
}

var (
	ErrInvalidToken   = errors.New("auth: invalid token")
	ErrMissingTenant  = errors.New("auth: token missing tenant_id")
	ErrMissingSubject = errors.New("auth: token missing subject")
)

// Issuer signs and verifies platform JWTs with a single shared secret.
type Issuer struct {
	secret []byte
	issuer string
}

func NewIssuer(secret, issuerName string) *Issuer {
	return &Issuer{secret: []byte(secret), issuer: issuerName}
}

// Issue mints a token for subject (a player or staff user id) scoped to
// tenantID with the given role and ttl. This is a Stage 1 development/
// testing convenience - real player/staff login flows (Stage 2) will
// call this from a verified authentication event, never from
// unauthenticated input.
func (i *Issuer) Issue(subject string, tenantID uuid.UUID, role Role, ttl time.Duration) (string, error) {
	if subject == "" {
		return "", ErrMissingSubject
	}
	if tenantID == uuid.Nil {
		return "", ErrMissingTenant
	}
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			Issuer:    i.issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		TenantID: tenantID,
		Role:     role,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(i.secret)
}

// Verify parses and validates a bearer token, returning its claims. It
// rejects tokens signed with any algorithm other than HS256 (preventing
// the classic "alg: none" / algorithm-confusion attack class), tokens
// with no tenant_id, and expired/not-yet-valid tokens.
func (i *Issuer) Verify(tokenString string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("%w: unexpected signing method %v", ErrInvalidToken, t.Header["alg"])
		}
		return i.secret, nil
	}, jwt.WithIssuer(i.issuer), jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if !token.Valid {
		return nil, ErrInvalidToken
	}
	if claims.TenantID == uuid.Nil {
		return nil, ErrMissingTenant
	}
	if claims.Subject == "" {
		return nil, ErrMissingSubject
	}
	return claims, nil
}

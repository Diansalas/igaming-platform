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

	"github.com/Diansalas/igaming-platform/internal/staffmfa"
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
	// RoleRiskManager is Stage 4G's own role - see identity.StaffRoleRiskManager.
	RoleRiskManager Role = "risk_manager"

	// RolePromotionsManager/RoleBonusOperations are Stage 4H-B1 Wave 2's
	// two new Bonus Engine roles (security-architecture.md §B1.1 "Role
	// wiring"). Deliberately two separate roles, never one: "the author of
	// a Campaign must not also be able to hand out its value directly"
	// (§B1.1 hard constraint 1 - no principal may hold both). See
	// permission.go's rolePermissions map for exactly what each holds.
	RolePromotionsManager Role = "promotions_manager"
	RoleBonusOperations   Role = "bonus_operations"
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
//
// ADR 0119 section 7.1 (slice S3): a STAFF token additionally carries the
// session assurance claims sid, aal, amr, auth_time, mfa_at and sv, and Verify
// refuses a staff token without every one of them or with an aal other than
// StaffAAL. A player or service token never carries them (Verify refuses one
// that does). Staff tokens are minted only by IssueStaff, after a verified
// second factor; Issue refuses PrincipalStaff.
type Claims struct {
	jwt.RegisteredClaims
	TenantID      uuid.UUID     `json:"tenant_id"`
	Role          Role          `json:"role"`
	PrincipalType PrincipalType `json:"principal_type"`

	// Staff session assurance (ADR 0119 section 7.1). Absent on player and
	// service tokens.
	SessionID    string           `json:"sid,omitempty"`
	AAL          int              `json:"aal,omitempty"`
	AMR          []string         `json:"amr,omitempty"`
	AuthTime     *jwt.NumericDate `json:"auth_time,omitempty"`
	MFAAt        *jwt.NumericDate `json:"mfa_at,omitempty"`
	StaffVersion *int64           `json:"sv,omitempty"`
}

// StaffAAL is the only authenticator assurance level a staff access token may
// carry (ADR 0119 section 7.1): every staff session is issued after a verified
// second factor. There is no lower level and no bypass.
const StaffAAL = 2

var (
	ErrInvalidToken   = errors.New("auth: invalid token")
	ErrMissingSubject = errors.New("auth: token missing subject")
	// ErrStaffAssuranceRequired: Issue was asked for a staff token. Staff
	// tokens are minted only by IssueStaff, with session assurance claims
	// (ADR 0119 section 4.1: there is no password-only staff token).
	ErrStaffAssuranceRequired = errors.New("auth: staff tokens are issued only with session assurance (IssueStaff)")
	// ErrRolePrincipalMismatch: a staff role on a non-staff principal, a
	// non-staff role on a staff principal, or an unknown principal type (ADR
	// 0119 S3 security review C1). Permissions are looked up by role alone,
	// so the role and the principal type must agree before any route sees
	// the claims.
	ErrRolePrincipalMismatch = errors.New("auth: role does not match the principal type")
)

// rolePrincipalValid is the role/principal binding (ADR 0119 S3 security
// review C1): a staff principal holds a role from the CLOSED staff role set
// (S4a, condition S3-R12: IsStaffRole; an unknown role string is refused, not
// merely permission-less); a player or service principal holds RolePlayer
// only; no other principal type exists. Every staff role is reachable only
// through a staff token, which IssueStaff mints after a verified second factor.
func rolePrincipalValid(role Role, principalType PrincipalType) bool {
	switch principalType {
	case PrincipalStaff:
		return IsStaffRole(role)
	case PrincipalPlayer, PrincipalService:
		return role == RolePlayer
	default:
		return false
	}
}

// StaffTokenParams are the claims of a staff access token (ADR 0119 section
// 7.1), taken from the staff session row the token belongs to.
type StaffTokenParams struct {
	StaffID      uuid.UUID
	TenantID     uuid.UUID // uuid.Nil for platform staff
	Role         Role
	SessionID    uuid.UUID
	AAL          int
	AMR          []string
	AuthTime     time.Time
	MFAAt        time.Time
	StaffVersion int64
}

func (p StaffTokenParams) valid() bool {
	return p.StaffID != uuid.Nil && p.SessionID != uuid.Nil && p.Role != "" && p.AAL == StaffAAL &&
		len(p.AMR) > 0 && HasSecondFactorAMR(p.AMR) && !p.AuthTime.IsZero() && !p.MFAAt.IsZero() &&
		!p.MFAAt.Before(p.AuthTime) && p.StaffVersion >= 0
}

// staffAssuranceTimesValid is condition S3-R13 (ADR 0119 S4b, before step-up
// ships) on a staff token's times, at now: auth_time <= mfa_at <= now + skew
// (whole seconds: the claims are NumericDates), and auth_time no older than the
// longest admissible staff session (StaffSessionAbsoluteTTLBound.Max, 12 h; the
// configured, possibly shorter, absolute expiry is enforced on the sealed
// session row by liveness and refresh).
func staffAssuranceTimesValid(authTime, mfaAt, now time.Time) bool {
	if mfaAt.Before(authTime) {
		return false
	}
	if mfaAt.After(now.Add(StepUpClockSkew)) || authTime.After(now.Add(StepUpClockSkew)) {
		return false
	}
	return !authTime.Before(now.Add(-staffmfa.StaffSessionAbsoluteTTLBound.Max))
}

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
//
// A staff token is refused here (ErrStaffAssuranceRequired): staff tokens are
// minted only by IssueStaff. A role that does not match the principal type
// (any staff role on a player or service principal) is refused too
// (ErrRolePrincipalMismatch).
func (i *Issuer) Issue(subject string, tenantID uuid.UUID, role Role, principalType PrincipalType, ttl time.Duration) (string, error) {
	if subject == "" {
		return "", ErrMissingSubject
	}
	if principalType == PrincipalStaff {
		return "", ErrStaffAssuranceRequired
	}
	if !rolePrincipalValid(role, principalType) {
		return "", ErrRolePrincipalMismatch
	}
	return i.sign(i.baseClaims(subject, tenantID, role, principalType, ttl))
}

// IssueStaff mints a staff access token carrying the session assurance claims
// of p (ADR 0119 section 7.1). It is called only after a verified second
// factor (login, enrolment) or a refresh of a sealed staff session. ttl is the
// staff access-token TTL (STAFF_ACCESS_TOKEN_TTL_SECONDS, at most 15 min).
func (i *Issuer) IssueStaff(p StaffTokenParams, ttl time.Duration) (string, error) {
	if !p.valid() {
		return "", fmt.Errorf("%w: incomplete staff session assurance", ErrStaffAssuranceRequired)
	}
	if !rolePrincipalValid(p.Role, PrincipalStaff) {
		return "", ErrRolePrincipalMismatch
	}
	claims := i.baseClaims(p.StaffID.String(), p.TenantID, p.Role, PrincipalStaff, ttl)
	sv := p.StaffVersion
	claims.SessionID = p.SessionID.String()
	claims.AAL = p.AAL
	claims.AMR = append([]string(nil), p.AMR...)
	claims.AuthTime = jwt.NewNumericDate(p.AuthTime)
	claims.MFAAt = jwt.NewNumericDate(p.MFAAt)
	claims.StaffVersion = &sv
	return i.sign(claims)
}

func (i *Issuer) baseClaims(subject string, tenantID uuid.UUID, role Role, principalType PrincipalType, ttl time.Duration) Claims {
	now := time.Now()
	return Claims{
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
}

func (i *Issuer) sign(claims Claims) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["kid"] = i.keys.ActiveKID()
	return token.SignedString(i.keys.ActiveSecret())
}

// checkAssurance applies ADR 0119 section 7.1: a staff token carries every
// assurance claim and aal == StaffAAL; any other token carries none.
func (c *Claims) checkAssurance() error {
	if !rolePrincipalValid(c.Role, c.PrincipalType) {
		return fmt.Errorf("%w: %v", ErrInvalidToken, ErrRolePrincipalMismatch)
	}
	if c.PrincipalType != PrincipalStaff {
		if c.SessionID != "" || c.AAL != 0 || c.AMR != nil || c.AuthTime != nil || c.MFAAt != nil || c.StaffVersion != nil {
			return fmt.Errorf("%w: staff assurance claims on a non-staff token", ErrInvalidToken)
		}
		return nil
	}
	sid, err := uuid.Parse(c.SessionID)
	if err != nil || sid == uuid.Nil {
		return fmt.Errorf("%w: staff token without a valid sid", ErrInvalidToken)
	}
	if c.AAL != StaffAAL {
		return fmt.Errorf("%w: staff token aal is not %d", ErrInvalidToken, StaffAAL)
	}
	if len(c.AMR) == 0 || c.AuthTime == nil || c.MFAAt == nil || c.StaffVersion == nil || *c.StaffVersion < 0 {
		return fmt.Errorf("%w: staff token without its assurance claims", ErrInvalidToken)
	}
	// S3-R13: amr names a second factor; auth_time <= mfa_at <= now + skew; auth_time within the absolute TTL bound.
	if !HasSecondFactorAMR(c.AMR) {
		return fmt.Errorf("%w: staff token amr names no second factor", ErrInvalidToken)
	}
	if !staffAssuranceTimesValid(c.AuthTime.Time, c.MFAAt.Time, time.Now()) {
		return fmt.Errorf("%w: staff token assurance times are not coherent", ErrInvalidToken)
	}
	return nil
}

// Verify parses and validates a bearer token, returning its claims. It
// rejects: any algorithm other than HS256 (preventing "alg: none" /
// algorithm-confusion attacks), a "kid" header naming a key not present
// in the registry, a wrong issuer or audience, a missing or empty
// subject, and a missing, expired, or not-yet-valid exp/nbf. It does NOT
// reject a nil tenant_id - see the Claims doc comment. ADR 0119 S3: it also
// refuses a staff token without every session assurance claim or with an aal
// other than StaffAAL, and a non-staff token that carries any of them; and
// (security review C1) any token whose role does not match its principal
// type, so a hand-signed player token with a staff role is a 401.
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
	if err := claims.checkAssurance(); err != nil {
		return nil, err
	}
	return claims, nil
}

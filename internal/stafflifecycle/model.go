// Package stafflifecycle is ADR 0119 slices S5 + S6 (STAFF-LIFECYCLE-1): the staff lifecycle
// invited -> active <-> suspended -> offboarded, role changes, MFA resets, invitations, and the platform-staff
// four-eyes (staff_lifecycle_requests / staff_lifecycle_approvals, migration 0139). Every mutation:
//
//   - runs in the TARGET's scope (db.WithPrincipalScope for a tenant actor in its own tenant; db.WithTenant(target)
//     plus db.SetActorStaffIDForCurrentTx for a platform actor acting on a tenant staff member; db.WithPlatformAdmin
//     for a platform target);
//   - takes the actor's own staff row FOR SHARE and the target's FOR UPDATE, in ascending id order, and re-asserts
//     the actor (active, the token's role, the token's staff_version: ADR 0119 section 9.5, ADR 0082 A9);
//   - writes one staff_lifecycle_events row (before / after, actor, reason code) and one audit_log row in the same
//     transaction (section 10.6);
//   - leaves the database triggers (SF010-SF014, SF020, SF021, SF031-SF033) as the backstop of every Go check.
//
// Suspension and offboarding use the two-transaction phasing of section 9.5: T1 revokes every session, live
// challenge and enrolment token of the target and commits; T2 changes the status under a 5 s lock_timeout, retried
// once, else ErrBusy (the sessions stay revoked).
//
// Tokens (invite, mfa_reset) are issued through the S3 enrolment machinery (internal/staffmfa/store) and emailed
// after the commit through email.Provider; they are never returned to the caller (section 6.3).
package stafflifecycle

import (
	"errors"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Errors (the HTTP layer maps each to one closed apierror code).
var (
	ErrInvalidInput        = errors.New("stafflifecycle: invalid input")
	ErrNotFound            = errors.New("stafflifecycle: not found")
	ErrForbidden           = errors.New("stafflifecycle: forbidden")
	ErrSelfTarget          = errors.New("stafflifecycle: a staff member may not act on their own account")
	ErrRoleNotGrantable    = errors.New("stafflifecycle: the caller may not grant or act on this role")
	ErrPersonRequired      = errors.New("stafflifecycle: the role requires a linked Person")
	ErrPersonNotFound      = errors.New("stafflifecycle: person_id does not reference an existing Person")
	ErrTransition          = errors.New("stafflifecycle: the target is not in a state the action applies to")
	ErrLastAdmin           = errors.New("stafflifecycle: the last active, enrolled admin cannot be removed")
	ErrBusy                = errors.New("stafflifecycle: the staff row is busy; the sessions are revoked, retry")
	ErrConflict            = errors.New("stafflifecycle: the email is already used in this scope")
	ErrFourEyes            = errors.New("stafflifecycle: four-eyes violation")
	ErrNotImplemented      = errors.New("stafflifecycle: not implemented in this slice")
	ErrUnavailable         = errors.New("stafflifecycle: unavailable (no proof issuer or MFA keys)")
	ErrPayloadHashMismatch = errors.New("stafflifecycle: payload_hash does not match the request")
	// ErrPersonInUse (review fix SR-3): the invitee Person is already bound to a live staff account in the scope.
	ErrPersonInUse = errors.New("stafflifecycle: the Person is already linked to a live staff account")
	// ErrAlreadyLinked: a person link on an account that already has a Person (0034: append-only).
	ErrAlreadyLinked = errors.New("stafflifecycle: the staff account is already linked to a Person")
	// ErrProofRefused (review fix F10): the database refused the actor proof (AP001-AP005).
	ErrProofRefused = errors.New("stafflifecycle: the actor proof was refused")
)

// Roles (mirrors identity.StaffRole; kept as strings so the package carries no auth dependency for the matrix).
const (
	RolePlatformAdmin     = "platform_admin"
	RoleTenantAdmin       = "tenant_admin"
	RoleSupport           = "support"
	RoleCompliance        = "compliance"
	RoleFinance           = "finance"
	RoleRiskManager       = "risk_manager"
	RolePromotionsManager = "promotions_manager"
	RoleBonusOperations   = "bonus_operations"
)

// TenantRoles is every role a tenant staff member may hold.
var TenantRoles = []string{RoleTenantAdmin, RoleSupport, RoleCompliance, RoleFinance, RoleRiskManager, RolePromotionsManager, RoleBonusOperations}

// tenantAdminGrantable is what a tenant admin may invite into, change into or out of, and act on (ADR 0119 s10.1;
// the platform-only roles finance, risk_manager, promotions_manager and bonus_operations are platform-admin only).
var tenantAdminGrantable = map[string]bool{RoleTenantAdmin: true, RoleSupport: true, RoleCompliance: true}

// personRequiredRoles need a linked Person (ADR 0119 s9.4, SF014).
var personRequiredRoles = map[string]bool{RolePlatformAdmin: true, RoleTenantAdmin: true, RoleFinance: true, RoleCompliance: true}

// IsTenantRole reports whether role is a tenant staff role.
func IsTenantRole(role string) bool {
	for _, r := range TenantRoles {
		if r == role {
			return true
		}
	}
	return false
}

// PersonRequired reports whether role needs a linked Person.
func PersonRequired(role string) bool { return personRequiredRoles[role] }

// Grantable reports whether actor may grant (invite into, change into or out of, act on) role in a tenant.
func Grantable(actor Actor, role string) bool {
	if !IsTenantRole(role) {
		return false
	}
	if actor.Platform() {
		return true
	}
	return actor.Role == RoleTenantAdmin && tenantAdminGrantable[role]
}

// Action is a lifecycle action with its own closed reason-code list (ADR 0119 s10.1).
type Action string

const (
	ActionSuspend      Action = "suspend"
	ActionOffboard     Action = "offboard"
	ActionReactivate   Action = "reactivate"
	ActionRoleChange   Action = "role_change"
	ActionMFAReset     Action = "mfa_reset"
	ActionInviteRevoke Action = "invite_revoke"
)

// ReasonCodes is the closed vocabulary per action (a CHECK in the database plus these constants).
var ReasonCodes = map[Action][]string{
	ActionSuspend:      {"security_compromise", "policy_violation", "investigation", "leave_of_absence", "role_review", "other"},
	ActionOffboard:     {"employment_ended", "contract_ended", "security_compromise", "duplicate_account", "other"},
	ActionReactivate:   {"investigation_closed", "returned_from_leave", "error_correction", "other"},
	ActionRoleChange:   {"role_change_approved", "error_correction", "other"},
	ActionMFAReset:     {"device_lost", "device_replaced", "hard_locked", "security_compromise", "other"},
	ActionInviteRevoke: {"no_longer_required", "wrong_details", "other"},
}

// Reason is a validated reason code plus its optional note.
type Reason struct {
	Code string
	Note string
}

// ValidateReason checks code against action's list, and the note: required for `other`, 10-500 characters, no
// control, bidi-override / isolate or zero-width format character (the ADR 0104 display-name hygiene, mirrored by
// the database CHECK).
func ValidateReason(action Action, r Reason) error {
	ok := false
	for _, c := range ReasonCodes[action] {
		if c == r.Code {
			ok = true
			break
		}
	}
	if !ok {
		return ErrInvalidInput
	}
	if r.Note == "" {
		if r.Code == "other" {
			return ErrInvalidInput
		}
		return nil
	}
	return ValidateNote(r.Note)
}

// ValidateNote is the note hygiene rule.
func ValidateNote(note string) error {
	if n := utf8.RuneCountInString(note); n < 10 || n > 500 || !utf8.ValidString(note) {
		return ErrInvalidInput
	}
	for _, c := range note {
		if unicode.IsControl(c) || unsafeFormatRune(c) {
			return ErrInvalidInput
		}
	}
	return nil
}

func unsafeFormatRune(r rune) bool {
	switch {
	case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069, r >= 0x200B && r <= 0x200F,
		r >= 0x2060 && r <= 0x2064, r == 0xFEFF, r == 0x061C:
		return true
	}
	return false
}

// Meta is the request context recorded with every event and audit row.
type Meta struct {
	RequestID string
	IP        string
	UserAgent string
}

// Actor is the verified staff principal of a lifecycle call: every field comes from the verified token (subject,
// tenant, role, sv), never from a request body.
type Actor struct {
	StaffID      uuid.UUID
	TenantID     uuid.UUID // uuid.Nil: a platform principal
	Role         string
	StaffVersion int64
	Meta         Meta
}

// Platform reports whether the actor is a platform principal.
func (a Actor) Platform() bool { return a.TenantID == uuid.Nil }

// Staff is a staff member as the lifecycle API returns it (never a credential).
type Staff struct {
	ID              uuid.UUID
	TenantID        uuid.UUID
	Email           string
	DisplayName     *string
	Role            string
	Status          string
	StaffVersion    int64
	PersonLinked    bool
	MFAEnrolled     bool
	MFAHardLocked   bool
	LastLoginAt     *time.Time
	CreatedAt       time.Time
	StatusChangedAt *time.Time
}

// State is the before / after snapshot of an event (never a secret).
type State struct {
	Status       string `json:"status"`
	Role         string `json:"role"`
	StaffVersion int64  `json:"staff_version"`
	PersonLinked bool   `json:"person_linked"`
}

// Event is one staff_lifecycle_events row.
type Event struct {
	ID                 uuid.UUID
	StaffUserID        uuid.UUID
	Event              string
	ActorType          string
	ActorStaffID       *uuid.UUID
	ActorIsTenantStaff bool // the actor is a staff member of the target's own tenant (ADR 0114 H1 redaction)
	Before, After      State
	ReasonCode         *string
	Note               *string
	LifecycleRequestID *uuid.UUID
	CreatedAt          time.Time
}

// Delivery is the outcome of an email hand-off.
type Delivery string

const (
	DeliveryNone   Delivery = ""
	DeliverySent   Delivery = "sent"
	DeliveryFailed Delivery = "failed"
)

// Result is what a mutation returns: the target after the change, and the email delivery (if any).
type Result struct {
	Staff    Staff
	Delivery Delivery
}

package staffmfa

import (
	"crypto/hmac"
	"crypto/sha256"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SessionAssurance is the sealed part of a staff `sessions` row (ADR 0119
// section 7.2, slice S3). The seal is
//
//	HMAC(subkey, "ss-v1|" || id || "|" || principal_type || "|" || principal_id || "|" ||
//	     coalesce(tenant_id,'platform') || "|" || aal || "|" || join(amr,',') || "|" ||
//	     auth_time || "|" || mfa_at || "|" || absolute_expires_at)
//
// with every time as a microsecond UTC integer (the database stores
// microseconds). The subkey is the MAC family's staff-session-assurance-v1
// subkey of the kid the row names. TenantID uuid.Nil means a platform staff
// member. None of these values is secret; the seal binds them so a session
// row written or edited by SQL (threat M5) is detected on refresh.
type SessionAssurance struct {
	SessionID         uuid.UUID
	PrincipalType     string
	PrincipalID       uuid.UUID
	TenantID          uuid.UUID
	AAL               int
	AMR               []string
	AuthTime          time.Time
	MFAAt             time.Time
	AbsoluteExpiresAt time.Time
}

// SessionTime normalises a time to what the database stores (UTC,
// microsecond precision, truncated). Callers write exactly this value so the
// seal computed before the INSERT matches the row read back.
func SessionTime(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// amrValid admits RFC 8176 style values: 1-16 lowercase ASCII letters. The
// separator characters of the canonical form (',' and '|') can therefore
// never appear inside a value, so the encoding is unambiguous.
func amrValid(v string) bool {
	if len(v) == 0 || len(v) > 16 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 'a' || v[i] > 'z' {
			return false
		}
	}
	return true
}

func (a SessionAssurance) message() ([]byte, error) {
	if a.SessionID == uuid.Nil || a.PrincipalID == uuid.Nil || a.PrincipalType != "staff" ||
		a.AAL < 1 || a.AAL > 3 || len(a.AMR) == 0 || len(a.AMR) > 8 ||
		a.AuthTime.IsZero() || a.MFAAt.IsZero() || a.AbsoluteExpiresAt.IsZero() {
		return nil, ErrInvalidInput
	}
	for _, v := range a.AMR {
		if !amrValid(v) {
			return nil, ErrInvalidInput
		}
	}
	tenant := "platform"
	if a.TenantID != uuid.Nil {
		tenant = a.TenantID.String()
	}
	us := func(t time.Time) string { return strconv.FormatInt(t.UTC().UnixMicro(), 10) }
	return []byte("ss-v1|" + a.SessionID.String() + "|" + a.PrincipalType + "|" + a.PrincipalID.String() + "|" + tenant +
		"|" + strconv.Itoa(a.AAL) + "|" + strings.Join(a.AMR, ",") + "|" + us(a.AuthTime) + "|" + us(a.MFAAt) +
		"|" + us(a.AbsoluteExpiresAt)), nil
}

func (k *Keys) sessionMAC(kid string, a SessionAssurance) ([]byte, error) {
	msg, err := a.message()
	if err != nil {
		return nil, err
	}
	sub, ok := k.m().mac[kid]
	if !ok {
		return nil, ErrUnknownKID
	}
	m := hmac.New(sha256.New, sub.session)
	m.Write(msg)
	return m.Sum(nil), nil
}

// SealSession computes the assurance seal of a under the active MAC kid. The
// row stores the kid (assurance_kid) and the 32-byte MAC (assurance_seal).
func (k *Keys) SealSession(a SessionAssurance) (KeyedHash, error) {
	kid := k.m().macActive
	mac, err := k.sessionMAC(kid, a)
	if err != nil {
		return KeyedHash{}, err
	}
	return KeyedHash{KID: kid, MAC: mac}, nil
}

// VerifySessionSeal recomputes the seal under the kid the row names (a
// retained verify-only kid is accepted; a retired or unknown kid is not) and
// compares it in constant time. Any malformed input is a mismatch.
func (k *Keys) VerifySessionSeal(a SessionAssurance, stored KeyedHash) bool {
	if len(stored.MAC) != MACBytes {
		return false
	}
	want, err := k.sessionMAC(stored.KID, a)
	if err != nil {
		return false
	}
	return hmac.Equal(want, stored.MAC)
}

package payoutinstrument

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
)

// PayoutDestination is the destination an adapter pays to (ADR 0111 2.7).
// It carries the decrypted Detail, so it redacts under every rendering path:
// String, GoString, Format (%v %+v %#v %s %q ...), LogValue (slog) and JSON.
// There is deliberately NO Fingerprint and NO VerificationSource field: an
// adapter must not be able to read or echo the platform's fingerprint, and it
// has no business with the verification tier.
type PayoutDestination struct {
	InstrumentID   uuid.UUID
	Kind           string          // open set
	Detail         json.RawMessage // decrypted, kind-validated; never a PAN
	FingerprintKid string          // the adapter computes its echo under this kid
}

// String never renders Detail.
func (d PayoutDestination) String() string {
	return fmt.Sprintf("PayoutDestination{instrument=%s kind=%s detail=[REDACTED]}", d.InstrumentID, d.Kind)
}

// GoString never renders Detail.
func (d PayoutDestination) GoString() string { return d.String() }

// Format redacts under every verb.
func (d PayoutDestination) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(d.String())) }

// LogValue redacts for slog: only the instrument id, kind and kid.
func (d PayoutDestination) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("instrument_id", d.InstrumentID.String()),
		slog.String("kind", d.Kind),
		slog.String("detail", "[REDACTED]"),
	)
}

// MarshalJSON never renders Detail.
func (d PayoutDestination) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		InstrumentID uuid.UUID `json:"instrument_id"`
		Kind         string    `json:"kind"`
		Detail       string    `json:"detail"`
	}{d.InstrumentID, d.Kind, "[REDACTED]"})
}

// Empty reports whether no destination was supplied.
func (d PayoutDestination) Empty() bool { return d.InstrumentID == uuid.Nil || len(d.Detail) == 0 }

// DestinationEchoSemantics is the mandatory, explicit declaration every payout adapter makes about destination
// evidence (ADR 0111 24, owner decision 5 of ADR 0095 48). There is no default: the zero value is INVALID and is
// refused at registration and at startup.
type DestinationEchoSemantics uint8

const (
	// DestinationEchoUnset is the zero value: nothing was declared. INVALID; never a default.
	DestinationEchoUnset DestinationEchoSemantics = iota
	// DestinationEchoSupported: the adapter computes a tenant-bound fingerprint echo (inside the adapter, with the
	// injected DestinationFingerprinter) and returns it on every payout success, status poll and callback. A success
	// without it is ambiguous.
	DestinationEchoSupported
	// DestinationEchoUnsupported: the adapter explicitly cannot provide destination evidence. No echo is expected; an
	// absent echo is not evidence of anything and an echo that nevertheless arrives is untrusted (fail closed). The
	// compensating control is the statement-level evidence of the M4/reconciliation model. Visible at startup.
	DestinationEchoUnsupported
)

// Valid reports whether s is a declared state (not the zero value, not out of range).
func (s DestinationEchoSemantics) Valid() bool {
	return s == DestinationEchoSupported || s == DestinationEchoUnsupported
}

// String is the closed name used in logs and errors.
func (s DestinationEchoSemantics) String() string {
	switch s {
	case DestinationEchoSupported:
		return "supported"
	case DestinationEchoUnsupported:
		return "unsupported"
	case DestinationEchoUnset:
		return "unset"
	default:
		return "invalid"
	}
}

// DestinationEcho is the only destination evidence a provider result may carry
// (S95-C10 retained): a fingerprint computed INSIDE the adapter by the injected
// DestinationFingerprinter under the snapshot's kid. No raw payer-identifying
// field ever enters a result type.
type DestinationEcho struct {
	Fingerprint string
	Kid         string
}

// String renders only the kid.
func (e DestinationEcho) String() string { return "DestinationEcho{kid=" + e.Kid + "}" }

// DestinationFingerprinter is what an adapter is given to compute its echo
// (M-2). It exposes no key, no seal and no other capability.
type DestinationFingerprinter interface {
	// Echo normalises vendorDestination as the given kind (the same normaliser
	// registration uses) and returns the tenant-bound fingerprint under kid.
	// An unknown kid or an invalid vendor destination is an error; the caller
	// treats an error as a mismatch (A-8).
	Echo(kid, kind string, vendorDestination json.RawMessage) (string, error)
}

type fingerprinter struct {
	keys     *Keys
	tenantID uuid.UUID
	kinds    *KindRegistry
}

// FingerprinterFor returns the injectable, tenant-bound echo capability.
func (k *Keys) FingerprinterFor(tenantID uuid.UUID, kinds *KindRegistry) DestinationFingerprinter {
	return fingerprinter{keys: k, tenantID: tenantID, kinds: kinds}
}

func (f fingerprinter) Echo(kid, kind string, vendorDestination json.RawMessage) (string, error) {
	spec, err := f.kinds.Spec(kind)
	if err != nil {
		return "", err
	}
	n, err := spec.Normalize(vendorDestination)
	if err != nil {
		return "", err
	}
	return f.keys.Fingerprint(kid, f.tenantID, kind, n.FingerprintInput)
}

// String / GoString / Format keep the capability from rendering its keys.
func (f fingerprinter) String() string { return "payoutinstrument.DestinationFingerprinter{redacted}" }

// GoString redacts like String.
func (f fingerprinter) GoString() string { return f.String() }

// Format redacts under every verb.
func (f fingerprinter) Format(s fmt.State, _ rune) { _, _ = s.Write([]byte(f.String())) }

package payoutinstrument

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestDestinationEchoSemantics_ZeroIsInvalid(t *testing.T) {
	var zero DestinationEchoSemantics
	if zero.Valid() || zero != DestinationEchoUnset {
		t.Fatal("the zero value must be the invalid Unset state")
	}
	if !DestinationEchoSupported.Valid() || !DestinationEchoUnsupported.Valid() {
		t.Fatal("both explicit states are valid")
	}
	if DestinationEchoSemantics(7).Valid() {
		t.Fatal("an out-of-range value is invalid")
	}
}

func TestEchoWellFormed(t *testing.T) {
	good := strings.Repeat("ab", 32)
	for _, c := range []struct {
		name string
		e    DestinationEcho
		want bool
	}{
		{"ok", DestinationEcho{Fingerprint: good, Kid: "fp-1"}, true},
		{"kid space", DestinationEcho{Fingerprint: good, Kid: "a b"}, false},
		{"kid empty", DestinationEcho{Fingerprint: good, Kid: ""}, false},
		{"kid 33", DestinationEcho{Fingerprint: good, Kid: strings.Repeat("k", 33)}, false},
		{"short", DestinationEcho{Fingerprint: good[:63], Kid: "k"}, false},
		{"long", DestinationEcho{Fingerprint: good + "a", Kid: "k"}, false},
		{"upper", DestinationEcho{Fingerprint: strings.ToUpper(good), Kid: "k"}, false},
		{"non-hex", DestinationEcho{Fingerprint: strings.Repeat("g", 64), Kid: "k"}, false},
	} {
		if got := EchoWellFormed(c.e); got != c.want {
			t.Errorf("%s: %v want %v", c.name, got, c.want)
		}
	}
}

// EvaluateEcho: the declaration decides whether an echo may be considered at all. An echo from a non-Supported adapter is
// never a match, even when it equals the snapshot.
func TestEvaluateEcho(t *testing.T) {
	fp := strings.Repeat("ab", 32)
	snap := Snapshot{Fingerprint: fp, FingerprintKID: "f1"}
	same := &DestinationEcho{Fingerprint: fp, Kid: "f1"}
	for _, c := range []struct {
		name string
		echo *DestinationEcho
		sem  DestinationEchoSemantics
		want EchoVerdict
	}{
		{"supported match", same, DestinationEchoSupported, EchoMatch},
		{"supported absent", nil, DestinationEchoSupported, EchoAbsent},
		{"supported different", &DestinationEcho{Fingerprint: strings.Repeat("cd", 32), Kid: "f1"}, DestinationEchoSupported, EchoMismatch},
		{"supported unknown kid", &DestinationEcho{Fingerprint: fp, Kid: "zz"}, DestinationEchoSupported, EchoMismatch},
		{"supported malformed kid", &DestinationEcho{Fingerprint: fp, Kid: "a b"}, DestinationEchoSupported, EchoMalformed},
		{"supported malformed fp", &DestinationEcho{Fingerprint: "AB", Kid: "f1"}, DestinationEchoSupported, EchoMalformed},
		{"unsupported absent", nil, DestinationEchoUnsupported, EchoAbsent},
		{"unsupported equal echo is NOT a match", same, DestinationEchoUnsupported, EchoUnexpected},
		{"unsupported different echo", &DestinationEcho{Fingerprint: strings.Repeat("cd", 32), Kid: "f1"}, DestinationEchoUnsupported, EchoUnexpected},
		{"unset equal echo is NOT a match", same, DestinationEchoUnset, EchoUnexpected},
	} {
		got := EvaluateEcho(snap, c.echo, c.sem)
		if got != c.want {
			t.Errorf("%s: %v want %v", c.name, got, c.want)
		}
		if c.want == EchoMatch && got.Mismatch() || (c.want != EchoMatch && c.want != EchoAbsent) != got.Mismatch() {
			t.Errorf("%s: Mismatch() class wrong for %v", c.name, got)
		}
	}
}

type declAdapter struct{ id string }
type synthDeclAdapter struct{ id string }

func (synthDeclAdapter) SyntheticComponent() {}

type uncomparableAdapter []int

func TestVerifyStartup_EchoDeclarations(t *testing.T) {
	keys := newTestKeys(t)
	real := &declAdapter{id: "real-1"}
	synth := &synthDeclAdapter{id: "mock-1"}
	decl := func(a any, id string, capable bool, s DestinationEchoSemantics) PayoutEchoDeclaration {
		return PayoutEchoDeclaration{Adapter: a, ProviderID: id, PayoutCapable: capable, Semantics: s}
	}
	cases := []struct {
		name    string
		regs    Registrations
		wantErr bool
		warned  bool
	}{
		{"non-synthetic, supported", Registrations{PaymentAdapters: []any{real}, PayoutEchoDeclarations: []PayoutEchoDeclaration{decl(real, "real-1", true, DestinationEchoSupported)}}, false, false},
		{"non-synthetic, unsupported: starts but is marked", Registrations{PaymentAdapters: []any{real}, PayoutEchoDeclarations: []PayoutEchoDeclaration{decl(real, "real-1", true, DestinationEchoUnsupported)}}, false, true},
		{"non-synthetic, UNSET: refuses", Registrations{PaymentAdapters: []any{real}, PayoutEchoDeclarations: []PayoutEchoDeclaration{decl(real, "real-1", true, DestinationEchoUnset)}}, true, false},
		{"non-synthetic, out of range: refuses", Registrations{PaymentAdapters: []any{real}, PayoutEchoDeclarations: []PayoutEchoDeclaration{decl(real, "real-1", true, DestinationEchoSemantics(9))}}, true, false},
		{"non-synthetic, no declaration entry at all: refuses", Registrations{PaymentAdapters: []any{real}}, true, false},
		{"non-synthetic, declaration for a different adapter: refuses", Registrations{PaymentAdapters: []any{real}, PayoutEchoDeclarations: []PayoutEchoDeclaration{decl(&declAdapter{id: "other"}, "other", true, DestinationEchoSupported)}}, true, false},
		{"non-synthetic deposit-only: no payout declaration needed", Registrations{PaymentAdapters: []any{real}, PayoutEchoDeclarations: []PayoutEchoDeclaration{decl(real, "real-1", false, DestinationEchoUnset)}}, false, false},
		{"synthetic, unset: refuses (universally mandatory)", Registrations{PaymentAdapters: []any{synth}, PayoutEchoDeclarations: []PayoutEchoDeclaration{decl(synth, "mock-1", true, DestinationEchoUnset)}}, true, false},
		{"synthetic, unsupported: starts, no marker (MOCK)", Registrations{PaymentAdapters: []any{synth}, PayoutEchoDeclarations: []PayoutEchoDeclaration{decl(synth, "mock-1", true, DestinationEchoUnsupported)}}, false, false},
		{"synthetic, supported", Registrations{PaymentAdapters: []any{synth}, PayoutEchoDeclarations: []PayoutEchoDeclaration{decl(synth, "mock-1", true, DestinationEchoSupported)}}, false, false},
		{"uncomparable adapter value never panics and is not matched", Registrations{PaymentAdapters: []any{uncomparableAdapter(nil)}, PayoutEchoDeclarations: []PayoutEchoDeclaration{decl(uncomparableAdapter(nil), "u", true, DestinationEchoSupported)}}, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			defer slog.SetDefault(old)
			err := VerifyStartup("staging", keys, c.regs)
			if (err != nil) != c.wantErr || (err != nil && !errors.Is(err, ErrStartupGate)) {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			marked := strings.Contains(buf.String(), EchoUnsupportedStartupEvent)
			if marked != c.warned {
				t.Fatalf("startup marker present=%v want %v (log: %s)", marked, c.warned, buf.String())
			}
			if c.warned && !strings.Contains(buf.String(), "provider_id=real-1") {
				t.Fatalf("marker must name the provider: %s", buf.String())
			}
		})
	}
}

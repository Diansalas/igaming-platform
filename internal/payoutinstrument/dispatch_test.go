package payoutinstrument

import "testing"

// CompareEcho (ADR 0111 2.6): a provider-reported destination can only be COMPARED with the snapshot.
func TestCompareEcho(t *testing.T) {
	snap := Snapshot{Fingerprint: "aa11", FingerprintKID: "f1"}
	cases := []struct {
		name string
		echo *DestinationEcho
		want EchoVerdict
	}{
		{"absent", nil, EchoAbsent},
		{"equal under the snapshot kid", &DestinationEcho{Fingerprint: "aa11", Kid: "f1"}, EchoMatch},
		{"different fingerprint", &DestinationEcho{Fingerprint: "bb22", Kid: "f1"}, EchoMismatch},
		{"same fingerprint, different kid", &DestinationEcho{Fingerprint: "aa11", Kid: "f2"}, EchoMismatch},
		{"unknown kid", &DestinationEcho{Fingerprint: "aa11", Kid: "nope"}, EchoMismatch},
		{"empty kid", &DestinationEcho{Fingerprint: "aa11", Kid: ""}, EchoMismatch},
		{"empty fingerprint is never equal", &DestinationEcho{Fingerprint: "", Kid: "f1"}, EchoMismatch},
		{"empty snapshot fingerprint is never equal to an empty echo", &DestinationEcho{Fingerprint: "", Kid: ""}, EchoMismatch},
	}
	for _, c := range cases {
		if got := CompareEcho(snap, c.echo); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if CompareEcho(Snapshot{}, &DestinationEcho{}) != EchoMismatch {
		t.Error("an all-empty snapshot and echo must not compare equal")
	}
}

func TestRefusalConstructors(t *testing.T) {
	if g, ok := IsGateRefusal(RefuseIntegrity(ReasonSnapshotMissing)); !ok || !g.Integrity() || g.Reason != ReasonSnapshotMissing {
		t.Error("RefuseIntegrity must produce an integrity refusal")
	}
	if g, ok := IsGateRefusal(RefuseNotUsable(ReasonNoGate)); !ok || g.Integrity() || g.Reason != ReasonNoGate {
		t.Error("RefuseNotUsable must produce a non-integrity refusal")
	}
}

package totp

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// rfcSeed is the published RFC 6238 Appendix B SHA-1 test seed. It is a public
// test vector, not a secret.
var rfcSeed = []byte("12345678901234567890")

// RFC 6238 Appendix B, the SHA-1 rows (8-digit values).
var rfcVectors = []struct {
	unix int64
	step int64
	code string
}{
	{59, 0x1, "94287082"},
	{1111111109, 0x23523EC, "07081804"},
	{1111111111, 0x23523ED, "14050471"},
	{1234567890, 0x273EF07, "89005924"},
	{2000000000, 0x3F940AA, "69279037"},
	{20000000000, 0x27BC86AA, "65353130"},
}

func TestRFC6238_AppendixB_SHA1_EightDigits(t *testing.T) {
	for _, v := range rfcVectors {
		step, err := StepAt(time.Unix(v.unix, 0))
		if err != nil {
			t.Fatal(err)
		}
		if step != v.step {
			t.Fatalf("T=%d: step %X, want %X", v.unix, step, v.step)
		}
		if got := string(hotp(rfcSeed, uint64(step), 8)); got != v.code {
			t.Fatalf("T=%d: code %s, want %s", v.unix, got, v.code)
		}
	}
}

func TestRFC6238_AppendixB_SHA1_SixDigitsAreTheLowOrderDigits(t *testing.T) {
	for _, v := range rfcVectors {
		got, err := CodeAt(rfcSeed, time.Unix(v.unix, 0))
		if err != nil {
			t.Fatal(err)
		}
		if want := v.code[2:]; got != want {
			t.Fatalf("T=%d: 6-digit code %s, want %s", v.unix, got, want)
		}
		// Verify accepts it at that time with a fresh factor.
		step, err := Verify(rfcSeed, got, time.Unix(v.unix, 0), 0)
		if err != nil || step != v.step {
			t.Fatalf("T=%d: Verify = %d, %v; want %d", v.unix, step, err, v.step)
		}
	}
}

// RFC 4226 Appendix D, HOTP with the same seed: pins the dynamic truncation.
func TestRFC4226_AppendixD_HOTP(t *testing.T) {
	want := []string{"755224", "287082", "359152", "969429", "338314", "254676", "287922", "162583", "399871", "520489"}
	for i, w := range want {
		if got := string(hotp(rfcSeed, uint64(i), 6)); got != w {
			t.Fatalf("counter %d: %s, want %s", i, got, w)
		}
	}
}

func TestParameters_Pinned(t *testing.T) {
	if Digits != 6 || Period != 30 || Skew != 1 || SecretBytes != 20 {
		t.Fatal("ADR 0119 section 4.3 parameters changed")
	}
}

// at returns a time in the middle of step s.
func at(s int64) time.Time { return time.Unix(s*Period+15, 0) }

func code(t *testing.T, s int64) string {
	t.Helper()
	c, err := CodeAtStep(rfcSeed, s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const baseStep = int64(57000000)

func TestVerify_WindowIsExactlyPlusMinusOne(t *testing.T) {
	now := at(baseStep)
	for d := int64(-3); d <= 3; d++ {
		step, err := Verify(rfcSeed, code(t, baseStep+d), now, 0)
		inWindow := d >= -1 && d <= 1
		if inWindow && (err != nil || step != baseStep+d) {
			t.Fatalf("offset %d must be accepted at its own step: %d, %v", d, step, err)
		}
		if !inWindow && !errors.Is(err, ErrRejected) {
			t.Fatalf("offset %d must be refused, got %d, %v", d, step, err)
		}
	}
}

func TestVerify_WindowBoundariesWithinAStep(t *testing.T) {
	// First and last second of the current step: the window is step based.
	for _, now := range []time.Time{time.Unix(baseStep*Period, 0), time.Unix(baseStep*Period+Period-1, 0)} {
		if _, err := Verify(rfcSeed, code(t, baseStep+1), now, 0); err != nil {
			t.Fatalf("T+1 at %v: %v", now, err)
		}
		if _, err := Verify(rfcSeed, code(t, baseStep-1), now, 0); err != nil {
			t.Fatalf("T-1 at %v: %v", now, err)
		}
		if _, err := Verify(rfcSeed, code(t, baseStep+2), now, 0); !errors.Is(err, ErrRejected) {
			t.Fatalf("T+2 at %v must be refused: %v", now, err)
		}
	}
}

func TestVerify_ReplayRule_SameCodeTwice(t *testing.T) {
	now := at(baseStep)
	c := code(t, baseStep)
	step, err := Verify(rfcSeed, c, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The caller persists step; the same code again is refused.
	if _, err := Verify(rfcSeed, c, now, step); !errors.Is(err, ErrRejected) {
		t.Fatalf("a used code must be refused, got %v", err)
	}
	// So is it one step later, while still inside the window.
	if _, err := Verify(rfcSeed, c, at(baseStep+1), step); !errors.Is(err, ErrRejected) {
		t.Fatalf("a used code must be refused inside the window, got %v", err)
	}
}

func TestVerify_ReplayRule_OlderStepAfterNewerIsRefused(t *testing.T) {
	now := at(baseStep)
	if _, err := Verify(rfcSeed, code(t, baseStep), now, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(rfcSeed, code(t, baseStep-1), now, baseStep); !errors.Is(err, ErrRejected) {
		t.Fatalf("T-1 after T was accepted must be refused (monotonic), got %v", err)
	}
	// The next step is accepted.
	if s, err := Verify(rfcSeed, code(t, baseStep+1), now, baseStep); err != nil || s != baseStep+1 {
		t.Fatalf("T+1 after T: %d, %v", s, err)
	}
}

func TestVerify_ReplayRule_StrictlyGreater(t *testing.T) {
	// lastUsedStep == candidate is refused; lastUsedStep == candidate-1 accepted.
	now := at(baseStep)
	c := code(t, baseStep)
	if _, err := Verify(rfcSeed, c, now, baseStep); !errors.Is(err, ErrRejected) {
		t.Fatalf("s == last_used_step must be refused, got %v", err)
	}
	if s, err := Verify(rfcSeed, c, now, baseStep-1); err != nil || s != baseStep {
		t.Fatalf("s == last_used_step+1 must be accepted, got %d, %v", s, err)
	}
}

func TestVerify_WrongCodeRefused(t *testing.T) {
	now := at(baseStep)
	c := code(t, baseStep)
	wrong := []byte(c)
	wrong[5] = '0' + (wrong[5]-'0'+1)%10
	if _, err := Verify(rfcSeed, string(wrong), now, 0); !errors.Is(err, ErrRejected) {
		t.Fatalf("a wrong code must be refused, got %v", err)
	}
	other := []byte("abcdefghijabcdefghij")
	if _, err := Verify(other, c, now, 0); !errors.Is(err, ErrRejected) {
		t.Fatalf("a code for another secret must be refused, got %v", err)
	}
}

func TestVerify_HighestMatchingStepIsBurned(t *testing.T) {
	// Find a step where two window candidates share a code (a 1e-6 event) by
	// search over a bounded range, then check the highest is returned.
	for s := int64(1); s < 3_000_000; s++ {
		a, _ := CodeAtStep(rfcSeed, s)
		b, _ := CodeAtStep(rfcSeed, s+1)
		if a != b {
			continue
		}
		got, err := Verify(rfcSeed, a, at(s), 0)
		if err != nil || got != s+1 {
			t.Fatalf("collision at %d: got %d, %v; want %d", s, got, err, s+1)
		}
		// With s+1 burned, the code is refused even though s is in the window.
		if _, err := Verify(rfcSeed, a, at(s), s+1); !errors.Is(err, ErrRejected) {
			t.Fatalf("collision at %d: replay accepted", s)
		}
		// With only s burned, s+1 is still acceptable.
		if got, err := Verify(rfcSeed, a, at(s), s); err != nil || got != s+1 {
			t.Fatalf("collision at %d: %d, %v", s, got, err)
		}
		return
	}
	t.Skip("no adjacent-step collision in the searched range")
}

func TestVerify_FailsClosedOnMalformedInput(t *testing.T) {
	now := at(baseStep)
	good := code(t, baseStep)
	cases := []struct {
		name   string
		secret []byte
		code   string
		now    time.Time
		last   int64
		want   error
	}{
		{"nil secret", nil, good, now, 0, ErrInvalidSecret},
		{"short secret", rfcSeed[:19], good, now, 0, ErrInvalidSecret},
		{"long secret", append(append([]byte{}, rfcSeed...), 'x'), good, now, 0, ErrInvalidSecret},
		{"empty code", rfcSeed, "", now, 0, ErrInvalidCode},
		{"five digits", rfcSeed, good[:5], now, 0, ErrInvalidCode},
		{"seven digits", rfcSeed, good + "0", now, 0, ErrInvalidCode},
		{"space", rfcSeed, good[:3] + " " + good[4:], now, 0, ErrInvalidCode},
		{"leading space", rfcSeed, " " + good[1:], now, 0, ErrInvalidCode},
		{"letter", rfcSeed, good[:5] + "a", now, 0, ErrInvalidCode},
		{"sign", rfcSeed, "+" + good[1:], now, 0, ErrInvalidCode},
		{"unicode digit", rfcSeed, good[:5] + "١", now, 0, ErrInvalidCode},
		{"negative last step", rfcSeed, good, now, -1, ErrInvalidState},
		{"before epoch", rfcSeed, good, time.Unix(-1, 0), 0, ErrInvalidState},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			step, err := Verify(tc.secret, tc.code, tc.now, tc.last)
			if !errors.Is(err, tc.want) || step != 0 {
				t.Fatalf("got %d, %v; want %v", step, err, tc.want)
			}
			if tc.code != "" && strings.Contains(err.Error(), tc.code) {
				t.Fatal("error echoes the code")
			}
		})
	}
}

func TestVerify_NearEpoch(t *testing.T) {
	// Step 0: the T-1 candidate is negative and never eligible.
	c0 := code(t, 0)
	if s, err := Verify(rfcSeed, c0, time.Unix(10, 0), 0); !errors.Is(err, ErrRejected) {
		t.Fatalf("step 0 can never be > last_used_step 0: %d, %v", s, err)
	}
	c1 := code(t, 1)
	if s, err := Verify(rfcSeed, c1, time.Unix(10, 0), 0); err != nil || s != 1 {
		t.Fatalf("step 1 at step 0: %d, %v", s, err)
	}
}

func TestCodeAt_FailsClosed(t *testing.T) {
	if _, err := CodeAt(rfcSeed[:10], time.Unix(100, 0)); !errors.Is(err, ErrInvalidSecret) {
		t.Fatal(err)
	}
	if _, err := CodeAt(rfcSeed, time.Unix(-30, 0)); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
	if _, err := CodeAtStep(rfcSeed, -1); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
}

func TestErrors_NeverCarrySecretMaterial(t *testing.T) {
	for _, e := range []error{ErrInvalidSecret, ErrInvalidCode, ErrInvalidState, ErrRejected} {
		if s := fmt.Sprintf("%v %+v %#v", e, e, e); strings.Contains(s, string(rfcSeed)) {
			t.Fatal("error leaks the secret")
		}
	}
}

func FuzzVerify_NeverPanicsAndNeverAcceptsMalformed(f *testing.F) {
	f.Add("123456", int64(1700000000), int64(0))
	f.Add("12345", int64(0), int64(-1))
	f.Fuzz(func(t *testing.T, c string, unix, last int64) {
		step, err := Verify(rfcSeed, c, time.Unix(unix, 0), last)
		if err == nil {
			if !validCode(c) || last < 0 || unix < 0 || step <= last {
				t.Fatalf("accepted malformed input %q unix=%d last=%d step=%d", c, unix, last, step)
			}
			cur := unix / Period
			if step < cur-1 || step > cur+1 {
				t.Fatalf("accepted step %d outside window of %d", step, cur)
			}
		}
	})
}

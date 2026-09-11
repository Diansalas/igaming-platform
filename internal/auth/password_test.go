package auth

import "testing"

func TestHashAndVerifyPassword_RoundTrip(t *testing.T) {
	hash, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("unexpected error hashing password: %v", err)
	}

	ok, err := VerifyPassword("correct-horse-battery-staple", hash)
	if err != nil {
		t.Fatalf("unexpected error verifying password: %v", err)
	}
	if !ok {
		t.Error("expected the correct password to verify")
	}
}

func TestVerifyPassword_RejectsWrongPassword(t *testing.T) {
	hash, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("unexpected error hashing password: %v", err)
	}

	ok, err := VerifyPassword("wrong-password", hash)
	if err != nil {
		t.Fatalf("unexpected error verifying password: %v", err)
	}
	if ok {
		t.Error("expected the wrong password to fail verification")
	}
}

func TestHashPassword_RejectsEmptyPassword(t *testing.T) {
	if _, err := HashPassword(""); err == nil {
		t.Fatal("expected an error hashing an empty password, got nil")
	}
}

func TestHashPassword_ProducesUniqueSaltPerCall(t *testing.T) {
	hash1, err := HashPassword("same-password")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	hash2, err := HashPassword("same-password")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hash1 == hash2 {
		t.Error("expected two hashes of the same password to differ (unique random salt per call)")
	}

	// Both must still verify correctly despite differing.
	for _, h := range []string{hash1, hash2} {
		ok, err := VerifyPassword("same-password", h)
		if err != nil || !ok {
			t.Errorf("expected hash %q to verify, ok=%v err=%v", h, ok, err)
		}
	}
}

func TestDummyPasswordHash_IsWellFormedAndRejectsOrdinaryInput(t *testing.T) {
	// DummyPasswordHash exists so login handlers can pay the same Argon2
	// cost on the "no such account" branch as a real wrong-password
	// check, closing a timing side-channel that would otherwise reveal
	// account existence. It must parse successfully (a parse error would
	// short-circuit before paying that cost) and must reject any
	// candidate a login request could plausibly submit - it is not
	// associated with any real account, so nothing should ever need to
	// verify against it.
	for _, candidate := range []string{"", "password", "correct-horse-battery-staple", "anything at all"} {
		ok, err := VerifyPassword(candidate, DummyPasswordHash)
		if err != nil {
			t.Fatalf("expected DummyPasswordHash to parse without error, got: %v", err)
		}
		if ok {
			t.Errorf("expected DummyPasswordHash to reject %q, but it verified", candidate)
		}
	}
}

func TestVerifyPassword_RejectsMalformedHash(t *testing.T) {
	cases := []string{
		"",
		"not-a-hash-at-all",
		"argon2id$v=19$m=65536,t=1,p=4$onlyonepart",
		"bcrypt$somehash", // wrong algorithm prefix
	}
	for _, c := range cases {
		if _, err := VerifyPassword("anything", c); err == nil {
			t.Errorf("expected an error verifying against malformed hash %q, got nil", c)
		}
	}
}

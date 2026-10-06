package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

// PRH-2 R5 (ADR 0110): ACTOR_PROOF_KEYS / ACTOR_PROOF_ACTIVE_KID validation and
// redaction. Keys are generated at runtime (never literals).

func b64Key(t *testing.T, n int) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString([]byte(randomKey(t, n)))
}

func TestConfig_ActorProofKeys_AbsentIsValidHere(t *testing.T) {
	baseEnv(t, "production")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("absent keys must not fail Load (production's requirement is main's startup gate): %v", err)
	}
	if cfg.ActorProofKeys.IsSet() {
		t.Fatal("absent keys must be unset")
	}
}

func TestConfig_ActorProofKeys_ValidationAndRotation(t *testing.T) {
	baseEnv(t, "production")
	k1, k2 := b64Key(t, 32), b64Key(t, 40)
	t.Setenv("ACTOR_PROOF_KEYS", "k1:"+k1+",k2:"+k2) // >= 2 active keys during a rotation
	t.Setenv("ACTOR_PROOF_ACTIVE_KID", "k2")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("two keys with an active kid must load: %v", err)
	}
	if cfg.ActorProofActiveKID != "k2" {
		t.Fatalf("active kid = %q", cfg.ActorProofActiveKID)
	}

	t.Setenv("ACTOR_PROOF_ACTIVE_KID", "k9")
	if _, err := Load(); err == nil {
		t.Fatal("an active kid with no key must be refused")
	}
	t.Setenv("ACTOR_PROOF_ACTIVE_KID", "")
	if _, err := Load(); err == nil {
		t.Fatal("keys without an active kid must be refused")
	}
	t.Setenv("ACTOR_PROOF_ACTIVE_KID", "k1")
	short := b64Key(t, 31)
	t.Setenv("ACTOR_PROOF_KEYS", "k1:"+short)
	if _, err := Load(); err == nil || strings.Contains(err.Error(), short) {
		t.Fatalf("a 31-byte key must be refused without echoing it, got %v", err)
	}
	t.Setenv("ACTOR_PROOF_KEYS", "k1:not base64!")
	if _, err := Load(); err == nil {
		t.Fatal("malformed entry must be refused")
	}
}

func TestConfig_ActorProofKeys_MustDifferFromJWTSecret(t *testing.T) {
	baseEnv(t, "development")
	secret := randomKey(t, 40)
	t.Setenv("JWT_SIGNING_SECRET", secret)
	t.Setenv("ACTOR_PROOF_KEYS", "k1:"+base64.StdEncoding.EncodeToString([]byte(secret)))
	t.Setenv("ACTOR_PROOF_ACTIVE_KID", "k1")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("an actor-proof key equal to the JWT secret must be refused, got %v", err)
	}
}

func TestConfig_ActorProofKeys_Redacted(t *testing.T) {
	baseEnv(t, "development")
	k := b64Key(t, 32)
	t.Setenv("ACTOR_PROOF_KEYS", "k1:"+k)
	t.Setenv("ACTOR_PROOF_ACTIVE_KID", "k1")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if out := cfg.ActorProofKeys.String(); strings.Contains(out, k) {
		t.Fatal("ActorProofKeys rendered key material")
	}
}

package config

import (
	"strings"
	"testing"
)

// Stage 10.3 W3b wiring: SecretStoreAWSRegion comes from
// AWS_SECRETSMANAGER_REGION, falling back to AWS_REGION, and is validated
// only when "awssm" is a configured backend.

func TestConfig_SecretStoreAWSRegion_Sources(t *testing.T) {
	for _, tc := range []struct {
		name, smRegion, awsRegion, want string
	}{
		{"secrets manager region", "eu-west-1", "", "eu-west-1"},
		{"falls back to AWS_REGION", "", "eu-central-1", "eu-central-1"},
		{"secrets manager region wins", "eu-west-1", "us-east-1", "eu-west-1"},
		{"gov partition shape", "us-gov-west-1", "", "us-gov-west-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			baseEnv(t, "staging")
			t.Setenv("SECRETSTORE_BACKENDS", "awssm")
			if tc.smRegion != "" {
				t.Setenv("AWS_SECRETSMANAGER_REGION", tc.smRegion)
			}
			if tc.awsRegion != "" {
				t.Setenv("AWS_REGION", tc.awsRegion)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.SecretStoreAWSRegion != tc.want {
				t.Fatalf("region = %q, want %q", cfg.SecretStoreAWSRegion, tc.want)
			}
		})
	}
}

func TestConfig_SecretStoreAWSRegion_RequiredAndWellFormedOnlyWithAWSSM(t *testing.T) {
	for _, tc := range []struct{ name, region string }{
		{"missing", ""},
		{"malformed", "EU-WEST-1"},
		{"not a region", "eu-west-1; rm -rf /"},
		{"no number", "eu-west"},
	} {
		t.Run("awssm/"+tc.name, func(t *testing.T) {
			baseEnv(t, "production")
			t.Setenv("SECRETSTORE_BACKENDS", "awssm")
			if tc.region != "" {
				t.Setenv("AWS_REGION", tc.region)
			}
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "AWS_") {
				t.Fatalf("awssm with region %q must refuse startup naming the variable, got %v", tc.region, err)
			}
		})
		t.Run("no awssm/"+tc.name, func(t *testing.T) {
			baseEnv(t, "production")
			if tc.region != "" {
				t.Setenv("AWS_REGION", tc.region)
			}
			if _, err := Load(); err != nil {
				t.Fatalf("without awssm the region must not be validated, got %v", err)
			}
		})
	}
}

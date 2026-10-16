//go:build stagingacceptance

package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// config is the whole input of a run. Secrets are never values here, only file paths (see actorSpec).
type config struct {
	BaseURL    string
	Tenant     string // tenant slug of the synthetic tenant under test
	Brand      string // brand slug of the synthetic brand under test
	OutFile    string
	RunID      string
	Timeout    time.Duration
	Debug      bool
	Only       *regexp.Regexp
	Strict     bool // exit non-zero when any check was skipped for a missing prerequisite
	ExpectEnv  string
	CORSOrigin string
	EdgeHeader string // auto | require | skip

	// Market gate / evidence (see README "Market evidence dependency").
	MarketExpect string // open | closed
	Residence    string // PLAYER_RESIDENCE_COUNTRY: declared by the player after registration when set
	KYCResidence string // KYC_VERIFIED_RESIDENCE_COUNTRY: set by the compliance reviewer on the KYC approval when set

	Asset          string
	SecondAsset    string // a second fiat asset for the wallet-per-asset check ("" disables)
	DepositAmount  int64
	WithdrawAmount int64
	StakeAmount    int64

	Configure       bool // apply the synthetic tenant's provider capabilities and casino game opt-in (idempotent PUTs)
	KillSwitchDrill bool // engage and release a payments kill switch (the sanctioned synthetic alert); runs last
	SecondTenant    string
	SecondBrand     string
	ProveStaleStep  bool
	ReconWait       time.Duration

	Actors map[string]actorSpec
}

var actorRoles = []string{"ADMIN", "ADMIN2", "TENANT_ADMIN", "FINANCE1", "FINANCE2", "COMPLIANCE", "RISK_MANAGER"}

func envOr(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return def
}

func envBool(name string, def bool) bool {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt64(name string, def int64) int64 {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

var (
	reSlug  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
	reAsset = regexp.MustCompile(`^[A-Z0-9]{2,12}$`)
	reCC    = regexp.MustCompile(`^[A-Z]{2}$`)
)

// parseConfig reads flags (defaults come from the environment) and validates them. It returns a user-facing error
// for any invalid or unsafe input; nothing here ever echoes a secret value.
func parseConfig(args []string) (*config, error) {
	fs := flag.NewFlagSet("staging-acceptance", flag.ContinueOnError)
	c := &config{Actors: map[string]actorSpec{}}
	fs.StringVar(&c.BaseURL, "base-url", envOr("BASE_URL", ""), "public API base URL (env BASE_URL), e.g. https://dxxxx.cloudfront.net")
	fs.StringVar(&c.Tenant, "tenant", envOr("TENANT", ""), "slug of the synthetic tenant under test (env TENANT)")
	fs.StringVar(&c.Brand, "brand", envOr("BRAND", ""), "slug of the synthetic brand under test (env BRAND)")
	fs.StringVar(&c.OutFile, "out", envOr("RESULTS_FILE", "staging-acceptance-results.txt"), "results file, RESULT|name|PASS/FAIL/SKIP|detail (env RESULTS_FILE)")
	fs.StringVar(&c.RunID, "run-id", envOr("RUN_ID", ""), "short run id used in request ids and synthetic names (default: random)")
	timeout := fs.Duration("timeout", 20*time.Second, "per-request timeout")
	fs.BoolVar(&c.Debug, "debug", envBool("DEBUG", false), "print scrubbed request/response lines to stderr (local use)")
	only := fs.String("only", envOr("ONLY", ""), "regexp: run only checks whose name matches (dependents will report blocked)")
	fs.BoolVar(&c.Strict, "strict", envBool("STRICT", false), "exit 3 if any check was skipped for a missing prerequisite")
	fs.StringVar(&c.ExpectEnv, "expect-app-env", envOr("EXPECT_APP_ENV", "staging"), "APP_ENV the stack must report in launch-readiness (env EXPECT_APP_ENV)")
	fs.StringVar(&c.CORSOrigin, "cors-origin", envOr("CORS_ORIGIN", ""), "an Origin that CORS_ALLOWED_ORIGINS allows (env CORS_ORIGIN); empty skips the CORS check")
	fs.StringVar(&c.EdgeHeader, "edge-headers", envOr("EDGE_HEADERS", "auto"), "auto|require|skip: CloudFront security headers (auto: required when BASE_URL is https)")
	fs.StringVar(&c.MarketExpect, "market-expect", envOr("MARKET_EXPECT", "open"), "open|closed: whether the mock country is expected OPEN for the brand (closed proves fail-closed refusal)")
	fs.StringVar(&c.Residence, "player-residence", envOr("PLAYER_RESIDENCE_COUNTRY", ""), "ISO-3166 alpha-2 residence the synthetic player declares (KYC-residence evidence); empty = do not declare")
	fs.StringVar(&c.KYCResidence, "kyc-verified-residence", envOr("KYC_VERIFIED_RESIDENCE_COUNTRY", ""), "ISO-3166 alpha-2 verified-residence determination attached to the KYC approval by the compliance reviewer (needs jurisdiction evidence collection active); empty = none")
	fs.StringVar(&c.Asset, "asset", envOr("ASSET_CODE", "EUR"), "fiat asset code used for deposits/withdrawals/casino")
	fs.StringVar(&c.SecondAsset, "second-asset", envOr("SECOND_ASSET", "USD"), "second fiat asset for the wallet-per-asset check (empty disables)")
	fs.Int64Var(&c.DepositAmount, "deposit-amount", envInt64("DEPOSIT_AMOUNT", 10000), "deposit amount in minor units")
	fs.Int64Var(&c.WithdrawAmount, "withdraw-amount", envInt64("WITHDRAW_AMOUNT", 2500), "withdrawal amount in minor units")
	fs.Int64Var(&c.StakeAmount, "stake-amount", envInt64("STAKE_AMOUNT", 1000), "casino stake in minor units")
	fs.BoolVar(&c.Configure, "configure", envBool("CONFIGURE", true), "apply idempotent synthetic-tenant configuration (provider capabilities, casino opt-in) when credentials allow")
	fs.BoolVar(&c.KillSwitchDrill, "kill-switch-drill", envBool("KILL_SWITCH_DRILL", true), "engage then release a payments kill switch (raises the sanctioned synthetic alert); needs ADMIN and ADMIN2")
	fs.StringVar(&c.SecondTenant, "second-tenant", envOr("SECOND_TENANT", "acceptance-tenant-b"), "slug of the isolation tenant created/reused by the suite (empty disables)")
	fs.BoolVar(&c.ProveStaleStep, "prove-stale-stepup", envBool("PROVE_STALE_STEPUP", false), "wait past the critical step-up window and prove a stale second factor is refused (adds ~5.5 min)")
	reconWait := fs.Duration("recon-wait", 0, "wait up to this long for the first reconciliation run (env RECON_WAIT_SECONDS also accepted)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: staging-acceptance [flags]   (see deploy/aws/acceptance/README.md; secrets are FILE paths in the environment)")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	c.Timeout = *timeout
	c.ReconWait = *reconWait
	if v := envInt64("RECON_WAIT_SECONDS", 0); v > 0 && c.ReconWait == 0 {
		c.ReconWait = time.Duration(v) * time.Second
	}
	if *only != "" {
		re, err := regexp.Compile(*only)
		if err != nil {
			return nil, fmt.Errorf("-only is not a valid regexp: %v", err)
		}
		c.Only = re
	}
	c.SecondBrand = envOr("SECOND_BRAND", "")
	if c.SecondBrand == "" && c.SecondTenant != "" {
		c.SecondBrand = c.SecondTenant + "-brand"
	}
	if c.RunID == "" {
		c.RunID = randHex(4)
	}
	for _, r := range actorRoles {
		spec := actorSpec{
			Role:          r,
			Email:         envOr(r+"_EMAIL", ""),
			PasswordFile:  envOr(r+"_PASSWORD_FILE", ""),
			TOTPFile:      envOr(r+"_TOTP_SECRET_FILE", ""),
			BootstrapFile: envOr(r+"_BOOTSTRAP_TOKEN_FILE", ""),
		}
		if r != "ADMIN" && r != "ADMIN2" {
			spec.TenantSlug = c.Tenant
		}
		c.Actors[r] = spec
	}
	return c, c.validate()
}

func (c *config) validate() error {
	var errs []string
	if c.BaseURL == "" {
		errs = append(errs, "BASE_URL is required")
	}
	if !reSlug.MatchString(c.Tenant) {
		errs = append(errs, "TENANT (tenant slug) is required and must be a lowercase slug")
	}
	if !reSlug.MatchString(c.Brand) {
		errs = append(errs, "BRAND (brand slug) is required and must be a lowercase slug")
	}
	if !reAsset.MatchString(c.Asset) {
		errs = append(errs, "ASSET_CODE must look like EUR")
	}
	if c.SecondAsset != "" && (!reAsset.MatchString(c.SecondAsset) || c.SecondAsset == c.Asset) {
		errs = append(errs, "SECOND_ASSET must look like USD and differ from ASSET_CODE (or be empty)")
	}
	if c.MarketExpect != "open" && c.MarketExpect != "closed" {
		errs = append(errs, "MARKET_EXPECT must be open or closed")
	}
	if c.Residence != "" && !reCC.MatchString(c.Residence) {
		errs = append(errs, "PLAYER_RESIDENCE_COUNTRY must be an uppercase ISO-3166 alpha-2 code")
	}
	if c.KYCResidence != "" && !reCC.MatchString(c.KYCResidence) {
		errs = append(errs, "KYC_VERIFIED_RESIDENCE_COUNTRY must be an uppercase ISO-3166 alpha-2 code")
	}
	switch c.EdgeHeader {
	case "auto", "require", "skip":
	default:
		errs = append(errs, "EDGE_HEADERS must be auto, require or skip")
	}
	if c.DepositAmount <= 0 || c.WithdrawAmount <= 0 || c.StakeAmount <= 0 || c.WithdrawAmount > c.DepositAmount || c.StakeAmount > c.DepositAmount {
		errs = append(errs, "amounts must be positive and withdraw/stake must not exceed the deposit")
	}
	if c.SecondTenant != "" && (!reSlug.MatchString(c.SecondTenant) || c.SecondTenant == c.Tenant) {
		errs = append(errs, "SECOND_TENANT must be a slug different from TENANT")
	}
	admin := c.Actors["ADMIN"]
	if !admin.configured() {
		errs = append(errs, "ADMIN_EMAIL and ADMIN_PASSWORD_FILE are required")
	}
	if admin.TOTPFile == "" {
		errs = append(errs, "ADMIN_TOTP_SECRET_FILE is required (existing secret, or the 0600 path the new enrolment secret is written to)")
	}
	for _, r := range actorRoles {
		s := c.Actors[r]
		if (s.Email != "") != (s.PasswordFile != "") {
			errs = append(errs, r+"_EMAIL and "+r+"_PASSWORD_FILE must be set together")
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

//go:build e2efixture

// Command aml-fixture is a TEST-SUPPORT tool of the Back Office end-to-end harness (lic22). It is NOT a production path: it carries the
// build tag e2efixture (never built by `go build ./...`, never part of platform-api) and refuses to run unless APP_ENV is
// "development" or "test".
//
// Why it exists: the jurisdiction SCREENING policy (lists, re-screen interval, backfill grace) that a withdrawal-gating trigger policy
// requires has no HTTP route and no Back Office screen (lic20 record, section 3). The only way to author it is the AML officer service
// (aml.Service.SetScreeningPolicy). This tool calls exactly that service, as a real platform officer, over the RUNTIME database role,
// signing the actor proof with the same configured proof key platform-api uses. Every database check (the in-force aml_officer grant,
// the officer quorum of the owner scope, the signed proof, the tenant-jurisdiction rule) applies unchanged; nothing is bypassed.
//
// Usage: aml-fixture screening-policy -tenant UUID -staff UUID -jurisdiction UUID -lists sanctions,pep -legal-ref REF
// Env: DATABASE_URL (runtime role, private database), ACTOR_PROOF_KEYS, ACTOR_PROOF_ACTIVE_KID, APP_ENV.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/db"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "aml-fixture:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if env := os.Getenv("APP_ENV"); env != "development" && env != "test" {
		return fmt.Errorf("refused: APP_ENV=%q (this tool runs only in a development or test environment)", env)
	}
	if len(args) == 0 || args[0] != "screening-policy" {
		return fmt.Errorf("usage: aml-fixture screening-policy -tenant UUID -staff UUID -jurisdiction UUID [-lists sanctions,pep] [-legal-ref REF]")
	}
	fs := flag.NewFlagSet("screening-policy", flag.ContinueOnError)
	tenant := fs.String("tenant", "", "tenant id")
	staff := fs.String("staff", "", "platform officer staff id")
	jur := fs.String("jurisdiction", "", "licensing jurisdiction id")
	lists := fs.String("lists", "sanctions,pep", "comma separated lists")
	legal := fs.String("legal-ref", "E2E-SYNTHETIC-NOT-A-LEGAL-REVIEW", "legal review reference (a synthetic marker)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	tid, e1 := uuid.Parse(*tenant)
	sid, e2 := uuid.Parse(*staff)
	jid, e3 := uuid.Parse(*jur)
	if e1 != nil || e2 != nil || e3 != nil {
		return fmt.Errorf("-tenant, -staff and -jurisdiction must be UUIDs")
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" || strings.Contains(url, "prod") {
		return fmt.Errorf("DATABASE_URL is required and must not name a production database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, url, 2, 10*time.Second)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	issuer, err := actorproof.NewIssuerFromConfig(os.Getenv("ACTOR_PROOF_ACTIVE_KID"), os.Getenv("ACTOR_PROOF_KEYS"))
	if err != nil {
		return fmt.Errorf("actor proof keys: %w", err)
	}
	svc, err := aml.New(aml.Config{Pool: pool, Proofs: issuer})
	if err != nil {
		return err
	}
	in := aml.SetScreeningPolicyInput{
		JurisdictionID: jid, RescreenInterval: 24 * time.Hour, BackfillGrace: time.Hour,
		EffectiveFrom: time.Now().Add(-time.Minute), LegalReviewReference: *legal, ReasonCode: "e2e_synthetic_fixture",
	}
	for _, l := range strings.Split(*lists, ",") {
		in.Lists = append(in.Lists, aml.ListType(strings.TrimSpace(l)))
	}
	id, err := svc.SetScreeningPolicy(ctx, aml.Actor{StaffID: sid, Scope: aml.ScopePlatformActing, TenantID: tid}, in)
	if err != nil {
		return fmt.Errorf("SetScreeningPolicy refused: %w", err)
	}
	fmt.Println(id.String())
	return nil
}

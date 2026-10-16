//go:build stagingacceptance

// Command staging-acceptance is the black-box acceptance suite for the deployed AWS staging stack
// (docs/runbooks/aws-staging-deploy-plan.md sections 6.3 to 6.6, 9 and 10). It talks to the public API URL only,
// against MOCK providers and synthetic data, and never touches AWS. It is built with the `stagingacceptance` build
// tag so it can never run as part of `go test ./...` or CI:
//
//	go run -tags stagingacceptance ./deploy/aws/acceptance        (see README.md for the full input list)
//
// Secrets are given as 0600 FILE paths in the environment, never as arguments or values, and every line that leaves
// the process (stdout, stderr, the results file) passes through the scrubber.
package main

import (
	"fmt"
	"io"
	"os"
	"time"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// Exit codes: 0 all checks passed (skips allowed unless -strict); 1 at least one FAIL; 2 invalid configuration or
// the results file could not be written; 3 -strict and a prerequisite SKIP remains.
func run(args []string, stdout, stderr io.Writer) int {
	cfg, err := parseConfig(args)
	if err != nil {
		if err.Error() != "flag: help requested" {
			_, _ = fmt.Fprintln(stderr, "staging-acceptance:", err)
		}
		return 2
	}
	scrub := newScrubber()
	out, err := openResultsFile(cfg.OutFile)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "staging-acceptance: cannot create the results file:", pathErr(err))
		return 2
	}
	defer func() { _ = out.Close() }()
	rep := newReporter(scrub, out, stdout)
	var dbg io.Writer
	if cfg.Debug {
		dbg = stderr
	}
	c, err := newClient(cfg.BaseURL, rep, cfg.RunID, cfg.Timeout, cfg.Debug, dbg)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "staging-acceptance:", err)
		return 2
	}
	w := newWorld(cfg, rep, scrub, c)
	w.warnTo = stderr
	for _, a := range w.actors {
		if err := a.load(); err != nil {
			_, _ = fmt.Fprintln(stderr, "staging-acceptance:", scrub.clean(err.Error()))
			return 2
		}
	}

	started := time.Now()
	rep.line("# staging acceptance run %s started %s base=%s tenant=%s brand=%s", cfg.RunID, started.UTC().Format(time.RFC3339), cfg.BaseURL, cfg.Tenant, cfg.Brand)
	w.runAll()
	meta := map[string]string{
		"run_id":        cfg.RunID,
		"base_url":      cfg.BaseURL,
		"tenant":        cfg.Tenant,
		"brand":         cfg.Brand,
		"market_expect": cfg.MarketExpect,
		"expect_env":    cfg.ExpectEnv,
		"started_utc":   started.UTC().Format(time.RFC3339),
		"finished_utc":  time.Now().UTC().Format(time.RFC3339),
		"actors":        w.actorSummary(),
	}
	for _, k := range sortedKeys(w.facts) {
		meta["fact_"+k] = w.facts[k]
	}
	cnt := rep.summary(meta)
	switch {
	case cnt.Fail > 0:
		return 1
	case cfg.Strict && cnt.SkipPrereq > 0:
		return 3
	}
	return 0
}

func (w *world) actorSummary() string {
	s := ""
	for _, r := range actorRoles {
		a := w.actors[r]
		switch {
		case a == nil:
			s += r + "=not-configured "
		case a.enrolled:
			s += r + "=enrolled-this-run "
		case a.access != "":
			s += r + "=signed-in "
		default:
			s += r + "=configured "
		}
	}
	return s
}

// runAll is the check order. Later groups rely on state established by earlier ones; a failed prerequisite makes its
// dependents report SKIP [blocked-by-fail], never PASS.
func (w *world) runAll() {
	w.healthChecks()
	w.captureStaleProbe()
	w.staffAuthChecks()
	w.setupChecks()
	w.platformChecks()
	w.playerChecks()
	w.ledgerChecks()
	w.casinoChecks()
	w.sportsbookChecks()
	w.withdrawalChecks()
	w.rgChecks()
	w.isolationChecks()
	w.marketChecks()
	w.reconciliationChecks()
	w.alertingChecks()
	w.killSwitchChecks()
	w.observabilityChecks()
	w.staleStepUpChecks()
}

//go:build loadtest

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

// Usage: screeningbench <mode> -scenario FILE -db capa_xxx [-out FILE] [-run N]
//
//	seed      load the scenario into the (migrated) private database
//	fn        per-function latency vs persons per tenant (measurement a)
//	growth    policy-version growth (measurement b)
//	runpass   full RunPass with the MOCK provider (c)
//	stale     mass-stale event and drain (d)
//	retry     retry storm / starvation (e)
//	crash     worker kill / lease expiry / reclaim (f); also `worker` (the child process)
//	race      concurrent policy change during claim / finish (g)
func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: screeningbench <seed|fn|growth|runpass|stale|retry|crash|worker|race> -scenario FILE -db capa_x [-out FILE] [-run N]")
		os.Exit(2)
	}
	mode := os.Args[1]
	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	scPath := fs.String("scenario", "", "scenario file")
	db := fs.String("db", "", "private database (capa_*)")
	out := fs.String("out", "", "result JSON file (default stdout)")
	run := fs.Int("run", 1, "repetition number")
	versions := fs.Int("versions", 0, "override policy_versions (growth)")
	fs.Parse(os.Args[2:])
	scenarioPath = *scPath
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sc, err := loadScenario(*scPath)
	if err != nil {
		fatal(err)
	}
	if *versions > 0 {
		sc.PolicyVersions = *versions
	}
	if sc.Seed == 0 {
		sc.Seed = 1
	}
	if v := intExtra(sc, "gate_pace_ms", 0); v > 0 {
		bgGatePace = time.Duration(v) * time.Millisecond
	}
	if v := intExtra(sc, "pass_interval_secs", 0); v > 0 {
		passInterval = time.Duration(v) * time.Second // EXPERIMENT only: production is 10 s (cmd/platform-api/main.go)
	}
	var res any
	switch mode {
	case "seed":
		res, err = seedDB(ctx, *db, sc)
	case "fn":
		res, err = benchFn(ctx, *db, sc, *run)
	case "growth":
		res, err = benchGrowth(ctx, *db, sc, *run)
	case "runpass":
		res, err = benchRunPass(ctx, *db, sc, *run)
	case "stale":
		res, err = benchStale(ctx, *db, sc, *run)
	case "retry":
		res, err = benchRetry(ctx, *db, sc, *run)
	case "crash":
		res, err = benchCrash(ctx, *db, sc, *run)
	case "worker":
		err = workerMain(ctx, *db, sc)
	case "sweep":
		res, err = benchSweep(ctx, *db, sc, *run)
	case "tenantcap":
		res, err = benchTenantCap(ctx, *db, sc, *run)
	case "replay":
		res, err = benchReplay(ctx, *db, sc, *run)
	case "race":
		res, err = benchRace(ctx, *db, sc, *run)
	default:
		err = fmt.Errorf("unknown mode %q", mode)
	}
	if err != nil {
		fatal(err)
	}
	if res == nil {
		return
	}
	b, _ := json.MarshalIndent(map[string]any{"mode": mode, "scenario": sc.Name, "scenario_version": sc.Version, "seed": sc.Seed, "run": *run,
		"finished_at": nowUTC(), "result": res}, "", " ")
	if *out == "" {
		fmt.Println(string(b))
		return
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "screeningbench:", err)
	os.Exit(1)
}

var _ = time.Second
var scenarioPath string

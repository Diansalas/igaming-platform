// Command smoke is a SMALL local HTTP smoke/load harness (standard library only, no k6/vegeta).
//
// It is NOT a capacity test and produces NO capacity claim. It exists to (a) prove the platform-api
// started in dev mode on a scratch database answers a few read paths under modest concurrency without
// errors, and (b) give the load/soak plan (docs/runbooks/load-and-soak-test-plan.md) a runnable
// starting point. The target is a LOCAL platform-api only: non-loopback hosts are refused.
//
// Scenarios: healthz (no DB), readyz (DB ping), me (register once per worker, then authenticated
// GET /v1/me: JWT verify + a tenant-scoped RLS read).
//
//	go run ./deploy/loadtest/smoke -url http://127.0.0.1:18080 -brand drill-brand-1 -c 8 -d 10s
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"sync"
	"time"
)

type result struct {
	scenario string
	status   int
	dur      time.Duration
	err      bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "smoke:", err)
		os.Exit(1)
	}
}

func run() error {
	base := flag.String("url", "http://127.0.0.1:18080", "base URL of a LOCAL platform-api")
	brand := flag.String("brand", "drill-brand-1", "brand slug of an ACTIVE brand in the scratch database")
	conc := flag.Int("c", 8, "concurrent workers (keep modest: other agents share this machine)")
	dur := flag.Duration("d", 10*time.Second, "test duration")
	maxErrRate := flag.Float64("max-error-rate", 0.0, "exit non-zero if the overall error rate exceeds this fraction")
	flag.Parse()

	u, err := url.Parse(*base)
	if err != nil {
		return err
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("refusing non-loopback target %q: this harness is local-only", host)
	}
	if *conc < 1 || *conc > 64 {
		return fmt.Errorf("-c must be between 1 and 64")
	}

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: *conc * 2}}

	// Setup: one synthetic player per worker (needs AUTH_RATE_LIMIT_PER_MINUTE raised on the server).
	tokens := make([]string, *conc)
	for i := range tokens {
		email := fmt.Sprintf("smoke-%d-%d@example.invalid", time.Now().UnixNano(), i)
		body, _ := json.Marshal(map[string]string{"brand_slug": *brand, "email": email, "password": "smoke-test-password-1"})
		resp, err := client.Post(*base+"/v1/auth/register", "application/json", bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("setup register: %w", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			return fmt.Errorf("setup register: status %d: %s (is the brand active and AUTH_RATE_LIMIT_PER_MINUTE high enough?)", resp.StatusCode, string(raw))
		}
		var tp struct {
			AccessToken string `json:"access_token"`
		}
		if err := json.Unmarshal(raw, &tp); err != nil || tp.AccessToken == "" {
			return fmt.Errorf("setup register: no access token in response")
		}
		tokens[i] = tp.AccessToken
	}

	var mu sync.Mutex
	var results []result
	var wg sync.WaitGroup
	deadline := time.Now().Add(*dur)
	for w := 0; w < *conc; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			var local []result
			for n := 0; time.Now().Before(deadline); n++ {
				switch n % 3 {
				case 0:
					local = append(local, hit(client, "healthz", *base+"/healthz", ""))
				case 1:
					local = append(local, hit(client, "readyz", *base+"/readyz", ""))
				default:
					local = append(local, hit(client, "me", *base+"/v1/me", tokens[w]))
				}
			}
			mu.Lock()
			results = append(results, local...)
			mu.Unlock()
		}(w)
	}
	start := time.Now()
	wg.Wait()
	elapsed := time.Since(start)

	byScenario := map[string][]result{}
	for _, r := range results {
		byScenario[r.scenario] = append(byScenario[r.scenario], r)
	}
	names := make([]string, 0, len(byScenario))
	for k := range byScenario {
		names = append(names, k)
	}
	sort.Strings(names)

	fmt.Printf("smoke: target=%s workers=%d duration=%s total_requests=%d\n", *base, *conc, elapsed.Round(time.Millisecond), len(results))
	fmt.Printf("%-8s %8s %8s %9s %9s %9s %9s\n", "scenario", "requests", "errors", "p50", "p95", "p99", "max")
	totalErr := 0
	for _, name := range names {
		rs := byScenario[name]
		ds := make([]time.Duration, 0, len(rs))
		errs := 0
		for _, r := range rs {
			ds = append(ds, r.dur)
			if r.err || r.status >= 400 {
				errs++
			}
		}
		totalErr += errs
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		fmt.Printf("%-8s %8d %8d %9s %9s %9s %9s\n", name, len(rs), errs,
			pct(ds, 0.50), pct(ds, 0.95), pct(ds, 0.99), ds[len(ds)-1].Round(10*time.Microsecond))
	}
	rate := 0.0
	if len(results) > 0 {
		rate = float64(totalErr) / float64(len(results))
	}
	fmt.Printf("smoke: overall error rate %.4f, throughput %.0f req/s (LOCAL, shared machine: not a capacity figure)\n",
		rate, float64(len(results))/elapsed.Seconds())
	if rate > *maxErrRate {
		return fmt.Errorf("error rate %.4f exceeds -max-error-rate %.4f", rate, *maxErrRate)
	}
	return nil
}

func pct(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	i := int(float64(len(ds)-1) * p)
	return ds[i].Round(10 * time.Microsecond)
}

func hit(c *http.Client, scenario, target, bearer string) result {
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return result{scenario: scenario, err: true}
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	t0 := time.Now()
	resp, err := c.Do(req)
	d := time.Since(t0)
	if err != nil {
		return result{scenario: scenario, dur: d, err: true}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return result{scenario: scenario, status: resp.StatusCode, dur: d}
}

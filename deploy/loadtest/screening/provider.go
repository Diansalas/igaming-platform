//go:build loadtest

package main

import (
	"context"
	"errors"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Diansalas/igaming-platform/internal/aml"
)

// LatencyProvider is a MOCK aml.ScreeningProvider with injected latency and failures. It never talks to anything. It answers
// `clear` (the capacity question is the queue and the database, not match handling) unless a failure is injected.
type LatencyProvider struct {
	cfg   atomic.Pointer[ProviderCfg]
	mu    sync.Mutex
	rng   *rand.Rand
	calls atomic.Int64
	fails atomic.Int64
	hangs atomic.Int64
	lat   []time.Duration // observed per-call durations (capped)
}

// NewLatencyProvider returns a provider with a seeded RNG (reproducible latency sequence per call order).
func NewLatencyProvider(cfg ProviderCfg, seed int64) *LatencyProvider {
	p := &LatencyProvider{rng: rand.New(rand.NewPCG(uint64(seed), 0x9e3779b97f4a7c15))}
	p.cfg.Store(&cfg)
	return p
}

// SetConfig swaps the injected behaviour while running (retry storm: fail, then recover).
func (p *LatencyProvider) SetConfig(c ProviderCfg) { p.cfg.Store(&c) }

// SyntheticComponent marks the provider synthetic (providerkind.Synthetic), like the in-tree MOCK.
func (*LatencyProvider) SyntheticComponent() {}

// ID reuses the in-tree MOCK's id: the heartbeat the worker writes names this id and the database accepts worker results for it.
func (*LatencyProvider) ID() string { return aml.MockProviderID }

func (*LatencyProvider) Capabilities() aml.ScreeningCapabilities {
	return aml.ScreeningCapabilities{Sanctions: true, PEP: true, SupportsSubjectReference: true}
}

func (p *LatencyProvider) draw() (lat time.Duration, fail, hang bool) {
	c := p.cfg.Load()
	p.mu.Lock()
	defer p.mu.Unlock()
	switch c.Dist {
	case "lognormal":
		lat = time.Duration(c.LatencyMs * math.Exp(c.Sigma*p.rng.NormFloat64()) * float64(time.Millisecond))
	case "bimodal":
		if p.rng.Float64() < c.SlowFrac {
			lat = time.Duration(c.SlowMs * float64(time.Millisecond))
		} else {
			lat = time.Duration(c.LatencyMs * float64(time.Millisecond))
		}
	default:
		lat = time.Duration(c.LatencyMs * float64(time.Millisecond))
	}
	fail = c.FailRate > 0 && p.rng.Float64() < c.FailRate
	hang = c.HangRate > 0 && p.rng.Float64() < c.HangRate
	return
}

func subjectFails(c *ProviderCfg, subj string) bool {
	if c.FailPersist <= 0 {
		return false
	}
	h := fnv.New32a()
	h.Write([]byte(subj))
	return float64(h.Sum32()%10000)/10000 < c.FailPersist
}

// Screen implements aml.ScreeningProvider.
func (p *LatencyProvider) Screen(ctx context.Context, in aml.ScreeningRequest) (aml.ScreeningResult, error) {
	t0 := time.Now()
	p.calls.Add(1)
	lat, fail, hang := p.draw()
	c := p.cfg.Load()
	rec := func() {
		p.mu.Lock()
		if len(p.lat) < 2_000_000 {
			p.lat = append(p.lat, time.Since(t0))
		}
		p.mu.Unlock()
	}
	defer rec()
	if hang {
		p.hangs.Add(1)
		<-ctx.Done() // a vendor that never answers: the worker's CallTimeout decides
		return aml.ScreeningResult{}, ctx.Err()
	}
	if lat > 0 {
		select {
		case <-time.After(lat):
		case <-ctx.Done():
			return aml.ScreeningResult{}, ctx.Err()
		}
	}
	if fail || subjectFails(c, in.SubjectRef) {
		p.fails.Add(1)
		return aml.ScreeningResult{}, aml.ErrProviderUnavailable
	}
	return aml.ScreeningResult{Outcome: aml.OutcomeClear, ProviderReference: "lt-" + in.IdempotencyKey, ScreenedAt: time.Now().UTC()}, nil
}

// Latencies returns a copy of the observed per-call durations.
func (p *LatencyProvider) Latencies() []time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]time.Duration(nil), p.lat...)
}

func (p *LatencyProvider) Counts() (calls, fails, hangs int64) {
	return p.calls.Load(), p.fails.Load(), p.hangs.Load()
}

var _ = errors.New

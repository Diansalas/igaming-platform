//go:build stagingacceptance

package main

import (
	"fmt"
	"time"
)

// staleProbe is a staff access token captured right after sign-in and kept unused until its critical step-up window
// has passed (STAFF_STEP_UP_CRITICAL_MAX_AGE_SECONDS: default and maximum 5 minutes).
type staleProbe struct {
	token string
	at    time.Time
	err   error
}

// captureStaleProbe signs ADMIN2 in (ADMIN when only one admin exists) with a dedicated actor and remembers that first
// token, so later step-ups of the shared actor cannot refresh this token's second-factor proof.
func (w *world) captureStaleProbe() {
	if !w.cfg.ProveStaleStep || w.probe != nil {
		return
	}
	w.probe = &staleProbe{}
	role := w.roleOr("ADMIN2", "ADMIN")
	a := w.actor(role)
	if a == nil {
		w.probe.err = fmt.Errorf("no admin configured")
		return
	}
	if err := a.signIn(); err != nil {
		w.probe.err = err
		return
	}
	// Share the TOTP state (replay window) but keep the first token aside.
	w.probe.token, w.probe.at = a.access, w.now()
	w.scrub.add(a.access)
	// Force the shared actor to obtain its own, later token on next use.
	a.access, a.refresh = "", ""
}

func (w *world) staleStepUpChecks() {
	w.check("auth.stepup_stale_refused", func(t *T) {
		if !w.cfg.ProveStaleStep {
			t.skipPrereq("not requested: pass -prove-stale-stepup (adds about 5.5 minutes) to prove a critical route refuses a second factor older than the critical window")
			return
		}
		if w.probe == nil || w.probe.err != nil || w.probe.token == "" {
			t.fail("could not capture the probe session")
			return
		}
		w.waitUntil(w.probe.at.Add(5*time.Minute + 20*time.Second))
		r := t.call("POST", "/v1/admin/tenants", w.probe.token, map[string]any{})
		if r == nil {
			return
		}
		if !t.expect(r, 403, "step_up_required", "critical route with a second factor older than the critical window") {
			return
		}
		if r.Header.Get("WWW-Authenticate") == "" {
			t.fail("the step-up refusal carries no WWW-Authenticate challenge (RFC 9470)")
			return
		}
		t.pass("a session whose second factor is older than the critical window is refused 403 step_up_required (max_age=%v) with an RFC 9470 challenge", pick(r.obj(), "max_age"))
	})
}

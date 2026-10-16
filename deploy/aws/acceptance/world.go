//go:build stagingacceptance

package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable")
	}
	return hex.EncodeToString(b)
}

// world is the shared state of one run: the config, the HTTP client, the staff actors and the ids learned so far.
type world struct {
	cfg    *config
	c      *client
	rep    *reporter
	scrub  *scrubber
	now    func() time.Time
	sleep  func(time.Duration)
	warnTo io.Writer

	actors map[string]*actor
	status map[string]status // outcome of every check so far, by name
	kinds  map[string]skipKind

	// learned during the run
	tenantID string
	brandID  string
	players  map[string]*player

	// facts recorded by earlier checks for later ones
	facts map[string]string
	bal   map[string]int64 // balances verified so far, "<player tag>/<asset>"
	probe *staleProbe
}

func newWorld(cfg *config, rep *reporter, scrub *scrubber, c *client) *world {
	w := &world{cfg: cfg, c: c, rep: rep, scrub: scrub, now: time.Now, sleep: time.Sleep, warnTo: os.Stderr,
		actors: map[string]*actor{}, status: map[string]status{}, kinds: map[string]skipKind{}, players: map[string]*player{}, facts: map[string]string{}, bal: map[string]int64{}}
	for _, r := range actorRoles {
		spec := cfg.Actors[r]
		if spec.configured() {
			w.actors[r] = &actor{spec: spec, w: w}
		}
	}
	return w
}

func (w *world) warnf(format string, a ...any) {
	if w.warnTo != nil {
		fmt.Fprintln(w.warnTo, "WARN: "+w.scrub.clean(fmt.Sprintf(format, a...)))
	}
}

// actor returns the configured actor for a role or nil.
func (w *world) actor(role string) *actor { return w.actors[role] }

// T is the handle a check uses to conclude. Exactly one of pass/fail/skip* takes effect (the first).
type T struct {
	w      *world
	name   string
	done   bool
	status status
	detail string
	skip   skipKind
}

func (t *T) set(s status, k skipKind, format string, a ...any) {
	if t.done {
		return
	}
	t.done, t.status, t.skip = true, s, k
	t.detail = fmt.Sprintf(format, a...)
}

func (t *T) pass(format string, a ...any) { t.set(statusPass, "", format, a...) }
func (t *T) fail(format string, a ...any) { t.set(statusFail, "", format, a...) }

// skipPrereq: a documented prerequisite (an input the operator can supply, or an owner action) is missing.
func (t *T) skipPrereq(format string, a ...any) { t.set(statusSkip, skipPrereq, format, a...) }

// skipManual: the property cannot be observed through this API at all; the README names the operator's evidence step.
func (t *T) skipManual(format string, a ...any) { t.set(statusSkip, skipManual, format, a...) }

// skipGap: the property cannot be exercised black-box because the platform offers no test-support path for it (a
// reported platform gap, not something the operator can fix by supplying an input).
func (t *T) skipGap(format string, a ...any) { t.set(statusSkip, skipGap, format, a...) }

// need reports whether every named earlier check PASSED; otherwise it concludes this check as blocked and returns false.
func (t *T) need(names ...string) bool {
	for _, n := range names {
		s, ok := t.w.status[n]
		if !ok {
			t.set(statusSkip, skipBlocked, "BLOCKED: required check %s did not run", n)
			return false
		}
		if s == statusFail {
			t.set(statusSkip, skipBlocked, "BLOCKED by FAIL of %s", n)
			return false
		}
		if s == statusSkip {
			// A dependency that was skipped for a documented reason makes this check skipped for the same reason class.
			k := t.w.kinds[n]
			if k == skipBlocked || k == "" {
				k = skipBlocked
			}
			t.set(statusSkip, k, "depends on %s, which was skipped (%s)", n, k)
			return false
		}
	}
	return true
}

// call is client.call that concludes the check as FAIL on a transport error and returns nil.
func (t *T) call(method, path, token string, body any, opts ...reqOpt) *response {
	r, err := t.w.c.call(method, path, token, body, opts...)
	if err != nil {
		t.fail("transport error: %v", err)
		return nil
	}
	return r
}

// acall is call as a staff actor (signs in on first use). A sign-in failure concludes the check as FAIL.
func (t *T) acall(role, method, path string, body any) *response {
	a := t.w.actor(role)
	if a == nil {
		t.skipPrereq("actor %s is not configured (%s_EMAIL / %s_PASSWORD_FILE)", role, role, role)
		return nil
	}
	r, err := a.call(method, path, body)
	if err != nil {
		t.fail("%v", err)
		return nil
	}
	return r
}

// expect concludes FAIL unless r has the wanted status (and code, when non-empty). It returns whether it matched.
func (t *T) expect(r *response, status int, code string, what string) bool {
	if r == nil {
		return false
	}
	if r.Status != status || (code != "" && r.code() != code) {
		t.fail("%s: want HTTP %d%s, got %s", what, status, ifs(code != "", " code="+code, ""), r.brief())
		return false
	}
	return true
}

func ifs(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

// check runs fn as the named check and records the result. Panics become FAIL.
func (w *world) check(name string, fn func(t *T)) {
	if w.cfg.Only != nil && !w.cfg.Only.MatchString(name) {
		return
	}
	t := &T{w: w, name: name}
	w.c.cur.Store(name)
	start := w.now()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.set(statusFail, "", "check panicked: %v", r)
			}
		}()
		fn(t)
	}()
	if !t.done {
		t.set(statusFail, "", "check did not reach a verdict (internal error)")
	}
	w.status[name] = t.status
	w.kinds[name] = t.skip
	w.rep.add(result{Name: name, Status: t.status, Detail: t.detail, Skip: t.skip, Duration: w.now().Sub(start)})
}

func (w *world) email(tag string) string {
	return strings.ToLower(fmt.Sprintf("acc-%s-%s@acceptance.invalid", w.cfg.RunID, tag))
}

// assets is the list of fiat assets the synthetic tenant is configured for.
func (w *world) assets() []string {
	if w.cfg.SecondAsset != "" {
		return []string{w.cfg.Asset, w.cfg.SecondAsset}
	}
	return []string{w.cfg.Asset}
}

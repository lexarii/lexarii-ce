// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"strings"
	"sync"
	"time"
)

// Throttle limits repeated failed sign-in attempts.
//
// Deliberately in memory rather than in Postgres. Lexarii is a single process
// serving one household, and a database round trip on every login attempt is
// cost paid on the successful path to slow down the failing one. The trade-off
// is honest and stated: a restart clears the counters, so an attacker who can
// also restart the server can bypass this - but an attacker who can restart the
// server has already won.
//
// A deployment that ever runs more than one instance needs this moved to shared
// state, which is why the limiter is behind an interface at the call site.
type Throttle struct {
	mu       sync.Mutex
	attempts map[string]*bucket

	// MaxAttempts before a key is locked out. Zero means DefaultMaxAttempts.
	MaxAttempts int
	// Window is how long failures are remembered. Zero means DefaultWindow.
	Window time.Duration
	// Lockout is how long a key stays blocked once it trips. Zero means
	// DefaultLockout.
	Lockout time.Duration

	now func() time.Time
}

// Defaults chosen to stop a brute-force attack without locking out a person who
// genuinely cannot remember which password they used.
const (
	DefaultMaxAttempts = 8
	DefaultWindow      = 15 * time.Minute
	DefaultLockout     = 15 * time.Minute
)

type bucket struct {
	failures  int
	firstSeen time.Time
	lockedTil time.Time
}

// NewThrottle builds a limiter with the default settings.
func NewThrottle() *Throttle {
	return &Throttle{attempts: make(map[string]*bucket), now: time.Now}
}

// Allow reports whether an attempt may proceed, and how long to wait if not.
func (t *Throttle) Allow(key string) (bool, time.Duration) {
	if t == nil {
		return true, 0
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked()

	b, ok := t.attempts[key]
	if !ok {
		return true, 0
	}

	now := t.clock()
	if now.Before(b.lockedTil) {
		return false, b.lockedTil.Sub(now)
	}
	return true, 0
}

// Fail records a failed attempt, locking the key out once it trips the limit.
func (t *Throttle) Fail(key string) {
	if t == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.clock()
	b, ok := t.attempts[key]
	if !ok || now.Sub(b.firstSeen) > t.window() {
		// A fresh window. Someone who mistyped twice last month is not suspect.
		b = &bucket{firstSeen: now}
		t.attempts[key] = b
	}

	b.failures++

	// The limit check runs on EVERY failure, including the first of a window.
	// An earlier version returned early when starting a fresh bucket, which
	// skipped the check entirely and meant a limit of one never locked anything
	// out at all.
	if b.failures >= t.maxAttempts() {
		b.lockedTil = now.Add(t.lockout())
	}
}

// Reserve counts an attempt against a key before it is made, and refuses it
// once the key is locked out: the check and the count in one step, under the
// lock. Allow followed by Fail leaves a gap between them in which any number
// of concurrent attempts all pass; Reserve does not, so at most MaxAttempts
// attempts in a window get through however many arrive at once. An attempt
// that turns out not to count is handed back with Refund.
func (t *Throttle) Reserve(key string) (bool, time.Duration) {
	if t == nil {
		return true, 0
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked()

	now := t.clock()
	b, ok := t.attempts[key]
	if ok && now.Before(b.lockedTil) {
		return false, b.lockedTil.Sub(now)
	}
	if !ok || now.Sub(b.firstSeen) > t.window() {
		b = &bucket{firstSeen: now}
		t.attempts[key] = b
	}
	b.failures++
	if b.failures >= t.maxAttempts() {
		b.lockedTil = now.Add(t.lockout())
	}
	return true, 0
}

// Refund hands back an attempt Reserve counted that turned out not to be a
// failure, lifting a lockout that attempt itself tripped. Only call it for a
// reservation that was granted.
func (t *Throttle) Refund(key string) {
	if t == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	b, ok := t.attempts[key]
	if !ok || b.failures == 0 {
		return
	}
	b.failures--
	// Locked by reaching the limit, and now below it: the attempt that
	// reached it was this one, since none is granted once the key is locked.
	if b.failures < t.maxAttempts() {
		b.lockedTil = time.Time{}
	}
}

// Succeed clears a key after a successful sign-in.
func (t *Throttle) Succeed(key string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.attempts, key)
}

// sweepLocked discards entries that have aged out, so the map cannot grow
// without bound from an attacker cycling usernames.
func (t *Throttle) sweepLocked() {
	now := t.clock()
	for key, b := range t.attempts {
		expired := now.Sub(b.firstSeen) > t.window() && now.After(b.lockedTil)
		if expired {
			delete(t.attempts, key)
		}
	}
}

func (t *Throttle) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func (t *Throttle) maxAttempts() int {
	if t.MaxAttempts > 0 {
		return t.MaxAttempts
	}
	return DefaultMaxAttempts
}

func (t *Throttle) window() time.Duration {
	if t.Window > 0 {
		return t.Window
	}
	return DefaultWindow
}

func (t *Throttle) lockout() time.Duration {
	if t.Lockout > 0 {
		return t.Lockout
	}
	return DefaultLockout
}

// ---------------------------------------------------------------------------
// Sign-in attempts
// ---------------------------------------------------------------------------

// SignInAttempt is the throttle applied to one sign-in, across every key it
// should be counted against. Both doors - the browser login and the Basic
// credentials a reader app sends - go through this, so there is one policy
// rather than two that drift.
//
// Three keys, and only two of them deny:
//
//   - The ADDRESS. One host may make a limited number of wrong guesses at
//     anything before it is shut out. This is what bounds the cost of an
//     attack, including the argon2 work each guess forces.
//   - The ADDRESS AND USERNAME TOGETHER. One host may make a limited number of
//     wrong guesses at ONE account. Without it, a host is free to spend its
//     whole allowance on the administrator.
//   - The USERNAME alone, which records and reports but NEVER denies.
//
// That last one used to deny, and it was a lever anybody could pull: eight
// wrong passwords for a name you know locked that person out of their own
// library for fifteen minutes, from anywhere, as often as you liked. The
// protection it was there for - a botnet grinding one account from many hosts
// - is still paid for by the two address-scoped keys, which cap every
// individual host. What is left of the username count is an alarm: it says
// this account is being attacked from more than one place, which is worth
// saying out loud and is not worth locking the owner out over.
type SignInAttempt struct {
	throttle *Throttle
	addrKey  string
	pairKey  string
	userKey  string
}

// SignIn builds the limiter for one attempt at a username from an address.
func (t *Throttle) SignIn(username, addr string) *SignInAttempt {
	name := strings.ToLower(strings.TrimSpace(username))
	return &SignInAttempt{
		throttle: t,
		addrKey:  "addr:" + addr,
		pairKey:  "pair:" + addr + "|" + name,
		userKey:  "user:" + name,
	}
}

// Allow reports whether the attempt may be verified at all, and how long to
// wait if not. Only the address-scoped keys are consulted: a correct password
// must always be able to get its owner in, whatever a stranger has been doing
// with their username.
func (a *SignInAttempt) Allow() (bool, time.Duration) {
	if a == nil || a.throttle == nil {
		return true, 0
	}
	for _, key := range []string{a.addrKey, a.pairKey} {
		if ok, wait := a.throttle.Allow(key); !ok {
			return false, wait
		}
	}
	return true, 0
}

// Contested reports that this username has collected enough failures to trip
// the limit while the address in front of it has not - which is what a
// distributed attack on one account looks like. Nothing is denied on the
// strength of it; it is there to be logged.
func (a *SignInAttempt) Contested() bool {
	if a == nil || a.throttle == nil {
		return false
	}
	ok, _ := a.throttle.Allow(a.userKey)
	return !ok
}

// Fail records a wrong password against every key.
func (a *SignInAttempt) Fail() {
	if a == nil || a.throttle == nil {
		return
	}
	a.throttle.Fail(a.addrKey)
	a.throttle.Fail(a.pairKey)
	a.throttle.Fail(a.userKey)
}

// Succeed clears every key. A right password ends the matter: the person is
// who they said, and the failures before it were them mistyping.
func (a *SignInAttempt) Succeed() {
	if a == nil || a.throttle == nil {
		return
	}
	a.throttle.Succeed(a.addrKey)
	a.throttle.Succeed(a.pairKey)
	a.throttle.Succeed(a.userKey)
}

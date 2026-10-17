// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestThrottleLocksOutAfterRepeatedFailures(t *testing.T) {
	th := NewThrottle()
	th.MaxAttempts = 3
	th.Lockout = time.Minute

	for i := 0; i < 2; i++ {
		if ok, _ := th.Allow("user:adam"); !ok {
			t.Fatalf("locked out after %d failures, want at least 3 allowed", i)
		}
		th.Fail("user:adam")
	}

	// The third failure trips it.
	if ok, _ := th.Allow("user:adam"); !ok {
		t.Fatal("locked out too early")
	}
	th.Fail("user:adam")

	ok, wait := th.Allow("user:adam")
	if ok {
		t.Fatal("not locked out after reaching the limit")
	}
	if wait <= 0 || wait > time.Minute {
		t.Errorf("retry-after = %v, want a positive duration within the lockout", wait)
	}
}

// One person's mistyped password must not lock out everyone else.
func TestThrottleIsPerKey(t *testing.T) {
	th := NewThrottle()
	th.MaxAttempts = 2

	th.Fail("user:adam")
	th.Fail("user:adam")

	if ok, _ := th.Allow("user:adam"); ok {
		t.Error("the failing key was not locked out")
	}
	if ok, _ := th.Allow("user:someone-else"); !ok {
		t.Error("an unrelated key was locked out")
	}
	if ok, _ := th.Allow("addr:192.0.2.1"); !ok {
		t.Error("an unrelated address was locked out")
	}
}

// Signing in successfully clears the count: someone who finally remembers their
// password must not stay locked out.
func TestSuccessClearsTheCount(t *testing.T) {
	th := NewThrottle()
	th.MaxAttempts = 3

	th.Fail("user:adam")
	th.Fail("user:adam")
	th.Succeed("user:adam")

	// Back to a full allowance rather than one attempt from a lockout.
	for i := 0; i < 2; i++ {
		if ok, _ := th.Allow("user:adam"); !ok {
			t.Fatalf("still throttled after a success, at attempt %d", i)
		}
		th.Fail("user:adam")
	}
}

// Someone who mistyped twice last month is not suspect today.
func TestFailuresAgeOut(t *testing.T) {
	now := time.Now()
	th := NewThrottle()
	th.MaxAttempts = 3
	th.Window = time.Minute
	th.now = func() time.Time { return now }

	th.Fail("user:adam")
	th.Fail("user:adam")

	now = now.Add(2 * time.Minute)

	th.Fail("user:adam")
	if ok, _ := th.Allow("user:adam"); !ok {
		t.Fatal("old failures outside the window still counted")
	}
}

func TestLockoutExpires(t *testing.T) {
	now := time.Now()
	th := NewThrottle()
	th.MaxAttempts = 1
	th.Lockout = time.Minute
	th.now = func() time.Time { return now }

	th.Fail("user:adam")
	if ok, _ := th.Allow("user:adam"); ok {
		t.Fatal("not locked out")
	}

	now = now.Add(2 * time.Minute)
	if ok, _ := th.Allow("user:adam"); !ok {
		t.Fatal("still locked out after the lockout expired")
	}
}

// An attacker cycling usernames must not be able to grow the map without bound.
func TestThrottleDoesNotGrowWithoutBound(t *testing.T) {
	now := time.Now()
	th := NewThrottle()
	th.Window = time.Minute
	th.Lockout = time.Minute
	th.now = func() time.Time { return now }

	for i := 0; i < 500; i++ {
		th.Fail("user:" + string(rune('a'+i%26)) + string(rune('a'+i/26)))
	}

	now = now.Add(10 * time.Minute)
	// Any call sweeps aged entries.
	th.Allow("user:trigger")

	th.mu.Lock()
	size := len(th.attempts)
	th.mu.Unlock()

	if size > 1 {
		t.Errorf("%d entries retained after they aged out, want them swept", size)
	}
}

func TestThrottleIsSafeUnderConcurrency(t *testing.T) {
	th := NewThrottle()
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := "user:shared"
			th.Allow(key)
			th.Fail(key)
			if n%10 == 0 {
				th.Succeed(key)
			}
		}(i)
	}
	wg.Wait()
}

// A nil throttle allows everything, so a handler can be constructed without one
// in a test without special-casing every call.
func TestNilThrottleAllows(t *testing.T) {
	var th *Throttle
	if ok, _ := th.Allow("anything"); !ok {
		t.Fatal("a nil throttle blocked a request")
	}
	th.Fail("anything")
	th.Succeed("anything")
}

// The limit must apply on the first failure of a window too. An early return
// when starting a fresh bucket skipped the check, so a limit of one never
// locked anything out.
func TestLimitAppliesOnTheFirstFailure(t *testing.T) {
	th := NewThrottle()
	th.MaxAttempts = 1

	th.Fail("user:adam")
	if ok, _ := th.Allow("user:adam"); ok {
		t.Fatal("a limit of one did not lock out after one failure")
	}
}

// And after a window rolls over, so does the one that trips it.
func TestLimitAppliesAfterAWindowRollsOver(t *testing.T) {
	now := time.Now()
	th := NewThrottle()
	th.MaxAttempts = 2
	th.Window = time.Minute
	th.now = func() time.Time { return now }

	th.Fail("user:adam")
	now = now.Add(2 * time.Minute) // the old failure ages out

	th.Fail("user:adam") // first of the new window
	if ok, _ := th.Allow("user:adam"); !ok {
		t.Fatal("locked out after one failure in a fresh window")
	}
	th.Fail("user:adam") // second trips it
	if ok, _ := th.Allow("user:adam"); ok {
		t.Fatal("did not lock out on reaching the limit in a fresh window")
	}
}

// ---------------------------------------------------------------------------
// Sign-in attempts
// ---------------------------------------------------------------------------

// The lever this was written to remove: a stranger who knows a username could
// lock its owner out of their own library for fifteen minutes, from anywhere,
// as often as they liked. The owner's own password must always get them in.
func TestAStrangerCannotLockSomebodyOutOfTheirOwnLibrary(t *testing.T) {
	throttle := NewThrottle()

	// An attacker grinds bob's account from their own address.
	for i := 0; i < 50; i++ {
		throttle.SignIn("bob", "203.0.113.9").Fail()
	}

	// The attacker is shut out, which is the point.
	if ok, _ := throttle.SignIn("bob", "203.0.113.9").Allow(); ok {
		t.Error("the attacker's address is still allowed to guess")
	}

	// Bob, at home, is not.
	if ok, _ := throttle.SignIn("bob", "198.51.100.4").Allow(); !ok {
		t.Fatal("bob is locked out of his own account by somebody else's failures")
	}
}

// The attack is still visible even though nothing is denied for it. A count
// that only ever warns has to actually warn, or it is decoration.
func TestAContestedAccountIsReportedWithoutBeingLocked(t *testing.T) {
	throttle := NewThrottle()

	// Failures for one name from many addresses: a distributed attack.
	for i := 0; i < DefaultMaxAttempts; i++ {
		throttle.SignIn("bob", fmt.Sprintf("203.0.113.%d", i)).Fail()
	}

	elsewhere := throttle.SignIn("bob", "198.51.100.4")
	if !elsewhere.Contested() {
		t.Error("an account failing from eight different addresses is not reported as contested")
	}
	if ok, _ := elsewhere.Allow(); !ok {
		t.Error("a contested account was locked; the whole point is that it is not")
	}
}

// One host does not get to spend its whole allowance on the administrator and
// then start again on everybody else.
func TestOneHostIsLimitedPerAccountAsWellAsOverall(t *testing.T) {
	throttle := NewThrottle()

	for i := 0; i < DefaultMaxAttempts; i++ {
		throttle.SignIn("alice", "203.0.113.9").Fail()
	}

	if ok, _ := throttle.SignIn("alice", "203.0.113.9").Allow(); ok {
		t.Error("the same host may keep guessing at alice")
	}
	// And the address bucket caught it too, so moving to another name does not
	// buy a fresh allowance.
	if ok, _ := throttle.SignIn("carol", "203.0.113.9").Allow(); ok {
		t.Error("the same host got a fresh allowance by switching username")
	}
}

// A right password ends the matter: the failures before it were somebody
// mistyping, and the next attempt must not inherit them.
func TestSucceedingClearsEveryKey(t *testing.T) {
	throttle := NewThrottle()

	for i := 0; i < DefaultMaxAttempts-1; i++ {
		throttle.SignIn("bob", "198.51.100.4").Fail()
	}
	throttle.SignIn("bob", "198.51.100.4").Succeed()

	for i := 0; i < DefaultMaxAttempts-1; i++ {
		throttle.SignIn("bob", "198.51.100.4").Fail()
	}
	if ok, _ := throttle.SignIn("bob", "198.51.100.4").Allow(); !ok {
		t.Error("failures from before a successful sign-in were still counted")
	}
}

// The username is one bucket however it is typed or spaced, or the limit is
// bypassed by sending "Bob " instead of "bob".
func TestTheUsernameKeyIsNormalised(t *testing.T) {
	throttle := NewThrottle()

	for i := 0; i < DefaultMaxAttempts; i++ {
		throttle.SignIn("  BoB  ", "203.0.113.9").Fail()
	}
	if throttle.SignIn("bob", "198.51.100.4").Contested() != true {
		t.Error("a differently-cased username started a fresh count")
	}
}

// A nil throttle is the "limiting disabled" case and must not panic.
func TestANilThrottleAllowsEverything(t *testing.T) {
	var throttle *Throttle
	attempt := throttle.SignIn("bob", "198.51.100.4")
	if ok, _ := attempt.Allow(); !ok {
		t.Error("a nil throttle refused an attempt")
	}
	attempt.Fail()
	attempt.Succeed()
	if attempt.Contested() {
		t.Error("a nil throttle reported a contested account")
	}
}

// Reserve checks and counts in one step: the attempt that reaches the limit
// is granted, every one after it refused until the lockout ends; a refunded
// attempt is handed back, lifting a lockout only it tripped; a window that
// has passed starts over.
func TestReserveCountsBeforeTheAttempt(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	th := &Throttle{attempts: map[string]*bucket{}, MaxAttempts: 3, now: func() time.Time { return now }}

	for i := range 3 {
		if ok, _ := th.Reserve("k"); !ok {
			t.Fatalf("reservation %d refused, want the first three granted", i+1)
		}
	}
	if ok, wait := th.Reserve("k"); ok || wait <= 0 {
		t.Fatalf("a fourth reservation = %v (wait %v), want refused with a wait", ok, wait)
	}

	// The third turns out not to count: the key is open again, for one.
	th.Refund("k")
	if ok, _ := th.Reserve("k"); !ok {
		t.Fatal("after a refund, the next reservation was refused")
	}
	if ok, _ := th.Reserve("k"); ok {
		t.Fatal("a refund handed back more than one attempt")
	}

	// Refunding what was never reserved changes nothing.
	th.Refund("other")
	if ok, _ := th.Reserve("other"); !ok {
		t.Fatal("an unrelated key was refused")
	}

	// The lockout ends, and a fresh window starts from nothing.
	now = now.Add(DefaultLockout + DefaultWindow + time.Second)
	for i := range 3 {
		if ok, _ := th.Reserve("k"); !ok {
			t.Fatalf("after the lockout, reservation %d refused", i+1)
		}
	}
}

func TestANilThrottleReservesEverything(t *testing.T) {
	var th *Throttle
	if ok, _ := th.Reserve("k"); !ok {
		t.Fatal("a nil throttle refused")
	}
	th.Refund("k")
}

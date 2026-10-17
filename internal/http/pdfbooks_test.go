// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// file writes a file of a given size and returns its path. Only the size
// matters: the cache estimates from a stat and never reads the bytes.
func file(t *testing.T, megabytes int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "book.pdf")
	if err := os.WriteFile(path, make([]byte, megabytes<<20), 0o600); err != nil {
		t.Fatalf("writing a fake book: %v", err)
	}
	return path
}

// The budget is in bytes because books are not comparable. Four small books
// fit where two large ones do not, which a count cannot express.
func TestTheCacheBudgetsMemoryNotBooks(t *testing.T) {
	c := newBookCache(300 << 20)

	small := make([]uuid.UUID, 4)
	for i := range small {
		small[i] = uuid.New()
		// 40MB estimates to 68MB; four of them is 272MB, inside the budget.
		c.admit(t, small[i], file(t, 40))
	}
	if got := c.order.Len(); got != 4 {
		t.Errorf("held %d small books, want all 4 inside a 300MB budget", got)
	}

	// One 150MB book estimates to 255MB and pushes almost everything out.
	big := uuid.New()
	c.admit(t, big, file(t, 150))
	if got := c.order.Len(); got > 2 {
		t.Errorf("held %d books after a large one arrived, want the budget enforced", got)
	}
	if _, ok := c.entries[big]; !ok {
		t.Error("the book just asked for was evicted")
	}
	if c.held > c.budget {
		t.Errorf("held %d bytes against a budget of %d", c.held, c.budget)
	}
}

// A book larger than the whole budget still has to be readable. Refusing to
// open somebody's book because it is big is a worse answer than using the
// memory for as long as they are reading it.
func TestABookBiggerThanTheBudgetIsStillServed(t *testing.T) {
	c := newBookCache(64 << 20)

	id := uuid.New()
	c.admit(t, id, file(t, 200))

	if _, ok := c.entries[id]; !ok {
		t.Fatal("a book larger than the budget was refused")
	}
	if got := c.order.Len(); got != 1 {
		t.Errorf("held %d books, want exactly the one being read", got)
	}

	// And it goes as soon as anything else is opened, rather than sitting
	// there because it never fits.
	next := uuid.New()
	c.admit(t, next, file(t, 10))
	if _, ok := c.entries[id]; ok {
		t.Error("the oversized book survived the next open")
	}
}

// Eviction has to give the memory back, or the cache fills with accounting
// for books it no longer holds and stops admitting anything.
func TestEvictionReleasesWhatItAccountedFor(t *testing.T) {
	c := newBookCache(100 << 20)
	for range 10 {
		c.admit(t, uuid.New(), file(t, 30))
	}
	if c.held > c.budget {
		t.Errorf("held %d bytes against a budget of %d after ten opens", c.held, c.budget)
	}

	var sum int64
	for _, el := range c.entries {
		sum += el.Value.(*bookEntry).cost
	}
	if sum != c.held {
		t.Errorf("accounting says %d bytes, the entries add up to %d", c.held, sum)
	}
}

// admit puts an entry in the cache the way Book does, without parsing: these
// are not real PDFs and the cache's bookkeeping is what is under test.
func (c *bookCache) admit(t *testing.T, id uuid.UUID, path string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := &bookEntry{id: id, path: path, cost: estimate(path), ready: make(chan struct{})}
	close(entry.ready)
	c.entries[id] = c.order.PushFront(entry)
	c.held += entry.cost
	c.evict()
}

// A parse still running must not be evicted.
//
// It used to be, and it was the worst of both: the cost came off the total
// while the memory was still being allocated, so the budget under-reported at
// exactly the moment it was supposed to bite - and the book, now absent from
// the map, was re-parsed on the reader's very next page turn. Four people
// opening four large books evicted each other in a ring and every one of them
// re-parsed, permanently.
func TestAParseInFlightIsNotEvicted(t *testing.T) {
	c := newBookCache(64 << 20)

	// One finished book, and one still being parsed, both large enough that
	// the budget is already exceeded.
	finished, parsing := uuid.New(), uuid.New()
	c.admit(t, finished, file(t, 100))

	inflight := &bookEntry{
		id: parsing, path: file(t, 100), cost: estimate(file(t, 100)),
		ready: make(chan struct{}), // deliberately never closed
	}
	c.mu.Lock()
	c.entries[parsing] = c.order.PushFront(inflight)
	c.held += inflight.cost
	c.evict()
	_, stillThere := c.entries[parsing]
	_, finishedKept := c.entries[finished]
	c.mu.Unlock()

	if !stillThere {
		t.Error("a book that was still being parsed was evicted; it would be parsed again immediately")
	}
	if finishedKept {
		t.Error("the finished book should have been evicted first: it is the one that can be re-read cheaply")
	}
}

// And eviction must still terminate when nothing may be evicted.
func TestEvictionStopsWhenEveryBookIsStillParsing(t *testing.T) {
	c := newBookCache(1 << 20)

	for i := 0; i < 3; i++ {
		entry := &bookEntry{
			id: uuid.New(), path: file(t, 50), cost: 50 << 20,
			ready: make(chan struct{}),
		}
		c.mu.Lock()
		c.entries[entry.id] = c.order.PushFront(entry)
		c.held += entry.cost
		c.evict() // must return rather than spin
		c.mu.Unlock()
	}

	if c.order.Len() != 3 {
		t.Errorf("held %d books, want all 3: none of them had finished parsing", c.order.Len())
	}
}

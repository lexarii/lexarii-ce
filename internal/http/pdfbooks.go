// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"container/list"
	"os"
	"sync"

	"github.com/google/uuid"

	pdfingest "scm.poniatowski.dev/adam/lexarii/internal/ingest/pdf"
)

// Serving a PDF page by page only works because the book is parsed once.
// Measured on a 54 MB reference work: reading, validating and optimising it
// takes 3.6 seconds, and writing one page out of the parsed result takes 2
// milliseconds. Parsing per page turn would make a shared PDF unreadable; not
// caching at all would mean doing exactly that.
//
// So a small number of books are held open. Small because a parsed context is
// the book's structure in memory and a reference PDF is not a small object -
// this is a household server, not a CDN, and four people reading four different
// books at once is already an unusual day.

// openBooks is how many parsed PDFs to keep by default.
//
// Four, because that is more than the number of books one household reads
// simultaneously. The eviction is least-recently-used, so the book someone is
// actually turning pages in is the last to go.
//
// The cache is bounded by MEMORY, not by a number of books, because books are
// not comparable: this library holds one that parses to 76MB and another that
// parses to 176MB, and "four books" means either 300MB or 700MB depending on
// which four somebody opens. A limit that cannot say how much memory it allows
// is not a limit anybody can set.
//
// What it costs was measured over the fourteen largest PDFs here, after
// pdf.Open stopped running pdfcpu's validation pass:
//
//	file     56MB  61MB  63MB  79MB  98MB 112MB 124MB 158MB
//	parsed   76MB 101MB  87MB 102MB 119MB 176MB 156MB 167MB
//
// Between 1.06 and 1.66 times the file, and the file size is known from a
// stat before anything is read. So the cost is estimated rather than weighed:
// no GC sampling, no measuring a thing after it is too late to refuse it.
//
// The budget is what the cache HOLDS, and resident memory runs about 1.45
// times that, because Go's collector keeps headroom and does not hand pages
// straight back. Measured end to end, opening the fourteen largest books in
// this library one after another:
//
//	budget 768MB -> 1,114MB resident, levelling there
//	budget 256MB ->   168-393MB resident across all fourteen
//
// Against 2.7GB before any of this, for four books rather than fourteen.
//
// DefaultBudget is 512MB - around 750MB resident, holding two of the largest
// books or half a dozen ordinary ones. LEXARII_PDF_CACHE_MB is the knob, and
// 128 is a reasonable setting for a Raspberry Pi: it holds one book at a time
// and the eviction costs a re-parse of about a third of a second.
const (
	// DefaultBudget is how much memory parsed PDFs may hold by default.
	DefaultBudget = 512 << 20

	// costRatio turns a file size into an estimate of what parsing it holds.
	//
	// 1.7, the top of the measured range rather than the middle: the budget is
	// there to stop a machine running out of memory, and an estimate that is
	// too low on the one book that matters defeats it. Being wrong in this
	// direction costs a re-parse; being wrong in the other costs the server.
	costRatio = 1.7

	// minimumCost is what an unstattable file is assumed to hold. Generous on
	// purpose: a file the cache cannot size is one it should be wary of.
	minimumCost = 64 << 20
)

// bookCache holds parsed PDFs, keyed by book file.
type bookCache struct {
	mu      sync.Mutex
	order   *list.List // most recent at the front
	entries map[uuid.UUID]*list.Element
	budget  int64
	held    int64
}

type bookEntry struct {
	id   uuid.UUID
	path string
	// cost is what this book is estimated to hold, from its file size.
	cost int64
	// ready is closed once the book is parsed. A second request for the same
	// book waits on it rather than starting a second parse: two people opening
	// the same title at once should cost 3.6 seconds, not 7.2.
	ready chan struct{}
	book  *pdfingest.Book
	err   error
}

func newBookCache(budget int64) *bookCache {
	if budget <= 0 {
		budget = DefaultBudget
	}
	return &bookCache{
		order:   list.New(),
		entries: make(map[uuid.UUID]*list.Element, 4),
		budget:  budget,
	}
}

// estimate is what parsing this file is expected to hold.
func estimate(path string) int64 {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= 0 {
		return minimumCost
	}
	return int64(float64(info.Size()) * costRatio)
}

// evict drops the least recently used books until the budget is met.
//
// Never the newest one, however large it is. A book bigger than the whole
// budget still has to be readable - refusing to open somebody's book because
// it is big would be a worse answer than using the memory - so it is held
// alone and evicted as soon as anything else is opened.
func (c *bookCache) evict() {
	// Walks backwards skipping anything still being parsed, rather than
	// stopping at the first one.
	//
	// Evicting a parse in flight was worse than useless. Its cost came off the
	// total while its memory was very much still being allocated, so the
	// budget under-reported exactly when it mattered: four readers opening
	// four large books each evicted the one before, the counter read as though
	// one book were held, and all four parses ran anyway. Then, because each
	// was gone from the map, every one of those readers re-parsed their book
	// on the next page turn - permanently, since the same race repeats.
	for element := c.order.Back(); element != nil && c.held > c.budget; {
		previous := element.Prev()
		entry := element.Value.(*bookEntry)

		if c.order.Len() <= 1 {
			break
		}
		if !ready(entry) {
			element = previous
			continue
		}

		c.order.Remove(element)
		delete(c.entries, entry.id)
		c.held -= entry.cost
		element = previous
	}
}

// ready reports a parse that has finished, without waiting for one that has
// not.
func ready(entry *bookEntry) bool {
	select {
	case <-entry.ready:
		return true
	default:
		return false
	}
}

// Book returns a parsed PDF, parsing it if this is the first ask.
//
// The path is checked against the cached entry: a file replaced on disk under
// the same id must not be served from a stale parse.
func (c *bookCache) Book(id uuid.UUID, path string) (*pdfingest.Book, error) {
	c.mu.Lock()
	if el, ok := c.entries[id]; ok {
		entry := el.Value.(*bookEntry)
		if entry.path == path {
			c.order.MoveToFront(el)
			c.mu.Unlock()
			<-entry.ready
			return entry.book, entry.err
		}
		// Same book, different file: drop the stale parse.
		c.order.Remove(el)
		delete(c.entries, id)
		c.held -= entry.cost
	}

	entry := &bookEntry{id: id, path: path, cost: estimate(path), ready: make(chan struct{})}
	c.entries[id] = c.order.PushFront(entry)
	c.held += entry.cost
	c.evict()
	c.mu.Unlock()

	// Parsing happens outside the lock: it takes seconds, and holding the cache
	// lock for that would stop every other reader in the house.
	entry.book, entry.err = pdfingest.Open(path)
	close(entry.ready)

	if entry.err != nil {
		// A failed parse is not cached. The next attempt should try again
		// rather than inherit a transient failure for as long as the entry
		// survives eviction.
		c.mu.Lock()
		if el, ok := c.entries[id]; ok && el.Value.(*bookEntry) == entry {
			c.order.Remove(el)
			delete(c.entries, id)
			c.held -= entry.cost
		}
		c.mu.Unlock()
	}
	return entry.book, entry.err
}

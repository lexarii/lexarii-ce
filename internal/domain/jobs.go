// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import "time"

// DefaultJobRetention is how long a finished job is kept before it is
// deleted: thirty days, long enough to look back at what went wrong with an
// import, short enough that the queue is not a second, permanent record of
// what everybody uploaded.
const DefaultJobRetention = 30 * 24 * time.Hour

// MinJobRetention is the shortest retention allowed. Enrichment on ingest
// reads a week of finished enrichment jobs to avoid asking the model about
// the same work again for nothing; deleting them sooner would undo that.
const MinJobRetention = 7 * 24 * time.Hour

// JobType identifies a kind of asynchronous work.
type JobType string

const (
	JobScan    JobType = "scan"
	JobExtract JobType = "extract"
	JobEnrich  JobType = "enrich"
	JobChunk   JobType = "chunk"
	JobEmbed   JobType = "embed"
)

// Priority is where this kind of work stands in the queue: a worker takes the
// highest priority runnable job first, and the oldest of equals. The one
// place the order is decided:
//
//	scan     40  discovery is cheap and unblocks everything else
//	extract  30  a book has to be on the shelf before anything else
//	chunk    20  local CPU, milliseconds, and it makes a book searchable
//	embed    10  seconds a batch; finishes making it searchable
//	enrich  -10  a minute of the model's time per book; it waits for all the rest
//
// Enrichment comes last, and below the zero every job had before the order was
// written down, on purpose: queued alongside a first scan of a large library -
// as enrichment on ingest does - it must not hold every worker at the model
// while extraction crawls behind it.
func (t JobType) Priority() int {
	switch t {
	case JobScan:
		return 40
	case JobExtract:
		return 30
	case JobChunk:
		return 20
	case JobEmbed:
		return 10
	case JobEnrich:
		return -10
	default:
		return 0
	}
}

// JobStatus is where a job is in its lifecycle.
type JobStatus string

const (
	JobPending   JobStatus = "pending"
	JobRunning   JobStatus = "running"
	JobSucceeded JobStatus = "succeeded"
	JobFailed    JobStatus = "failed"
	JobCancelled JobStatus = "cancelled"
)

// Terminal reports whether a job has finished and will not run again.
func (s JobStatus) Terminal() bool {
	return s == JobSucceeded || s == JobFailed || s == JobCancelled
}

// NeedsLLM reports whether this job type requires the model server.
//
// Jobs that need it are skipped rather than failed when it is unreachable: the
// library and reader must keep working with the model switched off, and a queue
// full of failed enrichments is not "keep working".
func (t JobType) NeedsLLM() bool {
	return t == JobEnrich || t == JobEmbed
}

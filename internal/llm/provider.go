// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"scm.poniatowski.dev/adam/lexarii/internal/config"
)

// Hosted OpenAI-compatible providers (OpenRouter is the one this was checked
// against; see docs/ARCHITECTURE.md) behave like llama.cpp on the happy path
// and differ in how they fail. Everything here is about the failing path, plus
// the two operator-supplied extension points: extra body fields and extra
// headers.

// Sentinels for the failures an operator has to act on. They are matched with
// errors.Is against an *HTTPError, so callers do not compare status codes.
var (
	// ErrUnauthorized is a 401: the API key is missing, wrong, or disabled.
	ErrUnauthorized = errors.New("the model server rejected the API key")
	// ErrInsufficientCredits is a 402: the account or key has no credit left.
	// Retrying cannot help until somebody tops it up.
	ErrInsufficientCredits = errors.New("the model account is out of credit")
)

// HTTPError is a non-200 from the model server.
type HTTPError struct {
	Status int
	// Body is the raw response body, as received (bounded).
	Body string
	// Message is the server's own explanation when the body is the common
	// {"error":{"message":...}} shape, and empty otherwise.
	Message string
	// RetryAfter is the server's Retry-After, zero when it sent none.
	RetryAfter time.Duration
	// LimitSource is error.metadata.limit_source where a provider names which
	// limit it hit; it separates a transient in-flight budget from an empty
	// balance, which share a status code.
	LimitSource string
}

func (e *HTTPError) Error() string {
	detail := e.Message
	if detail == "" {
		detail = e.Body
	}
	if detail == "" {
		return fmt.Sprintf("model server returned %d", e.Status)
	}
	return fmt.Sprintf("model server returned %d: %s", e.Status, truncate(detail, 300))
}

// Is lets errors.Is(err, ErrUnauthorized) and ErrInsufficientCredits work.
func (e *HTTPError) Is(target error) bool {
	switch target {
	case ErrUnauthorized:
		return e.Status == http.StatusUnauthorized
	case ErrInsufficientCredits:
		return e.Status == http.StatusPaymentRequired
	}
	return false
}

// Retryable reports whether trying again could plausibly succeed.
//
// A 400 means the request was wrong and will be wrong every time; retrying it
// only wastes the user's GPU. A 429 or a 503 means the server is busy, which is
// exactly what waiting is for. A 402 normally means the balance is empty, which
// waiting does not fix - except when the server attaches a Retry-After, which
// is how OpenRouter marks a transient in-flight spending cap.
func (e *HTTPError) Retryable() bool {
	switch {
	case e.Status == http.StatusTooManyRequests, e.Status == http.StatusRequestTimeout, e.Status >= 500:
		return true
	case e.Status == http.StatusPaymentRequired:
		return e.RetryAfter > 0
	}
	return false
}

// newHTTPError builds the error for a non-200 response whose body is raw.
func newHTTPError(resp *http.Response, raw []byte) *HTTPError {
	e := &HTTPError{
		Status:     resp.StatusCode,
		Body:       strings.TrimSpace(string(raw)),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
	}
	var envelope struct {
		Error *wireError `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Error != nil {
		e.Message = envelope.Error.Message
		e.LimitSource = envelope.Error.limitSource()
	}
	return e
}

// parseRetryAfter reads either form of Retry-After: seconds, or an HTTP date.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	// Clamped BEFORE multiplying: time.Duration is nanoseconds in an int64, so
	// 9223372037 seconds wraps negative and 18446744074 wraps to 290ms - a
	// server asking for a very long wait would be read as asking for none.
	if secs, err := strconv.ParseInt(value, 10, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return min(time.Duration(min(secs, maxRetryAfterSeconds))*time.Second, maxRetryAfter)
	}
	if at, err := http.ParseTime(value); err == nil {
		if d := at.Sub(now); d > 0 {
			return min(d, maxRetryAfter)
		}
	}
	return 0
}

// maxRetryAfterSeconds bounds what a Retry-After is believed to mean: a day.
const (
	maxRetryAfterSeconds = 86400
	maxRetryAfter        = maxRetryAfterSeconds * time.Second
)

// dropExtraHeadersAcrossHosts is the redirect policy: Go's default (ten hops),
// except that the operator's extra headers are removed from a request that has
// left the host they were configured for. Go already withholds Authorization
// across hosts; these headers can be credentials too (a gateway token), and a
// redirect from a provider must not hand them to somebody else.
func dropExtraHeadersAcrossHosts(extra map[string]string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if req.URL.Host != via[0].URL.Host {
			for name := range extra {
				req.Header.Del(name)
			}
		}
		return nil
	}
}

// wireError is the error object providers embed in a response body or a
// stream. OpenRouter documents code as a number in HTTP bodies and shows it as a
// string ("server_error") in a mid-stream event, so it is kept raw.
type wireError struct {
	Code     json.RawMessage `json:"code"`
	Message  string          `json:"message"`
	Type     string          `json:"type"`
	Metadata struct {
		LimitSource string `json:"limit_source"`
	} `json:"metadata"`
}

func (w *wireError) limitSource() string { return w.Metadata.LimitSource }

// codeString renders code whichever way it arrived.
func (w *wireError) codeString() string {
	return strings.Trim(string(w.Code), `" `)
}

func (w *wireError) asError() error {
	return &ProviderError{Code: w.codeString(), Message: w.Message, Type: w.Type}
}

// ProviderError is a failure the provider reported inside a 200 response: in a
// stream after the headers were committed, or in a choice. There is no HTTP
// status to look at, so the provider's own code stands in for it.
type ProviderError struct {
	Code    string
	Message string
	Type    string
}

func (e *ProviderError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "an unspecified error"
	}
	if e.Code != "" {
		return fmt.Sprintf("model reported (%s): %s", e.Code, msg)
	}
	return "model reported: " + msg
}

// Retryable reports whether the same request could succeed later. A code that
// is a client error (400-404) is final; anything else, including codes this
// does not recognise, gets the benefit of the doubt, as errors always did.
func (e *ProviderError) Retryable() bool {
	if n, err := strconv.Atoi(e.Code); err == nil {
		return n == http.StatusRequestTimeout || n == http.StatusTooManyRequests || n >= 500
	}
	return true
}

// Is maps the provider's own codes onto the same sentinels HTTPError uses.
func (e *ProviderError) Is(target error) bool {
	switch target {
	case ErrUnauthorized:
		return e.Code == "401"
	case ErrInsufficientCredits:
		return e.Code == "402"
	}
	return false
}

// maxRetryWait is the longest a worker will sit on a Retry-After.
//
// Longer than this and the better answer is to give the error back, so the job
// is requeued and the worker is free for other work, rather than a goroutine
// asleep for minutes.
const maxRetryWait = 30 * time.Second

// maxInteractiveRetryWait is the same limit for a request a person is waiting on.
const maxInteractiveRetryWait = 2 * time.Second

// backoff is how long to wait before retry number attempt (1 is the first
// retry), or -1 when the server asked for more than is worth waiting for.
//
// Doubling from a quarter second with jitter, so a batch of workers that all
// failed together do not all come back together. A Retry-After is a floor, not
// a suggestion.
func backoff(attempt int, last error, priority Priority) time.Duration {
	base := time.Duration(1<<(attempt-1)) * 250 * time.Millisecond
	// +-25%.
	jittered := base - base/4 + time.Duration(rand.Int64N(int64(base/2)+1))

	var httpErr *HTTPError
	if errors.As(last, &httpErr) && httpErr.RetryAfter > 0 {
		limit := maxRetryWait
		if priority == Interactive {
			// Somebody is watching a cursor. Routing falls back to a plain
			// search when the model fails, so a long wait costs more than the
			// answer it buys.
			limit = maxInteractiveRetryWait
		}
		if httpErr.RetryAfter > limit {
			return -1
		}
		return max(jittered, httpErr.RetryAfter)
	}
	return jittered
}

// effort resolves the reasoning effort to send: the operator's override if there
// is one, otherwise what the caller asked for.
//
// When the extra body carries OpenRouter's own "reasoning" object, no
// reasoning_effort is sent at all: the two overlap, and a request naming both
// is ambiguous at best. The operator who wrote "reasoning" has said what they
// want, so theirs stands. (Config refuses that together with an explicit
// LEXARII_LLM_REASONING_EFFORT.)
func (c *Client) effort(requested string) string {
	for key := range c.extraBody {
		if strings.EqualFold(key, "reasoning") {
			return ""
		}
	}
	switch c.reasoning {
	case "":
		return requested
	case config.ReasoningDefault:
		return ""
	}
	return c.reasoning
}

// encode marshals a request and merges the operator's extra body fields into it.
//
// Fields the client owns always win: a reserved key is skipped even though
// configuration already refused it, and a key the request carries is never
// replaced. The merge is a belt over config's braces, because a request that
// quietly changes model or token budget is a bug nobody would think to look for.
func (c *Client) encode(body chatRequest) ([]byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encoding request: %w", err)
	}
	if len(c.extraBody) == 0 {
		return encoded, nil
	}

	var merged map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &merged); err != nil {
		return nil, fmt.Errorf("encoding request: %w", err)
	}
	for key, value := range c.extraBody {
		if config.ReservedBodyKey(key) {
			continue
		}
		if _, owned := merged[key]; owned {
			continue
		}
		merged[key] = value
	}
	encoded, err = json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("encoding request: %w", err)
	}
	return encoded, nil
}

// authorise sets the operator's extra headers and then the API key. The key is
// set last and extra headers cannot name Authorization, so it has one source.
func (c *Client) authorise(req *http.Request) {
	for name, value := range c.extraHeaders {
		if config.ForbiddenHeader(name) {
			continue
		}
		req.Header.Set(name, value)
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
}

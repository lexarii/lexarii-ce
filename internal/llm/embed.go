// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
)

// embedRequest is the wire format for an embeddings call.
//
// Input is always an array, even for one string: servers differ on whether a
// bare string is accepted, and none of them object to an array of one.
type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Vector is one embedding, normalised to unit length.
type Vector []float32

// ErrDimensionMismatch reports an embedding of an unexpected width.
//
// Worth its own error because the consequence is specific and bad: the vector
// column has a fixed width, so a model that quietly changes dimensions makes
// every new row unstorable and every old row incomparable.
type ErrDimensionMismatch struct {
	Got, Want int
}

func (e *ErrDimensionMismatch) Error() string {
	return fmt.Sprintf("the model returned %d-dimensional vectors, but this library stores %d; "+
		"re-embedding the whole library is the only way to change that", e.Got, e.Want)
}

// maxEmbedBatch bounds one request.
//
// The embedding server here runs with a 2048-token batch, and a request of a
// hundred chunks would simply queue inside it while holding one HTTP request
// open. Smaller batches also mean a failure costs less work.
const maxEmbedBatch = 16

// Embed turns text into vectors, in order.
//
// The returned slice is parallel to the input: out[i] is the embedding of
// texts[i]. Every vector is normalised, so cosine distance and inner product
// agree and the database can use whichever operator its index prefers.
func (c *Client) Embed(ctx context.Context, texts []string, dimensions int) ([]Vector, error) {
	return c.embedAs(ctx, texts, dimensions, Interactive)
}

// EmbedBackground embeds without competing with anyone waiting on an answer.
func (c *Client) EmbedBackground(ctx context.Context, texts []string, dimensions int) ([]Vector, error) {
	return c.embedAs(ctx, texts, dimensions, Background)
}

func (c *Client) embedAs(ctx context.Context, texts []string, dimensions int, priority Priority) ([]Vector, error) {
	if c == nil {
		return nil, ErrNotConfigured
	}
	if len(texts) == 0 {
		return nil, nil
	}

	out := make([]Vector, 0, len(texts))
	for start := 0; start < len(texts); start += maxEmbedBatch {
		end := min(start+maxEmbedBatch, len(texts))

		batch, err := c.embedBatch(ctx, texts[start:end], dimensions, priority)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
	}
	return out, nil
}

// ErrTextTooLong reports one input the model refused as too large.
//
// Separate from a general failure because the caller can do something specific
// about it: the offending text is identified by index, so one unembeddable
// passage can be dropped or shortened instead of blocking the other sixty-five
// thousand.
type ErrTextTooLong struct {
	Index int
	Chars int
	Cause error
}

func (e *ErrTextTooLong) Error() string {
	return fmt.Sprintf("the model refused input %d (%d characters) as too large: %v",
		e.Index, e.Chars, e.Cause)
}

func (e *ErrTextTooLong) Unwrap() error { return e.Cause }

// EmbedTolerant embeds texts, isolating any the model refuses.
//
// Returns a vector per input, with nil where the model would not accept the
// text, and the indexes it refused.
//
// This exists because the model is the only authority on what fits. No
// character or token estimate computed here can be trusted to agree with a
// tokeniser it does not have, and being wrong once should cost one passage
// rather than the whole library: a single oversized chunk failed an entire
// batch, the batch failed the job, and the job retried forever.
func (c *Client) EmbedTolerant(ctx context.Context, texts []string, dimensions int) ([]Vector, []int, error) {
	return c.embedTolerantAs(ctx, texts, dimensions, Background)
}

func (c *Client) embedTolerantAs(ctx context.Context, texts []string, dimensions int, priority Priority) ([]Vector, []int, error) {
	if c == nil {
		return nil, nil, ErrNotConfigured
	}
	if len(texts) == 0 {
		return nil, nil, nil
	}

	out := make([]Vector, len(texts))
	var refused []int

	for start := 0; start < len(texts); start += maxEmbedBatch {
		end := min(start+maxEmbedBatch, len(texts))

		batch, err := c.embedBatch(ctx, texts[start:end], dimensions, priority)
		if err == nil {
			copy(out[start:end], batch)
			continue
		}

		// A whole batch failed. It may be one bad input or a real outage, and
		// those need opposite responses, so they are told apart by trying the
		// inputs one at a time.
		if !worthIsolating(err) {
			return nil, nil, err
		}
		// A 500 is ambiguous - llama.cpp answers one for an input larger
		// than its batch, and so does a server falling over - so a 500 is
		// only taken as "this text" when another input of the same batch
		// embedded, which shows the server is answering now. A batch of one
		// has no other input to show it, and an earlier batch's success
		// says nothing about a server that has since fallen over.
		if end-start == 1 {
			if !refusalOf(err, false) {
				return nil, nil, err
			}
			refused = append(refused, start)
			continue
		}

		var ambiguous []int
		var lastErr error
		answering := false
		for i := start; i < end; i++ {
			single, singleErr := c.embedBatch(ctx, texts[i:i+1], dimensions, priority)
			if singleErr == nil {
				out[i] = single[0]
				answering = true
				continue
			}
			if !worthIsolating(singleErr) {
				return nil, nil, singleErr
			}
			// A 500 is decided after the batch, once it is known whether
			// any of its other inputs embedded.
			if refusalOf(singleErr, false) {
				refused = append(refused, i)
				continue
			}
			ambiguous = append(ambiguous, i)
			lastErr = singleErr
		}
		if len(ambiguous) > 0 {
			// Every input of the batch answered 500 one at a time, and none
			// embedded: that is a server that is not working, not a batch of
			// bad passages, and dropping them would store nothing and call
			// it success.
			if !answering {
				return nil, nil, lastErr
			}
			refused = append(refused, ambiguous...)
		}
	}

	return out, refused, nil
}

// worthIsolating reports whether an error might be caused by one input rather
// than by the server being unavailable.
//
// A 4xx about the request, or a 500, can mean "this particular text is
// unacceptable" - llama.cpp answers 500 for an input larger than its batch -
// while a connection failure cannot. Retrying the inputs individually is what
// tells them apart, and this decides whether that is worth doing.
//
// A server that says it is busy, overloaded or not there yet - 408, 429, 502,
// 503, 504 and every other 5xx but 500 - or that the key or balance is wrong
// (401, 402, 403), is saying nothing about any text. Treating those as bad
// input once made an overloaded embedding server turn an index job into a
// "success" that stored nothing; returned as they are, the worker skips the
// job and tries again later.
func worthIsolating(err error) bool {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	switch s := httpErr.Status; {
	case s == http.StatusInternalServerError:
		return true
	case s >= 500:
		return false
	case s == http.StatusRequestTimeout, s == http.StatusTooManyRequests,
		s == http.StatusUnauthorized, s == http.StatusPaymentRequired,
		s == http.StatusForbidden:
		return false
	case s >= 400:
		return true
	}
	return false
}

// refusalOf reports whether an error worth isolating, from a request for one
// input, says the input itself was refused. A 4xx does. A 500 does only when
// the server is otherwise answering.
func refusalOf(err error, answering bool) bool {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) && httpErr.Status == http.StatusInternalServerError {
		return answering
	}
	return true
}

func (c *Client) embedBatch(ctx context.Context, texts []string, dimensions int, priority Priority) ([]Vector, error) {
	// An empty string embeds to nothing useful and some servers reject it
	// outright, failing the whole batch for one bad chunk.
	cleaned := make([]string, len(texts))
	for i, t := range texts {
		if strings.TrimSpace(t) == "" {
			return nil, fmt.Errorf("cannot embed an empty string at index %d", i)
		}
		cleaned[i] = t
	}

	body, err := json.Marshal(embedRequest{Model: c.model, Input: cleaned})
	if err != nil {
		return nil, fmt.Errorf("encoding embedding request: %w", err)
	}

	raw, err := c.postRaw(ctx, "/embeddings", body, priority)
	if err != nil {
		return nil, err
	}

	var decoded embedResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decoding embedding response: %w", err)
	}
	if decoded.Error != nil {
		return nil, fmt.Errorf("model reported: %s", decoded.Error.Message)
	}
	if len(decoded.Data) != len(texts) {
		// Silently returning fewer vectors than inputs would misalign every
		// chunk with somebody else's embedding.
		return nil, fmt.Errorf("asked for %d embeddings and got %d", len(texts), len(decoded.Data))
	}

	out := make([]Vector, len(texts))
	for _, item := range decoded.Data {
		if item.Index < 0 || item.Index >= len(out) {
			return nil, fmt.Errorf("the model returned an embedding for index %d, outside the batch", item.Index)
		}
		if dimensions > 0 && len(item.Embedding) != dimensions {
			return nil, &ErrDimensionMismatch{Got: len(item.Embedding), Want: dimensions}
		}
		out[item.Index] = normalise(item.Embedding)
	}

	for i, v := range out {
		if v == nil {
			return nil, fmt.Errorf("the model returned no embedding for index %d", i)
		}
	}
	return out, nil
}

// postRaw sends a request and returns the body, with the gate held.
func (c *Client) postRaw(ctx context.Context, path string, body []byte, priority Priority) ([]byte, error) {
	release, err := c.acquireAs(ctx, priority)
	if err != nil {
		return nil, err
	}
	defer release()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorise(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("model unreachable at %s: %w", c.baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, newHTTPError(resp, raw)
	}
	return raw, nil
}

// normalise scales a vector to unit length.
//
// Done here rather than in the database so that cosine distance and inner
// product give the same ordering, and the index can use whichever is cheaper.
// A zero vector is left alone: it cannot be normalised, and pretending
// otherwise would turn a degenerate embedding into a silent NaN.
func normalise(v []float32) Vector {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return Vector(v)
	}

	length := float32(math.Sqrt(sum))
	out := make(Vector, len(v))
	for i, x := range v {
		out[i] = x / length
	}
	return out
}

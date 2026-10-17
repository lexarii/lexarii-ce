// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Streaming exists because an answer takes fourteen to thirty-six seconds on a
// local model, and a person watching a spinner for thirty seconds has no way to
// tell a slow answer from a broken one. The tokens arrive at the same rate
// either way; what changes is that the wait becomes legible.
//
// It is deliberately NOT used for structured output. Routing, classification
// and reranking all parse JSON, and half a JSON object is not a partial result
// but an invalid one - there is nothing useful to show and nothing to gain.

// streamChunk is one server-sent delta.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			// Reasoning is OpenRouter's name for the same thing.
			Reasoning string `json:"reasoning"`
		} `json:"delta"`
		FinishReason string     `json:"finish_reason"`
		Error        *wireError `json:"error"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	// Error is a failure after the stream began. OpenRouter puts it at the
	// top level of an ordinary-looking chunk, alongside a choice whose
	// finish_reason is "error", and it can be the very first event.
	Error *wireError `json:"error"`
}

// maxStreamLine bounds one SSE line.
//
// A delta is a few tokens. A megabyte line is a server malfunctioning or a
// stream that is not what it claims to be, and reading it into memory helps
// nobody.
const maxStreamLine = 1 << 20

// Stream sends one chat completion and calls onDelta with each piece of text as
// it arrives.
//
// The complete Reply is returned as well, assembled from the deltas, so a
// caller that also needs to store the answer does not have to concatenate it
// again. onDelta must not block for long: the model server is waiting on the
// other end of the connection.
//
// Returning an error means the stream failed; any text already delivered
// through onDelta stands, and the caller decides whether a partial answer is
// worth keeping.
func (c *Client) Stream(ctx context.Context, req Request, onDelta func(string)) (Reply, error) {
	if c == nil {
		return Reply{}, ErrNotConfigured
	}

	body := chatRequest{
		Model:           c.model,
		Messages:        req.Messages,
		Temperature:     req.Temperature,
		MaxTokens:       req.MaxTokens,
		Stop:            req.Stop,
		ReasoningEffort: c.effort(req.ReasoningEffort),
		Stream:          true,
		StreamOptions:   &streamOptions{IncludeUsage: true},
	}

	encoded, err := c.encode(body)
	if err != nil {
		return Reply{}, err
	}

	release, err := c.acquireAs(ctx, req.Priority)
	if err != nil {
		return Reply{}, err
	}
	defer release()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return Reply{}, fmt.Errorf("building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	c.authorise(httpReq)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return Reply{}, fmt.Errorf("model unreachable at %s: %w", c.baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Before the provider commits to a stream, a failure is an ordinary JSON
	// error with a real status code. (After it commits, the status is 200 and
	// the failure arrives as an event; readStream handles that.)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return Reply{}, newHTTPError(resp, raw)
	}

	return readStream(ctx, resp.Body, onDelta)
}

// readStream consumes the SSE body and assembles the reply.
func readStream(ctx context.Context, body io.Reader, onDelta func(string)) (Reply, error) {

	var out Reply
	var text strings.Builder

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxStreamLine)

	for scanner.Scan() {
		// A cancelled request should stop reading rather than drain whatever
		// the server has queued.
		if err := ctx.Err(); err != nil {
			out.Content = text.String()
			return out, err
		}

		line := strings.TrimSpace(scanner.Text())
		// Blank lines separate events and comment lines are keep-alives.
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		// The sentinel that ends an OpenAI-compatible stream.
		if data == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// One malformed frame is not worth discarding an answer that is
			// otherwise arriving correctly.
			continue
		}
		if chunk.Error != nil {
			out.Content = text.String()
			return out, chunk.Error.asError()
		}
		if chunk.Usage != nil {
			out.PromptTokens = chunk.Usage.PromptTokens
			out.CompletionTokens = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]
		if choice.Error != nil {
			out.Content = text.String()
			return out, choice.Error.asError()
		}
		if choice.FinishReason == "error" {
			// An error finish with no error object to explain it. Still a
			// failure: the answer stopped for a reason that is not "done".
			out.Content = text.String()
			return out, (&wireError{}).asError()
		}
		if choice.FinishReason != "" {
			out.FinishReason = choice.FinishReason
		}
		if choice.Delta.ReasoningContent != "" || choice.Delta.Reasoning != "" {
			// Recorded but not forwarded: thinking is not the answer, and
			// showing it as though it were is how a model's working gets quoted
			// back as a claim about a book.
			out.Reasoned = true
		}
		if choice.Delta.Content != "" {
			text.WriteString(choice.Delta.Content)
			if onDelta != nil {
				onDelta(choice.Delta.Content)
			}
		}
	}

	out.Content = text.String()

	if err := scanner.Err(); err != nil {
		return out, fmt.Errorf("reading stream: %w", err)
	}
	if out.Content == "" && !out.Reasoned {
		return out, ErrEmptyReply
	}
	// A model that spent the whole budget thinking and never reached an answer
	// is a different failure from one that answered badly, and the caller must
	// not retry it into the same wall.
	if out.Content == "" && out.Reasoned {
		return out, fmt.Errorf("%w: the reply was cut off at the %d-token limit",
			ErrTokenBudget, out.CompletionTokens)
	}
	return out, nil
}

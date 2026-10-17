// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"scm.poniatowski.dev/adam/lexarii/internal/llm"
)

// The chat model is somebody's own server, or a provider with rate limits, and
// it fails in ways that are nobody's bug: it is busy, it is slow, it is off.
// Reported as 500 "unexpected error" those read as Lexarii being broken, and
// tell the person nothing about whether to try again. These are the three
// answers instead. Only asking is affected; the library and the reader never
// call the model.

// Messages for the person asking. The provider's own words never reach them:
// they can name accounts, keys and limits, and they are written for whoever
// runs the server.
const (
	msgModelBusy    = "The assistant is busy right now. Try again in a minute."
	msgModelOff     = "The assistant cannot be reached right now. Your library and reader still work."
	msgModelRefused = "The assistant could not answer: its model server rejected the request or is misconfigured. The server log says why."
	msgModelEmpty   = "The model returned an empty answer."
	msgModelTooLong = "The model ran out of room before finishing; try a narrower question."
)

// defaultRetryAfter is the wait suggested when the model server did not name
// one: the minute the message promises.
const defaultRetryAfter = time.Minute

// describeModelFailure maps a failure of the chat model to what the client is
// told, and logs what the operator needs. False when err is not one.
func describeModelFailure(err error) (failure, bool) {
	switch {
	// The model answered, and the answer was unusable. Asking the same thing
	// again gets the same result, so these are not "busy"; what went wrong is
	// known exactly and can be said without any of the provider's text.
	case errors.Is(err, llm.ErrEmptyReply):
		slog.Error("the chat model returned an empty answer", "error", err)
		return failure{status: http.StatusBadGateway, code: CodeModelError, message: msgModelEmpty}, true
	case errors.Is(err, llm.ErrTokenBudget):
		slog.Error("the chat model ran out of tokens before answering", "error", err)
		return failure{status: http.StatusBadGateway, code: CodeModelError, message: msgModelTooLong}, true

	case errors.Is(err, llm.ErrRejected):
		// The one log line that says why, in the provider's own words: this is
		// a configuration to fix, not a load to wait out.
		slog.Error("the chat model server refused the request",
			"status", providerStatus(err), "providerMessage", providerMessage(err), "error", err)
		return failure{status: http.StatusBadGateway, code: CodeModelError,
			message: msgModelRefused}, true

	case errors.Is(err, llm.ErrBusy):
		wait := llm.RetryAfter(err)
		if wait <= 0 {
			wait = defaultRetryAfter
		}
		slog.Warn("the chat model is busy", "error", err, "retryAfter", wait)
		return failure{status: http.StatusServiceUnavailable, code: CodeLLMOffline,
			message: msgModelBusy, retryAfter: wait}, true

	case errors.Is(err, llm.ErrUnreachable):
		slog.Warn("the chat model cannot be reached", "error", err)
		return failure{status: http.StatusServiceUnavailable, code: CodeLLMOffline,
			message: msgModelOff, retryAfter: defaultRetryAfter}, true
	}
	return failure{}, false
}

// providerMessage is the model server's own explanation, for the log.
func providerMessage(err error) string {
	var httpErr *llm.HTTPError
	if errors.As(err, &httpErr) {
		if httpErr.Message != "" {
			return httpErr.Message
		}
		return httpErr.Body
	}
	var providerErr *llm.ProviderError
	if errors.As(err, &providerErr) {
		return providerErr.Message
	}
	return ""
}

// providerStatus is the status the model server answered with, or its own
// code for a failure reported inside a stream.
func providerStatus(err error) string {
	var httpErr *llm.HTTPError
	if errors.As(err, &httpErr) {
		return strconv.Itoa(httpErr.Status)
	}
	var providerErr *llm.ProviderError
	if errors.As(err, &providerErr) {
		return providerErr.Code
	}
	return ""
}

// retryAfterSeconds renders a wait as Retry-After does: whole seconds, rounded
// up, so a server that asked for 1.5s is not retried after one.
func retryAfterSeconds(d time.Duration) int {
	return int(math.Ceil(d.Seconds()))
}

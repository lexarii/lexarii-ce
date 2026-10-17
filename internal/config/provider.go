// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Provider-neutral extension points for the chat endpoint.
//
// Hosted OpenAI-compatible services (OpenRouter is the one this was written
// against) accept request fields and headers that llama.cpp has never heard of.
// Rather than a code path per provider, the operator supplies them: a JSON
// object merged into the request body, and a JSON object of headers. Both are
// validated once at startup, and both refuse the fields the client owns.

// ReasoningDefault, as LEXARII_LLM_REASONING_EFFORT, means "never send a
// reasoning effort; let the model use its own default".
//
// Some reasoning models cannot be switched off and reject "none"; this is the
// way out for them.
const ReasoningDefault = "default"

// reservedBodyKeys are request fields the client builds itself, with the reason
// each is refused. Letting configuration override them would silently change
// what a prompt means (another model, another token budget) or break the
// response parser (stream, tools).
var reservedBodyKeys = map[string]string{
	"model":                 "the model is LEXARII_LLM_MODEL",
	"models":                "fallback models would make the recorded model id untrue",
	"messages":              "the conversation is built by Lexarii",
	"prompt":                "the conversation is built by Lexarii",
	"stream":                "streaming is chosen per request",
	"stream_options":        "streaming is chosen per request",
	"response_format":       "structured output is chosen per request",
	"max_tokens":            "the token budget is chosen per task",
	"max_completion_tokens": "the token budget is chosen per task",
	"temperature":           "extraction runs at temperature 0",
	"stop":                  "stop sequences are chosen per task",
	"reasoning_effort":      "use LEXARII_LLM_REASONING_EFFORT",
	"tools":                 "Lexarii does not use tool calls",
	"tool_choice":           "Lexarii does not use tool calls",
	"functions":             "Lexarii does not use tool calls",
	"function_call":         "Lexarii does not use tool calls",
}

// hasKeyFold reports whether m has key, ignoring case.
func hasKeyFold(m map[string]json.RawMessage, key string) bool {
	for k := range m {
		if strings.EqualFold(k, key) {
			return true
		}
	}
	return false
}

// ReservedBodyKey reports whether a request field belongs to the client.
func ReservedBodyKey(key string) bool {
	_, ok := reservedBodyKeys[strings.ToLower(key)]
	return ok
}

// ParseExtraBody parses LEXARII_LLM_EXTRA_BODY: a JSON object whose members
// are merged into every chat request. Blank means none.
func ParseExtraBody(raw string) (map[string]json.RawMessage, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return nil, errors.New(`must be a JSON object such as {"provider":{"require_parameters":true}}`)
	}
	for key := range out {
		// encoding/json matches keys case-insensitively AND folds some non-ASCII
		// letters onto ASCII ones ("ſtream" decodes into "stream"), so a
		// reserved-name check on the raw string can be walked around. Provider
		// request fields are ASCII; anything else is refused.
		for _, r := range key {
			if r >= 0x80 {
				return nil, fmt.Errorf("key %q contains non-ASCII characters", key)
			}
		}
		if why, reserved := reservedBodyKeys[strings.ToLower(key)]; reserved {
			return nil, fmt.Errorf("may not set %q: %s", key, why)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// forbiddenHeaders are owned by the client or by net/http. Authorization is the
// one that matters: the key has exactly one place to come from.
var forbiddenHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"content-type":        true,
	"content-length":      true,
	"accept":              true,
	"accept-encoding":     true,
	"host":                true,
	"connection":          true,
	"transfer-encoding":   true,
	"cookie":              true,
}

// ForbiddenHeader reports whether a header may not be set from configuration.
func ForbiddenHeader(name string) bool {
	return forbiddenHeaders[strings.ToLower(name)]
}

// ParseExtraHeaders parses LEXARII_LLM_EXTRA_HEADERS: a JSON object of header
// names to string values. Blank means none. Values are never echoed in errors,
// because a header is as likely as not to be a credential.
func ParseExtraHeaders(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return nil, errors.New(`must be a JSON object of header names to string values, such as {"X-Title":"Lexarii"}`)
	}
	for name, value := range out {
		if !validHeaderName(name) {
			return nil, fmt.Errorf("%q is not a valid header name", name)
		}
		if ForbiddenHeader(name) {
			return nil, fmt.Errorf("may not set %q: it is controlled by Lexarii", name)
		}
		if strings.ContainsFunc(value, func(r rune) bool { return (r < ' ' && r != '\t') || r == 0x7f }) {
			return nil, fmt.Errorf("the value of %q contains a control character", name)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// validHeaderName reports whether name is an RFC 9110 token.
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return false
		}
	}
	return true
}

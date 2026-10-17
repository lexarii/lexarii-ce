// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"strings"
	"testing"
)

func TestParseExtraBody(t *testing.T) {
	ok, err := ParseExtraBody(`{"provider":{"require_parameters":true,"data_collection":"deny"}}`)
	if err != nil || string(ok["provider"]) != `{"require_parameters":true,"data_collection":"deny"}` {
		t.Fatalf("ok = %v, %v", ok, err)
	}
	for _, blank := range []string{"", "  ", "{}"} {
		if got, err := ParseExtraBody(blank); got != nil || err != nil {
			t.Errorf("ParseExtraBody(%q) = %v, %v; want nothing", blank, got, err)
		}
	}

	refused := map[string]string{
		`not json`:                  "JSON object",
		`[1,2]`:                     "JSON object",
		`"text"`:                    "JSON object",
		`null`:                      "JSON object",
		`{"model":"x"}`:             `"model"`,
		`{"Messages":[]}`:           `"Messages"`,
		`{"stream":true}`:           `"stream"`,
		`{"response_format":{}}`:    `"response_format"`,
		`{"max_tokens":5}`:          `"max_tokens"`,
		`{"reasoning_effort":"x"}`:  "LEXARII_LLM_REASONING_EFFORT",
		`{"models":["a","b"]}`:      `"models"`,
		`{"provider":{},"tools":1}`: `"tools"`,
	}
	for in, want := range refused {
		_, err := ParseExtraBody(in)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseExtraBody(%s) = %v, want an error mentioning %s", in, err, want)
		}
	}

	// encoding/json folds these onto reserved names when decoding, so a check
	// on the raw key alone would let them through.
	// (The third uses a Cyrillic "o": a look-alike, not a fold.)
	for _, in := range []string{`{"ſtream":false}`, `{"proviḋer":{}}`, `{"mоdel":"x"}`} {
		_, err := ParseExtraBody(in)
		if err == nil || !strings.Contains(err.Error(), "non-ASCII") {
			t.Errorf("ParseExtraBody(%s) = %v, want a non-ASCII refusal", in, err)
		}
	}

	// Fields OpenRouter documents that Lexarii does not own.
	for _, in := range []string{`{"reasoning":{"effort":"low"}}`, `{"plugins":[{"id":"response-healing"}]}`, `{"user":"u"}`} {
		if _, err := ParseExtraBody(in); err != nil {
			t.Errorf("ParseExtraBody(%s) = %v", in, err)
		}
	}
}

func TestParseExtraHeaders(t *testing.T) {
	got, err := ParseExtraHeaders(`{"HTTP-Referer":"https://books.example","X-Title":"Lexarii"}`)
	if err != nil || got["X-Title"] != "Lexarii" || len(got) != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := ParseExtraHeaders(""); got != nil || err != nil {
		t.Fatalf("blank = %v, %v", got, err)
	}

	refused := []string{
		`not json`, `["a"]`, `{"X-Title":3}`,
		`{"Authorization":"Bearer x"}`, `{"authorization":"Bearer x"}`, `{"Proxy-Authorization":"x"}`,
		`{"Content-Type":"text/plain"}`, `{"Host":"evil"}`, `{"Cookie":"a=b"}`,
		`{"Bad Name":"x"}`, `{"":"x"}`, `{"X-Title":"line\nbreak"}`,
	}
	for _, in := range refused {
		if _, err := ParseExtraHeaders(in); err == nil {
			t.Errorf("ParseExtraHeaders(%s) accepted", in)
		}
	}
}

// A header value may be a credential; a rejection must not repeat it.
func TestHeaderErrorsDoNotEchoValues(t *testing.T) {
	_, err := ParseExtraHeaders(`{"X-Token":"super-secret\n"}`)
	if err == nil || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadValidatesProviderSettings(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("LEXARII_LLM_BASE_URL", "https://openrouter.ai/api/v1")
	t.Setenv("LEXARII_LLM_MODEL", "vendor/model")
	t.Setenv("LEXARII_LLM_EXTRA_BODY", `{"provider":{"require_parameters":true}}`)
	t.Setenv("LEXARII_LLM_EXTRA_HEADERS", `{"X-Title":"Lexarii"}`)
	t.Setenv("LEXARII_LLM_REASONING_EFFORT", "Default")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.ExtraBody["provider"] == nil || cfg.LLM.ExtraHeaders["X-Title"] != "Lexarii" || cfg.LLM.ReasoningEffort != ReasoningDefault {
		t.Fatalf("LLM config = %+v", cfg.LLM)
	}
	// Chat only: the embedding model is never switched with the chat provider.
	if cfg.Embed.ExtraBody != nil || cfg.Embed.ExtraHeaders != nil {
		t.Fatal("provider settings leaked into the embedding config")
	}

	// Two ways to say the same thing are refused, not silently ordered.
	t.Setenv("LEXARII_LLM_EXTRA_BODY", `{"reasoning":{"effort":"low"}}`)
	t.Setenv("LEXARII_LLM_EXTRA_HEADERS", "")
	t.Setenv("LEXARII_LLM_REASONING_EFFORT", "high")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "LEXARII_LLM_REASONING_EFFORT") {
		t.Fatalf("Load = %v; want the clash named", err)
	}
	for _, effort := range []string{"", "default"} {
		t.Setenv("LEXARII_LLM_REASONING_EFFORT", effort)
		if _, err := Load(); err != nil {
			t.Fatalf("Load with effort %q: %v", effort, err)
		}
	}

	t.Setenv("LEXARII_LLM_EXTRA_BODY", `{"model":"other"}`)
	t.Setenv("LEXARII_LLM_EXTRA_HEADERS", `{"Authorization":"x"}`)
	_, err = Load()
	if err == nil || !strings.Contains(err.Error(), "LEXARII_LLM_EXTRA_BODY") || !strings.Contains(err.Error(), "LEXARII_LLM_EXTRA_HEADERS") {
		t.Fatalf("Load = %v; want both variables named", err)
	}
}

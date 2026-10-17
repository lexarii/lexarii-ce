// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestSignInLinksAreReadInOrder(t *testing.T) {
	cfg, err := load(t, map[string]string{"LEXARII_SIGNIN_LINKS": `[
		{"label": " Request access ", "href": "https://example.org/request?from=lexarii#form"},
		{"label": "Forgot password?", "href": "/help/password"},
		{"label": "Terms", "href": "/terms"}
	]`})
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	want := []SignInLink{
		{"Request access", "https://example.org/request?from=lexarii#form"},
		{"Forgot password?", "/help/password"},
		{"Terms", "/terms"},
	}
	if fmt.Sprint(cfg.SignInLinks) != fmt.Sprint(want) {
		t.Errorf("links = %v, want %v", cfg.SignInLinks, want)
	}

	for _, blank := range []string{"", "  ", "[]"} {
		cfg, err := load(t, map[string]string{"LEXARII_SIGNIN_LINKS": blank})
		if err != nil || len(cfg.SignInLinks) != 0 {
			t.Errorf("%q: links = %v, err = %v; want none and no error", blank, cfg.SignInLinks, err)
		}
	}
}

// The page is shown to strangers, so every href that is not plainly a path
// here or an https link elsewhere is refused at startup, naming the variable.
func TestSignInLinksRefuseAnythingButPathsAndHTTPS(t *testing.T) {
	link := func(href string) string {
		return fmt.Sprintf(`[{"label": "Terms", "href": %q}]`, href)
	}
	cases := map[string]string{
		"javascript":         link("javascript:alert(1)"),
		"javascript in caps": link("JavaScript:alert(1)"),
		"data":               link("data:text/html,<script>alert(1)</script>"),
		"plain http":         link("http://example.org/terms"),
		"mailto":             link("mailto:admin@example.org"),
		"protocol-relative":  link("//evil.example/terms"),
		"backslash":          link(`/\evil.example`),
		"tab":                link("/\t/evil.example"),
		"newline":            link("/te\nrms"),
		"no host":            link("https:///terms"),
		"opaque https":       link("https:evil.example"),
		"credentials":        link("https://user:pass@example.org/"),
		"relative, no slash": link("terms"),
		"empty href":         link(""),
		"empty label":        `[{"label": " ", "href": "/terms"}]`,
		"control in label":   `[{"label": "Te\u0007rms", "href": "/terms"}]`,
		"long label":         fmt.Sprintf(`[{"label": %q, "href": "/terms"}]`, strings.Repeat("a", MaxSignInLinkLabel+1)),
		"long href":          link("/" + strings.Repeat("a", MaxSignInLinkHref)),
		"too many":           "[" + strings.TrimSuffix(strings.Repeat(`{"label":"a","href":"/a"},`, MaxSignInLinks+1), ",") + "]",
		"unknown field":      `[{"label": "Terms", "href": "/terms", "target": "_blank"}]`,
		"an object":          `{"label": "Terms", "href": "/terms"}`,
		"not json":           `Terms=/terms`,
		"two arrays":         `[] []`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, map[string]string{"LEXARII_SIGNIN_LINKS": raw})
			if err == nil || !strings.Contains(err.Error(), "LEXARII_SIGNIN_LINKS") {
				t.Errorf("%s: err = %v, want a refusal naming LEXARII_SIGNIN_LINKS", raw, err)
			}
		})
	}
}

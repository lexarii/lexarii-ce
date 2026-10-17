// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SignInLink is one administrator-configured link on the sign-in page.
type SignInLink struct {
	Label string `json:"label"`
	Href  string `json:"href"`
}

// Limits on LEXARII_SIGNIN_LINKS. The page is a narrow form on a phone: a
// handful of short links fit under it, and anything longer is a mistake.
const (
	MaxSignInLinks     = 8
	MaxSignInLinkLabel = 60
	MaxSignInLinkHref  = 2048
)

// ParseSignInLinks reads LEXARII_SIGNIN_LINKS, a JSON array of
// {"label": "...", "href": "..."}. Blank means no links.
//
// The page is shown to people who are not signed in, so these are checked as
// strictly as anything a stranger can click: an href is either a path on this
// server, beginning with exactly one "/", or an https:// URL with a host and
// no credentials in it. Nothing else - no javascript:, no data:, no plain
// http: that would send somebody off the encrypted connection they arrived
// on, no "//host" that a browser reads as another site. Labels are plain text,
// required, and short; unknown fields are refused so a typo is not silently
// dropped.
func ParseSignInLinks(raw string) ([]SignInLink, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	var links []SignInLink
	if err := dec.Decode(&links); err != nil {
		return nil, fmt.Errorf(`must be a JSON array of {"label": ..., "href": ...}: %w`, err)
	}
	if dec.More() {
		return nil, errors.New("must be a single JSON array")
	}
	if len(links) > MaxSignInLinks {
		return nil, fmt.Errorf("has %d links; at most %d fit on the sign-in page", len(links), MaxSignInLinks)
	}

	for n := range links {
		link := &links[n]
		link.Label = strings.TrimSpace(link.Label)
		link.Href = strings.TrimSpace(link.Href)

		if link.Label == "" {
			return nil, fmt.Errorf("link %d has no label", n+1)
		}
		if utf8.RuneCountInString(link.Label) > MaxSignInLinkLabel {
			return nil, fmt.Errorf("link %d: the label is longer than %d characters", n+1, MaxSignInLinkLabel)
		}
		if strings.IndexFunc(link.Label, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("link %d: the label contains a control character", n+1)
		}
		if err := checkSignInHref(link.Href); err != nil {
			return nil, fmt.Errorf("link %d (%q): %w", n+1, link.Label, err)
		}
	}
	return links, nil
}

// checkSignInHref accepts a path on this server or an https URL, and nothing
// else.
func checkSignInHref(href string) error {
	if href == "" {
		return errors.New("has no href")
	}
	if len(href) > MaxSignInLinkHref {
		return fmt.Errorf("the href is longer than %d bytes", MaxSignInLinkHref)
	}
	// Browsers strip tabs and newlines from URLs and read a backslash as a
	// slash, which is how "/\evil.example" and "/\t/evil.example" become
	// another site. None of them has a place in a link anybody typed on
	// purpose.
	if strings.IndexFunc(href, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 ||
		strings.Contains(href, `\`) {
		return errors.New("the href contains whitespace, a control character or a backslash")
	}

	u, err := url.Parse(href)
	if err != nil {
		return fmt.Errorf("the href is not a URL: %w", err)
	}

	if strings.HasPrefix(href, "/") {
		if strings.HasPrefix(href, "//") || u.Scheme != "" || u.Host != "" {
			return errors.New(`a path must begin with exactly one "/"; "//" leaves this server`)
		}
		return nil
	}

	if u.Scheme != "https" || u.Host == "" || u.Opaque != "" {
		return errors.New(`must be a path on this server beginning with "/", or an https:// URL`)
	}
	if u.User != nil {
		return errors.New("an https URL must not carry a username or password")
	}
	return nil
}

// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
)

// The sign-in page learns its links from the same open request that tells it
// how to sign in, in the order they were configured - and gets an empty list,
// never null, when there are none, so the client has one shape to handle.
func TestTheProvidersListCarriesTheSignInLinks(t *testing.T) {
	h, _, _ := tokenHandler(t)

	get := func() (dto.AuthOptions, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ProvidersList(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/providers", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
		}
		var out dto.AuthOptions
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		return out, rec.Body.String()
	}

	if _, body := get(); !strings.Contains(body, `"links":[]`) {
		t.Errorf("with no links configured the body is %s, want \"links\":[]", body)
	}

	h.SignInLinks = []dto.SignInLink{
		{Label: "Request access", Href: "https://example.org/access"},
		{Label: "Terms", Href: "/terms"},
	}
	out, _ := get()
	if len(out.Links) != 2 || out.Links[0] != h.SignInLinks[0] || out.Links[1] != h.SignInLinks[1] {
		t.Errorf("links = %+v, want %+v in that order", out.Links, h.SignInLinks)
	}
}

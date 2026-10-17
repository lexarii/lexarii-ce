// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The branding directory is optional, and when it is given it has to be one:
// a mistyped path stops the server at startup rather than leaving a sign-in
// page that quietly looks like the default.
func TestBrandingDir(t *testing.T) {
	t.Run("unset serves nothing", func(t *testing.T) {
		cfg, err := load(t, nil)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.BrandingDir != "" {
			t.Errorf("BrandingDir = %q by default, want empty", cfg.BrandingDir)
		}
	})

	t.Run("a directory is accepted", func(t *testing.T) {
		dir := t.TempDir()
		cfg, err := load(t, map[string]string{"LEXARII_BRANDING_DIR": dir})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.BrandingDir != dir {
			t.Errorf("BrandingDir = %q, want %q", cfg.BrandingDir, dir)
		}
	})

	file := filepath.Join(t.TempDir(), "branding.css")
	if err := os.WriteFile(file, []byte("body{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"a path that does not exist": filepath.Join(t.TempDir(), "missing"),
		"a file, not a directory":    file,
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			_, err := load(t, map[string]string{"LEXARII_BRANDING_DIR": value})
			if err == nil {
				t.Fatalf("accepted LEXARII_BRANDING_DIR=%q", value)
			}
			if !strings.Contains(err.Error(), "LEXARII_BRANDING_DIR") {
				t.Errorf("error does not name the variable: %v", err)
			}
		})
	}
}

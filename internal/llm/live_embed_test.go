// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"scm.poniatowski.dev/adam/lexarii/internal/config"
)

// TestEmbedAgainstARealServer runs against the configured embedding model, and
// skips without one.
//
//	set -a; . ./.env; set +a; go test ./internal/llm -run RealServer -v
//
// A fake proves the client handles the shapes the spec allows. Only a real
// server proves which shape THIS one returns - and the vector width it reports
// is the one the database column is fixed at.
func TestEmbedAgainstARealServer(t *testing.T) {
	base := os.Getenv("LEXARII_EMBED_BASE_URL")
	if base == "" {
		t.Skip("LEXARII_EMBED_BASE_URL not set")
	}

	client := New(config.ModelConfig{
		BaseURL: base,
		Model:   os.Getenv("LEXARII_EMBED_MODEL"),
		APIKey:  os.Getenv("LEXARII_EMBED_API_KEY"),
		// One at a time: this is a check, not a benchmark.
		MaxConcurrency: 1,
		Timeout:        60 * time.Second,
	})

	texts := []string{
		"Linear algebra is the study of vector spaces and linear maps between them.",
		"A vector space is a set closed under addition and scalar multiplication.",
		"Sourdough bread is leavened by a culture of wild yeast and lactobacilli.",
	}

	start := time.Now()
	vectors, err := client.Embed(context.Background(), texts, 768)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	t.Logf("%d vectors of %d dimensions in %v", len(vectors), len(vectors[0]), time.Since(start).Round(time.Millisecond))

	for i, v := range vectors {
		var sum float64
		for _, x := range v {
			sum += float64(x) * float64(x)
		}
		if length := math.Sqrt(sum); math.Abs(length-1) > 1e-4 {
			t.Errorf("vector %d has length %v, want 1", i, length)
		}
	}

	// The property retrieval depends on: related text must sit closer together
	// than unrelated text. If this fails, the model is not doing what the rest
	// of the system assumes.
	related := dot(vectors[0], vectors[1])
	unrelated := dot(vectors[0], vectors[2])
	t.Logf("similarity: related=%.3f unrelated=%.3f", related, unrelated)

	if related <= unrelated {
		t.Errorf("two sentences about linear algebra scored %.3f, and one about bread scored %.3f: "+
			"this model cannot support retrieval", related, unrelated)
	}
}

func dot(a, b Vector) float64 {
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

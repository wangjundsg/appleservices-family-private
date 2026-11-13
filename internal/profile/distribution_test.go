package profile

import (
	"fmt"
	"testing"
)

// TestGeneratedDistributionIsWindowsOnly pins the two properties the pipeline
// depends on: every generated profile is a Windows Chrome, and the version mix
// follows the weighted table (154 dominating, 150 rare).
func TestGeneratedDistributionIsWindowsOnly(t *testing.T) {
	const samples = 2000
	counts := map[string]int{}
	for i := 0; i < samples; i += 1 {
		p, err := Generate(fmt.Sprintf("acct-%04d", i))
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		if p.Platform != "Win32" {
			t.Fatalf("seed %d: platform %q, want Win32", i, p.Platform)
		}
		counts[p.ChromeMajor()] += 1
	}

	// 154 carries 80 of the 100 weight units; allow a wide band so the test is
	// about "dominant", not about the exact RNG.
	if share := float64(counts["154"]) / samples; share < 0.70 || share > 0.90 {
		t.Fatalf("chrome 154 share %.2f, want roughly 0.80 (counts: %v)", share, counts)
	}
	for _, major := range []string{"150", "151", "152", "153"} {
		if counts[major] == 0 {
			t.Fatalf("major %s never appeared in %d samples (counts: %v)", major, samples, counts)
		}
	}
	if counts["150"] > counts["153"] {
		t.Fatalf("150 (%d) should be rarer than 153 (%d)", counts["150"], counts["153"])
	}
}

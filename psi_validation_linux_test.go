//go:build linux && (amd64 || arm64)

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPSIRejectsNonFiniteAndInvalidPercentages(t *testing.T) {
	p := filepath.Join(t.TempDir(), "io.pressure")
	for _, value := range []string{"NaN", "+Inf", "-1", "100.01", "bad"} {
		if err := os.WriteFile(p, []byte("some avg10="+value+" avg60=0.00 total=0\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if psi(p) != nil {
			t.Fatal("invalid PSI became a usable guard", value)
		}
	}
	if err := os.WriteFile(p, []byte("some avg10=0.00 avg60=0.00 total=0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if value := psi(p); value == nil || *value != 0 {
		t.Fatal("known zero confused with unavailable")
	}
}

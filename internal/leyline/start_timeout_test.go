package leyline

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestLeylineStartTimeout pins the MACHE_LEYLINE_START_TIMEOUT override that
// mache-0a1ded added so a cold leyline daemon start (which can exceed the old
// fixed 5s) doesn't spuriously fail with "socket did not appear".
func TestLeylineStartTimeout(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"unset falls back to default", "", defaultLeylineStartTimeout},
		{"go duration seconds", "30s", 30 * time.Second},
		{"go duration minutes", "2m", 2 * time.Minute},
		{"bare integer is seconds", "20", 20 * time.Second},
		{"garbage falls back", "not-a-duration", defaultLeylineStartTimeout},
		{"zero falls back", "0", defaultLeylineStartTimeout},
		{"negative falls back", "-5s", defaultLeylineStartTimeout},
		{"whitespace trimmed", "  45s  ", 45 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("MACHE_LEYLINE_START_TIMEOUT", c.env)
			require.Equal(t, c.want, leylineStartTimeout())
		})
	}
}

// TestDefaultLeylineStartTimeout guards the default from silently regressing
// back to the too-short 5s that motivated mache-0a1ded.
func TestDefaultLeylineStartTimeout(t *testing.T) {
	require.GreaterOrEqual(t, defaultLeylineStartTimeout, 10*time.Second,
		"default must stay comfortably above the old 5s cold-start failure point")
}

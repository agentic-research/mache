package leyline

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCheckLeylineVersionCompat is the external-review M3 negative test: prove
// the client-side handshake actually REFUSES on an incompatible daemon version,
// and stays quiet when it can't tell (old daemon) or when compatible.
func TestCheckLeylineVersionCompat(t *testing.T) {
	const client = "v0.5.7"
	cases := []struct {
		name    string
		resp    map[string]any
		wantErr string // substring; "" = expect nil
	}{
		{
			name: "compatible — matching wire + client >= compat_min",
			resp: map[string]any{"wire_format_major": float64(1), "compat_min": "0.4.1", "schema_version": "0.5.7"},
		},
		{
			name:    "wire-format major mismatch refuses (M3)",
			resp:    map[string]any{"wire_format_major": float64(2), "compat_min": "0.4.1", "schema_version": "0.6.0"},
			wantErr: "wire-format is v2 but this mache build speaks v1",
		},
		{
			name:    "wire major as JSON string still refuses (capnp-json int-as-string)",
			resp:    map[string]any{"wire_format_major": "2"},
			wantErr: "wire-format is v2",
		},
		{
			name:    "client older than compat_min refuses",
			resp:    map[string]any{"wire_format_major": float64(1), "compat_min": "0.9.0"},
			wantErr: "requires >= 0.9.0",
		},
		{
			name: "absent fields — cannot determine, do not refuse (old daemon)",
			resp: map[string]any{"ok": true},
		},
		{
			name: "matching wire, no compat_min — ok",
			resp: map[string]any{"wire_format_major": float64(1)},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkLeylineVersionCompat(c.resp, client)
			if c.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantErr)
		})
	}
}

func TestCompareSemver(t *testing.T) {
	assert.Equal(t, 0, compareSemver("v0.5.7", "0.5.7"))
	assert.Equal(t, -1, compareSemver("0.4.1", "0.5.0"))
	assert.Equal(t, 1, compareSemver("0.5.7", "0.4.1"))
	assert.Equal(t, -1, compareSemver("0.5.6", "0.5.7"))
	assert.Equal(t, 1, compareSemver("1.0.0", "0.99.99"))
	assert.Equal(t, 0, compareSemver("v0.5.7-rc1", "0.5.7"), "pre-release suffix ignored")
	assert.Equal(t, 0, compareSemver("0.5", "0.5.0"), "missing patch treated as 0")
}

func TestVersionUint(t *testing.T) {
	u, ok := versionUint(float64(3))
	assert.True(t, ok)
	assert.Equal(t, uint64(3), u)

	u, ok = versionUint("7")
	assert.True(t, ok)
	assert.Equal(t, uint64(7), u)

	_, ok = versionUint("nope")
	assert.False(t, ok)
	_, ok = versionUint(nil)
	assert.False(t, ok)
	_, ok = versionUint(float64(-1))
	assert.False(t, ok)
}

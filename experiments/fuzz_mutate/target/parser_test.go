package target

import (
	"testing"
)

func FuzzParseToken(f *testing.F) {
	f.Add([]byte("\x03ABC"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseToken(data)
	})
}

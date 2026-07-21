package ingest

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// escapeLikePrefix must render leyline node-id metacharacters literal so the
// scoped call query (LIKE ... ESCAPE '\') doesn't treat '_' or '%' as wildcards
// and over-scope (mache-702f9b).
func TestEscapeLikePrefix(t *testing.T) {
	assert.Equal(t, `func\_declaration\_1`, escapeLikePrefix("func_declaration_1"))
	assert.Equal(t, `x\%y`, escapeLikePrefix("x%y"))
	assert.Equal(t, `p\\q`, escapeLikePrefix(`p\q`))
	assert.Equal(t, `plain.go/identifier`, escapeLikePrefix("plain.go/identifier")) // '/' and '.' untouched
}

package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRefTokens(t *testing.T) {
	assert.Nil(t, RefTokens(""))
	assert.Equal(t, []string{"A"}, RefTokens("A"))
	assert.Equal(t, []string{" spaced "}, RefTokens(" spaced "), "a single-line ref is one token, exactly as rendered")
	assert.Equal(t, []string{"A", "B", "C"}, RefTokens("A\nB\n\n  C \r\n"))
	assert.Empty(t, RefTokens("\n\n"), "a range over an empty list names nothing")
}

func TestDefTokensForName(t *testing.T) {
	assert.Equal(t, []string{"Alpha"}, DefTokensForName("Alpha"))
	assert.Equal(t, []string{"Theory.EulerSum", "EulerSum"}, DefTokensForName("Theory.EulerSum"))
	assert.Equal(t, []string{"a.b.c", "c"}, DefTokensForName("a.b.c"), "only the last segment")
	assert.Equal(t, []string{".hidden"}, DefTokensForName(".hidden"), "a leading dot has no name before it")
	assert.Equal(t, []string{"trailing."}, DefTokensForName("trailing."), "a trailing dot has no segment after it")
	assert.Equal(t, []string{""}, DefTokensForName(""))
}

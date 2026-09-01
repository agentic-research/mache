package mcpserve

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIsTransientRootsError_TimeoutsRetryPermanentFailuresCache is the
// regression for the session-poisoning half of mache-c5e114.
//
// fallbackGraphForSession memoized EVERY roots-discovery failure for the
// session, including a timeout. Observed live: a loaded machine (64 stray
// leyline workers) pushed one roots/list past its 5s deadline, that error was
// cached, and every subsequent tool call in the session failed — reporting a
// workspace problem that had already passed.
//
// The split matters in both directions, so both are asserted. Caching a
// timeout makes a recoverable condition permanent; NOT caching a genuine
// capability gap makes every later call pay the full 5s wait again.
func TestIsTransientRootsError_TimeoutsRetryPermanentFailuresCache(t *testing.T) {
	transient := map[string]error{
		"deadline exceeded": context.DeadlineExceeded,
		"cancelled":         context.Canceled,
		"wrapped deadline":  fmt.Errorf("list roots: %w", context.DeadlineExceeded),
		"deeply wrapped":    fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", context.DeadlineExceeded)),
	}
	for name, err := range transient {
		t.Run("transient/"+name, func(t *testing.T) {
			assert.True(t, isTransientRootsError(err),
				"a timeout describes THIS attempt, not the client — caching it poisons the session")
		})
	}

	permanent := map[string]error{
		"no roots support": errors.New("client does not support MCP roots"),
		"empty roots":      errors.New("client returned no workspace roots"),
		"invalid root":     errors.New("client returned an invalid workspace root"),
	}
	for name, err := range permanent {
		t.Run("permanent/"+name, func(t *testing.T) {
			assert.False(t, isTransientRootsError(err),
				"a capability gap will not change mid-session; caching it makes later calls fail fast")
		})
	}

	assert.False(t, isTransientRootsError(nil), "no error is not a transient error")
}

// TestFallbackGraphForSession_DoesNotCacheATimeout proves the wiring, not just
// the predicate: a timed-out session must be able to resolve on a later call.
//
// Asserted by observing the registry rather than the returned graph — the
// session-error cache is what made the failure permanent, so its emptiness is
// the actual contract.
func TestFallbackGraphForSession_DoesNotCacheATimeout(t *testing.T) {
	r := newGraphRegistry("", nil)

	lg := r.fallbackGraphForSession("sess-timeout", fmt.Errorf("list roots: %w", context.DeadlineExceeded))
	require.NotNil(t, lg)
	_, err := lg.get()
	require.Error(t, err, "the call still fails — it just must not be remembered as permanent")

	_, cached := r.sessionErrors.Load("sess-timeout")
	assert.False(t, cached,
		"a timed-out session must retry on the next tool call, not inherit a cached failure")
}

// TestFallbackGraphForSession_CachesAPermanentFailure is the other half: a
// client that cannot do roots must not re-pay the timeout on every call.
func TestFallbackGraphForSession_CachesAPermanentFailure(t *testing.T) {
	r := newGraphRegistry("", nil)

	lg := r.fallbackGraphForSession("sess-nosupport", errors.New("client does not support MCP roots"))
	require.NotNil(t, lg)

	_, cached := r.sessionErrors.Load("sess-nosupport")
	assert.True(t, cached, "a capability gap is worth remembering so later calls fail fast")
}

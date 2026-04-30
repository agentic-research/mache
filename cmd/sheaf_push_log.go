package cmd

import (
	"log"
	"sync"
)

// sheafPushLogOnce caps the noisy "sheaf topology push" logs to one
// per process. The fire-and-forget push runs on every
// get_communities call; if the daemon doesn't support
// sheaf_set_topology (older builds, non-LLO setups), every call
// would log the error. Once is enough — the user knows the
// daemon's capability state after the first attempt.
var sheafPushLogOnce sync.Once

// logSheafPushOnce logs the push error the first time it's called
// in this process and silently swallows subsequent invocations.
// Intended for the leyline sheaf-cache push goroutine in
// makeGetCommunitiesHandler — every call would otherwise re-log
// the same "unknown op" error from older daemons.
func logSheafPushOnce(err error) {
	sheafPushLogOnce.Do(func() {
		log.Printf("sheaf topology push: %v (further occurrences silenced)", err)
	})
}

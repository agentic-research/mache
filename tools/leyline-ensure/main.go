// Command leyline-ensure provisions Mache's exact published Leyline pin into
// the local cache for conformance and integration tests.
package main

import (
	"fmt"
	"os"

	"github.com/agentic-research/mache/internal/leyline"
)

func main() {
	path, err := leyline.EnsureCachedBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ensure cached leyline: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("cached pinned leyline: %s\n", path)
}

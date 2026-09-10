package main

import (
	"fmt"
	"os"

	"golden/store"
)

// cfg is addressed with & below so the projection records an address ref.
var cfg = store.Config{Limit: 2}

func main() {
	s := store.New(&cfg)
	if err := s.Add("alpha"); err != nil {
		fmt.Println(helper(err))
		os.Exit(1)
	}
	fmt.Println(s.Len(), Version)
}

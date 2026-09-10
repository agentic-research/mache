package main

import "fmt"

// Version is a package-level constant.
const Version = "0.0.1"

// defaultName is a package-level variable.
var defaultName = "golden"

// init here contests main/functions/init with tool/main.go's init.
func init() {
	defaultName = "golden-root"
}

func helper(err error) string {
	return fmt.Sprintf("%s: %v", defaultName, err)
}

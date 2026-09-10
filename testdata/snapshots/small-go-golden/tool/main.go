package main

// init in a subdirectory that shares package name "main" with the root:
// whichever file is projected first owns the bare id.
func init() {
	run()
}

func run() {}

func main() {}

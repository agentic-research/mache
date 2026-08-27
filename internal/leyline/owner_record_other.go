//go:build !unix

package leyline

// processAlive on non-unix conservatively reports DEAD: an unattributable
// process must not be reported as a live owner. When a real port lands, this
// is the seam it fills (OpenProcess on windows).
func processAlive(_ int) bool { return false }

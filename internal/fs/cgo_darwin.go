//go:build darwin

package fs

/*
#cgo CFLAGS: -I/Library/Frameworks/fuse_t.framework/Versions/Current/Headers
#cgo LDFLAGS: -F/Library/Frameworks -framework fuse_t -Wl,-rpath,/Library/Frameworks
*/
import "C"

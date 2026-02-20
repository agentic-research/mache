//go:build leyline

// Package leyline provides Go bindings to the ley-line C FFI.
//
// This wraps the Rust staticlib (libleyline_fs.a) via Cgo, giving mache
// access to the graph and vector search without compiling sqlite-vec or
// fastembed for Go.
//
// Build requirements:
//
//	export LEYLINE_ROOT=/path/to/ley-line/rs
//	cargo build -p leyline-fs --features vec   # in $LEYLINE_ROOT
//
// The CGO_CFLAGS and CGO_LDFLAGS env vars must point to the header and
// library. See the #cgo directives below for the default layout.
//
// This file is gated behind the "leyline" build tag because the C header
// and static library are only available when ley-line is built locally.
package leyline

/*
#cgo CFLAGS: -I${SRCDIR}/../../../ley-line/rs/crates/fs/include
#cgo LDFLAGS: -L${SRCDIR}/../../../ley-line/rs/target/debug -lleyline_fs -lm -ldl -framework Security -framework CoreFoundation
#include "leyline_fs.h"
#include <stdlib.h>
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"runtime"
	"unsafe"
)

// SearchResult represents a single KNN search hit from the vector index.
type SearchResult struct {
	ID       string  `json:"id"`
	Distance float64 `json:"distance"`
}

// Client wraps an opaque leyline context handle.
// It is NOT safe for concurrent use — callers must synchronize externally.
type Client struct {
	ctx *C.struct_LeylineCtx
}

// Open creates a new Client by opening the arena via the control file.
// The control path must point to a .ctrl file created by `leyline serve`.
func Open(controlPath string) (*Client, error) {
	cPath := C.CString(controlPath)
	defer C.free(unsafe.Pointer(cPath))

	ctx := C.leyline_open(cPath)
	if ctx == nil {
		return nil, fmt.Errorf("leyline_open failed for %s", controlPath)
	}

	c := &Client{ctx: ctx}
	runtime.SetFinalizer(c, (*Client).Close)
	return c, nil
}

// Close releases the underlying Rust resources.
// Safe to call multiple times.
func (c *Client) Close() {
	if c.ctx != nil {
		C.leyline_close(c.ctx)
		c.ctx = nil
		runtime.SetFinalizer(c, nil)
	}
}

// Search performs KNN search over the attached VectorIndex.
//
// query is the embedding vector (e.g. 384 floats for MiniLM).
// k is the maximum number of results to return.
//
// Returns nil, error if no VectorIndex is attached or the search fails.
func (c *Client) Search(query []float32, k int) ([]SearchResult, error) {
	if c.ctx == nil {
		return nil, fmt.Errorf("client is closed")
	}
	if len(query) == 0 {
		return nil, fmt.Errorf("empty query vector")
	}

	cQuery := (*C.float)(unsafe.Pointer(&query[0]))

	cJSONPtr := C.leyline_knn_search(
		c.ctx,
		cQuery,
		C.uintptr_t(len(query)),
		C.uintptr_t(k),
	)

	if cJSONPtr == nil {
		return nil, fmt.Errorf("leyline_knn_search returned null (is VectorIndex attached?)")
	}
	defer C.leyline_free_string(cJSONPtr)

	goJSON := C.GoString(cJSONPtr)

	var results []SearchResult
	if err := json.Unmarshal([]byte(goJSON), &results); err != nil {
		return nil, fmt.Errorf("parse search results: %w", err)
	}

	return results, nil
}

// GetNode retrieves a node by ID. Returns the raw JSON string.
func (c *Client) GetNode(id string) (string, error) {
	if c.ctx == nil {
		return "", fmt.Errorf("client is closed")
	}

	cID := C.CString(id)
	defer C.free(unsafe.Pointer(cID))

	var buf [4096]C.uint8_t
	n := C.leyline_get_node(c.ctx, cID, &buf[0], C.uintptr_t(len(buf)))
	if n < 0 {
		return "", fmt.Errorf("leyline_get_node failed (code %d)", n)
	}

	return C.GoStringN((*C.char)(unsafe.Pointer(&buf[0])), C.int(n)), nil
}

// ListChildren returns the JSON array of child nodes under parentID.
func (c *Client) ListChildren(parentID string) (string, error) {
	if c.ctx == nil {
		return "", fmt.Errorf("client is closed")
	}

	cID := C.CString(parentID)
	defer C.free(unsafe.Pointer(cID))

	var buf [65536]C.uint8_t
	n := C.leyline_list_children(c.ctx, cID, &buf[0], C.uintptr_t(len(buf)))
	if n < 0 {
		return "", fmt.Errorf("leyline_list_children failed (code %d)", n)
	}

	return C.GoStringN((*C.char)(unsafe.Pointer(&buf[0])), C.int(n)), nil
}

// ReadContent reads the record content for a node.
func (c *Client) ReadContent(id string, offset uint64) ([]byte, error) {
	if c.ctx == nil {
		return nil, fmt.Errorf("client is closed")
	}

	cID := C.CString(id)
	defer C.free(unsafe.Pointer(cID))

	var buf [65536]C.uint8_t
	n := C.leyline_read_content(c.ctx, cID, &buf[0], C.uintptr_t(len(buf)), C.uint64_t(offset))
	if n < 0 {
		return nil, fmt.Errorf("leyline_read_content failed (code %d)", n)
	}

	out := make([]byte, n)
	copy(out, (*[65536]byte)(unsafe.Pointer(&buf[0]))[:n])
	return out, nil
}

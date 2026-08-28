package mcpserve

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/agentic-research/mache/graph"
)

// Read modes for the read_file MCP tool (mache-qzsk).
//
// Why this exists, measured rather than assumed (tools/token-bench, and
// docs/benchmarks/token-efficiency-baseline.json):
//
//	whole file      9,363 B   1.0 round trips   no intent needed
//	all signatures    528 B   1.0 round trips   no intent needed   <- 17.7x
//	one construct     748 B   1.05 round trips  needs a symbol name
//
// The interesting property of `signatures` is not that it is smaller — a line
// cap is also smaller. It is that it needs no POSITIONING. A bounded line read
// (`read(path, 80, 160)`) cannot express "the function I care about", so the
// offsets have to be computed first, and computing them is a search: measured
// at ~2.05 round trips, an effective ratio of 0.98x once round trips are
// charged, i.e. no saving at all per answer.
//
// `signatures` sidesteps that by not needing to know WHICH construct: it
// returns every construct's declaration line, so one call with the same single
// `path` argument the caller already passes lands at 94.4% of the achievable
// reduction ceiling.
//
// Deliberately derived at READ time from the existing `source` leaf rather than
// from a new `signature` leaf. The schema has no signature capture today (see
// mache-fc737b), and adding one is per-language ingestion work; taking the
// declaration line from content already stored needs no re-ingestion and works
// on every language whose schema splits constructs into child nodes.
const (
	readModeFull       = "full"
	readModeSignatures = "signatures"
	readModeMap        = "map"
)

// validReadModes is the accepted set; an unknown mode is an error rather than
// a silent fallback to full, because silently returning 9KB when the caller
// asked for 528 B is the failure this whole feature exists to prevent.
var validReadModes = map[string]bool{
	readModeFull:       true,
	readModeSignatures: true,
	readModeMap:        true,
}

// declarationLine extracts the first non-blank line of a construct's source —
// the declaration head for every brace language whose signature precedes the
// body on one line.
//
// Known limitation, stated rather than hidden: a signature split across lines
// (a long parameter list) is truncated at the first newline, and a one-line
// function returns its whole body. Both are acceptable for orientation, which
// is what this mode is for; a caller that needs the exact signature should read
// the construct in full. A real `signature` leaf (mache-fc737b) would remove
// the approximation.
func declarationLine(src string) string {
	for line := range strings.SplitSeq(src, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// readProjected renders a directory node under the requested mode. It returns
// ok=false when the mode does not apply to this node (a leaf, or full mode), so
// the caller falls through to the normal single-file read path.
func readProjected(g graph.Graph, nodePath, mode string) (string, bool, error) {
	if mode == "" || mode == readModeFull {
		return "", false, nil
	}
	if !validReadModes[mode] {
		return "", false, fmt.Errorf("unknown mode %q — valid modes: full, signatures, map", mode)
	}
	node, err := g.GetNode(nodePath)
	if err != nil {
		return "", false, fmt.Errorf("not found: %s", nodePath)
	}
	if !node.Mode.IsDir() {
		// A leaf under `signatures` is its own declaration line; under `map` it
		// is just its name. Both are well-defined, so handle rather than reject.
		if mode == readModeMap {
			return path.Base(nodePath), true, nil
		}
		content, rerr := readNodeContent(g, nodePath, node)
		if rerr != nil {
			return "", false, rerr
		}
		return declarationLine(content), true, nil
	}

	children, err := g.ListChildren(nodePath)
	if err != nil {
		return "", false, fmt.Errorf("list %s: %v", nodePath, err)
	}
	// Deterministic output so a caller can diff two reads of the same node.
	names := append([]string(nil), children...)
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		if mode == readModeMap {
			b.WriteString(name)
			b.WriteByte('\n')
			continue
		}
		// signatures: prefer the child's own `source` leaf; a construct dir
		// holds its body there. Fall back to the child itself when the schema
		// stores content directly on the child.
		line := constructSignature(g, nodePath, name)
		if line == "" {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String(), true, nil
}

// constructSignature resolves one child of a construct directory to its
// declaration line, trying the `<child>/source` leaf first.
func constructSignature(g graph.Graph, parent, name string) string {
	for _, candidate := range []string{parent + "/" + name + "/source", parent + "/" + name} {
		node, err := g.GetNode(candidate)
		if err != nil || node.Mode.IsDir() {
			continue
		}
		content, err := readNodeContent(g, candidate, node)
		if err != nil {
			continue
		}
		if line := declarationLine(content); line != "" {
			return line
		}
	}
	return ""
}

// readNodeContent reads a leaf node's full content, honouring the same size cap
// the normal read path uses.
func readNodeContent(g graph.Graph, nodePath string, node *graph.Node) (string, error) {
	size := node.ContentSize()
	if size == 0 {
		return "", nil
	}
	if size > maxReadFileSize {
		return "", fmt.Errorf("%s too large (%d bytes, max %d)", nodePath, size, maxReadFileSize)
	}
	buf := make([]byte, size)
	n, err := g.ReadContent(nodePath, buf, 0)
	if err != nil {
		return "", fmt.Errorf("read %s: %v", nodePath, err)
	}
	return string(buf[:n]), nil
}

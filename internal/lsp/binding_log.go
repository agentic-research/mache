// Package lsp consumes LLO's typed event-log artifacts (.bindings.capnp,
// future .ast.capnp / .source.capnp) sitting next to a SQLite .db.
//
// The capnp event log is the canonical Σ data plane (per
// ley-line-open/rs/ll-core/schema-capnp/README.md, T8 thread). The
// SQLite tables (`_lsp_refs`, `_lsp_defs`, `_ast`) are local
// projections of the same records. Consuming the log directly avoids
// the leaf-vs-call-expression ambiguity that broke Falsifiability B
// at the SQL boundary — the capnp record carries BOTH `constructNodeId`
// and `refSiteNodeId`, so each consumer picks its own AST level
// without rebuilding the walk-up at query time.
package lsp

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	capnp "capnproto.org/go/capnp/v3"
	"github.com/agentic-research/mache/internal/lsp/bindings"
)

// Binding is the Go-native projection of one bindings.BindingRecord.
// Decoupling consumers from the generated capnp types means swapping
// the underlying schema (e.g. when LLO ships T8.5's parseGen → Hash
// reframe) is a one-file change here, not a fan-out across the rule
// engine.
type Binding struct {
	// TargetNodeID is the symbol's defining node — what the LSP
	// resolved to. Same as `_lsp_refs.node_id` in the SQL projection.
	TargetNodeID string

	// RefToken is the textual lemma at the ref site (e.g. "Validate").
	RefToken string

	// ConstructNodeID is the smallest enclosing function/method/
	// constructor declaration. What `find_callers` MCP wants and
	// what `node_refs.node_id` records (after construct-level
	// normalization). Empty when the ref site has no enclosing
	// construct in `_ast`.
	ConstructNodeID string

	// RefSiteNodeID is the smallest enclosing AST node at the ref
	// location — typically a leaf identifier. Most precise locator
	// but can sit several levels below ConstructNodeID. Empty under
	// the same missing-_ast conditions as ConstructNodeID.
	RefSiteNodeID string

	// RefURI is the file URI the ref appears in (canonicalized;
	// matches `_source.path` post-be6136). `file://` scheme.
	RefURI string

	// RefRange locates the reference site for byte-precise tooling.
	RefRange Range

	// ParseGen is the parse generation that emitted this record.
	// T8.5 will replace this with a content hash linked into Σ root.
	ParseGen uint64
}

// Range is the Go-native projection of common.Range — inclusive-start,
// exclusive-end, matching tree-sitter's convention.
type Range struct {
	StartLine, StartColumn uint32
	StartByte              uint64
	EndLine, EndColumn     uint32
	EndByte                uint64
}

// SiblingBindingLogPath returns the path to the binding event log that
// LLO writes alongside a `.db`. Mirrors LLO's `Path::with_extension`
// semantics from cmd_lsp.rs and daemon/lsp_pass.rs: the trailing
// extension on dbPath (typically `.db`) is REPLACED with
// `bindings.capnp`. Pins the convention on the consumer side so a
// typo can't silently drift from the producer.
//
// Examples:
//
//	"/tmp/foo.db"       → "/tmp/foo.bindings.capnp"
//	"/tmp/no-ext"       → "/tmp/no-ext.bindings.capnp"
//	"/tmp/multi.dot.db" → "/tmp/multi.dot.bindings.capnp"
func SiblingBindingLogPath(dbPath string) string {
	ext := filepath.Ext(dbPath)
	return strings.TrimSuffix(dbPath, ext) + ".bindings.capnp"
}

// ReadBindingLog opens a `.bindings.capnp` event log and returns every
// record in stream order. Returns (nil, nil) for an empty file — that
// is a valid LLO output when the LSP pass found no references.
//
// Returns an os.ErrNotExist-wrapping error when the file is absent so
// callers can distinguish "no log produced yet" from "log corrupt".
//
// Reads everything into memory. Mache-scale event logs are ≪100 MB
// even for large repos (323K NVD records map to <30K LSP refs in
// practice; idiomatic Go projects are smaller still). If a real
// workload pushes past that, switch to IterateBindingLog.
func ReadBindingLog(path string) ([]Binding, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open binding log %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var out []Binding
	if err := iterateDecoder(f, func(b Binding) error {
		out = append(out, b)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("decode binding log %q after %d records: %w", path, len(out), err)
	}
	return out, nil
}

// IterateBindingLog streams records to fn one at a time without
// buffering the full set. Use when memory pressure matters or when
// the consumer can short-circuit (e.g. early-exit on first match).
// Returning a non-nil error from fn aborts iteration; the error is
// propagated up wrapped with the record index for diagnostics.
func IterateBindingLog(path string, fn func(Binding) error) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open binding log %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return iterateDecoder(f, fn)
}

func iterateDecoder(r io.Reader, fn func(Binding) error) error {
	dec := capnp.NewDecoder(r)
	for i := 0; ; i++ {
		msg, err := dec.Decode()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("decode record %d: %w", i, err)
		}
		rec, err := bindings.ReadRootBindingRecord(msg)
		if err != nil {
			return fmt.Errorf("read root of record %d: %w", i, err)
		}
		b, err := bindingFromRecord(rec)
		if err != nil {
			return fmt.Errorf("project record %d: %w", i, err)
		}
		if err := fn(b); err != nil {
			return err
		}
	}
}

func bindingFromRecord(rec bindings.BindingRecord) (Binding, error) {
	target, err := rec.TargetNodeId()
	if err != nil {
		return Binding{}, fmt.Errorf("targetNodeId: %w", err)
	}
	tok, err := rec.RefToken()
	if err != nil {
		return Binding{}, fmt.Errorf("refToken: %w", err)
	}
	construct, err := rec.ConstructNodeId()
	if err != nil {
		return Binding{}, fmt.Errorf("constructNodeId: %w", err)
	}
	refSite, err := rec.RefSiteNodeId()
	if err != nil {
		return Binding{}, fmt.Errorf("refSiteNodeId: %w", err)
	}
	uri, err := rec.RefUri()
	if err != nil {
		return Binding{}, fmt.Errorf("refUri: %w", err)
	}
	rng, err := rec.RefRange()
	if err != nil {
		return Binding{}, fmt.Errorf("refRange: %w", err)
	}
	start, err := rng.Start()
	if err != nil {
		return Binding{}, fmt.Errorf("refRange.start: %w", err)
	}
	end, err := rng.End()
	if err != nil {
		return Binding{}, fmt.Errorf("refRange.end: %w", err)
	}
	return Binding{
		TargetNodeID:    target,
		RefToken:        tok,
		ConstructNodeID: construct,
		RefSiteNodeID:   refSite,
		RefURI:          uri,
		ParseGen:        rec.ParseGen(),
		RefRange: Range{
			StartLine:   start.Line(),
			StartColumn: start.Column(),
			StartByte:   start.Byte(),
			EndLine:     end.Line(),
			EndColumn:   end.Column(),
			EndByte:     end.Byte(),
		},
	}, nil
}

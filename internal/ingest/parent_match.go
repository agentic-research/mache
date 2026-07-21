package ingest

import (
	"maps"
)

// parentAwareMatch wraps a Match and injects a _parent key into Values()
// containing the parent match's values. This allows child templates to
// reference parent fields via {{._parent.fieldName}}.
//
// Note: "_parent" is a reserved key. Schema data fields named "_parent" will
// be shadowed. This is by design — underscore-prefixed keys are reserved for
// engine-injected metadata (like _parent, _schema.json, _diagnostics).
//
// All other interfaces (OriginProvider, FileMeta, DocScope, CallExtractor) are
// forwarded to the inner match so that doc comments, location, write-back, and
// per-construct call extraction continue to work on nested matches.
type parentAwareMatch struct {
	inner        Match
	parentValues map[string]any
	cached       map[string]any // built once on first Values() call
}

func (m *parentAwareMatch) Values() map[string]any {
	if m.cached != nil {
		return m.cached
	}
	inner := m.inner.Values()
	v := make(map[string]any, len(inner)+1)
	maps.Copy(v, inner)
	v["_parent"] = m.parentValues
	m.cached = v
	return v
}

func (m *parentAwareMatch) Context() any {
	return m.inner.Context()
}

// CaptureOrigin forwards to the inner match if it implements OriginProvider.
// Required for write-back byte-range tracking.
func (m *parentAwareMatch) CaptureOrigin(name string) (startByte, endByte uint32, ok bool) {
	if op, is := m.inner.(OriginProvider); is {
		return op.CaptureOrigin(name)
	}
	return 0, 0, false
}

// Lang forwards to the inner match if it implements FileMeta. Without this,
// nested matches (which are always wrapped in parentAwareMatch) would lose the
// lang/pkg node properties the engine sets via FileMeta — a regression caught by
// TestEngine_MethodReceiverShape_RegistersBareLeafDef.
func (m *parentAwareMatch) Lang() string {
	if fm, ok := m.inner.(FileMeta); ok {
		return fm.Lang()
	}
	return ""
}

// PackageName forwards to the inner match if it implements FileMeta.
func (m *parentAwareMatch) PackageName() string {
	if fm, ok := m.inner.(FileMeta); ok {
		return fm.PackageName()
	}
	return ""
}

// ScopeSource forwards to the inner match if it implements DocScope. Like the
// FileMeta forwards above, nested matches are always wrapped — without this the
// engine's location/doc lookups would silently no-op on nested constructs.
func (m *parentAwareMatch) ScopeSource() []byte {
	if ds, ok := m.inner.(DocScope); ok {
		return ds.ScopeSource()
	}
	return nil
}

// DocRange forwards to the inner match if it implements DocScope.
func (m *parentAwareMatch) DocRange() (docStart, scopeStart, scopeEnd uint32, ok bool) {
	if ds, ok := m.inner.(DocScope); ok {
		return ds.DocRange()
	}
	return 0, 0, 0, false
}

// ScopeCalls forwards to the inner match if it implements CallExtractor. Nested
// matches are always wrapped, so without this the engine's per-scope call/refs
// extraction would no-op on nested constructs (the slice-1 lesson).
func (m *parentAwareMatch) ScopeCalls() []string {
	if ce, ok := m.inner.(CallExtractor); ok {
		return ce.ScopeCalls()
	}
	return nil
}

// ASTSourceID forwards to the inner match if it implements ASTScope. Nested
// constructs (e.g. methods under a struct) are always wrapped in
// parentAwareMatch — without this forward they'd lose the AST scope mapping
// the engine persists onto their graph node, and find_callees would stay
// broken for exactly the constructs most likely to need scoped resolution
// (bead mache-fd9982).
func (m *parentAwareMatch) ASTSourceID() string {
	if as, ok := m.inner.(ASTScope); ok {
		return as.ASTSourceID()
	}
	return ""
}

// ASTScopeID forwards to the inner match if it implements ASTScope.
func (m *parentAwareMatch) ASTScopeID() string {
	if as, ok := m.inner.(ASTScope); ok {
		return as.ASTScopeID()
	}
	return ""
}

package ingest

import "strings"

// The lookups a selector makes against a file's index (ast_walker_index.go):
// nodes by kind, a capture's leaf by the path the selector spells, and — one
// level up — the scope units and captures of one selector match. None of
// them touches the db; the index is the whole file.

// nodesByKind returns the in-memory nodes of a kind whose id lives under
// parentPrefix (empty = whole file). Mirrors findNodesByKind's SQL semantics
// (a.node_kind = kind AND n.id LIKE parentPrefix||'/%').
func (idx *fileIndex) nodesByKind(parentPrefix, kind string) []astNode {
	prefix := parentPrefix + "/"
	var out []astNode
	for _, i := range idx.byKind[kind] {
		n := idx.all[i]
		if parentPrefix != "" && !strings.HasPrefix(n.id, prefix) {
			continue
		}
		out = append(out, n.toAST())
	}
	return out
}

// childByKind returns the first (by start_byte) descendant of parentID that
// the selector path leaf+ancestry reaches, or nil: a direct child when
// ancestry is empty, else the node exactly len(ancestry)+1 levels below
// parentID whose ancestors match ancestry step for step.
func (idx *fileIndex) childByKind(parentID string, leaf pathStep, ancestry []pathStep) *astNode {
	if out := idx.descendantsByKind(parentID, leaf, ancestry, 1); len(out) > 0 {
		return &out[0]
	}
	return nil
}

// childrenByKind is the multi-result form of childByKind (all matching
// descendants, ordered by start_byte).
func (idx *fileIndex) childrenByKind(parentID string, leaf pathStep, ancestry []pathStep) []astNode {
	return idx.descendantsByKind(parentID, leaf, ancestry, 0)
}

// descendantsByKind returns up to limit (0 = all) descendants of parentID
// matching leaf whose path up to parentID reads ancestry — kinds and fields
// both, so `receiver: (parameter_list ...)` reaches the receiver's list and
// never the parameters' (mache-91d903). Candidates are the file's nodes of
// the leaf kind under parentID's id prefix, in start_byte order; each is
// verified by climbing.
func (idx *fileIndex) descendantsByKind(parentID string, leaf pathStep, ancestry []pathStep, limit int) []astNode {
	prefix := parentID + "/"
	var out []astNode
	for _, i := range idx.byKind[leaf.kind] {
		n := idx.all[i]
		if !strings.HasPrefix(n.id, prefix) || !leaf.matches(n) {
			continue
		}
		top, ok := idx.climb(i, ancestry)
		if !ok || idx.all[top].parentID != parentID {
			continue
		}
		out = append(out, n.toAST())
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

// climb walks up from node i through len(steps) parents, checking each against
// its step innermost-first: steps[len-1] must be i's parent, steps[0] the
// outermost. Returns the outermost ancestor reached (i itself for no steps)
// and whether every ancestor matched.
func (idx *fileIndex) climb(i int, steps []pathStep) (int, bool) {
	cur := i
	for s := len(steps) - 1; s >= 0; s-- {
		pi, ok := idx.byID[idx.all[cur].parentID]
		if !ok || !steps[s].matches(idx.all[pi]) {
			return cur, false
		}
		cur = pi
	}
	return cur, true
}

// scopeUnit is one match in the making: the outer node the selector matched
// and the @scope node the match projects. They differ only when @scope sits
// on an inner node.
type scopeUnit struct {
	outerID string  // capture base for captures NOT under the @scope
	scope   astNode // the @scope node: text/range/ParentPrefix + capture base for captures under it
}

// scopeUnits expands each outer node into its scope units. With no inner
// scope path (the common case) the outer node is its own scope. Otherwise a
// single outer node expands to one unit PER inner scope node reached by
// scopePath — so grouped declarations like `type ( Alpha; Beta )` project
// each member, matching tree-sitter's one-match-per-inner-node semantics —
// falling back to the outer node when none resolves, which preserves the
// single-node behavior for odd shapes.
func (idx *fileIndex) scopeUnits(outer []astNode, scopePath []pathStep) []scopeUnit {
	units := make([]scopeUnit, 0, len(outer))
	for _, o := range outer {
		var inners []astNode
		if len(scopePath) > 0 {
			inners = idx.childrenByKind(o.id, scopePath[len(scopePath)-1], scopePath[:len(scopePath)-1])
		}
		if len(inners) == 0 {
			units = append(units, scopeUnit{outerID: o.id, scope: o})
			continue
		}
		for _, in := range inners {
			units = append(units, scopeUnit{outerID: o.id, scope: in})
		}
	}
	return units
}

// resolveCaptures resolves pattern's captures for one scope unit: each
// capture's text (a leaf's record, else its source bytes) under its name, and
// its byte range for CaptureOrigin. "scope" carries the @scope node's own
// source text, so leaf templates like {{.scope}} render the construct.
//
// Captures nested under the @scope node resolve relative to it, with the
// scope path stripped from their ancestry; the rest resolve relative to the
// outer node with full ancestry. Reports false when a required capture — any
// whose name does not start with "_" — is missing: the unit is no match.
func (idx *fileIndex) resolveCaptures(pattern *selectorPattern, scopePath []pathStep, unit scopeUnit, source []byte) (map[string]any, map[string][2]int, bool) {
	values := map[string]any{}
	ranges := map[string][2]int{}
	if text, ok := sourceText(source, unit.scope); ok {
		values["scope"] = text
	}
	for _, cap := range pattern.captures {
		base, ancestry := unit.outerID, cap.ancestry
		if len(scopePath) > 0 && ancestryHasPrefix(cap.ancestry, scopePath) {
			base, ancestry = unit.scope.id, cap.ancestry[len(scopePath):]
		}
		child := idx.childByKind(base, pathStep{kind: cap.kind, field: cap.field}, ancestry)
		if child == nil {
			if !strings.HasPrefix(cap.name, "_") {
				return nil, nil, false
			}
			continue
		}
		if child.startByte < child.endByte {
			ranges[cap.name] = [2]int{child.startByte, child.endByte}
		}
		if child.record != "" {
			values[cap.name] = child.record
		} else if text, ok := sourceText(source, *child); ok {
			values[cap.name] = text
		}
	}
	return values, ranges, true
}

// sourceText is n's span of source, when source is present and the span is a
// non-empty range inside it.
func sourceText(source []byte, n astNode) (string, bool) {
	if source == nil || n.startByte >= n.endByte || n.endByte > len(source) {
		return "", false
	}
	return string(source[n.startByte:n.endByte]), true
}

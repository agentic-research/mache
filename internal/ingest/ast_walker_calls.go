package ingest

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/agentic-research/mache/graph"
)

// CallPattern describes one shape of function call for a language. The
// walker evaluates these over the file's in-memory node section (one
// ancestor-chain check per candidate leaf) — no SQL per pattern, and none of
// the N-queries-per-file pattern of the generic selector path.
//
// Examples:
//
//	Go bare:       OuterKind=call_expression, LeafKind=identifier
//	Go qualified:  OuterKind=call_expression, Ancestors=[selector_expression],
//	               LeafKind=field_identifier, QualifierKind=identifier
type CallPattern struct {
	OuterKind     string   // e.g. "call_expression"
	Ancestors     []string // intermediate kinds between outer and leaf
	LeafKind      string   // node kind whose record is the call name (@call)
	QualifierKind string   // optional sibling-leaf kind for the package qualifier (@pkg); empty when not qualified
	// RequirePriorSibling, when true, restricts the match to leaves whose
	// immediate ancestor (Ancestors[0], the direct child of OuterKind) has
	// an EARLIER same-kind sibling under OuterKind. This reproduces
	// tree-sitter's positional matching for value-position captures like
	//   (keyed_element (literal_element) (literal_element (identifier) @x))
	// where only the SECOND literal_element (the value) is captured, not
	// the first (the key). Requires len(Ancestors) >= 1.
	RequirePriorSibling bool
}

// callPatternRegistry stores per-language CallPatterns for ASTWalker.
// Each language maps to a slice (multiple shapes per language).
var callPatternRegistry sync.Map // langName → []CallPattern

// RegisterASTCallPatterns registers per-language structured call patterns for
// the batched ExtractCalls / ExtractQualifiedCalls fast path.
func RegisterASTCallPatterns(langName string, patterns []CallPattern) {
	callPatternRegistry.Store(langName, patterns)
}

// ExtractCalls returns deduplicated function-call tokens for the given
// source file, evaluating every registered pattern over the file's node
// section — one statement to load it, none per pattern or per scope.
//
// Returns (nil, nil) when the language has no registered patterns — the
// caller treats that as "no calls" and falls through to other paths.
func (w *ASTWalker) ExtractCalls(sourcePath, langName string) ([]string, error) {
	raw, ok := callPatternRegistry.Load(langName)
	if !ok {
		return nil, nil
	}
	patterns := raw.([]CallPattern)
	if len(patterns) == 0 {
		return nil, nil
	}

	sourceID := filepath.Base(sourcePath)
	seen := make(map[string]bool)
	var calls []string
	for _, p := range patterns {
		rows, err := w.callRows(sourceID, p, false, "")
		if err != nil {
			continue
		}
		for _, r := range rows {
			if r.token != "" && !seen[r.token] {
				seen[r.token] = true
				calls = append(calls, r.token)
			}
		}
	}
	return calls, nil
}

// ExtractCallsScoped is the per-construct equivalent of ExtractCalls: it
// returns only call tokens whose leaf node lives under scopeID's id-path —
// matching SitterWalker's per-scope ExtractCalls(scopeNode, ...). sourceID is
// the _source key (filepath.Base of the file). Empty scopeID would match the
// whole file; callers pass a real construct id.
func (w *ASTWalker) ExtractCallsScoped(sourceID, scopeID, langName string) ([]string, error) {
	toks, err := w.fileCallTokens(sourceID, langName)
	if err != nil {
		return nil, err
	}
	prefix := scopeID + "/"
	seen := make(map[string]bool)
	var calls []string
	for _, t := range toks {
		// Byte-identical to the old per-construct `n_leaf.id LIKE scopeID||'/%'`
		// scope filter, but over the whole-file token set computed once.
		if scopeID != "" && !strings.HasPrefix(t.nodeID, prefix) {
			continue
		}
		if !seen[t.token] {
			seen[t.token] = true
			calls = append(calls, t.token)
		}
	}
	return calls, nil
}

// scopedCallToken is one whole-file call token plus the id of its leaf node,
// so per-construct ExtractCallsScoped can attribute it by node-id prefix.
type scopedCallToken struct {
	nodeID string
	token  string
}

// fileCallTokens runs every registered call pattern against the WHOLE file
// ONCE (callRows is O(nodes) per pattern over the in-memory section) and
// caches the (leaf id, token) pairs keyed by (sourceID, lang).
// ExtractCallsScoped then filters these by construct prefix in Go instead of
// re-evaluating every pattern per construct — that per-construct path was 49%
// of a whole-repo projection after the node-index fix (mache-4f3840). Tokens
// are kept undeduplicated here (the same token may live under several
// constructs); the per-construct caller dedups within its scope.
func (w *ASTWalker) fileCallTokens(sourceID, langName string) ([]scopedCallToken, error) {
	key := sourceID + "\x00" + langName
	if v, ok := w.callTokenCache.Load(key); ok {
		return v.([]scopedCallToken), nil
	}

	var out []scopedCallToken
	if raw, ok := callPatternRegistry.Load(langName); ok {
		if patterns := raw.([]CallPattern); len(patterns) > 0 {
			for _, p := range patterns {
				rows, err := w.callRows(sourceID, p, false, "") // whole file
				if err != nil {
					// callRows evaluates a STRUCTURED kind-chain, so an error
					// is a real/transient DB failure, not an unsupported
					// selector. Surface it and DON'T cache the
					// partial set — caching an empty result forever would
					// silently and permanently empty this file's callees on a
					// long-lived serve (mache-015f5c).
					return nil, fmt.Errorf("file call tokens %s/%s (%s/%s): %w",
						sourceID, langName, p.OuterKind, p.LeafKind, err)
				}
				for _, r := range rows {
					if r.token == "" {
						continue
					}
					out = append(out, scopedCallToken{nodeID: r.leafID, token: r.token})
				}
			}
		}
	}

	w.callTokenCache.Store(key, out)
	return out, nil
}

// fileLevelRefPatternRegistry stores per-language CallPatterns used by
// ExtractFileLevelRefs — the file-wide identifier-capture shapes that
// per-scope ExtractCalls can't see (e.g. Go top-level cobra var func
// values, mache-02r9). Mirrors SitterWalker's file-level ref query, but
// as structured kind-chains queried over _ast/nodes.
var fileLevelRefPatternRegistry sync.Map // langName → []CallPattern

// RegisterASTFileLevelRefPatterns registers the per-language file-level
// ref capture shapes for ExtractFileLevelRefs.
func RegisterASTFileLevelRefPatterns(langName string, patterns []CallPattern) {
	fileLevelRefPatternRegistry.Store(langName, patterns)
}

// ExtractFileLevelRefs returns deduplicated identifier tokens captured by
// the language's file-level ref patterns across the WHOLE file. Mirrors
// SitterWalker.ExtractFileLevelRefs (the _file_level: sentinel feed that
// dead_code reads) but queries _ast/nodes via SQL instead of CGO
// tree-sitter. sourceID is the _source key (filepath.Base of the file).
// Returns (nil, nil) when no patterns are registered for the language.
func (w *ASTWalker) ExtractFileLevelRefs(sourceID, langName string) ([]string, error) {
	raw, ok := fileLevelRefPatternRegistry.Load(langName)
	if !ok {
		return nil, nil
	}
	patterns := raw.([]CallPattern)
	if len(patterns) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool)
	var refs []string
	for _, p := range patterns {
		// Unlike the generic Query path (where a selector may legitimately
		// be unsupported), callRows evaluates a structured kind-chain — an
		// error here is a real bug (invalid pattern or a failed section
		// load), not an unsupported pattern. Surface it rather than
		// returning a silently-incomplete ref set: these tokens feed the
		// _file_level: sentinel dead_code reads, so a short set with nil
		// error would manifest as silent dead_code false positives.
		rows, err := w.callRows(sourceID, p, false, "")
		if err != nil {
			return nil, fmt.Errorf("file-level ref pattern %s/%s: %w", p.OuterKind, p.LeafKind, err)
		}
		for _, r := range rows {
			if r.token != "" && !seen[r.token] {
				seen[r.token] = true
				refs = append(refs, r.token)
			}
		}
	}
	return refs, nil
}

// ExtractQualifiedCalls returns call tokens with optional package qualifiers.
// Patterns whose QualifierKind is non-empty produce QualifiedCall with both
// fields; bare patterns produce QualifiedCall with empty Qualifier.
func (w *ASTWalker) ExtractQualifiedCalls(sourcePath, langName string) ([]graph.QualifiedCall, error) {
	raw, ok := callPatternRegistry.Load(langName)
	if !ok {
		return nil, nil
	}
	patterns := raw.([]CallPattern)

	sourceID := filepath.Base(sourcePath)
	seen := make(map[string]bool)
	var calls []graph.QualifiedCall
	for _, p := range patterns {
		rows, err := w.callRows(sourceID, p, true, "")
		if err != nil {
			// callRows evaluates a structured kind-chain, so an error is a
			// real/transient DB failure, not an unsupported selector.
			// Surface it — a silent short list is indistinguishable
			// from "calls nothing", the class mache-015f5c closes.
			return nil, fmt.Errorf("extract qualified calls %s (%s/%s): %w",
				sourceID, p.OuterKind, p.LeafKind, err)
		}
		for _, r := range rows {
			if r.token == "" {
				continue
			}
			key := r.qualifier + "." + r.token
			if seen[key] {
				continue
			}
			seen[key] = true
			calls = append(calls, graph.QualifiedCall{Token: r.token, Qualifier: r.qualifier})
		}
	}
	return calls, nil
}

// ExtractQualifiedCallsScoped is the per-construct equivalent of
// ExtractQualifiedCalls: it returns qualified call tokens whose leaf node
// lives under scopeID's id-path, instead of the whole file. sourceID is the
// real `_ast`/`_source` key (e.g. "agent.go"), NOT a graph node id — feeding
// a graph node id here (e.g. "cmd/functions/evalOrAbs") matches zero `_ast`
// rows, which was the root cause of find_callees silently returning nothing
// on the serve/mount path (bead mache-fd9982). Callers recover the correct
// (sourceID, scopeID) pair from the construct's graph node Properties
// ("ast_source_id"/"ast_scope_id"), persisted at projection time by
// engine_walk.go via the ASTScope interface. Empty scopeID matches the
// whole file, mirroring ExtractQualifiedCalls.
func (w *ASTWalker) ExtractQualifiedCallsScoped(sourceID, scopeID, langName string) ([]graph.QualifiedCall, error) {
	raw, ok := callPatternRegistry.Load(langName)
	if !ok {
		return nil, nil
	}
	patterns := raw.([]CallPattern)

	seen := make(map[string]bool)
	var calls []graph.QualifiedCall
	for _, p := range patterns {
		rows, err := w.callRows(sourceID, p, true, scopeID)
		if err != nil {
			// A DB failure must surface, not silently drop this pattern's
			// calls (find_callees live path; mache-6ff371, matching
			// mache-015f5c for fileCallTokens/fileAddrRefs).
			return nil, fmt.Errorf("extract qualified calls scoped %s/%s (%s/%s): %w",
				sourceID, scopeID, p.OuterKind, p.LeafKind, err)
		}
		for _, r := range rows {
			if r.token == "" {
				continue
			}
			key := r.qualifier + "." + r.token
			if seen[key] {
				continue
			}
			seen[key] = true
			calls = append(calls, graph.QualifiedCall{Token: r.token, Qualifier: r.qualifier})
		}
	}
	return calls, nil
}

// callRow is one extracted call from a single pattern evaluation.
type callRow struct {
	token     string
	qualifier string
	leafID    string // the leaf node's id — used to attribute the call to a construct
}

// callRows evaluates one CallPattern over a source file from the file's
// in-memory section (fileIndex.callRows) — no SQL per pattern. The result
// has one row per matched call regardless of how many scope nodes the file
// has; scopePrefix restricts it to one construct's subtree ("" = whole file).
// A pattern that cannot be evaluated and a failed section load are errors,
// which every caller surfaces: a silent short list is indistinguishable
// from "calls nothing" (mache-015f5c).
func (w *ASTWalker) callRows(sourceID string, p CallPattern, wantQualifier bool, scopePrefix string) ([]callRow, error) {
	if p.OuterKind == "" || p.LeafKind == "" {
		return nil, fmt.Errorf("invalid CallPattern: OuterKind and LeafKind required")
	}
	// RequirePriorSibling constrains the immediate ancestor, so it is
	// meaningless without at least one. Reject rather than silently dropping
	// the constraint, which would over-capture undetectably.
	if p.RequirePriorSibling && len(p.Ancestors) == 0 {
		return nil, fmt.Errorf("invalid CallPattern: RequirePriorSibling requires at least one ancestor")
	}
	idx, err := w.fileIndex(sourceID)
	if err != nil {
		return nil, fmt.Errorf("call pattern index: %w", err)
	}
	return idx.callRows(p, wantQualifier, scopePrefix), nil
}

// callRows evaluates one CallPattern over the file: every LeafKind node whose
// ancestor chain reads Ancestors[len-1] … Ancestors[0] up to an OuterKind
// node, in leaf start_byte order. scopePrefix restricts leaves to that node's
// subtree (its id path plus "/"); "" is the whole file.
//
// With wantQualifier on a qualified pattern (QualifierKind non-empty), one
// row is produced per sibling of the leaf, carrying that sibling's text as
// the qualifier — "" when the sibling is not a leaf (the operand of `a.b.C()`
// is a selector_expression) — and a leaf with no sibling produces none. The
// sibling's kind is NOT matched against QualifierKind: the SQL this replaced
// tested it only inside a LEFT JOIN's ON clause, which drops nothing, and a
// strict match would lose `a.b.C()` as a call entirely rather than degrade
// it to an unqualified one.
func (idx *fileIndex) callRows(p CallPattern, wantQualifier bool, scopePrefix string) []callRow {
	qualified := wantQualifier && p.QualifierKind != ""
	prefix := scopePrefix + "/"
	ancestry := make([]pathStep, len(p.Ancestors))
	for i, kind := range p.Ancestors {
		ancestry[i] = pathStep{kind: kind}
	}
	var out []callRow
	for _, li := range idx.byKind[p.LeafKind] {
		leaf := idx.all[li]
		if scopePrefix != "" && !strings.HasPrefix(leaf.id, prefix) {
			continue
		}
		// Walk up: the nearest ancestor is Ancestors[len-1], the child of the
		// outer node is Ancestors[0]. Call patterns constrain kinds only.
		cur, ok := idx.climb(li, ancestry)
		if !ok {
			continue
		}
		anc0 := idx.all[cur]
		oi, ok := idx.byID[anc0.parentID]
		if !ok || idx.all[oi].astKind != p.OuterKind {
			continue
		}
		// Value-position constraint: an earlier same-kind sibling of the
		// immediate ancestor under the outer node (the key literal_element
		// before the value one in a keyed_element). len(Ancestors) >= 1 is
		// guaranteed by the caller's validation.
		if p.RequirePriorSibling && !idx.hasPriorSibling(anc0) {
			continue
		}
		if !qualified {
			out = append(out, callRow{token: leaf.record, leafID: leaf.id})
			continue
		}
		for _, si := range idx.children[leaf.parentID] {
			if si == li {
				continue
			}
			out = append(out, callRow{token: leaf.record, qualifier: idx.all[si].record, leafID: leaf.id})
		}
	}
	return out
}

// hasPriorSibling reports whether n has a same-kind sibling starting before it.
func (idx *fileIndex) hasPriorSibling(n idxNode) bool {
	for _, si := range idx.children[n.parentID] {
		s := idx.all[si]
		if s.astKind == n.astKind && s.startByte < n.startByte {
			return true
		}
	}
	return false
}

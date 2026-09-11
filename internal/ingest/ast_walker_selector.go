package ingest

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// selectorPattern is the parsed representation of a tree-sitter S-expression
// selector. ASTWalker evaluates it against a file's in-memory index of the
// _ast/nodes/node_child rows.
type selectorPattern struct {
	outerKind     string // the node kind to match (e.g., "function_declaration")
	captures      []selectorCapture
	predicates    []selectorPredicate      // #eq? filters
	matchPreds    []selectorMatchPredicate // #match? regex filters
	notMatchPreds []selectorMatchPredicate // #not-match? negated regex filters

	// scopeKind/scopeField/scopeAncestry locate the @scope node when it is NOT
	// the outer match node — e.g. `(type_declaration (type_spec ...) @scope)`
	// binds @scope to the inner type_spec. Empty scopeKind (or scopeKind ==
	// outerKind) means @scope is the outer node. Used to resolve the
	// construct's source text and write-back byte range to the correct node.
	scopeKind     string
	scopeField    string
	scopeAncestry []pathStep
}

// pathStep is one node on the path a selector spells from the outer node down
// to a capture: the node's tree-sitter kind and, when the selector labelled it,
// the field it must sit under in its parent. An empty field is a wildcard —
// `(parameter_list (parameter_declaration ...))` accepts a parameter_declaration
// under any field, which is tree-sitter's own semantics for an unlabelled
// child pattern. So is the kind wildcardKind: `(impl_item type: (_ type:
// (type_identifier) @receiver))` reaches the receiver's type_identifier
// through whatever wraps it — generic_type, reference_type, pointer_type —
// which is tree-sitter's `(_)`, any named node.
type pathStep struct {
	kind  string
	field string
}

// wildcardKind is tree-sitter's `_`: a step that matches a node of any kind.
// Only named nodes are indexed, so `(_)` and bare `_` are the same here.
const wildcardKind = "_"

// matches reports whether n is this step: its kind (any, for the wildcard),
// and its field when the step names one.
func (p pathStep) matches(n idxNode) bool {
	return (p.kind == wildcardKind || n.astKind == p.kind) && (p.field == "" || n.field == p.field)
}

type selectorCapture struct {
	kind  string // leaf node kind to match (e.g., "type_identifier")
	name  string // capture name (e.g., "receiver")
	field string // the field the leaf sits under, when the selector labelled it ("" = any)
	// ancestry is the path from the outer node to the leaf, exclusive of
	// both: for the pointer-receiver selector,
	// [parameter_list@receiver, parameter_declaration, pointer_type@type].
	// A capture is resolved by walking UP from each candidate leaf through
	// exactly these steps to the outer node (fileIndex.climb) — the kinds AND
	// the fields, which is what tells a receiver's parameter_list from the
	// parameters' (mache-91d903).
	ancestry []pathStep
}

// selectorPredicate represents a #eq? filter: capture text must equal literal.
type selectorPredicate struct {
	capture string // capture name to check (e.g., "_type")
	literal string // expected text value (e.g., "resource")
}

// selectorMatchPredicate represents a #match? or #not-match? filter:
// capture text must (or must not) match the regex.
type selectorMatchPredicate struct {
	capture string         // capture name to check
	regex   *regexp.Regexp // compiled regex; tree-sitter regex syntax is RE2-compatible for the patterns we care about
	pattern string         // original pattern source (for error messages)
}

// parseSelector parses a tree-sitter S-expression into a selectorPattern.
// Builds the ancestry path for each capture — kinds and `field:` labels — so
// that nested constraints (pointer_type > type_identifier vs a bare
// type_identifier; a `receiver:` parameter_list vs the `parameters:` one) are
// matched against the parse tree the way tree-sitter itself would match them.
//
// Supports #eq?, #match?, and #not-match? predicates. Captured text for
// match predicates is resolved from the _source byte ranges already populated
// by the capture loop. Other tree-sitter predicates (#not-eq?, #any-eq?,
// #is?, #is-not?) are not supported.
func parseSelector(selector string) (*selectorPattern, error) {
	s := strings.TrimSpace(selector)
	if s == "" {
		return nil, fmt.Errorf("empty selector")
	}
	for _, un := range []string{"#not-eq?", "#any-eq?", "#is?", "#is-not?"} {
		if strings.Contains(s, un) {
			return nil, fmt.Errorf("%s predicates require SitterWalker (CGO)", un)
		}
	}

	// Tokenize the S-expression
	tokens := tokenizeSExpr(s)
	if len(tokens) == 0 {
		return nil, fmt.Errorf("empty selector after tokenize")
	}

	pattern := &selectorPattern{}
	pos := 0

	// Parse the outermost (kind ...) @scope
	_ = parseSExprNode(tokens, pos, nil, "", pattern)

	// Extract #eq? / #match? / #not-match? predicates
	for i := 0; i < len(tokens)-4; i++ {
		if tokens[i] != "(" {
			continue
		}
		predName := tokens[i+1]
		if predName != "#eq?" && predName != "#match?" && predName != "#not-match?" {
			continue
		}
		if i+4 >= len(tokens) || !strings.HasPrefix(tokens[i+2], "@") {
			continue
		}
		capName := tokens[i+2][1:]
		if capName == "" {
			continue
		}
		literal := strings.Trim(tokens[i+3], "\"")
		switch predName {
		case "#eq?":
			pattern.predicates = append(pattern.predicates, selectorPredicate{
				capture: capName,
				literal: literal,
			})
		case "#match?", "#not-match?":
			re, err := regexp.Compile(literal)
			if err != nil {
				return nil, fmt.Errorf("compile %s regex %q: %w", predName, literal, err)
			}
			mp := selectorMatchPredicate{capture: capName, regex: re, pattern: literal}
			if predName == "#match?" {
				pattern.matchPreds = append(pattern.matchPreds, mp)
			} else {
				pattern.notMatchPreds = append(pattern.notMatchPreds, mp)
			}
		}
	}

	if pattern.outerKind == "" {
		return nil, fmt.Errorf("no node kind in selector: %s", selector)
	}
	// The outer node is looked up by kind across the file; a wildcard there
	// would be every node.
	if pattern.outerKind == wildcardKind {
		return nil, fmt.Errorf("outer node of selector cannot be the wildcard (_): %s", selector)
	}

	return pattern, nil
}

// tokenizeSExpr splits an S-expression into tokens: "(", ")", identifiers, @captures, "field:", "#eq?", strings.
func tokenizeSExpr(s string) []string {
	var tokens []string
	i := 0
	for i < len(s) {
		ch := s[i]
		switch ch {
		case '(', ')':
			tokens = append(tokens, string(ch))
			i++
		case ' ', '\t', '\n':
			i++
		case '"':
			// Quoted string
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j < len(s) {
				j++ // consume closing quote
			}
			tokens = append(tokens, s[i:j])
			i = j
		default:
			// Identifier, @capture, field:, #predicate
			j := i
			for j < len(s) && s[j] != ' ' && s[j] != '(' && s[j] != ')' && s[j] != '\t' && s[j] != '\n' {
				j++
			}
			tokens = append(tokens, s[i:j])
			i = j
		}
	}
	return tokens
}

// parseSExprNode parses one (kind children...) node from tokens starting at pos.
// path is the node-kind path from the root of the parse down to this node's
// parent; its first entry is always the outerKind. field is the `field:` label
// this node was introduced under in its parent ("" when unlabelled). Captures
// record their ancestry via ancestryFromPath (skip the outer node, keep the
// rest). Returns the position after the closing paren and any @captures that
// follow it — a node consumes its own captures, so its parent never sees them.
func parseSExprNode(tokens []string, pos int, path []pathStep, field string, pattern *selectorPattern) int {
	if pos >= len(tokens) || tokens[pos] != "(" {
		return pos
	}
	pos++ // consume "("

	// A predicate like (#eq? ...) is not a node; parseSelector reads those.
	if pos < len(tokens) && strings.HasPrefix(tokens[pos], "#") {
		return skipBalanced(tokens, pos, 1)
	}
	if pos >= len(tokens) {
		return pos
	}
	nodeKind := tokens[pos]
	pos++
	if pattern.outerKind == "" {
		pattern.outerKind = nodeKind
	}
	self := pathStep{kind: nodeKind, field: field}
	ancestry := ancestryFromPath(path)

	// Children: `field:` labels, nested (kind ...) patterns, @captures. A label
	// applies to the child pattern that follows it and to nothing else.
	childField := ""
	for pos < len(tokens) && tokens[pos] != ")" {
		switch tok := tokens[pos]; {
		case strings.HasSuffix(tok, ":"):
			childField = strings.TrimSuffix(tok, ":")
			pos++
		case tok == "(":
			pos = parseSExprNode(tokens, pos, append(path, self), childField, pattern)
			childField = ""
		case strings.HasPrefix(tok, "@"):
			pattern.capture(tok[1:], self, ancestry)
			pos++
		default:
			pos++ // some other token — skip
		}
	}
	if pos < len(tokens) {
		pos++ // consume ")"
	}

	// Captures after the closing paren name this node: ") @scope", ") @name",
	// ") @_type".
	for pos < len(tokens) && strings.HasPrefix(tokens[pos], "@") {
		pattern.capture(tokens[pos][1:], self, ancestry)
		pos++
	}
	return pos
}

// capture records the @name written against node self, whose path from the
// outer node is ancestry. @scope marks self as the construct the match
// projects (only meaningful when self is an inner node — on the outer node it
// is a no-op in Query); any other name is a capture, including the leading-"_"
// ones that exist only to feed predicates.
func (p *selectorPattern) capture(name string, self pathStep, ancestry []pathStep) {
	switch name {
	case "":
	case "scope":
		p.scopeKind, p.scopeField, p.scopeAncestry = self.kind, self.field, ancestry
	default:
		p.captures = append(p.captures, selectorCapture{
			kind: self.kind, name: name, field: self.field, ancestry: ancestry,
		})
	}
}

// skipBalanced advances pos past the parens that close depth open ones.
func skipBalanced(tokens []string, pos, depth int) int {
	for pos < len(tokens) && depth > 0 {
		switch tokens[pos] {
		case "(":
			depth++
		case ")":
			depth--
		}
		pos++
	}
	return pos
}

// ancestryFromPath returns the intermediate steps between the outer node and
// the node at the end of path. path[0] is always the outerKind — skip it.
//
//	path=[call, arguments, call] → [arguments, call]
func ancestryFromPath(path []pathStep) []pathStep {
	if len(path) <= 1 {
		return nil
	}
	out := make([]pathStep, len(path)-1)
	copy(out, path[1:])
	return out
}

// ancestryHasPrefix reports whether a capture's ancestry (relative to the outer
// node) begins with the given @scope prefix (scopeAncestry + the scope step).
// When it does, the capture lives under the @scope node, so it can be resolved
// relative to the inner scope node with the prefix stripped — the mechanism
// that lets grouped declarations resolve each member's captures against the
// right inner node instead of the first one under the shared outer node.
func ancestryHasPrefix(ancestry, prefix []pathStep) bool {
	return len(ancestry) >= len(prefix) && slices.Equal(ancestry[:len(prefix)], prefix)
}

// innerScopePath is the path from the outer node to the @scope node when
// @scope sits on an INNER node — e.g. `(type_declaration (type_spec ...) @scope)`
// binds it to the type_spec — and nil when @scope is the outer node itself
// (the common case), where every outer node is its own scope.
func (p *selectorPattern) innerScopePath() []pathStep {
	if p.scopeKind == "" || p.scopeKind == p.outerKind {
		return nil
	}
	return append(slices.Clone(p.scopeAncestry), pathStep{kind: p.scopeKind, field: p.scopeField})
}

// accepts applies the pattern's predicates to one match's captured text:
// every #eq? capture must equal its literal, every #match? capture must match
// its regex, every #not-match? capture must not. A capture a predicate names
// but the match did not resolve fails the predicate.
func (p *selectorPattern) accepts(values map[string]any) bool {
	text := func(capture string) (string, bool) {
		s, ok := values[capture].(string)
		return s, ok
	}
	for _, pred := range p.predicates {
		if s, ok := text(pred.capture); !ok || s != pred.literal {
			return false
		}
	}
	for _, mp := range p.matchPreds {
		if s, ok := text(mp.capture); !ok || !mp.regex.MatchString(s) {
			return false
		}
	}
	for _, mp := range p.notMatchPreds {
		if s, ok := text(mp.capture); !ok || mp.regex.MatchString(s) {
			return false
		}
	}
	return true
}

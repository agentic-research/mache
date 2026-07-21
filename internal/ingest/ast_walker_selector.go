package ingest

import (
	"fmt"
	"regexp"
	"strings"
)

// selectorPattern is the parsed representation of a tree-sitter S-expression
// selector. ASTWalker translates this into SQL queries against the _ast/nodes
// tables.
type selectorPattern struct {
	outerKind     string // the node kind to match (e.g., "function_declaration")
	captures      []selectorCapture
	predicates    []selectorPredicate      // #eq? filters
	matchPreds    []selectorMatchPredicate // #match? regex filters
	notMatchPreds []selectorMatchPredicate // #not-match? negated regex filters

	// scopeKind/scopeAncestry locate the @scope node when it is NOT the outer
	// match node — e.g. `(type_declaration (type_spec ...) @scope)` binds @scope
	// to the inner type_spec. Empty scopeKind (or scopeKind == outerKind) means
	// @scope is the outer node. Used to resolve the construct's source text and
	// write-back byte range to the correct node (parity with SitterWalker).
	scopeKind     string
	scopeAncestry []string
}

type selectorCapture struct {
	kind     string   // leaf node kind to match (e.g., "type_identifier")
	name     string   // capture name (e.g., "receiver")
	ancestry []string // intermediate node kinds from outer to leaf (excluding scope and leaf itself), e.g. ["parameter_list", "parameter_declaration", "pointer_type"]
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
// Builds ancestry chains for each capture so that nested type constraints
// (e.g., pointer_type > type_identifier vs bare type_identifier) are matched
// correctly against the _ast table's ID-path hierarchy.
//
// Supports #eq?, #match?, and #not-match? predicates. Captured text for
// match predicates is resolved from the _source byte ranges already populated
// by the capture loop. Other tree-sitter predicates (#not-eq?, #any-eq?,
// #is?, #is-not?) still require SitterWalker.
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
	_ = parseSExprNode(tokens, pos, nil, pattern)

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
// ancestorKinds accumulates the node-kind path from the root of the parse to
// this node's parent. The first entry is always the outerKind (scope).
// Captures record ancestry via ancestryFromKinds (skip scope, keep the rest).
// Returns the position after the closing paren.
func parseSExprNode(tokens []string, pos int, ancestorKinds []string, pattern *selectorPattern) int {
	if pos >= len(tokens) || tokens[pos] != "(" {
		return pos
	}
	pos++ // consume "("

	// Skip predicates like (#eq? ...)
	if pos < len(tokens) && strings.HasPrefix(tokens[pos], "#") {
		depth := 1
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

	// First token after "(" is the node kind
	if pos >= len(tokens) {
		return pos
	}
	nodeKind := tokens[pos]
	pos++

	// Set outer kind if this is the first node
	if pattern.outerKind == "" {
		pattern.outerKind = nodeKind
	}

	// Process children: field: labels, nested (kind ...), @captures
	for pos < len(tokens) {
		tok := tokens[pos]

		if tok == ")" {
			pos++ // consume closing paren
			break
		}

		if strings.HasSuffix(tok, ":") {
			// field: label — skip it, next token is the child
			pos++
			continue
		}

		if tok == "(" {
			if pos+1 < len(tokens) && strings.HasPrefix(tokens[pos+1], "#") {
				// Predicate — skip
				depth := 1
				pos++
				for pos < len(tokens) && depth > 0 {
					switch tokens[pos] {
					case "(":
						depth++
					case ")":
						depth--
					}
					pos++
				}
				continue
			}
			// Remember the nested node's kind before recursing
			nestedKind := ""
			if pos+1 < len(tokens) && tokens[pos+1] != "(" && tokens[pos+1] != ")" && !strings.HasPrefix(tokens[pos+1], "#") {
				nestedKind = tokens[pos+1]
			}
			// Nested node — recurse with this node's kind added to ancestry
			pos = parseSExprNode(tokens, pos, append(ancestorKinds, nodeKind), pattern)
			// Check for @capture after the nested node's closing paren
			// e.g., (identifier) @_type — the capture belongs to "identifier", not "block"
			if pos < len(tokens) && strings.HasPrefix(tokens[pos], "@") && nestedKind != "" {
				capName := tokens[pos][1:]
				pos++
				if capName != "" && capName != "scope" {
					pattern.captures = append(pattern.captures, selectorCapture{
						kind:     nestedKind,
						name:     capName,
						ancestry: ancestryFromKinds(ancestorKinds),
					})
				}
			}
			continue
		}

		if strings.HasPrefix(tok, "@") {
			capName := tok[1:]
			pos++
			if capName != "scope" && capName != "" && capName[0] != '_' {
				pattern.captures = append(pattern.captures, selectorCapture{
					kind:     nodeKind,
					name:     capName,
					ancestry: ancestryFromKinds(ancestorKinds),
				})
			} else if capName != "" && capName[0] == '_' {
				// Captures starting with _ are for #eq? predicates — still record them
				pattern.captures = append(pattern.captures, selectorCapture{
					kind: nodeKind,
					name: capName,
				})
			}
			continue
		}

		// Some other token — skip
		pos++
	}

	// Check for @capture after the closing paren (e.g., ") @scope", ") @_type")
	if pos < len(tokens) && strings.HasPrefix(tokens[pos], "@") {
		capName := tokens[pos][1:]
		pos++
		if capName == "scope" {
			// Record where @scope sits. nodeKind is the @scope node; ancestorKinds[0]
			// is always the outerKind, so the intermediate path from outer→scope is
			// ancestryFromKinds(ancestorKinds). For @scope on the outer node both are
			// the outer (scopeKind == outerKind), handled as a no-op in Query.
			pattern.scopeKind = nodeKind
			pattern.scopeAncestry = ancestryFromKinds(ancestorKinds)
		} else if capName != "" {
			pattern.captures = append(pattern.captures, selectorCapture{
				kind:     nodeKind,
				name:     capName,
				ancestry: ancestryFromKinds(ancestorKinds),
			})
		}
	}

	return pos
}

// ancestryFromKinds returns the intermediate node kinds between the scope and
// the leaf. ancestorKinds[0] is always the outerKind (scope) — skip it.
//
//	ancestorKinds=["call","arguments","call"] → ["arguments","call"]
func ancestryFromKinds(ancestorKinds []string) []string {
	if len(ancestorKinds) <= 1 {
		return nil
	}
	out := make([]string, len(ancestorKinds)-1)
	copy(out, ancestorKinds[1:])
	return out
}

// ancestryHasPrefix reports whether a capture's ancestry (relative to the outer
// node) begins with the given @scope prefix (scopeAncestry + scopeKind). When it
// does, the capture lives under the @scope node, so it can be resolved relative
// to the inner scope node with the prefix stripped — the mechanism that lets
// grouped declarations resolve each member's captures against the right inner
// node instead of the first one under the shared outer node.
func ancestryHasPrefix(ancestry, prefix []string) bool {
	if len(ancestry) < len(prefix) {
		return false
	}
	for i, p := range prefix {
		if ancestry[i] != p {
			return false
		}
	}
	return true
}

// matchAncestry checks that the path from the scope node to the leaf node
// matches the expected ancestor chain EXACTLY. The ancestry slice lists the
// intermediate node kinds from outermost to innermost (excluding the scope
// and the leaf itself).
//
// For the pointer receiver selector:
//
//	ancestry=["parameter_list", "parameter_declaration", "pointer_type"]
//	matches: .../parameter_list_0/parameter_declaration/pointer_type/type_identifier ✓
//	rejects: .../parameter_list_0/parameter_declaration/type_identifier               ✗
//
// For the value receiver selector:
//
//	ancestry=["parameter_list", "parameter_declaration"]
//	matches: .../parameter_list_0/parameter_declaration/type_identifier               ✓
//	rejects: .../parameter_list_0/parameter_declaration/pointer_type/type_identifier   ✗
//
// "Exact" means every segment in the path between scope and leaf must be
// accounted for by the ancestry chain. Extra intermediate nodes cause rejection.
func matchAncestry(pathSuffix string, ancestry []string) bool {
	segments := strings.Split(pathSuffix, "/")
	// The last segment is the leaf node itself — exclude it
	if len(segments) > 0 {
		segments = segments[:len(segments)-1]
	}

	// Strip numeric suffixes from all segments
	stripped := make([]string, len(segments))
	for i, seg := range segments {
		stripped[i] = stripNumericSuffix(seg)
	}

	// The stripped segments must match the ancestry exactly
	if len(stripped) != len(ancestry) {
		return false
	}
	for i := range ancestry {
		if stripped[i] != ancestry[i] {
			return false
		}
	}
	return true
}

// stripNumericSuffix removes a trailing _N (e.g., "parameter_list_0" → "parameter_list").
func stripNumericSuffix(s string) string {
	idx := strings.LastIndexByte(s, '_')
	if idx <= 0 {
		return s
	}
	tail := s[idx+1:]
	for _, c := range tail {
		if c < '0' || c > '9' {
			return s
		}
	}
	return s[:idx]
}

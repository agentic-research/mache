package smells

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/lloschema"
)

// What differs between the copies in a clone set.
//
// A set is functions whose syntax-node KINDS match. Everything the kinds do
// not record is free to differ: names, constants, and the operators and
// keywords between nodes, which the parse does not store as nodes at all. So
// `if x > 0` and `if x < 0` are the same structure. Merging a set into one
// helper without looking picks one copy's behaviour; this is the looking.
//
// Each copy is read as an alternating sequence: the text between leaf nodes
// (operators, keywords, punctuation), then a leaf (a name or a literal).
// Equal structure means the sequences line up position by position, so
// comparing them is a walk, not a diff.

// maxCloneDifferences caps the differences listed for one set.
const maxCloneDifferences = 25

// Difference kinds, from least to most consequential for a merge.
const (
	// diffReference is a name that is not a local of the function: a called
	// function, a field, a type or a package. The helper must take it as a
	// parameter, or the copies do different things.
	diffReference = "reference"
	// diffLiteral is a constant: a string, number or boolean.
	diffLiteral = "literal"
	// diffOperator is the text between nodes: an operator or keyword.
	diffOperator = "operator"
)

// cloneDifference is one position at which the copies of a set disagree.
type cloneDifference struct {
	Kind string `json:"kind"`
	// Values is each copy's text at the position, in member order.
	Values []cloneDiffValue `json:"values"`
}

// cloneDiffValue is one copy's text at a differing position.
type cloneDiffValue struct {
	SourceID string `json:"source_id"`
	Line     int    `json:"line"`
	Text     string `json:"text"`
}

// cloneToken is one leaf of a copy with the text that precedes it.
type cloneToken struct {
	kind string
	text string
	// gap is the source between the previous leaf and this one, comments
	// removed and whitespace collapsed.
	gap string
	// declares marks a leaf that introduces a name: the function's own, a
	// parameter, or a local.
	declares bool
	offset   int
}

// cloneCopy is one member read for comparison.
type cloneCopy struct {
	member cloneMember
	src    []byte
	tokens []cloneToken
	// tail is the gap after the last leaf.
	tail string
	// locals are the names the copy declares.
	locals map[string]bool
}

// describeDifferences fills in what differs between each set's copies. A
// set whose copies cannot be read is marked rather than reported as
// identical: no claim is made about code that was not compared.
func describeDifferences(qg graph.RefsQuerier, sets []cloneSet) {
	sources := map[string][]byte{}
	for i := range sets {
		copies, err := readCopies(qg, sets[i].Members, sources)
		if err != nil {
			sets[i].DifferencesUnavailable = err.Error()
			continue
		}
		compareCopies(&sets[i], copies)
	}
}

// readCopies reads every member's leaves, or fails if any cannot be read or
// they do not line up.
func readCopies(qg graph.RefsQuerier, members []cloneMember, sources map[string][]byte) ([]cloneCopy, error) {
	copies := make([]cloneCopy, 0, len(members))
	for _, m := range members {
		src, err := sourceBytes(qg, m.SourceID, sources)
		if err != nil {
			return nil, err
		}
		c, err := readCopy(qg, m, src)
		if err != nil {
			return nil, err
		}
		if len(copies) > 0 && len(c.tokens) != len(copies[0].tokens) {
			return nil, fmt.Errorf("copies have %d and %d leaves; cannot align them", len(copies[0].tokens), len(c.tokens))
		}
		copies = append(copies, c)
	}
	return copies, nil
}

// sourceBytes is the parsed bytes of a source file, read once per call.
func sourceBytes(qg graph.RefsQuerier, sourceID string, cache map[string][]byte) ([]byte, error) {
	if src, ok := cache[sourceID]; ok {
		return src, nil
	}
	cols, err := lloschema.SourceColumns(qg)
	if err != nil {
		return nil, fmt.Errorf("read source of %s: %w", sourceID, err)
	}
	rows, err := qg.QueryRefs("SELECT "+cols+" FROM _source WHERE id = ?", sourceID)
	if err != nil {
		return nil, fmt.Errorf("read source of %s: %w", sourceID, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, fmt.Errorf("no source stored for %s", sourceID)
	}
	var s lloschema.SourceScan
	if err := rows.Scan(s.Targets()...); err != nil {
		return nil, fmt.Errorf("read source of %s: %w", sourceID, err)
	}
	src, err := s.Bytes()
	if err != nil {
		return nil, fmt.Errorf("read source of %s: %w", sourceID, err)
	}
	cache[sourceID] = src
	return src, nil
}

// astSpan is one syntax node of a copy.
type astSpan struct {
	kind       string
	start, end int
	// children counts the nodes seen directly under this one so far.
	children int
}

// readCopy reads a member's syntax nodes in document order and reduces them
// to leaves with the text between them.
func readCopy(qg graph.RefsQuerier, m cloneMember, src []byte) (cloneCopy, error) {
	rows, err := qg.QueryRefs(`
		SELECT d.node_kind, d.start_byte, d.end_byte
		FROM v_ast r
		JOIN v_ast d ON (d.node_id = r.node_id OR (d.node_id >= r.subtree_lo AND d.node_id < r.subtree_hi))
		WHERE r.node_id = ? AND d.is_comment = 0
		ORDER BY d.start_byte, d.node_id`, m.key)
	if err != nil {
		return cloneCopy{}, fmt.Errorf("read %s:%d: %w", m.SourceID, m.Line, err)
	}
	defer func() { _ = rows.Close() }()
	var spans []astSpan
	for rows.Next() {
		var s astSpan
		if err := rows.Scan(&s.kind, &s.start, &s.end); err != nil {
			return cloneCopy{}, fmt.Errorf("read %s:%d: %w", m.SourceID, m.Line, err)
		}
		if s.start < 0 || s.end > len(src) || s.start > s.end {
			return cloneCopy{}, fmt.Errorf("%s:%d: node outside its source", m.SourceID, m.Line)
		}
		spans = append(spans, s)
	}
	if err := rows.Err(); err != nil {
		return cloneCopy{}, fmt.Errorf("read %s:%d: %w", m.SourceID, m.Line, err)
	}
	if len(spans) == 0 {
		return cloneCopy{}, fmt.Errorf("%s:%d: no syntax nodes", m.SourceID, m.Line)
	}

	c := cloneCopy{member: m, src: src, locals: map[string]bool{}}
	var stack []*astSpan
	prevEnd := spans[0].start
	for i := range spans {
		s := &spans[i]
		for len(stack) > 0 && s.start >= stack[len(stack)-1].end {
			stack = stack[:len(stack)-1]
		}
		if len(stack) > 0 {
			stack[len(stack)-1].children++
		}
		leaf := i+1 == len(spans) || spans[i+1].start >= s.end
		if !leaf {
			stack = append(stack, s)
			continue
		}
		tok := cloneToken{
			kind:     s.kind,
			text:     string(src[s.start:s.end]),
			gap:      betweenNodes(src[prevEnd:max(s.start, prevEnd)]),
			declares: declaresName(s.kind, stack),
			offset:   s.start,
		}
		if tok.declares {
			c.locals[tok.text] = true
		}
		c.tokens = append(c.tokens, tok)
		prevEnd = s.end
	}
	c.tail = betweenNodes(src[prevEnd:max(spans[0].end, prevEnd)])
	return c, nil
}

// declaresName reports whether a leaf of kind, under the ancestors in stack,
// introduces a name in a Go function: the
// function or method name, a parameter or receiver, a := or range variable,
// or a var or const.
func declaresName(kind string, stack []*astSpan) bool {
	if len(stack) == 0 {
		return false
	}
	parent := stack[len(stack)-1].kind
	switch kind {
	case "field_identifier":
		return parent == "method_declaration"
	case "identifier":
	default:
		return false
	}
	switch parent {
	case "function_declaration", "parameter_declaration", "variadic_parameter_declaration", "var_spec", "const_spec":
		return true
	case "expression_list":
		if len(stack) < 2 {
			return false
		}
		// The left-hand list of `a, b := ...` and of `for a, b := range ...`
		// is the first child of its statement.
		grand := stack[len(stack)-2]
		return (grand.kind == "short_var_declaration" || grand.kind == "range_clause") && grand.children == 1
	}
	return false
}

// betweenNodes is the source between two nodes with comments removed and
// whitespace collapsed: what is left is operators, keywords and punctuation.
// A gap never contains a string or rune literal (those are nodes), so `//`
// and `/*` in it always open a comment.
func betweenNodes(gap []byte) string {
	var out []byte
	for len(gap) > 0 {
		switch {
		case bytes.HasPrefix(gap, []byte("//")):
			end := bytes.IndexByte(gap, '\n')
			if end < 0 {
				end = len(gap)
			}
			gap = gap[end:]
		case bytes.HasPrefix(gap, []byte("/*")):
			end := bytes.Index(gap, []byte("*/"))
			if end < 0 {
				gap = nil
			} else {
				gap = gap[end+2:]
			}
			out = append(out, ' ')
		default:
			out = append(out, gap[0])
			gap = gap[1:]
		}
	}
	return strings.Join(strings.Fields(string(out)), " ")
}

// compareCopies records where the copies of set disagree. The first copy is
// the reference; a position differs when any copy's text is not what the
// reference's text becomes under that copy's renaming of locals.
func compareCopies(set *cloneSet, copies []cloneCopy) {
	ref := copies[0]
	renames := make([]map[string]string, len(copies))
	for ci := range copies {
		renames[ci] = localRenames(ref, copies[ci])
	}
	add := func(kind string, text func(c cloneCopy) (string, int)) {
		set.differenceCount++
		if len(set.Differences) >= maxCloneDifferences {
			set.DifferencesTruncated = true
			return
		}
		d := cloneDifference{Kind: kind}
		for _, c := range copies {
			t, offset := text(c)
			d.Values = append(d.Values, cloneDiffValue{SourceID: c.member.SourceID, Line: lineAt(c.src, offset), Text: t})
		}
		set.Differences = append(set.Differences, d)
	}

	for i := range ref.tokens {
		if !allEqual(copies, func(c cloneCopy) string { return c.tokens[i].gap }) {
			add(diffOperator, func(c cloneCopy) (string, int) { return c.tokens[i].gap, c.tokens[i].offset })
		}
		kind := leafDifference(ref, copies, renames, i)
		switch kind {
		case "":
		case diffRename:
			set.RenamedPositions++
		default:
			add(kind, func(c cloneCopy) (string, int) { return c.tokens[i].text, c.tokens[i].offset })
		}
	}
	if !allEqual(copies, func(c cloneCopy) string { return c.tail }) {
		last := len(ref.tokens) - 1
		add(diffOperator, func(c cloneCopy) (string, int) { return c.tail, c.tokens[last].offset })
	}
	set.IdenticalModuloRenames = set.differenceCount == 0
}

// diffRename marks a position where the copies differ only by a consistent
// renaming of a local. It is counted, not listed.
const diffRename = "rename"

// leafDifference classifies leaf i across copies: "" when every copy has
// the same text, diffRename when each has its own name for the same local,
// else the kind of difference.
func leafDifference(ref cloneCopy, copies []cloneCopy, renames []map[string]string, i int) string {
	want := ref.tokens[i]
	same, renamed := true, true
	for ci, c := range copies {
		got := c.tokens[i].text
		if got != want.text {
			same = false
		}
		// A local of the reference must appear as the name this copy gave
		// that local, and a local of this copy must stand for one.
		expected, isLocal := renames[ci][want.text]
		if !isLocal || got != expected {
			renamed = false
		}
	}
	switch {
	case same:
		return ""
	case renamed:
		return diffRename
	case isLiteralKind(want.kind):
		return diffLiteral
	default:
		return diffReference
	}
}

// localRenames maps each local of ref to the name the same local has in c,
// read off the positions where both declare a name. A local c declares under
// two different names, or two locals c gives one name, is left out, so the
// positions that use it are reported as references rather than passed as a
// rename.
func localRenames(ref, c cloneCopy) map[string]string {
	forward, backward := map[string]string{}, map[string]string{}
	conflicted := map[string]bool{}
	for i, t := range ref.tokens {
		if !t.declares || !c.tokens[i].declares {
			continue
		}
		from, to := t.text, c.tokens[i].text
		if prev, ok := forward[from]; ok && prev != to {
			conflicted[from] = true
		}
		if prev, ok := backward[to]; ok && prev != from {
			conflicted[from], conflicted[prev] = true, true
		}
		forward[from], backward[to] = to, from
	}
	for name := range conflicted {
		delete(forward, name)
	}
	return forward
}

// isLiteralKind reports whether a leaf kind is a constant in Go source.
func isLiteralKind(kind string) bool {
	switch kind {
	case "true", "false", "nil", "iota", "escape_sequence":
		return true
	}
	return strings.Contains(kind, "literal")
}

// allEqual reports whether text gives the same string for every copy.
func allEqual(copies []cloneCopy, text func(cloneCopy) string) bool {
	first := text(copies[0])
	for _, c := range copies[1:] {
		if text(c) != first {
			return false
		}
	}
	return true
}

// lineAt is the 1-based line of a byte offset in src.
func lineAt(src []byte, offset int) int {
	return 1 + bytes.Count(src[:min(max(offset, 0), len(src))], []byte("\n"))
}

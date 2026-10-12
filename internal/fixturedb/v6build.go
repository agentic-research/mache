package fixturedb

import "strings"

// nameText is a directory's or file's interned name; the root's is empty.
func nameText(n *v6Node) string {
	if n.parent == nil {
		return ""
	}
	return n.seg
}

// v6Tree builds the projection-v6 tree once per build, on first use, from
// every path the spec names, and refuses a spec leyline could not have
// produced.
func (e *emitter) v6Tree() *v6Tree {
	if e.v6 != nil {
		return e.v6
	}
	b := e.b
	t := newV6Tree()
	add := func(id string, src SourceID, start int) *v6Node {
		n, err := t.add(id, src, start)
		if err != nil {
			b.t.Fatalf("fixturedb(leyline-v6): %v", err)
		}
		return n
	}
	for _, id := range b.srcOrder {
		t.ensureFile(string(id))
	}
	for _, a := range b.ast {
		n := add(a.nodeID, a.source, a.span.StartByte)
		if n.kind != a.kind {
			b.t.Fatalf("fixturedb(leyline-v6): ASTNode %q has kind %q, but its last segment names kind %q; "+
				"v6 derives the name from the kind, so they must agree", a.nodeID, a.kind, n.kind)
		}
	}
	for _, id := range b.order {
		add(string(id), b.constructs[id].source, -1)
	}
	e.v6Sites = make([]*v6Node, len(b.refs))
	for i, r := range b.refs {
		if r.synthetic {
			e.v6Sites[i] = t.addSyntheticSite(t.byPath[string(r.from)])
			continue
		}
		e.v6Sites[i] = add(string(r.at), b.sourceOf(r.from), -1)
	}
	t.assignIDs()
	if problems := t.nameProblems(); len(problems) > 0 {
		b.t.Fatalf("fixturedb(leyline-v6): leyline suffixes a syntax node \"_k\" only when its kind repeats "+
			"among its siblings, so these ids cannot exist in a projection-v6 database:\n  %s",
			strings.Join(problems, "\n  "))
	}
	e.v6 = t
	return t
}

// interner assigns ids to names in first-use order, writing each once.
type interner struct {
	e    *emitter
	stmt string
	ids  map[string]int
}

func newInterner(e *emitter, stmt string) *interner {
	return &interner{e: e, stmt: stmt, ids: map[string]int{}}
}

func (in *interner) id(text string) int {
	if id, ok := in.ids[text]; ok {
		return id
	}
	id := len(in.ids) + 1
	in.ids[text] = id
	in.e.exec(in.stmt, id, text)
	return id
}

// langOf is a source's declared language, or "" when the spec gave none.
func (b *Builder) langOf(id SourceID) string {
	if s, ok := b.sources[id]; ok {
		return s.lang
	}
	return ""
}

// nullableSpan is a declared ASTNode's span as SQL values, all NULL when the
// node has no ASTNode declaration.
type nullableSpan struct {
	startByte, endByte, startRow, startCol, endRow, endCol any
}

func (b *Builder) spanOf(id string) nullableSpan {
	for _, a := range b.ast {
		if a.nodeID == id {
			s := a.span
			return nullableSpan{s.StartByte, s.EndByte, s.StartRow, s.StartCol, s.EndRow, s.EndCol}
		}
	}
	return nullableSpan{}
}

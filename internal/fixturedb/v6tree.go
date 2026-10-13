package fixturedb

import (
	"cmp"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

// projection-v6 has no path-string ids. A node is an integer nid, its name is
// interned (directories and files) or DERIVED (syntax nodes), and its path is
// whatever LLO's v_node_path renders from the parent chain. So a v6 fixture
// cannot write a spec's ids; it has to build the tree those ids describe and
// let the naming rule reproduce them.
//
// The layout below was read from a real `leyline parse` at v0.20.3, not from
// the schema alone:
//
//   - every directory is a nodes row with nid = -dir_id; the root is dir 1, nid
//     -1, with an empty name and no parent;
//   - every file is a row with nid = file_id<<24, parent -dir_id, its name
//     interned, and the kind of its grammar's root (source_file for Go,
//     config_file for HCL), because the file node IS that root; file ids
//     follow sorted path order;
//   - every syntax node is file_id<<24 | ordinal, numbered in preorder by
//     sibling order, with no stored name: v_node_name renders it as its raw
//     kind, plus "_k" (the k-th sibling of that kind by ord) when the parent
//     has more than one child of that kind.

// v6Node is one row of a projection-v6 nodes table.
type v6Node struct {
	path   string // the id this node must render as under v_node_path
	seg    string // the last path segment, as declared
	parent *v6Node
	kids   []*v6Node

	kind string // raw_kind; "" for directories
	k    int    // the declared "_k" suffix, or -1 for a bare name

	isDir, isFile bool
	// declared is false for nodes only implied by a deeper path (an
	// intermediate block, say) and for synthetic call sites; their names are
	// whatever the naming rule derives, so only declared names are checked.
	declared bool
	// start orders siblings when the spec gave a span; -1 when it did not.
	start int
	// seq is declaration order, the last tie-break for sibling order.
	seq int

	dirID, fileID int
	nid           int64
	ord           int
}

// v6Tree holds every node a spec implies, addressable by the path the spec
// used for it.
type v6Tree struct {
	root   *v6Node
	byPath map[string]*v6Node
	dirs   []*v6Node
	files  []*v6Node
	seq    int
}

func newV6Tree() *v6Tree {
	root := &v6Node{isDir: true, declared: true, k: -1, start: -1}
	return &v6Tree{root: root, byPath: map[string]*v6Node{"": root}}
}

// splitKind parses a syntax-node segment into its raw kind and its "_k"
// suffix, or -1 when there is none: "function_declaration_2" is
// ("function_declaration", 2), "block" is ("block", -1).
func splitKind(seg string) (string, int) {
	i := strings.LastIndexByte(seg, '_')
	if i <= 0 || i == len(seg)-1 {
		return seg, -1
	}
	k, err := strconv.Atoi(seg[i+1:])
	if err != nil || k < 0 {
		return seg, -1
	}
	return seg[:i], k
}

// add places the node a spec names at id, whose source file is src, creating
// the directories, file and intermediate syntax nodes the path passes through.
func (tr *v6Tree) add(id string, src SourceID, start int) (*v6Node, error) {
	if n, ok := tr.byPath[id]; ok {
		n.declared = true
		if start >= 0 && n.start < 0 {
			n.start = start
		}
		return n, nil
	}
	file := string(src)
	if file == "" || (id != file && !strings.HasPrefix(id, file+"/")) {
		return nil, fmt.Errorf("%q is not under its source %q; projection-v6 places every "+
			"node below a file, so the id must be <source>/<syntax path>", id, src)
	}
	fileNode := tr.ensureFile(file)
	if id == file {
		fileNode.declared = true
		return fileNode, nil
	}
	n := tr.ensureSyntax(fileNode, strings.Split(strings.TrimPrefix(id, file+"/"), "/"))
	n.declared = true
	if start >= 0 {
		n.start = start
	}
	return n, nil
}

func (tr *v6Tree) ensureFile(file string) *v6Node {
	if n, ok := tr.byPath[file]; ok {
		return n
	}
	parent := tr.root
	dir := path.Dir(file)
	if dir != "." {
		parent = tr.ensureDir(dir)
	}
	n := tr.child(parent, file, path.Base(file))
	n.isFile = true
	// Until an ASTNode declares the root's kind, assume Go's.
	n.kind = "source_file"
	tr.files = append(tr.files, n)
	return n
}

func (tr *v6Tree) ensureDir(dir string) *v6Node {
	if n, ok := tr.byPath[dir]; ok {
		return n
	}
	parent := tr.root
	if up := path.Dir(dir); up != "." {
		parent = tr.ensureDir(up)
	}
	n := tr.child(parent, dir, path.Base(dir))
	n.isDir = true
	tr.dirs = append(tr.dirs, n)
	return n
}

// ensureSyntax walks segs below parent, creating any syntax node not yet seen.
func (tr *v6Tree) ensureSyntax(parent *v6Node, segs []string) *v6Node {
	n := parent
	for _, seg := range segs {
		p := n.path + "/" + seg
		if existing, ok := tr.byPath[p]; ok {
			n = existing
			continue
		}
		n = tr.child(n, p, seg)
		n.kind, n.k = splitKind(seg)
	}
	return n
}

func (tr *v6Tree) child(parent *v6Node, p, seg string) *v6Node {
	tr.seq++
	n := &v6Node{path: p, seg: seg, parent: parent, k: -1, start: -1, seq: tr.seq}
	parent.kids = append(parent.kids, n)
	tr.byPath[p] = n
	return n
}

// addSyntheticSite gives a call site the spec left unnamed its own
// expression_statement -> call_expression chain under from's statement list,
// as a real parse has one statement per call. Its names are derived, not
// declared, so a construct with one unnamed call renders it as
// ".../expression_statement/call_expression", exactly as leyline would.
func (tr *v6Tree) addSyntheticSite(from *v6Node) *v6Node {
	list := tr.ensureSyntax(from, []string{"block", "statement_list"})
	tr.seq++
	stmt := &v6Node{
		seg: "expression_statement", kind: "expression_statement", k: -1,
		parent: list, start: -1, seq: tr.seq,
	}
	list.kids = append(list.kids, stmt)
	tr.seq++
	call := &v6Node{
		seg: "call_expression", kind: "call_expression", k: -1,
		parent: stmt, start: -1, seq: tr.seq,
	}
	stmt.kids = append(stmt.kids, call)
	return call
}

// assignIDs orders every node's children, then numbers directories, files and
// syntax nodes the way leyline does.
func (tr *v6Tree) assignIDs() {
	orderKids(tr.root)
	slices.SortFunc(tr.dirs, func(a, b *v6Node) int { return strings.Compare(a.path, b.path) })
	tr.root.dirID, tr.root.nid = 1, -1
	for i, d := range tr.dirs {
		d.dirID = i + 2
		d.nid = -int64(d.dirID)
	}
	slices.SortFunc(tr.files, func(a, b *v6Node) int { return strings.Compare(a.path, b.path) })
	for i, f := range tr.files {
		f.fileID = i + 1
		next := int64(0)
		var walk func(*v6Node)
		walk = func(n *v6Node) {
			n.nid = int64(f.fileID)<<24 | next
			next++
			for _, c := range n.kids {
				walk(c)
			}
		}
		walk(f)
	}
}

// orderKids sorts each node's children into sibling order and records it in
// ord: by span start when the spec gave one, then by kind and "_k" suffix,
// then by declaration order.
func orderKids(n *v6Node) {
	slices.SortStableFunc(n.kids, func(a, b *v6Node) int {
		as, bs := a.start, b.start
		if as < 0 {
			as = int(^uint(0) >> 1)
		}
		if bs < 0 {
			bs = int(^uint(0) >> 1)
		}
		return cmp.Or(cmp.Compare(as, bs), strings.Compare(a.kind, b.kind),
			cmp.Compare(a.k, b.k), cmp.Compare(a.seq, b.seq))
	})
	for i, c := range n.kids {
		c.ord = i
		orderKids(c)
	}
}

// derivedName is what v_node_name renders for a syntax node: its raw kind,
// plus "_k" when its parent has more than one child of that kind.
func derivedName(n *v6Node) string {
	same, rank := 0, 0
	for _, s := range n.parent.kids {
		if s.kind != n.kind {
			continue
		}
		if s.ord < n.ord {
			rank++
		}
		same++
	}
	if same > 1 {
		return n.kind + "_" + strconv.Itoa(rank)
	}
	return n.kind
}

// nameProblems lists every declared syntax node whose id the naming rule
// cannot reproduce, with the name it would render instead. A fixture that
// declares "function_declaration_0" with no sibling of that kind describes a
// path no v6 database can contain, because leyline only suffixes a kind that
// repeats.
func (tr *v6Tree) nameProblems() []string {
	var out []string
	var walk func(*v6Node)
	walk = func(n *v6Node) {
		for _, c := range n.kids {
			if c.declared && !c.isDir && !c.isFile {
				if want := derivedName(c); want != c.seg {
					out = append(out, fmt.Sprintf("%s renders as %s/%s", c.path, c.parent.path, want))
				}
			}
			walk(c)
		}
	}
	walk(tr.root)
	return out
}

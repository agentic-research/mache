package leyline

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/machehome"
)

// regionSource is a Go package whose reference sites fall into two
// communities. Every site calls one(), so the token joins them all, but the
// alpha sites also share a.one and the beta sites b.one, which binds each
// side twice as tightly as the token binds the sides to each other: community
// detection yields two regions, and one() spans them, so one restriction
// edge joins them.
const regionSource = `package p

type alpha struct{}
type beta struct{}

func (alpha) one() {}
func (beta) one() {}

func alpha1(a alpha) { a.one() }
func alpha2(a alpha) { a.one() }
func alpha3(a alpha) { a.one() }
func alpha4(a alpha) { a.one() }
func alpha5(a alpha) { a.one() }

func beta1(b beta) { b.one() }
func beta2(b beta) { b.one() }
func beta3(b beta) { b.one() }
func beta4(b beta) { b.one() }
func beta5(b beta) { b.one() }
`

// TestE2E_SheafDefect_IsZeroUnderTheRegionHashContract is the gate for
// mache-a0d487 against the pinned daemon. A topology built from a real parse,
// with region hashes folded from the members' node_hash under the parse's
// parserId and boundaries that are their XOR, reports defect 0: every boundary
// the daemon recorded is one its pre-filter finds unchanged. A second parse of
// the same source folds to the same hashes, so re-announcing a region holds
// defect at 0, and a parse of a changed member moves its region's hash, so
// announcing that one makes the daemon count the boundary. Before this fix
// the pushed hashes could never satisfy regionHash(a) XOR regionHash(b) ==
// boundary_hash, and defect was non-zero from the first push.
func TestE2E_SheafDefect_IsZeroUnderTheRegionHashContract(t *testing.T) {
	leylineBin, err := ResolveBinary(false) // before HOME isolation, as the e2e tests do
	if err != nil {
		t.Skipf("pinned leyline unavailable: %v", err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv(machehome.Env, "")

	tree := newRegionTree(t, leylineBin)
	sockPath, stop := startDaemonForE2E(t, leylineBin)
	t.Cleanup(stop)
	sock := dialWithRetry(t, sockPath, 5*time.Second)
	t.Cleanup(func() { _ = sock.Close() })
	sc := NewSheafClient(sock)

	g := tree.parse()
	refs := g.RefsMap()
	require.NotEmpty(t, refs, "the parse records references")
	cr := graph.DetectCommunities(refs, 2)
	require.GreaterOrEqual(t, len(cr.Communities), 2, "two communities: %+v", cr.Communities)
	hashes, err := regionHashes(cr, g)
	require.NoError(t, err, "every member of a real parse has a node_hash")
	edges := buildRestrictions(cr, refs, hashes)
	require.NotEmpty(t, edges, "a restriction edge between the communities")

	require.NoError(t, sc.PushTopology(cr, refs, g))
	defect := defectOf(t, sc)
	t.Logf("%d regions, %d restriction edges; defect after push: %v", len(cr.Communities), len(edges), defect)
	assert.Equal(t, 0.0, defect, "every boundary holds at topology time")

	// The same bytes parsed again fold to the same region hashes: announcing
	// one is comparable with the boundaries the daemon recorded.
	region := edges[0].A
	again, err := regionHashes(cr, tree.parse())
	require.NoError(t, err)
	require.Equal(t, hashes, again, "a second parse of the same source folds to the same hashes")
	_, err = sc.InvalidateWithStalk(region, HashHex(again[region]), nil)
	require.NoError(t, err)
	defect = defectOf(t, sc)
	t.Logf("defect after re-announcing region %d unchanged: %v", region, defect)
	assert.Equal(t, 0.0, defect, "an unchanged region's hash holds its boundaries")

	// Change one member of that region in the source, at the bytes its node
	// spans, so its path survives and its node_hash does not.
	member := cr.Communities[indexOfCommunity(t, cr, region)].Members[0]
	span := tree.editMember(g, member)
	changed, err := regionHashes(cr, tree.parse())
	require.NoError(t, err, "the edited member keeps its path")
	require.NotEqual(t, hashes[region], changed[region], "a changed member moves its region's hash")
	for id, h := range hashes {
		if id != region {
			assert.Equal(t, h, changed[id], "region %d did not change", id)
		}
	}
	_, err = sc.InvalidateWithStalk(region, HashHex(changed[region]), nil)
	require.NoError(t, err)
	defect = defectOf(t, sc)
	t.Logf("defect after announcing region %d changed (%q edited): %v", region, span, defect)
	assert.Positive(t, defect, "a changed region's boundaries are counted")
}

// regionTree is regionSource on disk, parsed by the pinned leyline as often
// as a test asks. It lives under /tmp so the daemon's socket beside it stays
// under the sun_path limit.
type regionTree struct {
	t      *testing.T
	bin    string
	dir    string
	file   string
	parses int
}

func newRegionTree(t *testing.T, bin string) *regionTree {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "llregion")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	src := filepath.Join(dir, "src")
	require.NoError(t, os.MkdirAll(src, 0o755))
	file := filepath.Join(src, "p.go")
	require.NoError(t, os.WriteFile(file, []byte(regionSource), 0o644))
	return &regionTree{t: t, bin: bin, dir: dir, file: file}
}

// parse runs the pinned leyline over the tree and opens the result as the
// graph get_communities reads.
func (r *regionTree) parse() *graph.SQLiteGraph {
	r.t.Helper()
	r.parses++
	db := filepath.Join(r.dir, fmt.Sprintf("p%d.db", r.parses))
	out, err := exec.Command(r.bin, "parse", filepath.Dir(r.file), "-o", db).CombinedOutput()
	require.NoError(r.t, err, "leyline parse: %s", out)
	g, err := graph.OpenSQLiteGraph(db, &api.Topology{}, nil)
	require.NoError(r.t, err)
	r.t.Cleanup(func() { _ = g.Close() })
	return g
}

// editMember renames the callee at member's call site, within the bytes the
// node spans, so the node's kind and path survive and its content does not.
// Returns the text that was there.
func (r *regionTree) editMember(g *graph.SQLiteGraph, member string) string {
	r.t.Helper()
	node, err := g.GetNode(member)
	require.NoError(r.t, err)
	require.NotNil(r.t, node.Origin, "a reference site has a source span")
	content, err := os.ReadFile(r.file)
	require.NoError(r.t, err)
	span := string(content[node.Origin.StartByte:node.Origin.EndByte])
	require.True(r.t, strings.HasSuffix(span, ".one()"), "the member is a call site: %q", span)
	edited := string(content[:node.Origin.StartByte]) + strings.Replace(span, "(", "Changed(", 1) +
		string(content[node.Origin.EndByte:])
	require.NoError(r.t, os.WriteFile(r.file, []byte(edited), 0o644))
	return span
}

func defectOf(t *testing.T, sc *SheafClient) float64 {
	t.Helper()
	defect, err := sc.Defect()
	require.NoError(t, err)
	return defect
}

func indexOfCommunity(t *testing.T, cr *graph.CommunityResult, id int) int {
	t.Helper()
	for i, c := range cr.Communities {
		if c.ID == id {
			return i
		}
	}
	t.Fatalf("community %d is not in %+v", id, cr.Communities)
	return -1
}

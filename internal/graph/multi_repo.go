package graph

import (
	"errors"
	"io/fs"
	"sort"
	"strings"
)

// MultiRepoGraph composes multiple Graph backends and exposes them as
// a single graph with each repo as a top-level virtual directory.
//
// Node IDs are namespaced: "repo-name/path/inside/repo". The virtual
// root ("") returns the list of registered repo names as children.
//
// Federated queries — GetCallers and GetCallees walk every registered
// repo and merge results. Each result's ID is rewritten to include
// the repo prefix so callers can distinguish which repo a hit came
// from. This implements the cheapest of the three federation
// strategies sketched in mache-iegm and the Ref pointer kind from
// ADR-0011 (docs/adr/0011-pointer-abstraction.md).
//
// MultiRepoGraph does not own its child Graphs — closing or
// invalidating MultiRepoGraph does not propagate to children. The
// caller manages backend lifecycles. Per-repo Invalidate is routed
// through the namespaced ID so a write-back on one repo doesn't
// disturb others.
type MultiRepoGraph struct {
	repos map[string]Graph
	names []string // sorted for deterministic root listings
}

// NewMultiRepoGraph builds a MultiRepoGraph from a name → backend map.
// Repo names must not contain '/'; that's the namespace separator.
func NewMultiRepoGraph(repos map[string]Graph) *MultiRepoGraph {
	names := make([]string, 0, len(repos))
	for n := range repos {
		if strings.Contains(n, "/") {
			panic("MultiRepoGraph: repo name must not contain '/': " + n)
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return &MultiRepoGraph{
		repos: repos,
		names: names,
	}
}

// splitRepoID splits "repo/inner/path" into ("repo", "inner/path").
// "" maps to ("", "") meaning the virtual root.
// "repo" maps to ("repo", "") meaning the repo's root inside its backend.
func splitRepoID(id string) (repo, inner string) {
	id = strings.TrimPrefix(id, "/")
	if id == "" {
		return "", ""
	}
	if before, after, ok := strings.Cut(id, "/"); ok {
		return before, after
	}
	return id, ""
}

func (m *MultiRepoGraph) wrapID(repo, inner string) string {
	if inner == "" {
		return repo
	}
	return repo + "/" + inner
}

// GetNode returns the node at id. The virtual root synthesizes a
// directory whose children are the registered repo names. Otherwise
// the call is dispatched to repo's backend with the inner path.
func (m *MultiRepoGraph) GetNode(id string) (*Node, error) {
	repo, inner := splitRepoID(id)
	if repo == "" {
		children := make([]string, len(m.names))
		copy(children, m.names)
		return &Node{
			ID:       "",
			Mode:     fs.ModeDir | 0o555,
			Children: children,
		}, nil
	}
	g, ok := m.repos[repo]
	if !ok {
		return nil, ErrNotFound
	}
	n, err := g.GetNode(inner)
	if err != nil {
		return nil, err
	}
	// Shallow-copy + rewrite children IDs so consumers can navigate
	// back into MultiRepoGraph without dropping the repo prefix.
	out := *n
	out.ID = m.wrapID(repo, n.ID)
	if len(n.Children) > 0 {
		wrapped := make([]string, len(n.Children))
		for i, c := range n.Children {
			wrapped[i] = m.wrapID(repo, c)
		}
		out.Children = wrapped
	}
	return &out, nil
}

// ListChildren returns the children of id. Same routing as GetNode.
func (m *MultiRepoGraph) ListChildren(id string) ([]string, error) {
	repo, inner := splitRepoID(id)
	if repo == "" {
		out := make([]string, len(m.names))
		copy(out, m.names)
		return out, nil
	}
	g, ok := m.repos[repo]
	if !ok {
		return nil, ErrNotFound
	}
	children, err := g.ListChildren(inner)
	if err != nil {
		return nil, err
	}
	wrapped := make([]string, len(children))
	for i, c := range children {
		wrapped[i] = m.wrapID(repo, c)
	}
	return wrapped, nil
}

// ListChildStats returns stat snapshots. Same routing as ListChildren.
func (m *MultiRepoGraph) ListChildStats(id string) ([]NodeStat, error) {
	repo, inner := splitRepoID(id)
	if repo == "" {
		out := make([]NodeStat, len(m.names))
		for i, name := range m.names {
			out[i] = NodeStat{
				ID:    name,
				IsDir: true,
			}
		}
		return out, nil
	}
	g, ok := m.repos[repo]
	if !ok {
		return nil, ErrNotFound
	}
	stats, err := g.ListChildStats(inner)
	if err != nil {
		return nil, err
	}
	for i := range stats {
		stats[i].ID = m.wrapID(repo, stats[i].ID)
	}
	return stats, nil
}

// ReadContent dispatches to the repo holding id.
func (m *MultiRepoGraph) ReadContent(id string, buf []byte, offset int64) (int, error) {
	repo, inner := splitRepoID(id)
	if repo == "" {
		return 0, ErrNotFound
	}
	g, ok := m.repos[repo]
	if !ok {
		return 0, ErrNotFound
	}
	return g.ReadContent(inner, buf, offset)
}

// GetCallers walks every registered repo and merges results. Each
// returned node's ID is rewritten to include the repo prefix so
// callers can distinguish which repo a hit came from.
//
// Errors on individual repos are tolerated — the merged result is
// returned even if some repos failed. The first error is surfaced
// only when no callers were found anywhere.
func (m *MultiRepoGraph) GetCallers(token string) ([]*Node, error) {
	var out []*Node
	var firstErr error
	for _, name := range m.names {
		callers, err := m.repos[name].GetCallers(token)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, c := range callers {
			wrapped := *c
			wrapped.ID = m.wrapID(name, c.ID)
			out = append(out, &wrapped)
		}
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// GetCallees dispatches to the repo holding id. Cross-repo callees
// (where the called function lives in a different repo) are not
// resolved by this step — that requires an inter-repo defs index,
// filed as future work in mache-iegm.
func (m *MultiRepoGraph) GetCallees(id string) ([]*Node, error) {
	repo, inner := splitRepoID(id)
	if repo == "" {
		return nil, ErrNotFound
	}
	g, ok := m.repos[repo]
	if !ok {
		return nil, ErrNotFound
	}
	callees, err := g.GetCallees(inner)
	if err != nil {
		return nil, err
	}
	// Rewrite IDs back into the multi-repo namespace.
	for i, c := range callees {
		wrapped := *c
		wrapped.ID = m.wrapID(repo, c.ID)
		callees[i] = &wrapped
	}
	return callees, nil
}

// Invalidate routes to the repo holding id. The virtual root and
// unknown repos are no-ops.
func (m *MultiRepoGraph) Invalidate(id string) {
	repo, inner := splitRepoID(id)
	if repo == "" {
		return
	}
	if g, ok := m.repos[repo]; ok {
		g.Invalidate(inner)
	}
}

// Act dispatches to the repo holding id. The virtual root rejects
// all actions — there's nothing to act on at the registry level.
func (m *MultiRepoGraph) Act(id, action, payload string) (*ActionResult, error) {
	repo, inner := splitRepoID(id)
	if repo == "" {
		return nil, errors.New("MultiRepoGraph: cannot Act on virtual root")
	}
	g, ok := m.repos[repo]
	if !ok {
		return nil, ErrNotFound
	}
	return g.Act(inner, action, payload)
}

// Repos returns the registered repo names in deterministic order.
// Useful for diagnostics and listing.
func (m *MultiRepoGraph) Repos() []string {
	out := make([]string, len(m.names))
	copy(out, m.names)
	return out
}

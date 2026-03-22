package graph

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// QuotientGraph compresses a full node graph into a diagram-scale object by
// collapsing nodes into equivalence classes (communities) and aggregating
// inter-class edges. The quotient G/P is uniquely determined by the partition P.
type QuotientGraph struct {
	Classes []Class
	Edges   []QuotientEdge
	ClassOf map[string]int // node ID → class index
}

// Class is an equivalence class of nodes (a community collapsed into one unit).
type Class struct {
	ID        int
	Label     string   // emergent: most-referenced token among members
	Members   []string // constituent node IDs
	InternalW float64  // total internal edge weight (density measure)
}

// QuotientEdge is an aggregated edge between two classes.
type QuotientEdge struct {
	From         int                // class index (invariant: From < To)
	To           int                // class index
	Weight       float64            // aggregated cross-class coupling
	Tokens       []string           // boundary tokens creating this edge
	TokenWeights map[string]float64 // per-token contribution to Weight
}

// ComputeQuotient builds a QuotientGraph from a community partition and refs.
//
// The algorithm mirrors buildRestrictions in sheaf.go: for each ref token
// referenced by nodes in multiple communities, create an inter-class edge
// with weight = product of member counts (bipartite projection weight).
//
// Class labels are derived from the most-referenced token among class members.
// Ties are broken lexicographically for determinism.
func ComputeQuotient(cr *CommunityResult, refs map[string][]string) *QuotientGraph {
	q := &QuotientGraph{
		ClassOf: make(map[string]int),
	}

	if cr == nil || len(cr.Communities) == 0 {
		return q
	}

	// Build classes from communities.
	q.Classes = make([]Class, len(cr.Communities))
	for i, comm := range cr.Communities {
		members := make([]string, len(comm.Members))
		copy(members, comm.Members)

		q.Classes[i] = Class{
			ID:      i,
			Members: members,
		}
		for _, m := range members {
			q.ClassOf[m] = i
		}
	}

	if refs == nil {
		deriveLabels(q, refs)
		return q
	}

	// Compute inter-class edges and internal weights.
	type edgeKey struct{ a, b int }
	edges := map[edgeKey]float64{}
	edgeTokens := map[edgeKey][]string{}
	edgeTokenWeights := map[edgeKey]map[string]float64{} // per-token contribution

	for token, nodeIDs := range refs {
		// Count members per class that reference this token.
		classCount := map[int]int{}
		for _, nid := range nodeIDs {
			if cid, ok := q.ClassOf[nid]; ok {
				classCount[cid]++
			}
		}

		// Internal weight: for each class with count >= 2 referencing this token,
		// add C(count, 2) = count*(count-1)/2 internal pairs.
		for cid, count := range classCount {
			if count >= 2 {
				q.Classes[cid].InternalW += float64(count*(count-1)) / 2
			}
		}

		if len(classCount) < 2 {
			continue
		}

		// Cross-class edges between all pairs.
		classes := make([]int, 0, len(classCount))
		for c := range classCount {
			classes = append(classes, c)
		}
		sort.Ints(classes)

		for i := 0; i < len(classes); i++ {
			for j := i + 1; j < len(classes); j++ {
				key := edgeKey{classes[i], classes[j]}
				w := float64(classCount[classes[i]] * classCount[classes[j]])
				edges[key] += w
				edgeTokens[key] = append(edgeTokens[key], token)
				if edgeTokenWeights[key] == nil {
					edgeTokenWeights[key] = make(map[string]float64)
				}
				edgeTokenWeights[key][token] = w
			}
		}
	}

	// Collect edges, sorted deterministically.
	q.Edges = make([]QuotientEdge, 0, len(edges))
	for key, weight := range edges {
		tokens := edgeTokens[key]
		sort.Strings(tokens)

		q.Edges = append(q.Edges, QuotientEdge{
			From:         key.a,
			To:           key.b,
			Weight:       weight,
			Tokens:       tokens,
			TokenWeights: edgeTokenWeights[key],
		})
	}
	sort.Slice(q.Edges, func(i, j int) bool {
		if q.Edges[i].From != q.Edges[j].From {
			return q.Edges[i].From < q.Edges[j].From
		}
		return q.Edges[i].To < q.Edges[j].To
	})

	deriveLabels(q, refs)
	return q
}

// deriveLabels sets each class's label to the most distinctive token among
// its members using TF-IDF scoring. Raw reference count alone would select
// ubiquitous tokens (e.g. "Errorf", "Sprintf") that appear in every community.
// Instead, we score each token by TF * IDF where TF = count_in_class and
// IDF = log(num_classes / classes_containing_token). This selects tokens that
// are both frequent within a class and concentrated in it. Ties broken
// lexicographically for determinism.
func deriveLabels(q *QuotientGraph, refs map[string][]string) {
	if refs == nil {
		for i := range q.Classes {
			q.Classes[i].Label = fmt.Sprintf("cluster_%d", i)
		}
		return
	}

	// Phase 1: compute per-class counts AND class spread for each token.
	type tokenInfo struct {
		perClass   map[int]int // class ID -> count of members referencing this token
		numClasses int         // how many distinct classes reference this token
	}
	tokenStats := make(map[string]*tokenInfo)
	nClasses := len(q.Classes)

	for token, nodeIDs := range refs {
		info := &tokenInfo{perClass: make(map[int]int)}
		for _, nid := range nodeIDs {
			if cid, ok := q.ClassOf[nid]; ok {
				info.perClass[cid]++
			}
		}
		info.numClasses = len(info.perClass)
		if info.numClasses > 0 {
			tokenStats[token] = info
		}
	}

	// Phase 2: for each class, find the token with the highest TF-IDF score.
	//
	// Score = count_in_class * log(num_classes / classes_containing_token)
	//
	// This is standard TF-IDF:
	//   - TF (count_in_class): favors tokens widely used within the community.
	//   - IDF (log(N/df)): penalizes tokens appearing in many communities.
	//
	// A ubiquitous token like "Errorf" that appears in 10 of 11 classes gets
	// IDF = log(11/10) = 0.095, so even with TF=10 its score is only 0.95.
	// A domain token like "MemoryStore" appearing in 3 of 11 classes gets
	// IDF = log(11/3) = 1.30, so with TF=11 its score is 14.3.
	// This correctly selects domain-specific labels over mechanism tokens.
	type bestToken struct {
		token string
		score float64
		count int // raw count, used as tiebreaker
	}
	classBest := make([]bestToken, len(q.Classes))

	// When there is only one class, IDF is always log(1/1)=0 for every token,
	// so fall back to raw count (TF only). With multiple classes, use TF-IDF.
	useIDF := nClasses > 1

	for token, info := range tokenStats {
		var idf float64
		if useIDF {
			idf = math.Log(float64(nClasses) / float64(info.numClasses))
		} else {
			idf = 1.0 // degenerate: treat all tokens equally in IDF dimension
		}
		for cid, count := range info.perClass {
			score := float64(count) * idf
			best := &classBest[cid]
			if score > best.score ||
				(score == best.score && count > best.count) ||
				(score == best.score && count == best.count && token < best.token) {
				best.token = token
				best.score = score
				best.count = count
			}
		}
	}

	for i := range q.Classes {
		if classBest[i].token != "" {
			q.Classes[i].Label = classBest[i].token
		} else {
			q.Classes[i].Label = fmt.Sprintf("cluster_%d", i)
		}
	}
}

// Mermaid renders the quotient graph as mermaid syntax.
//
// Each Class becomes a subgraph (or a plain node if it has a single member).
// Each QuotientEdge becomes an arrow. Edge annotations show boundary tokens
// that contribute above the mean weight for that edge.
// The layout parameter maps to mermaid direction: "TD", "LR", "BT", "RL".
func (q *QuotientGraph) Mermaid(layout string) string {
	if layout == "" {
		layout = "TD"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "graph %s\n", layout)

	// Render classes.
	for _, c := range q.Classes {
		if len(c.Members) == 0 {
			continue
		}
		if len(c.Members) == 1 {
			// Single member: plain node.
			fmt.Fprintf(&b, "    %s[\"%s\"]\n", classNodeID(c.ID), c.Label)
			continue
		}
		// Multi-member: subgraph.
		fmt.Fprintf(&b, "    subgraph %s[\"%s\"]\n", classNodeID(c.ID), c.Label)
		for _, m := range c.Members {
			fmt.Fprintf(&b, "        %s\n", sanitizeMermaidID(m))
		}
		fmt.Fprintf(&b, "    end\n")
	}

	// Render edges.
	for _, e := range q.Edges {
		fromID := classNodeID(e.From)
		toID := classNodeID(e.To)

		label := edgeLabel(e)
		if label != "" {
			fmt.Fprintf(&b, "    %s -->|%s| %s\n", fromID, label, toID)
		} else {
			fmt.Fprintf(&b, "    %s --> %s\n", fromID, toID)
		}
	}

	return b.String()
}

// edgeLabel selects boundary tokens to show on an edge annotation.
// When per-token weights are available, shows tokens that contribute above the
// mean weight — the data determines what is prominent, not a hardcoded threshold.
// Falls back to showing all tokens when weights are absent.
func edgeLabel(e QuotientEdge) string {
	if len(e.Tokens) == 0 {
		return ""
	}
	if len(e.Tokens) == 1 {
		return e.Tokens[0]
	}

	// If per-token weights are available, select tokens above the mean.
	if len(e.TokenWeights) > 0 {
		mean := e.Weight / float64(len(e.Tokens))
		var prominent []string
		for _, t := range e.Tokens {
			if e.TokenWeights[t] >= mean {
				prominent = append(prominent, t)
			}
		}
		if len(prominent) == 0 {
			// All tokens below mean (shouldn't happen mathematically, but be safe).
			prominent = e.Tokens
		}
		if len(prominent) == len(e.Tokens) {
			return strings.Join(prominent, ", ")
		}
		return fmt.Sprintf("%s (+%d more)", strings.Join(prominent, ", "), len(e.Tokens)-len(prominent))
	}

	// Fallback: no per-token weights (manually constructed QuotientEdge).
	return strings.Join(e.Tokens, ", ")
}

func classNodeID(id int) string {
	return fmt.Sprintf("C%d", id)
}

func sanitizeMermaidID(id string) string {
	r := strings.NewReplacer("/", "_", ".", "_", "-", "_", " ", "_")
	return r.Replace(id)
}

package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/lattice"
	sitter "github.com/smacker/go-tree-sitter"
)

// inferFromTreeSitterFile reads a source file, parses it with tree-sitter,
// and infers a topology schema. Returns an error if parsing fails.
func inferFromTreeSitterFile(inf *lattice.Inferrer, path string, lang *sitter.Language, label string) (*api.Topology, error) {
	log.Printf("Inferring schema from %s source via Tree-sitter...", label)
	start := time.Now()
	defer func() { log.Printf("Schema inference done in %v", time.Since(start)) }()

	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, parseErr := parser.ParseCtx(context.Background(), nil, content)
	if parseErr != nil { // coverage:ignore — tree-sitter ParseCtx surfaces three error classes (per convertTSTree in smacker/go-tree-sitter): ctx cancellation, ErrNoLanguage, ErrOperationLimit. ErrNoLanguage is ruled out by SetLanguage(lang) above; ErrOperationLimit is unreachable because mache never calls SetOperationLimit anywhere in cmd/. Context cancellation requires a propagated cancel that this synchronous call path doesn't construct.
		return nil, fmt.Errorf("tree-sitter parse failed for %s: %w", path, parseErr) // coverage:ignore
	} // coverage:ignore
	if tree == nil { // coverage:ignore — defensive nil check; ParseCtx returns nil tree only when it also returns an error, already handled above
		return nil, fmt.Errorf("tree-sitter returned nil tree for %s", path) // coverage:ignore
	} // coverage:ignore
	return inf.InferFromTreeSitter(tree.RootNode())
}

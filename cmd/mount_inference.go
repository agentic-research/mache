package cmd

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/lang"
	"github.com/agentic-research/mache/internal/lattice"
)

// inferFromSourceFile infers a topology schema for a single source file by
// leyline-parsing it into an _ast database and running pure-Go FCA inference
// over that — no in-process tree-sitter (CGO-free inference path).
//
// leyline parse only accepts directories, so the file is copied into a
// throwaway temp dir first. Inference consumes only AST shape, never file
// paths, so the copy is lossless for this purpose.
func inferFromSourceFile(inf *lattice.Inferrer, path string, l *lang.Language) (*api.Topology, error) {
	log.Printf("Inferring schema from %s source via leyline _ast...", l.DisplayName)
	start := time.Now()
	defer func() { log.Printf("Schema inference done in %v", time.Since(start)) }()

	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	tmpDir, err := os.MkdirTemp("", "mache-infer-")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	if err := os.WriteFile(filepath.Join(tmpDir, filepath.Base(path)), content, 0o600); err != nil {
		return nil, fmt.Errorf("stage %s for leyline parse: %w", path, err)
	}

	astDB, cleanup, err := autoInvokeLeylineParse(tmpDir)
	if err != nil {
		return nil, fmt.Errorf("leyline parse %s: %w", path, err)
	}
	defer cleanup()

	savedLang := inf.Config.Language
	inf.Config.Language = l.Name
	defer func() { inf.Config.Language = savedLang }()

	topo, err := inf.InferFromASTDB(astDB)
	if err != nil {
		return nil, err
	}
	// The pinned leyline parses ~11 of the 28 registry languages; for the
	// rest the parse yields zero _ast rows and inference degrades to an
	// empty topology — the file then mounts under _project_files/. The old
	// in-process tree-sitter path could infer these languages, so say
	// LOUDLY why the schema is empty rather than silently degrading
	// (review finding on mache-73b885).
	if topo == nil || len(topo.Nodes) == 0 {
		log.Printf("WARNING: leyline produced no usable AST for %s (%s) — "+
			"no schema inferred; the pinned leyline may not have a grammar for this "+
			"language yet, so the file will project under _project_files/",
			path, l.DisplayName)
	}
	return topo, nil
}

// Package lang is the single source of truth for all supported languages.
// Add a language to Registry and every consumer (ingestion, watcher,
// write-back validation, schema presets, project detection) picks it up
// automatically — zero duplication, zero drift.
package lang

import (
	"path/filepath"
	"sort"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/bash"
	treec "github.com/smacker/go-tree-sitter/c"
	"github.com/smacker/go-tree-sitter/cpp"
	"github.com/smacker/go-tree-sitter/csharp"
	"github.com/smacker/go-tree-sitter/css"
	"github.com/smacker/go-tree-sitter/cue"
	"github.com/smacker/go-tree-sitter/dockerfile"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/groovy"
	"github.com/smacker/go-tree-sitter/hcl"
	"github.com/smacker/go-tree-sitter/html"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/kotlin"
	"github.com/smacker/go-tree-sitter/lua"
	markdownts "github.com/smacker/go-tree-sitter/markdown/tree-sitter-markdown"
	"github.com/smacker/go-tree-sitter/php"
	"github.com/smacker/go-tree-sitter/protobuf"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/ruby"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/scala"
	"github.com/smacker/go-tree-sitter/sql"
	"github.com/smacker/go-tree-sitter/swift"
	"github.com/smacker/go-tree-sitter/toml"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
	"github.com/smacker/go-tree-sitter/yaml"

	"github.com/agentic-research/mache/internal/treesitter/elixir"
)

// Language is the single source of truth for a supported language.
type Language struct {
	Name          string                                   // canonical name: "go", "python", "terraform"
	Aliases       []string                                 // backward-compat names: e.g. "hcl" for terraform
	DisplayName   string                                   // human label: "Go", "Python", "HCL/Terraform"
	Extensions    []string                                 // file extensions including dot: ".go", ".py"
	Grammar       func() *sitter.Language                  // tree-sitter grammar factory (lazy, CGO-safe)
	PresetSchema  string                                   // embedded schema key (empty = no preset)
	SentinelFiles []string                                 // files that identify a project: "go.mod", "Cargo.toml"
	EnrichNode    func(n *sitter.Node, rec map[string]any) // language-specific AST enrichment (nil for most)
}

// Registry is the authoritative list of all supported languages.
// Add a language here and every consumer picks it up automatically.
var Registry = []Language{
	{Name: "go", DisplayName: "Go", Extensions: []string{".go"}, Grammar: golang.GetLanguage, PresetSchema: "go", SentinelFiles: []string{"go.mod", "go.sum"}},
	{Name: "python", DisplayName: "Python", Extensions: []string{".py"}, Grammar: python.GetLanguage, PresetSchema: "python", SentinelFiles: []string{"pyproject.toml", "requirements.txt", "setup.py"}},
	{Name: "javascript", DisplayName: "JavaScript", Extensions: []string{".js"}, Grammar: javascript.GetLanguage, PresetSchema: "javascript", SentinelFiles: []string{"package.json"}},
	{Name: "typescript", DisplayName: "TypeScript", Extensions: []string{".ts", ".tsx"}, Grammar: typescript.GetLanguage, PresetSchema: "typescript"},
	{Name: "sql", DisplayName: "SQL", Extensions: []string{".sql"}, Grammar: sql.GetLanguage, PresetSchema: "sql"},
	{Name: "terraform", Aliases: []string{"hcl"}, DisplayName: "HCL/Terraform", Extensions: []string{".tf", ".hcl"}, Grammar: hcl.GetLanguage, PresetSchema: "terraform", EnrichNode: enrichHCLNode},
	{Name: "yaml", DisplayName: "YAML", Extensions: []string{".yaml", ".yml"}, Grammar: yaml.GetLanguage, PresetSchema: "yaml"},
	{Name: "rust", DisplayName: "Rust", Extensions: []string{".rs"}, Grammar: rust.GetLanguage, PresetSchema: "rust", SentinelFiles: []string{"Cargo.toml"}},
	{Name: "toml", DisplayName: "TOML", Extensions: []string{".toml"}, Grammar: toml.GetLanguage, PresetSchema: "toml"},
	{Name: "elixir", DisplayName: "Elixir", Extensions: []string{".ex", ".exs"}, Grammar: elixir.GetLanguage, PresetSchema: "elixir", SentinelFiles: []string{"mix.exs"}},
	{Name: "java", DisplayName: "Java", Extensions: []string{".java"}, Grammar: java.GetLanguage, PresetSchema: "java", SentinelFiles: []string{"pom.xml", "build.gradle"}},
	{Name: "c", DisplayName: "C", Extensions: []string{".c", ".h"}, Grammar: treec.GetLanguage, PresetSchema: "c"},
	{Name: "cpp", DisplayName: "C++", Extensions: []string{".cpp", ".cc", ".cxx", ".hpp", ".hxx", ".hh"}, Grammar: cpp.GetLanguage, PresetSchema: "cpp", SentinelFiles: []string{"CMakeLists.txt"}},
	{Name: "ruby", DisplayName: "Ruby", Extensions: []string{".rb"}, Grammar: ruby.GetLanguage, PresetSchema: "ruby", SentinelFiles: []string{"Gemfile"}},
	{Name: "php", DisplayName: "PHP", Extensions: []string{".php"}, Grammar: php.GetLanguage, PresetSchema: "php", SentinelFiles: []string{"composer.json"}},
	{Name: "kotlin", DisplayName: "Kotlin", Extensions: []string{".kt", ".kts"}, Grammar: kotlin.GetLanguage, PresetSchema: "kotlin"},
	{Name: "swift", DisplayName: "Swift", Extensions: []string{".swift"}, Grammar: swift.GetLanguage, PresetSchema: "swift", SentinelFiles: []string{"Package.swift"}},
	{Name: "scala", DisplayName: "Scala", Extensions: []string{".scala", ".sc"}, Grammar: scala.GetLanguage, PresetSchema: "scala", SentinelFiles: []string{"build.sbt"}},
	// --- Added grammars (no preset schemas yet) ---
	{Name: "bash", DisplayName: "Bash", Extensions: []string{".sh", ".bash"}, Grammar: bash.GetLanguage},
	{Name: "csharp", DisplayName: "C#", Extensions: []string{".cs"}, Grammar: csharp.GetLanguage},
	{Name: "css", DisplayName: "CSS", Extensions: []string{".css"}, Grammar: css.GetLanguage},
	{Name: "cue", DisplayName: "CUE", Extensions: []string{".cue"}, Grammar: cue.GetLanguage},
	{Name: "dockerfile", DisplayName: "Dockerfile", Extensions: []string{".dockerfile"}, Grammar: dockerfile.GetLanguage, SentinelFiles: []string{"Dockerfile"}},
	{Name: "groovy", DisplayName: "Groovy", Extensions: []string{".groovy"}, Grammar: groovy.GetLanguage, SentinelFiles: []string{"Jenkinsfile"}},
	{Name: "html", DisplayName: "HTML", Extensions: []string{".html", ".htm"}, Grammar: html.GetLanguage},
	{Name: "lua", DisplayName: "Lua", Extensions: []string{".lua"}, Grammar: lua.GetLanguage},
	{Name: "markdown", DisplayName: "Markdown", Extensions: []string{".md", ".markdown"}, Grammar: markdownts.GetLanguage},
	{Name: "protobuf", DisplayName: "Protocol Buffers", Extensions: []string{".proto"}, Grammar: protobuf.GetLanguage},
}

// Derived indexes — built once at init, never mutated.
var (
	byExt  map[string]*Language
	byName map[string]*Language
	srcSet map[string]bool // all extensions + .json
)

func init() {
	byExt = make(map[string]*Language, 32)
	byName = make(map[string]*Language, len(Registry))
	srcSet = make(map[string]bool, 32)

	for i := range Registry {
		l := &Registry[i]
		byName[l.Name] = l
		for _, alias := range l.Aliases {
			byName[alias] = l // backward compat: ForName("hcl") → terraform
		}
		for _, ext := range l.Extensions {
			byExt[ext] = l
			srcSet[ext] = true
		}
	}
	// Data format extensions are source files but not tree-sitter languages.
	srcSet[".json"] = true
}

// ForExt returns the language for a file extension (including dot), or nil.
// Case-insensitive to handle ".Go", ".PY" etc.
func ForExt(ext string) *Language {
	return byExt[strings.ToLower(ext)]
}

// ForName returns the language by canonical name or alias, or nil.
func ForName(name string) *Language {
	return byName[name]
}

// ForPath returns the language for a file path (by extension), or nil.
func ForPath(path string) *Language {
	return byExt[strings.ToLower(filepath.Ext(path))]
}

// IsSourceExt returns true if the extension is a recognized source file
// (tree-sitter languages + .json).
func IsSourceExt(ext string) bool {
	return srcSet[strings.ToLower(ext)]
}

// Extensions returns all recognized file extensions in sorted order.
func Extensions() []string {
	out := make([]string, 0, len(srcSet))
	for ext := range srcSet {
		out = append(out, ext)
	}
	sort.Strings(out)
	return out
}

// enrichHCLNode adds synthetic attributes for HCL blocks so FCA groups
// blocks with consistent structure. Moved here from ingest/language.go
// to keep the registry self-contained.
func enrichHCLNode(n *sitter.Node, rec map[string]any) {
	if n.Type() != "block" {
		return
	}

	var (
		hasIdentifier  bool
		stringLitCount int
		hasBody        bool
	)

	count := int(n.ChildCount())
	for i := range count {
		child := n.Child(i)
		if child == nil || !child.IsNamed() {
			continue
		}

		switch child.Type() {
		case "identifier":
			hasIdentifier = true
		case "string_lit":
			stringLitCount++
		case "body":
			hasBody = true
		}
	}

	if hasIdentifier && stringLitCount > 0 && hasBody {
		rec["type"] = "hcl_container"
		rec["has_name"] = true
		rec["field_name_type"] = "string_lit"
		rec["has_body"] = true
		rec["field_body_type"] = "body"
	}
}

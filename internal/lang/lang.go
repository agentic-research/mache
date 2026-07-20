// Package lang is the single source of truth for all supported languages.
// Add a language to Registry and every consumer (ingestion, watcher,
// write-back validation, schema presets, project detection) picks it up
// automatically — zero duplication, zero drift.
package lang

import (
	"path/filepath"
	"sort"
	"strings"
)

// Language is the single source of truth for a supported language.
//
// Grammar factories were removed in ADR-0012 step 4: mache no longer parses
// source in-process (CGO tree-sitter is gone). ley-line parses source into an
// `_ast` db and mache projects it, so the registry only needs identity and
// routing metadata — names, extensions, preset schema, sentinel files.
type Language struct {
	Name          string   // canonical name: "go", "python", "terraform"
	Aliases       []string // backward-compat names: e.g. "hcl" for terraform
	DisplayName   string   // human label: "Go", "Python", "HCL/Terraform"
	Extensions    []string // file extensions including dot: ".go", ".py"
	PresetSchema  string   // embedded schema key (empty = no preset)
	SentinelFiles []string // files that identify a project: "go.mod", "Cargo.toml"
}

// Registry is the authoritative list of all supported languages.
// Add a language here and every consumer picks it up automatically.
var Registry = []Language{
	{Name: "go", DisplayName: "Go", Extensions: []string{".go"}, PresetSchema: "go", SentinelFiles: []string{"go.mod", "go.sum"}},
	{Name: "python", DisplayName: "Python", Extensions: []string{".py"}, PresetSchema: "python", SentinelFiles: []string{"pyproject.toml", "requirements.txt", "setup.py"}},
	{Name: "javascript", DisplayName: "JavaScript", Extensions: []string{".js"}, PresetSchema: "javascript", SentinelFiles: []string{"package.json"}},
	{Name: "typescript", DisplayName: "TypeScript", Extensions: []string{".ts", ".tsx"}, PresetSchema: "typescript"},
	{Name: "sql", DisplayName: "SQL", Extensions: []string{".sql"}, PresetSchema: "sql"},
	{Name: "terraform", Aliases: []string{"hcl"}, DisplayName: "HCL/Terraform", Extensions: []string{".tf", ".hcl"}, PresetSchema: "terraform"},
	{Name: "yaml", DisplayName: "YAML", Extensions: []string{".yaml", ".yml"}, PresetSchema: "yaml"},
	{Name: "rust", DisplayName: "Rust", Extensions: []string{".rs"}, PresetSchema: "rust", SentinelFiles: []string{"Cargo.toml"}},
	{Name: "toml", DisplayName: "TOML", Extensions: []string{".toml"}, PresetSchema: "toml"},
	{Name: "elixir", DisplayName: "Elixir", Extensions: []string{".ex", ".exs"}, PresetSchema: "elixir", SentinelFiles: []string{"mix.exs"}},
	{Name: "java", DisplayName: "Java", Extensions: []string{".java"}, PresetSchema: "java", SentinelFiles: []string{"pom.xml", "build.gradle"}},
	{Name: "c", DisplayName: "C", Extensions: []string{".c", ".h"}, PresetSchema: "c"},
	{Name: "cpp", DisplayName: "C++", Extensions: []string{".cpp", ".cc", ".cxx", ".hpp", ".hxx", ".hh"}, PresetSchema: "cpp", SentinelFiles: []string{"CMakeLists.txt"}},
	{Name: "ruby", DisplayName: "Ruby", Extensions: []string{".rb"}, PresetSchema: "ruby", SentinelFiles: []string{"Gemfile"}},
	{Name: "php", DisplayName: "PHP", Extensions: []string{".php"}, PresetSchema: "php", SentinelFiles: []string{"composer.json"}},
	{Name: "kotlin", DisplayName: "Kotlin", Extensions: []string{".kt", ".kts"}, PresetSchema: "kotlin"},
	{Name: "swift", DisplayName: "Swift", Extensions: []string{".swift"}, PresetSchema: "swift", SentinelFiles: []string{"Package.swift"}},
	{Name: "scala", DisplayName: "Scala", Extensions: []string{".scala", ".sc"}, PresetSchema: "scala", SentinelFiles: []string{"build.sbt"}},
	// --- Added languages (preset schemas live alongside the core set) ---
	{Name: "bash", DisplayName: "Bash", Extensions: []string{".sh", ".bash"}, PresetSchema: "bash"},
	{Name: "csharp", DisplayName: "C#", Extensions: []string{".cs"}, PresetSchema: "csharp"},
	{Name: "css", DisplayName: "CSS", Extensions: []string{".css"}, PresetSchema: "css"},
	{Name: "cue", DisplayName: "CUE", Extensions: []string{".cue"}, PresetSchema: "cue"},
	{Name: "dockerfile", DisplayName: "Dockerfile", Extensions: []string{".dockerfile"}, PresetSchema: "dockerfile", SentinelFiles: []string{"Dockerfile"}},
	{Name: "groovy", DisplayName: "Groovy", Extensions: []string{".groovy"}, PresetSchema: "groovy", SentinelFiles: []string{"Jenkinsfile"}},
	{Name: "html", DisplayName: "HTML", Extensions: []string{".html", ".htm"}, PresetSchema: "html"},
	{Name: "lua", DisplayName: "Lua", Extensions: []string{".lua"}, PresetSchema: "lua"},
	{Name: "markdown", DisplayName: "Markdown", Extensions: []string{".md", ".markdown"}, PresetSchema: "markdown"},
	{Name: "protobuf", DisplayName: "Protocol Buffers", Extensions: []string{".proto"}, PresetSchema: "protobuf"},
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

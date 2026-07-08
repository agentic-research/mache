// Package lint provides static preflight analysis of mache's Taskfile.
//
// # The defect it catches
//
// A "gate" task (verify / audit / sign / lint / check / validate / smells /
// release / image / build …) that invokes an external binary with NO preflight
// guard is dangerous: a MISSING tool turns the gate into a cryptic failure —
// or, when the invocation is swallowed (`… || true`, a script wrapper), a
// silent no-op that reads as a false PASS (the classic "cargo-deny missing →
// supply-chain-check silently passes" trap).
//
// # The model (declaration-based, no heuristic verdict)
//
// Rather than heuristically infer "is this gate healthy", CheckTaskfile
// statically enforces that every external binary a gate task invokes is backed
// by one of go-task's NATIVE guard mechanisms:
//
//  1. a task-level `preconditions:` entry — `- sh: command -v <tool>` (with a
//     `msg:` install line);
//  2. an INLINE guard in the task's cmds — `command -v` / `which` / `type` /
//     `hash <tool>` (e.g. the flamegraphs task's `if ! command -v …`);
//  3. provisioning in the same task — `go install …/<tool>@…` (e.g. fmt:check
//     installs gofumpt);
//  4. an artifact-existence assertion — a standalone `test -f`/`test -x` /
//     `[ -f ]`/`[ -x ]` (the capnp false-PASS case: capnp can exit 0 on a
//     plugin error, so the task asserts its output actually exists).
//
// The pass / fail / not-ran ternary then falls out of go-task natively — a
// failed precondition blocks the task with a loud `msg` and a non-zero exit, so
// a missing tool can never be a silent no-op. The lint's only job is to prove,
// statically, that the guard EXISTS.
//
// # Parse surface
//
// The Taskfile is decoded straight into `github.com/go-task/task/v3/taskfile/ast`
// via `go.yaml.in/yaml/v3` (the ast package's own YAML library). We deliberately
// do NOT use `taskfile.Reader`: the Reader drags in go-getter → the AWS SDK,
// GCP, gRPC and OpenTelemetry (95 indirect modules vs 12 for the ast package
// alone) to support remote-include resolution we do not need. `Cmd.Cmd` holds
// the literal, un-expanded shell string — exactly right for static analysis.
package lint

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/go-task/task/v3/taskfile/ast"
	"go.yaml.in/yaml/v3"
	"mvdan.cc/sh/v3/syntax"
)

// Gap is a gate task that invokes an external binary with no preflight guard.
type Gap struct {
	Task string // the gate task's name
	Tool string // the unguarded external binary
	Cmd  string // the command line the binary was found in (first line)
}

// gateKeywords are the whole-word signals (matched case-insensitively against
// tokens of a task's name and Desc) that classify a task as a GATE — a task
// whose green/red verdict is load-bearing. Anything without one of these is
// treated as a convenience task and skipped.
var gateKeywords = map[string]struct{}{
	"verify": {}, "gate": {}, "check": {}, "lint": {}, "audit": {},
	"sign": {}, "sbom": {}, "cosign": {}, "supply-chain": {}, "validate": {},
	"smells": {}, "ci": {}, "release": {}, "publish": {}, "image": {},
	"build": {}, "sha-pin": {},
}

// safeBinaries are tokens that are NEVER external-tool gaps: shell builtins,
// coreutils, and the Go/git/bash toolchain — the ambient shell surface a
// Taskfile always has. Everything else, including container/network tooling
// (docker, curl, wget), is treated as an external tool that a gate task must
// guard, so a missing one yields a loud, actionable `msg` rather than a cryptic
// failure.
var safeBinaries = map[string]struct{}{
	// shell builtins / keywords
	":": {}, ".": {}, "[": {}, "[[": {}, "]]": {}, "test": {}, "true": {},
	"false": {}, "echo": {}, "printf": {}, "read": {}, "cd": {}, "pwd": {},
	"export": {}, "unset": {}, "local": {}, "set": {}, "shift": {}, "eval": {},
	"exec": {}, "exit": {}, "return": {}, "trap": {}, "wait": {}, "source": {},
	"alias": {}, "type": {}, "command": {}, "hash": {}, "umask": {}, "times": {},
	"if": {}, "then": {}, "else": {}, "elif": {}, "fi": {}, "for": {},
	"while": {}, "until": {}, "do": {}, "done": {}, "case": {}, "esac": {},
	"in": {}, "function": {}, "time": {}, "getopts": {}, "ulimit": {},
	// coreutils / ubiquitous userland
	"cat": {}, "ls": {}, "cp": {}, "mv": {}, "rm": {}, "rmdir": {}, "mkdir": {},
	"touch": {}, "ln": {}, "chmod": {}, "chown": {}, "chgrp": {}, "head": {},
	"tail": {}, "sort": {}, "uniq": {}, "wc": {}, "cut": {}, "tr": {}, "sed": {},
	"awk": {}, "gawk": {}, "grep": {}, "egrep": {}, "fgrep": {}, "find": {},
	"xargs": {}, "tee": {}, "diff": {}, "cmp": {}, "comm": {}, "join": {},
	"paste": {}, "split": {}, "sleep": {}, "env": {}, "date": {}, "seq": {},
	"basename": {}, "dirname": {}, "realpath": {}, "readlink": {}, "dd": {},
	"od": {}, "hexdump": {}, "xxd": {}, "tar": {}, "gzip": {}, "gunzip": {},
	"zcat": {}, "bzip2": {}, "cksum": {}, "sha256sum": {}, "sha1sum": {},
	"md5sum": {}, "mktemp": {}, "truncate": {}, "stat": {}, "yes": {}, "tac": {},
	"nl": {}, "fold": {}, "column": {}, "tput": {}, "uname": {}, "id": {},
	"whoami": {}, "hostname": {}, "arch": {}, "which": {},
	// language / SCM toolchain
	"go": {}, "git": {}, "bash": {}, "sh": {}, "zsh": {}, "make": {},
}

// guardRe extracts the tool name from an inline existence check.
var guardRe = regexp.MustCompile(`\b(?:command\s+-v|which|type|hash)\s+([^\s"';|&]+)`)

// goInstallRe extracts the installed tool basename from `go install <path>@ver`.
var goInstallRe = regexp.MustCompile(`\bgo\s+install\s+([^\s]+)`)

// CheckTaskfile parses the Taskfile at path and returns the preflight gaps —
// gate tasks that invoke an external binary with no guard — sorted by
// (Task, Tool) for deterministic output.
func CheckTaskfile(path string) ([]Gap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Taskfile: %w", err)
	}
	var tf ast.Taskfile
	if err := yaml.Unmarshal(data, &tf); err != nil {
		return nil, fmt.Errorf("parse Taskfile %s: %w", path, err)
	}

	var gaps []Gap
	// Iterate in declaration order (All(nil)); the output is sorted below, so
	// the traversal order does not affect determinism.
	for name, task := range tf.Tasks.All(nil) {
		if !isGate(name, task.Desc) {
			continue
		}
		guarded := collectGuards(task)
		// A standalone artifact-existence assertion (`test -f <out>` with no
		// `|| tool` fallback) mitigates the silent-success class for every
		// tool in the task — see the capnp case in the package doc.
		artifactGuarded := hasArtifactAssertion(task)

		seen := map[string]struct{}{} // dedup (task, tool)
		for _, c := range task.Cmds {
			if c.Task != "" || len(c.Platforms) > 0 {
				// task-ref (composition) or platform-scoped cmd (e.g. darwin
				// codesign) — not a portable external-binary invocation.
				continue
			}
			for _, bin := range externalBinaries(c.Cmd) {
				if _, ok := guarded[bin]; ok {
					continue
				}
				if artifactGuarded {
					continue
				}
				if _, ok := seen[bin]; ok {
					continue
				}
				seen[bin] = struct{}{}
				gaps = append(gaps, Gap{Task: name, Tool: bin, Cmd: firstLine(c.Cmd)})
			}
		}
	}

	sort.Slice(gaps, func(i, j int) bool {
		if gaps[i].Task != gaps[j].Task {
			return gaps[i].Task < gaps[j].Task
		}
		return gaps[i].Tool < gaps[j].Tool
	})
	return gaps, nil
}

// isGate reports whether a task's name or Desc carries a gate keyword as a
// whole word. Unmatched tasks are convenience tasks and are skipped.
func isGate(name, desc string) bool {
	for _, tok := range tokenize(name + " " + desc) {
		if _, ok := gateKeywords[tok]; ok {
			return true
		}
	}
	return false
}

// tokenize lowercases s and splits it into alphanumeric words, preserving the
// hyphen so compound keywords ("supply-chain", "sha-pin") match, and splitting
// task-name separators (":", ".", whitespace).
func tokenize(s string) []string {
	s = strings.ToLower(s)
	fields := strings.FieldsFunc(s, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z':
			return false
		case r >= '0' && r <= '9':
			return false
		case r == '-':
			return false
		default:
			return true
		}
	})
	return fields
}

// collectGuards returns the set of tool names guarded within a task via
// preconditions, inline existence checks, or `go install`.
func collectGuards(task *ast.Task) map[string]struct{} {
	guarded := map[string]struct{}{}
	add := func(tool string) {
		tool = strings.Trim(tool, `"'`)
		if tool != "" {
			guarded[baseName(tool)] = struct{}{}
		}
	}
	for _, p := range task.Preconditions {
		for _, m := range guardRe.FindAllStringSubmatch(p.Sh, -1) {
			add(m[1])
		}
	}
	for _, c := range task.Cmds {
		if c.Task != "" {
			continue
		}
		for _, m := range guardRe.FindAllStringSubmatch(c.Cmd, -1) {
			add(m[1])
		}
		for _, m := range goInstallRe.FindAllStringSubmatch(c.Cmd, -1) {
			// go install mvdan.cc/gofumpt@v0.9.2 → gofumpt
			add(baseName(stripVersion(m[1])))
		}
	}
	return guarded
}

// hasArtifactAssertion reports whether the task contains a standalone
// `test -f`/`test -x`/`[ -f ]`/`[ -x ]` assertion. A `test … || tool` compound
// (as in the image task's key regeneration) parses as a BinaryCmd, not a bare
// CallExpr statement, so it is correctly excluded — it is a conditional run,
// not an assertion.
func hasArtifactAssertion(task *ast.Task) bool {
	found := false
	for _, c := range task.Cmds {
		if c.Task != "" {
			continue
		}
		f, err := parseShell(c.Cmd)
		if err != nil {
			continue
		}
		// Only TOP-LEVEL statements count: a bare `test -f out` is an
		// assertion, whereas `test -f key || tool` parses as a BinaryCmd whose
		// left operand is a nested statement — that is a conditional run, not
		// an assertion, so it must not mark the task artifact-guarded.
		for _, stmt := range f.Stmts {
			call, ok := stmt.Cmd.(*syntax.CallExpr)
			if !ok || len(call.Args) == 0 {
				continue
			}
			head, ok := wordText(c.Cmd, call.Args[0])
			if !ok || (head != "test" && head != "[") {
				continue
			}
			for _, a := range call.Args[1:] {
				if t, ok := wordText(c.Cmd, a); ok && (t == "-f" || t == "-x") {
					found = true
				}
			}
		}
	}
	return found
}

// externalBinaries returns the distinct command heads of a shell line that are
// external binaries — not safe builtins/coreutils/toolchain, not the mache
// binary, not a variable-expanded command. It uses a real POSIX-shell parser
// (mvdan.cc/sh) so pipes inside quotes, case patterns, redirections, and
// assignment prefixes never masquerade as command names.
func externalBinaries(cmd string) []string {
	f, err := parseShell(cmd)
	if err != nil {
		return nil // unparseable shell — conservatively report nothing
	}
	var out []string
	seen := map[string]struct{}{}
	syntax.Walk(f, func(n syntax.Node) bool {
		call, ok := n.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true // assignment-only or non-command node
		}
		word, ok := wordText(cmd, call.Args[0])
		if !ok || word == "" {
			return true // command name is a variable/expansion — unresolvable
		}
		if isSafeToken(word) {
			return true
		}
		bin := baseName(word)
		if _, dup := seen[bin]; dup {
			return true
		}
		seen[bin] = struct{}{}
		out = append(out, bin)
		return true
	})
	return out
}

// parseShell parses a shell command string into a syntax tree.
func parseShell(cmd string) (*syntax.File, error) {
	return syntax.NewParser().Parse(strings.NewReader(cmd), "")
}

// wordText returns the raw source text of a shell word. It reports false when
// the word contains a parameter or command expansion (`$VAR`, `$(…)`), which
// makes the command name unresolvable for static analysis.
func wordText(src string, w *syntax.Word) (string, bool) {
	raw := src[w.Pos().Offset():w.End().Offset()]
	if strings.ContainsAny(raw, "$`") {
		return "", false
	}
	return raw, true
}

// isSafeToken reports whether word is a non-gap token: a safe binary, the mache
// binary (via BINARY_NAME or a bin/mache path), or a bare template variable.
func isSafeToken(word string) bool {
	w := strings.TrimPrefix(word, "./")
	if strings.HasPrefix(w, "{{") { // template var, incl. {{.BINARY_NAME}}
		return true
	}
	if strings.Contains(w, "bin/mache") || strings.Contains(w, "BINARY_NAME") {
		return true
	}
	if _, ok := safeBinaries[baseName(w)]; ok {
		return true
	}
	return false
}

// baseName returns the final path element of a command word.
func baseName(word string) string {
	if i := strings.LastIndexByte(word, '/'); i >= 0 {
		return word[i+1:]
	}
	return word
}

// stripVersion drops an `@version` suffix from a go-install path.
func stripVersion(path string) string {
	if i := strings.IndexByte(path, '@'); i >= 0 {
		return path[:i]
	}
	return path
}

// firstLine returns the first line of s, for compact gap reporting.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

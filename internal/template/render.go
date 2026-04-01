// Package template provides mache's schema template rendering functions.
//
// This package is pure Go with no CGO dependencies. It was extracted from
// internal/ingest to break the transitive dependency chain:
//
//	graph.SQLiteGraph → ingest.RenderTemplate → internal/lang → tree-sitter (CGO)
//
// After extraction the chain is:
//
//	graph.SQLiteGraph → template.RenderTemplate (pure Go)
package template

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"sync"
	"text/template"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// Funcs is the standard set of template functions available in all mache schemas.
var Funcs = template.FuncMap{
	"json": func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("<json error: %v>", err)
		}
		return string(b)
	},
	"first": func(v any) any {
		switch s := v.(type) {
		case []any:
			if len(s) > 0 {
				return s[0]
			}
		}
		return nil
	},
	// unquote strips Go string quotes: {{unquote .path}} → cobra from "cobra".
	// Tree-sitter captures of interpreted_string_literal include surrounding quotes.
	"unquote": func(s string) string {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
		return s
	},
	// slice extracts a substring: {{slice .someField 4 8}} → characters [4:8].
	// Used for temporal sharding: {{slice .item.cve.id 4 8}} → "2024" from "CVE-2024-0001".
	"slice": func(s string, start, end int) string {
		if start < 0 {
			start = 0
		}
		if end > len(s) {
			end = len(s)
		}
		if start >= end {
			return ""
		}
		return s[start:end]
	},
	// replace all occurrences: {{replace .name ":" " "}} → "alpine 3.18" from "alpine:3.18".
	"replace": func(s, old, new string) string {
		return strings.ReplaceAll(s, old, new)
	},
	// lower: {{lower .name}} → "rhel" from "RHEL".
	"lower": func(s string) string {
		return strings.ToLower(s)
	},
	// upper: {{upper .name}} → "DEBIAN" from "debian".
	"upper": func(s string) string {
		return strings.ToUpper(s)
	},
	// title: {{title .name}} → "Amazon Linux" from "amazon linux".
	"title": cases.Title(language.Und).String,
	// split: {{index (split .id ":") 0}} → "alpine" from "alpine:3.18".
	"split": func(s, sep string) []string {
		return strings.Split(s, sep)
	},
	// join: {{join ", " .parts}} or pipeline {{split .s ":" | join ", "}}.
	// sep is first so the piped value (slice) arrives as the last arg.
	// Accepts both []string and []any (JSON-parsed slices).
	"join": func(sep string, parts any) string {
		switch v := parts.(type) {
		case []string:
			return strings.Join(v, sep)
		case []any:
			strs := make([]string, len(v))
			for i, elem := range v {
				strs[i] = fmt.Sprintf("%v", elem)
			}
			return strings.Join(strs, sep)
		default:
			return fmt.Sprintf("%v", parts)
		}
	},
	// hasPrefix: {{if hasPrefix .s "CVE"}}...{{end}}.
	"hasPrefix": strings.HasPrefix,
	// hasSuffix: {{if hasSuffix .s ".go"}}...{{end}}.
	"hasSuffix": strings.HasSuffix,
	// trimPrefix: {{trimPrefix .s "pkg/"}} → "auth/login.go" from "pkg/auth/login.go".
	"trimPrefix": strings.TrimPrefix,
	// trimSuffix: {{trimSuffix .s ".go"}} → "main" from "main.go".
	"trimSuffix": strings.TrimSuffix,
	// dict: construct a map from key-value pairs. Errors on odd arg count.
	// {{dict "PkgName" .name "Severity" 4 | json}} → {"PkgName":"curl","Severity":4}
	"dict": func(pairs ...any) (map[string]any, error) {
		if len(pairs)%2 != 0 {
			return nil, fmt.Errorf("dict requires even number of args, got %d", len(pairs))
		}
		m := make(map[string]any, len(pairs)/2)
		for i := 0; i < len(pairs); i += 2 {
			m[fmt.Sprint(pairs[i])] = pairs[i+1]
		}
		return m, nil
	},
	// lookup: key-value enum mapping. Last odd arg is default.
	// {{lookup .Severity "Critical" 4 "High" 3 "Medium" 2 "Low" 1 0}}
	"lookup": func(val any, pairs ...any) any {
		s := fmt.Sprint(val)
		for i := 0; i+1 < len(pairs); i += 2 {
			if fmt.Sprint(pairs[i]) == s {
				return pairs[i+1]
			}
		}
		if len(pairs)%2 == 1 {
			return pairs[len(pairs)-1]
		}
		return ""
	},
	// default: return fallback when value is nil or empty string.
	// {{default .name "unknown"}}
	"default": func(val, fallback any) any {
		if val == nil {
			return fallback
		}
		if s, ok := val.(string); ok && s == "" {
			return fallback
		}
		return val
	},
	// dig: safely navigate nested maps/slices by dot-separated path.
	// Returns "" if any intermediate key is missing, nil, or out of bounds.
	// Supports map string keys and integer indices for slices.
	// {{dig "item.Vulnerability.NamespaceName" .}} → "alpine:3.18" or ""
	// {{dig "item.affected.0.package.ecosystem" .}} → "AlmaLinux:8" or ""
	"dig": func(path string, obj any) string {
		parts := strings.Split(path, ".")
		current := obj
		for _, part := range parts {
			if current == nil || part == "" {
				return ""
			}
			switch v := current.(type) {
			case map[string]any:
				val, ok := v[part]
				if !ok {
					return ""
				}
				current = val
			case []any:
				idx := 0
				for _, c := range part {
					if c < '0' || c > '9' {
						return "" // not a valid index
					}
					idx = idx*10 + int(c-'0')
				}
				if idx >= len(v) {
					return ""
				}
				current = v[idx]
			default:
				return ""
			}
		}
		if current == nil {
			return ""
		}
		return fmt.Sprint(current)
	},
}

// cache stores parsed templates keyed by their source string.
// template.Template.Execute is safe for concurrent use (Go docs guarantee this),
// so a shared cache with sync.Map is correct. Each caller uses its own bytes.Buffer.
var cache sync.Map // template string → *template.Template

// bufPool reuses bytes.Buffer across Render calls to reduce heap allocations.
// At 323K records with ~5 templates each, this avoids ~1.6M small allocs.
var bufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// Render renders a Go text/template with the standard mache template functions.
// Parsed templates are cached — repeated calls with the same template string skip parsing.
func Render(tmpl string, values map[string]any) (string, error) {
	var t *template.Template
	if cached, ok := cache.Load(tmpl); ok {
		t = cached.(*template.Template)
	} else {
		var err error
		t, err = template.New("").Funcs(Funcs).Parse(tmpl)
		if err != nil {
			return "", err
		}
		cache.Store(tmpl, t)
	}
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)
	if err := t.Execute(buf, values); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// RenderWithFuncs renders a Go text/template with the standard mache
// template functions plus additional per-engine functions (e.g., {{diagram}}).
// Templates are cached in the provided cache; the caller must ensure the
// extraFuncs map is stable for the cache's lifetime.
func RenderWithFuncs(tmpl string, values map[string]any, extraFuncs template.FuncMap, c *sync.Map) (string, error) {
	var t *template.Template
	if cached, ok := c.Load(tmpl); ok {
		t = cached.(*template.Template)
	} else {
		merged := make(template.FuncMap, len(Funcs)+len(extraFuncs))
		maps.Copy(merged, Funcs)
		maps.Copy(merged, extraFuncs)
		var err error
		t, err = template.New("").Funcs(merged).Parse(tmpl)
		if err != nil {
			return "", err
		}
		c.Store(tmpl, t)
	}
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)
	if err := t.Execute(buf, values); err != nil {
		return "", err
	}
	return buf.String(), nil
}

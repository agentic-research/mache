package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-cmp/cmp"
)

func writeAnswers(out string, plan []call, answers map[string][]answer) error {
	for side, as := range answers {
		dir := filepath.Join(out, side)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for i, a := range as {
			b, err := json.MarshalIndent(struct {
				Call   call   `json:"call"`
				Answer answer `json:"answer"`
			}{plan[i], a}, "", "  ")
			if err != nil {
				return err
			}
			name := fmt.Sprintf("%03d-%s.json", i, plan[i].Tool)
			if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeReport writes report.md and returns how many calls differ.
func writeReport(out, base, head, root string, plan []call, answers map[string][]answer, notes []string) (int, error) {
	f, err := os.Create(filepath.Join(out, "report.md"))
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	return writeDiffReport(f, base, head, root, plan, answers, notes)
}

func writeDiffReport(w io.Writer, base, head, root string, plan []call, answers map[string][]answer, notes []string) (int, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Release parity\n\n- base: `%s`\n- head: `%s`\n- corpus: `%s`\n- schema: `%s`\n- calls: %d\n\n", base, head, root, schemaLabel(), len(plan))
	if len(notes) > 0 {
		b.WriteString("## Base release leaks (not fatal: the gate judges the head)\n\n```\n")
		b.WriteString(strings.Join(notes, "\n\n"))
		b.WriteString("\n```\n\n")
	}
	b.WriteString("Diff lines read `-` base, `+` head.\n\n")
	changed := 0
	for i, c := range plan {
		ba, ha := answers["base"][i], answers["head"][i]
		args, _ := json.Marshal(c.Args)
		switch {
		case ba.Missing && ha.Missing:
			fmt.Fprintf(&b, "## %03d `%s` %s: not registered by either binary\n\n", i, c.Tool, args)
			changed++
		case ba.Missing:
			fmt.Fprintf(&b, "## %03d `%s` %s: NEW in head\n\n", i, c.Tool, args)
			changed++
		case ha.Missing:
			fmt.Fprintf(&b, "## %03d `%s` %s: REMOVED in head\n\n", i, c.Tool, args)
			changed++
		default:
			d := cmp.Diff(ba, ha)
			if d == "" {
				continue
			}
			changed++
			fmt.Fprintf(&b, "## %03d `%s` %s\n\n```diff\n%s```\n\n", i, c.Tool, args, d)
		}
	}
	if changed == 0 {
		b.WriteString("Every answer matches.\n")
	}
	_, err := io.WriteString(w, b.String())
	return changed, err
}

func schemaLabel() string {
	if schema == "" {
		return "raw projection"
	}
	return schema
}

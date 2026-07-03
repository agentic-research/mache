package cmd

import (
	_ "embed"
	"io"

	machetmpl "github.com/agentic-research/mache/internal/template"
)

//go:embed schemas/sarif.tmpl
var sarifTemplate string

// sarifLevel maps a rule's effective severity to a SARIF 2.1.0 level.
// SARIF levels are note/warning/error/none; mache rules resolve to
// warn|error via Effective() (off rules never produce findings), so
// error -> "error", everything else -> "warning".
func sarifLevel(r *SmellRule) string {
	if r.Effective() == SeverityError {
		return "error"
	}
	return "warning"
}

// buildSARIFDoc assembles a SARIF 2.1.0 document as a values map for the
// mache template engine. driver.rules[] holds each rule that produced
// findings; results[] is one entry per finding. source_id paths are
// relativized against baselineRoot so URIs are repo-relative and GitHub
// code-scanning resolves them against the checked-out tree. SARIF regions
// are 1-based, so line/column are floored at 1.
func buildSARIFDoc(results []ruleRunResult, baselineRoot string) map[string]any {
	rules := make([]map[string]any, 0, len(results))
	out := make([]map[string]any, 0)
	for _, rr := range results {
		if len(rr.findings) == 0 {
			continue
		}
		level := sarifLevel(rr.rule)
		rules = append(rules, map[string]any{
			"id":                   rr.rule.ID,
			"shortDescription":     map[string]any{"text": rr.rule.Description},
			"defaultConfiguration": map[string]any{"level": level},
		})
		for _, f := range relativizeFindings(rr.findings, baselineRoot) {
			out = append(out, map[string]any{
				"ruleId":  rr.rule.ID,
				"level":   level,
				"message": map[string]any{"text": rr.rule.Description},
				"locations": []map[string]any{{
					"physicalLocation": map[string]any{
						"artifactLocation": map[string]any{"uri": f.SourceID},
						"region": map[string]any{
							"startLine":   max(f.Line, 1),
							"startColumn": max(f.Column, 1),
						},
					},
				}},
			})
		}
	}
	return map[string]any{
		"runs": []map[string]any{{
			"tool": map[string]any{
				"driver": map[string]any{
					"name":           "mache find-smells",
					"informationUri": "https://github.com/agentic-research/mache",
					"rules":          rules,
				},
			},
			"results": out,
		}},
	}
}

// renderSARIF writes the SARIF document for all rule results to w.
func renderSARIF(w io.Writer, results []ruleRunResult, baselineRoot string) error {
	rendered, err := machetmpl.Render(sarifTemplate, buildSARIFDoc(results, baselineRoot))
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, rendered)
	return err
}

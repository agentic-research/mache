package mcpharness

import (
	"encoding/json"
	"errors"
)

// Call is one tool call of a plan: what release parity replays against two
// binaries, and what mcp-probe makes against a running server.
type Call struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// ParsePlan decodes a plan, a JSON array of calls. An empty plan is an error:
// it would check nothing and look like a pass.
func ParsePlan(b []byte) ([]Call, error) {
	var plan []Call
	if err := json.Unmarshal(b, &plan); err != nil {
		return nil, err
	}
	if len(plan) == 0 {
		return nil, errors.New("plan is empty")
	}
	return plan, nil
}

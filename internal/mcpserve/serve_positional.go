package mcpserve

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/fsutil"
	"github.com/agentic-research/mache/internal/projcfg"
	"github.com/agentic-research/mache/internal/schemainfer"
	publicschema "github.com/agentic-research/mache/schema"
)

// resolveArgSource serves the server's positional argument, with --schema,
// else an inferred schema for a directory, else an empty one.
func (lg *lazyGraph) resolveArgSource() (dataSource string, schema *api.Topology, err error) {
	dataSource = lg.args[0]

	if serveSchema != "" {
		resolved, err := projcfg.ResolveSchema(anchorSchemaFlag(serveSchema), fsutil.DirectoryOrParent(lg.resolvedBasePath()))
		if err != nil {
			return "", nil, fmt.Errorf("resolve schema: %w", err)
		}
		schema = resolved
	} else if filepath.Ext(dataSource) != ".db" {
		info, err := os.Stat(dataSource)
		if err == nil && info.IsDir() {
			schema, err = schemainfer.InferDirSchema(dataSource)
			if err != nil {
				return "", nil, fmt.Errorf("auto-detect schema: %w", err)
			}
		} else {
			schema = &api.Topology{Version: api.SchemaVersion}
		}
	} else {
		schema = &api.Topology{Version: api.SchemaVersion}
	}
	return dataSource, schema, nil
}

// anchorSchemaFlag checks the working directory for an explicitly supplied
// positional-source schema. Presets and paths relative to the session base
// are handled by ResolveSchema.
func anchorSchemaFlag(ref string) string {
	if ref == "" || publicschema.IsPreset(ref) || filepath.IsAbs(ref) {
		return ref
	}
	info, err := os.Stat(ref)
	if err != nil || info.IsDir() {
		return ref
	}
	abs, err := filepath.Abs(ref)
	if err != nil {
		return ref
	}
	return abs
}

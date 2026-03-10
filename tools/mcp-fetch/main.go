// mcp-fetch fetches MCP server entries from the official registry
// and stores them in a SQLite database compatible with mache.
//
// Usage:
//
//	go run ./tools/mcp-fetch -o mcp-registry.db
//	mache --schema examples/mcp-registry-schema.json mcp-registry.db /tmp/mcp
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

const (
	registryAPI = "https://registry.modelcontextprotocol.io/v0/servers"
	batchSize   = 100
)

// registryResponse is the paginated response from the MCP registry API.
type registryResponse struct {
	Servers  []json.RawMessage `json:"servers"`
	Metadata struct {
		NextCursor string `json:"nextCursor"`
		Count      int    `json:"count"`
	} `json:"metadata"`
}

// serverEntry extracts the server name for use as the record ID.
type serverEntry struct {
	Server struct {
		Name string `json:"name"`
	} `json:"server"`
}

func main() {
	outPath := flag.String("o", "mcp-registry.db", "Output SQLite database path")
	flag.Parse()

	if err := run(*outPath); err != nil {
		log.Fatal(err)
	}
}

func run(outPath string) error {
	// Remove existing DB to start fresh
	_ = os.Remove(outPath)

	db, err := sql.Open("sqlite", outPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE results (
			id TEXT PRIMARY KEY,
			record TEXT NOT NULL
		)
	`)
	if err != nil {
		return fmt.Errorf("create table: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	stmt, err := tx.Prepare("INSERT OR REPLACE INTO results (id, record) VALUES (?, ?)")
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	total := 0
	cursor := ""

	for {
		url := fmt.Sprintf("%s?limit=%d", registryAPI, batchSize)
		if cursor != "" {
			url += "&cursor=" + cursor
		}

		resp, err := http.Get(url) //nolint:gosec // registry URL is hardcoded
		if err != nil {
			return fmt.Errorf("fetch %s: %w", url, err)
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return fmt.Errorf("read body: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("registry returned %d: %s", resp.StatusCode, string(body))
		}

		var page registryResponse
		if err := json.Unmarshal(body, &page); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}

		for _, raw := range page.Servers {
			// Extract server name for ID
			var entry serverEntry
			if err := json.Unmarshal(raw, &entry); err != nil {
				log.Printf("skip unparseable entry: %v", err)
				continue
			}
			name := entry.Server.Name
			if name == "" {
				log.Printf("skip entry with empty name")
				continue
			}

			// Normalize: flatten the nested structure into the venturi-style
			// {schema, identifier, item} format that mache schemas expect.
			var full map[string]any
			if err := json.Unmarshal(raw, &full); err != nil {
				log.Printf("skip %s: %v", name, err)
				continue
			}

			record := map[string]any{
				"schema":     "mcp",
				"identifier": name,
				"item":       normalizeEntry(full),
			}

			recJSON, err := json.Marshal(record)
			if err != nil {
				log.Printf("skip %s: marshal: %v", name, err)
				continue
			}

			if _, err := stmt.Exec(name, string(recJSON)); err != nil {
				return fmt.Errorf("insert %s: %w", name, err)
			}
			total++
		}

		fmt.Printf("\rFetched %d servers...", total)

		if page.Metadata.NextCursor == "" || len(page.Servers) == 0 {
			break
		}
		cursor = page.Metadata.NextCursor
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	fmt.Printf("\rFetched %d servers. Saved to %s\n", total, outPath)
	return nil
}

// normalizeEntry flattens the registry's nested structure into a clean
// record that mache's JSONPath selectors can access without special characters.
func normalizeEntry(raw map[string]any) map[string]any {
	server, _ := raw["server"].(map[string]any)
	if server == nil {
		return raw
	}

	// Split qualified name "com.anthropic/claude-code" into namespace + short name
	fullName, _ := server["name"].(string)
	namespace := fullName
	shortName := fullName
	if idx := strings.Index(fullName, "/"); idx >= 0 {
		namespace = fullName[:idx]
		shortName = fullName[idx+1:]
	}

	item := map[string]any{
		"name":        fullName,
		"namespace":   namespace,
		"shortName":   shortName,
		"description": server["description"],
		"version":     server["version"],
		"websiteUrl":  server["websiteUrl"],
	}

	// Flatten repository
	if repo, ok := server["repository"].(map[string]any); ok {
		item["repository"] = repo["url"]
	}

	// Flatten remotes (transport endpoints)
	if remotes, ok := server["remotes"].([]any); ok {
		item["remotes"] = remotes
	}

	// Flatten packages
	if packages, ok := server["packages"].([]any); ok {
		item["packages"] = packages
	}

	// Flatten registry metadata
	if meta, ok := raw["_meta"].(map[string]any); ok {
		for _, v := range meta {
			if officialMeta, ok := v.(map[string]any); ok {
				item["status"] = officialMeta["status"]
				item["publishedAt"] = officialMeta["publishedAt"]
				item["updatedAt"] = officialMeta["updatedAt"]
			}
		}
	}

	return item
}

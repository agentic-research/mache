// notion-fetch queries a Notion database via the Notion API and stores
// pages as JSON records in a SQLite database compatible with mache.
//
// Usage:
//
//	export NOTION_TOKEN="ntn_..."
//	go run ./tools/notion-fetch -db <database-id> -o notion.db
//	mache --schema examples/notion-schema.json notion.db /tmp/notion
//
// The database ID can be found in the Notion URL:
//
//	https://www.notion.so/<workspace>/<database-id>?v=...
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

const notionAPI = "https://api.notion.com/v1"

func main() {
	dbID := flag.String("db", "", "Notion database ID (required)")
	outPath := flag.String("o", "notion.db", "Output SQLite database path")
	token := flag.String("token", "", "Notion API token (or set NOTION_TOKEN env var)")
	flag.Parse()

	if *dbID == "" {
		log.Fatal("required: -db <database-id>")
	}

	apiToken := *token
	if apiToken == "" {
		apiToken = os.Getenv("NOTION_TOKEN")
	}
	if apiToken == "" {
		log.Fatal("set NOTION_TOKEN env var or pass -token flag")
	}

	if err := run(*dbID, *outPath, apiToken); err != nil {
		log.Fatal(err)
	}
}

func run(dbID, outPath, token string) error {
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
		body := map[string]any{"page_size": 100}
		if cursor != "" {
			body["start_cursor"] = cursor
		}

		bodyJSON, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}

		url := fmt.Sprintf("%s/databases/%s/query", notionAPI, dbID)
		req, err := http.NewRequest("POST", url, strings.NewReader(string(bodyJSON)))
		if err != nil {
			return fmt.Errorf("new request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Notion-Version", "2022-06-28")
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("fetch: %w", err)
		}

		respBody, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return fmt.Errorf("read body: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("notion returned %d: %s", resp.StatusCode, string(respBody))
		}

		var page queryResponse
		if err := json.Unmarshal(respBody, &page); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}

		for _, result := range page.Results {
			id, item := normalizePage(result)
			if id == "" {
				log.Printf("skip page with no title")
				continue
			}

			record := map[string]any{
				"schema":     "notion",
				"identifier": id,
				"item":       item,
			}

			recJSON, err := json.Marshal(record)
			if err != nil {
				log.Printf("skip %s: marshal: %v", id, err)
				continue
			}

			if _, err := stmt.Exec(id, string(recJSON)); err != nil {
				return fmt.Errorf("insert %s: %w", id, err)
			}
			total++
		}

		fmt.Printf("\rFetched %d pages...", total)

		if !page.HasMore || page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	fmt.Printf("\rFetched %d pages. Saved to %s\n", total, outPath)
	return nil
}

// queryResponse is the Notion database query response.
type queryResponse struct {
	Results    []json.RawMessage `json:"results"`
	HasMore    bool              `json:"has_more"`
	NextCursor string            `json:"next_cursor"`
}

// normalizePage extracts a flat key-value map from a Notion page's properties.
func normalizePage(raw json.RawMessage) (string, map[string]any) {
	var page struct {
		ID          string                     `json:"id"`
		URL         string                     `json:"url"`
		CreatedTime string                     `json:"created_time"`
		LastEdited  string                     `json:"last_edited_time"`
		Properties  map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return "", nil
	}

	item := map[string]any{
		"id":               page.ID,
		"url":              page.URL,
		"created_time":     page.CreatedTime,
		"last_edited_time": page.LastEdited,
	}

	title := ""
	for name, propRaw := range page.Properties {
		val, isTitle := extractPropertyValue(propRaw)
		item[name] = val
		if isTitle && title == "" {
			if s, ok := val.(string); ok {
				title = s
			}
		}
	}

	if title == "" {
		title = page.ID
	}

	return title, item
}

// extractPropertyValue extracts the display value from a Notion property.
// Returns the value and whether this property is a title.
func extractPropertyValue(raw json.RawMessage) (any, bool) {
	var prop struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &prop); err != nil {
		return nil, false
	}

	switch prop.Type {
	case "title":
		var p struct {
			Title []struct {
				PlainText string `json:"plain_text"`
			} `json:"title"`
		}
		if json.Unmarshal(raw, &p) == nil {
			parts := make([]string, 0, len(p.Title))
			for _, t := range p.Title {
				parts = append(parts, t.PlainText)
			}
			return strings.Join(parts, ""), true
		}

	case "rich_text":
		var p struct {
			RichText []struct {
				PlainText string `json:"plain_text"`
			} `json:"rich_text"`
		}
		if json.Unmarshal(raw, &p) == nil {
			parts := make([]string, 0, len(p.RichText))
			for _, t := range p.RichText {
				parts = append(parts, t.PlainText)
			}
			return strings.Join(parts, ""), false
		}

	case "select":
		var p struct {
			Select *struct {
				Name string `json:"name"`
			} `json:"select"`
		}
		if json.Unmarshal(raw, &p) == nil && p.Select != nil {
			return p.Select.Name, false
		}

	case "multi_select":
		var p struct {
			MultiSelect []struct {
				Name string `json:"name"`
			} `json:"multi_select"`
		}
		if json.Unmarshal(raw, &p) == nil {
			names := make([]string, len(p.MultiSelect))
			for i, s := range p.MultiSelect {
				names[i] = s.Name
			}
			return strings.Join(names, ", "), false
		}

	case "number":
		var p struct {
			Number *float64 `json:"number"`
		}
		if json.Unmarshal(raw, &p) == nil && p.Number != nil {
			return *p.Number, false
		}

	case "checkbox":
		var p struct {
			Checkbox bool `json:"checkbox"`
		}
		if json.Unmarshal(raw, &p) == nil {
			return p.Checkbox, false
		}

	case "url":
		var p struct {
			URL *string `json:"url"`
		}
		if json.Unmarshal(raw, &p) == nil && p.URL != nil {
			return *p.URL, false
		}

	case "email":
		var p struct {
			Email *string `json:"email"`
		}
		if json.Unmarshal(raw, &p) == nil && p.Email != nil {
			return *p.Email, false
		}

	case "date":
		var p struct {
			Date *struct {
				Start string `json:"start"`
				End   string `json:"end"`
			} `json:"date"`
		}
		if json.Unmarshal(raw, &p) == nil && p.Date != nil {
			if p.Date.End != "" {
				return p.Date.Start + " → " + p.Date.End, false
			}
			return p.Date.Start, false
		}

	case "status":
		var p struct {
			Status *struct {
				Name string `json:"name"`
			} `json:"status"`
		}
		if json.Unmarshal(raw, &p) == nil && p.Status != nil {
			return p.Status.Name, false
		}

	case "unique_id":
		var p struct {
			UniqueID *struct {
				Prefix string `json:"prefix"`
				Number int    `json:"number"`
			} `json:"unique_id"`
		}
		if json.Unmarshal(raw, &p) == nil && p.UniqueID != nil {
			if p.UniqueID.Prefix != "" {
				return fmt.Sprintf("%s-%d", p.UniqueID.Prefix, p.UniqueID.Number), false
			}
			return fmt.Sprintf("%d", p.UniqueID.Number), false
		}

	case "relation":
		var p struct {
			Relation []struct {
				ID string `json:"id"`
			} `json:"relation"`
		}
		if json.Unmarshal(raw, &p) == nil {
			ids := make([]string, len(p.Relation))
			for i, r := range p.Relation {
				ids[i] = r.ID
			}
			return strings.Join(ids, ", "), false
		}

	case "people":
		var p struct {
			People []struct {
				Name string `json:"name"`
			} `json:"people"`
		}
		if json.Unmarshal(raw, &p) == nil {
			names := make([]string, len(p.People))
			for i, person := range p.People {
				names[i] = person.Name
			}
			return strings.Join(names, ", "), false
		}

	case "created_time":
		var p struct {
			CreatedTime string `json:"created_time"`
		}
		if json.Unmarshal(raw, &p) == nil {
			return p.CreatedTime, false
		}

	case "last_edited_time":
		var p struct {
			LastEditedTime string `json:"last_edited_time"`
		}
		if json.Unmarshal(raw, &p) == nil {
			return p.LastEditedTime, false
		}
	}

	return nil, false
}

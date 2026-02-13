# Mache Example Schemas

This directory contains example topologies for Mache.

## `nvd-schema.json`
Maps the National Vulnerability Database (NVD) JSON feed into a hierarchy of `year/severity/id`.
- **Source:** NVD JSON Feed (large)
- **Structure:**
  - `/:year` (e.g., `2024`)
    - `/:severity` (e.g., `HIGH`, `CRITICAL`)
      - `/:id` (e.g., `CVE-2024-1234`)
        - `summary`: Text file with the description.
        - `score`: File containing the CVSS score.

## `go-schema.json`
Projects a Go codebase into a logical view based on function receivers and types.
- **Source:** Go source code (via Tree-sitter)
- **Structure:**
  - `/pkg`
    - `/:package_name`
      - `/:receiver_type` (for methods)
        - `/:function_name.go`: The function body.
  - **Note:** Uses Tree-sitter selectors to extract specific AST nodes.

## `cli-schema.json`
A simple example for projecting CLI tool output or flat JSON.

## `kev-schema.json`
Maps the CISA Known Exploited Vulnerabilities (KEV) catalog.

#!/usr/bin/env python3
"""Index .audit/ markdown files into JSON for mache projection.

Parses the-firm's coordination files (STATUS.md, MASTER-TRIAGE.md,
SCORECARD.md, SHARED-STATE.md, BEAD-PLAN.md, *-audit.md) and produces
a single .audit/.index.json that mache can mount with audit-schema.json.

Usage:
    # Index and mount:
    python audit-indexer.py /path/to/.audit/
    mache mount .audit/.index.json --schema audit-schema.json /tmp/audit

    # Or as a PostToolUse hook (re-index after every Write/Edit to .audit/):
    python audit-indexer.py /path/to/.audit/ --watch
"""

import json
import re
import sys
from pathlib import Path


def parse_findings(audit_dir: Path) -> list[dict]:
    """Extract findings from MASTER-TRIAGE.md and per-dimension audit files."""
    findings = []

    # Parse MASTER-TRIAGE.md for consolidated findings
    triage = audit_dir / "MASTER-TRIAGE.md"
    if triage.exists():
        findings.extend(_parse_triage(triage.read_text()))

    # Parse individual audit files for detail
    for f in sorted(audit_dir.glob("*-audit.md")):
        dimension = f.stem.replace("-audit", "")
        findings.extend(_parse_audit_file(f.read_text(), dimension, f.name))

    # Deduplicate by ID (triage has canonical status)
    seen = {}
    for finding in findings:
        fid = finding["id"]
        if fid in seen:
            # Merge: triage status wins, audit description wins if longer
            existing = seen[fid]
            if len(finding.get("description", "")) > len(existing.get("description", "")):
                existing["description"] = finding["description"]
        else:
            seen[fid] = finding

    return list(seen.values())


def _parse_triage(text: str) -> list[dict]:
    """Parse MASTER-TRIAGE.md finding entries.

    Looks for patterns like:
        ### C1: SQL Injection in auth module
        **Status:** OPEN
        **Severity:** critical
        **Dimension:** security
    """
    findings = []
    # Split on finding headers, then parse each section
    sections = re.split(r"(?=###\s+[CHML]\d+:)", text)

    for section in sections:
        header_m = re.match(r"###\s+([CHML]\d+):\s+(.+)", section)
        if not header_m:
            continue
        fid, title = header_m.group(1), header_m.group(2).strip()
        body = section[header_m.end():]
        finding = {
            "id": fid,
            "title": title,
            "severity": _extract_field(body, "Severity", _severity_from_id(fid)),
            "status": _extract_field(body, "Status", "OPEN"),
            "dimension": _extract_field(body, "Dimension", "unknown"),
            "description": _extract_description(body),
            "source_file": "MASTER-TRIAGE.md",
        }
        findings.append(finding)

    return findings


def _parse_audit_file(text: str, dimension: str, filename: str) -> list[dict]:
    """Parse a per-dimension audit file (e.g., security-audit.md)."""
    findings = []
    sections = re.split(r"(?=###?\s+[CHML]\d+:)", text)

    for section in sections:
        header_m = re.match(r"###?\s+([CHML]\d+):\s+(.+)", section)
        if not header_m:
            continue
        fid, title = header_m.group(1), header_m.group(2).strip()
        body = section[header_m.end():]
        findings.append({
            "id": fid,
            "title": title,
            "severity": _extract_field(body, "Severity", _severity_from_id(fid)),
            "status": _extract_field(body, "Status", "OPEN"),
            "dimension": dimension,
            "description": _extract_description(body),
            "source_file": filename,
        })

    return findings


def parse_scores(audit_dir: Path) -> list[dict]:
    """Extract dimension scores from SCORECARD.md.

    Looks for table rows like:
        | Security | 4.5 | 1.5 | Strong auth, needs input validation |
    """
    scorecard = audit_dir / "SCORECARD.md"
    if not scorecard.exists():
        return []

    scores = []
    text = scorecard.read_text()

    # Match markdown table rows (skip header and separator)
    table_re = re.compile(
        r"\|\s*(\w[\w\s/]*?)\s*\|\s*([\d.?]+)\s*\|\s*([\d.?]+)\s*\|\s*(.*?)\s*\|"
    )
    for m in table_re.finditer(text):
        dim, score, weight, rubric = m.groups()
        if dim.strip().lower() in ("dimension", "---", ""):
            continue
        try:
            score_val = float(score) if score != "?" else 0.0
            weight_val = float(weight) if weight != "?" else 1.0
        except ValueError:
            continue

        scores.append({
            "dimension": dim.strip().lower().replace(" ", "-"),
            "score": score_val,
            "weight": weight_val,
            "rubric": rubric.strip(),
        })

    return scores


def parse_agents(audit_dir: Path) -> list[dict]:
    """Extract agent info from STATUS.md and SHARED-STATE.md."""
    agents = []

    # Parse STATUS.md for active agents
    status = audit_dir / "STATUS.md"
    if status.exists():
        text = status.read_text()
        # Look for agent entries like: - **Lead**: active
        agent_re = re.compile(r"\*\*(\w[\w\s]*?)\*\*.*?:\s*(\w+)")
        for m in agent_re.finditer(text):
            name, state = m.groups()
            agents.append({
                "name": name.strip().lower().replace(" ", "-"),
                "role": name.strip().lower(),
                "status": state.strip().lower(),
                "owned_files": "",
            })

    # Enrich with file ownership from SHARED-STATE.md
    shared = audit_dir / "SHARED-STATE.md"
    if shared.exists():
        text = shared.read_text()
        # Look for ownership entries like: engineer-1: src/auth.go, src/db.go
        ownership_re = re.compile(r"(\w[\w-]*?):\s*([\w/.,\s]+)")
        ownership = {}
        for m in ownership_re.finditer(text):
            name, files = m.groups()
            ownership[name.strip().lower()] = files.strip()

        for agent in agents:
            if agent["name"] in ownership:
                agent["owned_files"] = ownership[agent["name"]]

    return agents


def parse_beads(audit_dir: Path) -> list[dict]:
    """Extract bead specs from BEAD-PLAN.md.

    Looks for patterns like:
        ### fix-sql-injection (T1)
        **Dimension:** security
    """
    plan = audit_dir / "BEAD-PLAN.md"
    if not plan.exists():
        return []

    beads = []
    text = plan.read_text()

    # Split on bead headers
    sections = re.split(r"(?=###\s+[\w-]+\s*\(?T[1-4]\)?)", text)

    for section in sections:
        header_m = re.match(r"###\s+([\w-]+)\s*\(?(T[1-4])?\)?", section)
        if not header_m:
            continue
        bead_id, tier = header_m.group(1), header_m.group(2) or "T2"
        body = section[header_m.end():]
        beads.append({
            "id": bead_id,
            "title": bead_id.replace("-", " ").title(),
            "tier": tier,
            "dimension": _extract_field(body, "Dimension", "general"),
            "description": _extract_description(body),
        })

    return beads


def parse_cycles(audit_dir: Path) -> list[dict]:
    """Extract cycle history from CYCLE-LOG.md."""
    log = audit_dir / "CYCLE-LOG.md"
    if not log.exists():
        return []

    cycles = []
    text = log.read_text()

    # Match cycle entries like: ## Cycle 1 (2024-12-01)
    pattern = re.compile(
        r"##\s*Cycle\s+(\d+)\s*\(?([\d-]*)\)?"
        r"(.*?)(?=##\s*Cycle\s+\d+|$)",
        re.MULTILINE | re.DOTALL,
    )

    for m in pattern.finditer(text):
        num, timestamp, body = m.group(1), m.group(2), m.group(3)
        score = _extract_field(body, "Composite Score", "0.0")
        delta = _extract_field(body, "Delta", "0.0")

        cycles.append({
            "number": int(num),
            "timestamp": timestamp or "unknown",
            "composite_score": float(score) if score.replace(".", "").isdigit() else 0.0,
            "delta": float(delta) if delta.replace(".", "").replace("-", "").isdigit() else 0.0,
            "notes": _extract_description(body),
        })

    return cycles


# --- Helpers ---


def _extract_field(text: str, field_name: str, default: str = "") -> str:
    """Extract **FieldName:** value from markdown body."""
    pattern = re.compile(
        rf"\*\*{re.escape(field_name)}:\*\*\s*(.+)",
        re.IGNORECASE,
    )
    m = pattern.search(text)
    return m.group(1).strip() if m else default


def _extract_description(body: str) -> str:
    """Extract prose description, stripping metadata fields."""
    lines = []
    for line in body.strip().splitlines():
        stripped = line.strip()
        if stripped.startswith("**") and ":**" in stripped:
            continue  # skip metadata fields
        if stripped and not stripped.startswith("#"):
            lines.append(stripped)
    return " ".join(lines)[:500]  # cap at 500 chars


def _severity_from_id(fid: str) -> str:
    """Infer severity from finding ID prefix."""
    prefix = fid[0].upper()
    return {"C": "critical", "H": "high", "M": "medium", "L": "low"}.get(prefix, "unknown")


def index_audit_dir(audit_dir: Path) -> dict:
    """Build the complete index from .audit/ directory."""
    return {
        "findings": parse_findings(audit_dir),
        "scores": parse_scores(audit_dir),
        "agents": parse_agents(audit_dir),
        "beads": parse_beads(audit_dir),
        "cycles": parse_cycles(audit_dir),
    }


def main():
    if len(sys.argv) < 2:
        print(f"Usage: {sys.argv[0]} <path-to-.audit-dir>", file=sys.stderr)
        sys.exit(1)

    audit_dir = Path(sys.argv[1])
    if not audit_dir.is_dir():
        print(f"Error: {audit_dir} is not a directory", file=sys.stderr)
        sys.exit(1)

    index = index_audit_dir(audit_dir)
    output = audit_dir / ".index.json"
    output.write_text(json.dumps(index, indent=2))

    # Summary
    counts = {k: len(v) for k, v in index.items()}
    print(f"Indexed: {counts}")
    print(f"Written: {output}")


if __name__ == "__main__":
    main()

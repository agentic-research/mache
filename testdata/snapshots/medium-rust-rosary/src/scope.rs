//! `ScopeId` — generic identifier for "where a bead lives" (rosary-b5da2f).
//!
//! Today, every bead-related MCP call takes a `repo_path: &str` that points at
//! a local git checkout. That assumption is load-bearing in several places
//! (per-repo `.beads/` lookup, `LinkageStore` cross-repo dep schema, etc.) and
//! it breaks down for three real use cases:
//!
//! 1. **Cross-repo dependencies** — `rsry_bead_link` with `depends_on=signet-X`
//!    fails to find `signet-X` when the caller passes `repo_path=cloister`
//!    (`rosary-98ee93`). The partial fix in PR #205 auto-detects the prefix;
//!    the full fix is a richer identifier here.
//! 2. **External sources** — zen PR inbox, cloister-hosted MCP tools, etc.
//!    have bead-like work items that aren't tied to a local checkout.
//! 3. **Global / org-level beads** — the incoming triage queue
//!    (`rosary-1db9c9`) is conceptually scope-less; today it has to be filed
//!    in some repo as a placeholder.
//!
//! `ScopeId` is the type that names all three cases. This module is the
//! type + parsing only — no call-sites have been updated yet. Threading it
//! through `BeadStore`, `LinkageStore`, and the MCP handlers ships in later
//! PRs in the rosary-b5da2f series.
//!
//! ## Canonical string forms
//!
//! Designed for unambiguous round-tripping through MCP JSON args, CLI flags,
//! and bead-id prefixes:
//!
//! | Variant            | Canonical form        | Notes                                  |
//! |--------------------|-----------------------|----------------------------------------|
//! | `Repo("rosary")`   | `"repo:rosary"`       | bare `"rosary"` also parses for ergonomics |
//! | `External("uri")`  | `"external:uri"`      | uri is arbitrary; rosary doesn't fetch it |
//! | `Global`           | `"global"`            | no parameters                          |
//!
//! `FromStr` / `Display` are inverse on canonical forms. Path-shaped strings
//! (e.g. `~/remotes/art/rosary`) are NOT accepted by `FromStr` — use
//! [`ScopeId::from_repo_path`] instead so the path-vs-canonical distinction
//! is explicit at the call site.

use std::fmt;
use std::str::FromStr;

// ── Canonical encoding constants ──────────────────────────────────────
//
// Centralized here so the four places that touch these strings —
// `ScopeId::work_ref`, `WorkRef::scope_id`, `Display`, `FromStr` —
// stay in lock-step. Changing one side of the bridge without the
// other would silently break round-tripping.

/// Reserved repo-field value identifying [`ScopeId::Global`] when
/// encoded into a [`WorkRef`](crate::store::WorkRef).
pub(crate) const GLOBAL_REPO: &str = "global";

/// Prefix on the repo-field value identifying [`ScopeId::External`]
/// when encoded into a [`WorkRef`](crate::store::WorkRef). The remainder
/// after the prefix is the caller-supplied URI.
pub(crate) const EXTERNAL_PREFIX: &str = "external:";

/// Prefix used in the canonical [`ScopeId::Display`] / [`FromStr`]
/// string form for [`ScopeId::Repo`] (e.g. `"repo:rosary"`).
pub(crate) const REPO_DISPLAY_PREFIX: &str = "repo:";

/// Storage limit for encoded repo-field values, derived from
/// `cross_repo_deps.from_repo VARCHAR(128)` and the matching Dolt /
/// SQLite columns elsewhere (see `src/store_dolt.rs`, `src/store_sqlite.rs`).
/// `ScopeId::work_ref` refuses to silently produce a value longer than
/// this so we never get write failures or surprising truncation when
/// the bridge lands in [`crate::store::LinkageStore`].
pub(crate) const REPO_FIELD_MAX_BYTES: usize = 128;

/// Where a bead lives. Replaces the `repo_path: &str` parameter pattern
/// across the rosary surface for use cases that aren't naturally a local
/// git checkout (cross-repo deps, external sources, global queues).
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub enum ScopeId {
    /// A repo by name (resolved to a local path via config when needed).
    /// The canonical bead-id prefix scheme — `signet-9605a3` lives in
    /// `ScopeId::Repo("signet")`.
    Repo(String),
    /// A non-repo source — zen PR inbox, cloister-hosted MCP, anything
    /// addressable by URI but not a local checkout. The string is the
    /// caller-supplied identifier; rosary stores it but doesn't fetch.
    External(String),
    /// Org-level beads with no specific repo home — the incoming triage
    /// queue (`rosary-1db9c9`) is the canonical use case.
    Global,
}

/// Parse errors are kept narrow so callers can distinguish "bad input"
/// from "valid but unsupported." `FromStr` only ever returns these.
#[derive(Debug, PartialEq, Eq)]
pub enum ScopeParseError {
    /// The input was empty or whitespace-only after trimming.
    Empty,
    /// A `repo:` or `external:` prefix had nothing after it.
    EmptyAfterPrefix(&'static str),
}

impl fmt::Display for ScopeParseError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Empty => write!(f, "scope id is empty"),
            Self::EmptyAfterPrefix(prefix) => {
                write!(f, "scope id `{prefix}:` has no value after the prefix")
            }
        }
    }
}

impl std::error::Error for ScopeParseError {}

/// Errors encoding a [`ScopeId`] into a [`WorkRef`](crate::store::WorkRef).
/// The only failure mode today is exceeding [`REPO_FIELD_MAX_BYTES`] —
/// the storage column width that the [`crate::store::LinkageStore`]
/// schema enforces.
#[derive(Debug, PartialEq, Eq)]
pub enum ScopeEncodeError {
    /// Encoded repo-field value would exceed the storage column width.
    /// Carries the encoded value's actual length so callers (and
    /// reviewers reading test failures) can see by how much.
    RepoFieldTooLong { encoded_len: usize, limit: usize },
}

impl fmt::Display for ScopeEncodeError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::RepoFieldTooLong { encoded_len, limit } => {
                write!(
                    f,
                    "encoded scope repo-field is {encoded_len} bytes, exceeds storage limit of {limit} bytes"
                )
            }
        }
    }
}

impl std::error::Error for ScopeEncodeError {}

impl ScopeId {
    /// If this scope is a `Repo`, return the repo name. Otherwise None.
    /// Use to gate repo-only code paths (e.g. local Dolt store lookup)
    /// without exhaustive matching on every call site.
    pub fn as_repo_name(&self) -> Option<&str> {
        match self {
            Self::Repo(name) => Some(name),
            _ => None,
        }
    }

    /// Build a [`WorkRef`](crate::store::WorkRef) for `bead_id` under this
    /// scope. Bridges new `ScopeId`-thinking code to the existing repo-keyed
    /// `LinkageStore` surface without changing the on-disk schema.
    ///
    /// Mapping (until rosary-b5da2f PR 4 lands a richer schema):
    /// - `Repo(name)`  → `WorkRef { repo: name, scope: "", bead_id }`
    /// - `External(u)` → `WorkRef { repo: format!("external:{u}"), scope: "", bead_id }`
    /// - `Global`      → `WorkRef { repo: "global", scope: "", bead_id }`
    ///
    /// The `External` / `Global` variants occupy a reserved namespace
    /// (`external:...` / `global`) that won't collide with a legitimate
    /// repo name; this is what lets the existing `WorkRef`-keyed
    /// `LinkageStore` table store cross-scope links without a migration.
    ///
    /// Errors with [`ScopeEncodeError::RepoFieldTooLong`] when the
    /// encoded repo-field value would exceed [`REPO_FIELD_MAX_BYTES`]
    /// (the storage column width that `cross_repo_deps.from_repo` and
    /// related columns enforce). Surfacing this here prevents silent
    /// truncation or write failures when the bridge feeds
    /// [`crate::store::LinkageStore`].
    pub fn work_ref(
        &self,
        bead_id: impl Into<String>,
    ) -> Result<crate::store::WorkRef, ScopeEncodeError> {
        let repo = match self {
            Self::Repo(name) => name.clone(),
            Self::External(uri) => format!("{EXTERNAL_PREFIX}{uri}"),
            Self::Global => GLOBAL_REPO.to_string(),
        };
        if repo.len() > REPO_FIELD_MAX_BYTES {
            return Err(ScopeEncodeError::RepoFieldTooLong {
                encoded_len: repo.len(),
                limit: REPO_FIELD_MAX_BYTES,
            });
        }
        Ok(crate::store::WorkRef {
            repo,
            scope: String::new(),
            bead_id: bead_id.into(),
        })
    }

    /// Build a `Repo` scope from a filesystem path by taking the basename.
    /// This is the back-compat helper for the existing `repo_path: &str`
    /// MCP arg shape — when the caller passes `~/remotes/art/rosary`, the
    /// scope is `Repo("rosary")`. Trailing slashes are stripped.
    ///
    /// Returns `Repo("unknown")` for paths with no basename (e.g. `"/"`)
    /// so the surface stays infallible at call sites that don't want to
    /// thread a `Result` for what was previously an `&str`. Callers that
    /// need strictness should construct `ScopeId::Repo(...)` directly.
    pub fn from_repo_path(path: &str) -> Self {
        let trimmed = path.trim().trim_end_matches('/');
        let name = std::path::Path::new(trimmed)
            .file_name()
            .map(|n| n.to_string_lossy().to_string())
            .filter(|s| !s.is_empty())
            .unwrap_or_else(|| "unknown".to_string());
        Self::Repo(name)
    }
}

impl crate::store::WorkRef {
    /// Recover the [`ScopeId`] that produced this `WorkRef`'s `repo`
    /// field. Inverse of [`ScopeId::work_ref`] on the round-trip:
    /// `Repo(name).work_ref(b).unwrap().scope_id() == Repo(name)`.
    ///
    /// Reserved namespaces (constants live at the top of the module so
    /// the encode + decode paths stay in lock-step):
    /// - `repo == GLOBAL_REPO` → `ScopeId::Global`
    /// - `repo` starting with `EXTERNAL_PREFIX` → `ScopeId::External(rest)`
    /// - anything else → `ScopeId::Repo(repo.clone())`
    ///
    /// The `WorkRef.scope` field (monorepo team scoping) is preserved on
    /// the `WorkRef` but is NOT carried into the `ScopeId` — `ScopeId`
    /// is the "where does this bead live" axis; `WorkRef.scope` is the
    /// orthogonal "what team owns it" axis.
    pub fn scope_id(&self) -> ScopeId {
        if self.repo == GLOBAL_REPO {
            ScopeId::Global
        } else if let Some(uri) = self.repo.strip_prefix(EXTERNAL_PREFIX) {
            ScopeId::External(uri.to_string())
        } else {
            ScopeId::Repo(self.repo.clone())
        }
    }
}

impl fmt::Display for ScopeId {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Repo(name) => write!(f, "{REPO_DISPLAY_PREFIX}{name}"),
            Self::External(uri) => write!(f, "{EXTERNAL_PREFIX}{uri}"),
            Self::Global => write!(f, "{GLOBAL_REPO}"),
        }
    }
}

impl FromStr for ScopeId {
    type Err = ScopeParseError;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        let trimmed = s.trim();
        if trimmed.is_empty() {
            return Err(ScopeParseError::Empty);
        }
        if trimmed == GLOBAL_REPO {
            return Ok(Self::Global);
        }
        if let Some(rest) = trimmed.strip_prefix(REPO_DISPLAY_PREFIX) {
            if rest.is_empty() {
                return Err(ScopeParseError::EmptyAfterPrefix("repo"));
            }
            return Ok(Self::Repo(rest.to_string()));
        }
        if let Some(rest) = trimmed.strip_prefix(EXTERNAL_PREFIX) {
            if rest.is_empty() {
                return Err(ScopeParseError::EmptyAfterPrefix("external"));
            }
            return Ok(Self::External(rest.to_string()));
        }
        // Ergonomic fall-through: bare names parse as `Repo`. This keeps
        // existing call sites that pass `"rosary"` (vs `"repo:rosary"`)
        // working without churn.
        Ok(Self::Repo(trimmed.to_string()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // ── FromStr — canonical forms ─────────────────────────────────────

    #[test]
    fn parses_canonical_repo_form() {
        let s: ScopeId = "repo:rosary".parse().unwrap();
        assert_eq!(s, ScopeId::Repo("rosary".to_string()));
    }

    #[test]
    fn parses_canonical_external_form() {
        let s: ScopeId = "external:https://github.com/foo/bar#42".parse().unwrap();
        assert_eq!(
            s,
            ScopeId::External("https://github.com/foo/bar#42".to_string())
        );
    }

    #[test]
    fn parses_canonical_global() {
        let s: ScopeId = "global".parse().unwrap();
        assert_eq!(s, ScopeId::Global);
    }

    // ── FromStr — ergonomic fall-through ──────────────────────────────

    #[test]
    fn parses_bare_name_as_repo() {
        // No `repo:` prefix — fall through to Repo("..."). Lets existing
        // callers that pass bare repo names keep working.
        let s: ScopeId = "rosary".parse().unwrap();
        assert_eq!(s, ScopeId::Repo("rosary".to_string()));
    }

    #[test]
    fn parses_bare_hyphenated_repo() {
        // Hyphens in repo names (ley-line, ley-line-open) must survive
        // the bare-name fall-through; the lack of `:` is what
        // disambiguates from the prefixed forms.
        let s: ScopeId = "ley-line-open".parse().unwrap();
        assert_eq!(s, ScopeId::Repo("ley-line-open".to_string()));
    }

    #[test]
    fn parses_strips_surrounding_whitespace() {
        // Defensive: caller copies + pastes with trailing whitespace.
        let s: ScopeId = "  rosary\n".parse().unwrap();
        assert_eq!(s, ScopeId::Repo("rosary".to_string()));
    }

    // ── FromStr — error cases ─────────────────────────────────────────

    #[test]
    fn rejects_empty_string() {
        assert_eq!("".parse::<ScopeId>().unwrap_err(), ScopeParseError::Empty);
    }

    #[test]
    fn rejects_whitespace_only() {
        assert_eq!(
            "   \t\n".parse::<ScopeId>().unwrap_err(),
            ScopeParseError::Empty
        );
    }

    #[test]
    fn rejects_repo_prefix_with_no_value() {
        assert_eq!(
            "repo:".parse::<ScopeId>().unwrap_err(),
            ScopeParseError::EmptyAfterPrefix("repo")
        );
    }

    #[test]
    fn rejects_external_prefix_with_no_value() {
        assert_eq!(
            "external:".parse::<ScopeId>().unwrap_err(),
            ScopeParseError::EmptyAfterPrefix("external")
        );
    }

    // ── Display — canonical forms ─────────────────────────────────────

    #[test]
    fn display_repo_uses_canonical_prefix() {
        assert_eq!(ScopeId::Repo("rosary".into()).to_string(), "repo:rosary");
    }

    #[test]
    fn display_external_uses_canonical_prefix() {
        assert_eq!(
            ScopeId::External("https://example.com/q".into()).to_string(),
            "external:https://example.com/q"
        );
    }

    #[test]
    fn display_global_has_no_prefix() {
        assert_eq!(ScopeId::Global.to_string(), "global");
    }

    // ── Display ↔ FromStr round-trip ─────────────────────────────────

    #[test]
    fn roundtrip_repo() {
        let original = ScopeId::Repo("rosary".into());
        let parsed: ScopeId = original.to_string().parse().unwrap();
        assert_eq!(parsed, original);
    }

    #[test]
    fn roundtrip_external() {
        let original = ScopeId::External("zen://inbox/pr/42".into());
        let parsed: ScopeId = original.to_string().parse().unwrap();
        assert_eq!(parsed, original);
    }

    #[test]
    fn roundtrip_global() {
        let original = ScopeId::Global;
        let parsed: ScopeId = original.to_string().parse().unwrap();
        assert_eq!(parsed, original);
    }

    // ── from_repo_path back-compat helper ─────────────────────────────

    #[test]
    fn from_repo_path_extracts_basename() {
        let s = ScopeId::from_repo_path("/Users/jamesgardner/remotes/art/rosary");
        assert_eq!(s, ScopeId::Repo("rosary".into()));
    }

    #[test]
    fn from_repo_path_strips_trailing_slash() {
        // Many config + shell paths end with `/`. The basename of `foo/`
        // is `foo`, not `""` — without the trim, this would return
        // Repo("unknown").
        let s = ScopeId::from_repo_path("/Users/jamesgardner/remotes/art/rosary/");
        assert_eq!(s, ScopeId::Repo("rosary".into()));
    }

    #[test]
    fn from_repo_path_handles_relative() {
        let s = ScopeId::from_repo_path("art/rosary");
        assert_eq!(s, ScopeId::Repo("rosary".into()));
    }

    #[test]
    fn from_repo_path_handles_basename_only() {
        let s = ScopeId::from_repo_path("rosary");
        assert_eq!(s, ScopeId::Repo("rosary".into()));
    }

    #[test]
    fn from_repo_path_trims_whitespace() {
        let s = ScopeId::from_repo_path("  /tmp/foo  ");
        assert_eq!(s, ScopeId::Repo("foo".into()));
    }

    #[test]
    fn from_repo_path_with_no_basename_returns_unknown() {
        // Edge case: `/` (root) has no basename. `Path::file_name` returns
        // None. We surface "unknown" rather than panic so the call site
        // stays infallible.
        let s = ScopeId::from_repo_path("/");
        assert_eq!(s, ScopeId::Repo("unknown".into()));
    }

    #[test]
    fn from_repo_path_empty_returns_unknown() {
        let s = ScopeId::from_repo_path("");
        assert_eq!(s, ScopeId::Repo("unknown".into()));
    }

    // ── as_repo_name ──────────────────────────────────────────────────

    #[test]
    fn as_repo_name_some_for_repo() {
        assert_eq!(
            ScopeId::Repo("rosary".into()).as_repo_name(),
            Some("rosary")
        );
    }

    #[test]
    fn as_repo_name_none_for_external() {
        assert_eq!(ScopeId::External("x".into()).as_repo_name(), None);
    }

    #[test]
    fn as_repo_name_none_for_global() {
        assert_eq!(ScopeId::Global.as_repo_name(), None);
    }

    // ── ScopeParseError display + Error trait ─────────────────────────

    #[test]
    fn parse_error_empty_display() {
        assert_eq!(ScopeParseError::Empty.to_string(), "scope id is empty");
    }

    #[test]
    fn parse_error_empty_after_prefix_display() {
        assert_eq!(
            ScopeParseError::EmptyAfterPrefix("repo").to_string(),
            "scope id `repo:` has no value after the prefix"
        );
    }

    #[test]
    fn parse_error_is_std_error() {
        // Compiles iff `ScopeParseError: std::error::Error` — pins the
        // trait bound so anyhow / thiserror integration stays clean.
        fn assert_error<E: std::error::Error>(_: &E) {}
        assert_error(&ScopeParseError::Empty);
    }

    // ── WorkRef ↔ ScopeId bridge (PR 2 of rosary-b5da2f) ─────────────

    use crate::store::WorkRef;

    #[test]
    fn work_ref_for_repo_uses_bare_name_as_repo_field() {
        // For `Repo`, the WorkRef.repo field is the bare name —
        // unchanged from existing LinkageStore call sites. This is what
        // lets the bridge land without a schema migration.
        let wr = ScopeId::Repo("rosary".into())
            .work_ref("rosary-abc123")
            .expect("repo name fits in storage column");
        assert_eq!(wr.repo, "rosary");
        assert_eq!(wr.scope, "");
        assert_eq!(wr.bead_id, "rosary-abc123");
    }

    #[test]
    fn work_ref_for_external_uses_reserved_prefix() {
        // External uses an `external:` prefix so the same LinkageStore
        // table can hold both repo and external rows without colliding
        // with any real repo name.
        let wr = ScopeId::External("zen://inbox/pr/42".into())
            .work_ref("zen-pr-42")
            .expect("short external uri fits");
        assert_eq!(wr.repo, "external:zen://inbox/pr/42");
        assert_eq!(wr.bead_id, "zen-pr-42");
    }

    #[test]
    fn work_ref_for_global_uses_reserved_repo_string() {
        let wr = ScopeId::Global
            .work_ref("triage-001")
            .expect("global never overflows");
        assert_eq!(wr.repo, "global");
        assert_eq!(wr.bead_id, "triage-001");
    }

    /// Repo names that exceed the storage column width (VARCHAR(128) in
    /// `cross_repo_deps.from_repo` and related Dolt columns) must
    /// surface a clean error instead of writing silently-truncatable
    /// strings into LinkageStore. Copilot's #210 finding pins this
    /// contract — a real-world `External("https://github.com/.../...")`
    /// can easily exceed the limit.
    #[test]
    fn work_ref_errors_when_external_uri_overflows_storage() {
        // 200 chars of `a` + the 9-byte `external:` prefix → 209 bytes.
        let long_uri = "a".repeat(200);
        let err = ScopeId::External(long_uri)
            .work_ref("bead-1")
            .expect_err("external uri exceeds VARCHAR(128) — must surface");
        match err {
            ScopeEncodeError::RepoFieldTooLong { encoded_len, limit } => {
                assert_eq!(encoded_len, 209, "encoded len = uri + `external:` prefix");
                assert_eq!(limit, REPO_FIELD_MAX_BYTES);
                assert_eq!(limit, 128);
            }
        }
    }

    #[test]
    fn work_ref_errors_when_repo_name_overflows_storage() {
        // Repo names can technically be any length; the storage limit
        // is what's load-bearing. A 200-char repo name must also error.
        let long_repo = "r".repeat(200);
        let err = ScopeId::Repo(long_repo)
            .work_ref("bead-1")
            .expect_err("repo name exceeds VARCHAR(128) — must surface");
        match err {
            ScopeEncodeError::RepoFieldTooLong { encoded_len, .. } => {
                assert_eq!(encoded_len, 200);
            }
        }
    }

    #[test]
    fn work_ref_accepts_external_uri_at_exactly_the_limit() {
        // Boundary case: uri len + prefix len == REPO_FIELD_MAX_BYTES.
        // Must succeed (the constant is an inclusive upper bound).
        let uri = "x".repeat(REPO_FIELD_MAX_BYTES - EXTERNAL_PREFIX.len());
        let wr = ScopeId::External(uri.clone())
            .work_ref("bead-1")
            .expect("uri at exact limit must fit");
        assert_eq!(wr.repo.len(), REPO_FIELD_MAX_BYTES);
        assert_eq!(wr.repo, format!("{EXTERNAL_PREFIX}{uri}"));
    }

    #[test]
    fn workref_scope_id_recognizes_global_reserved_string() {
        let wr = WorkRef {
            repo: "global".into(),
            scope: String::new(),
            bead_id: "x".into(),
        };
        assert_eq!(wr.scope_id(), ScopeId::Global);
    }

    #[test]
    fn workref_scope_id_recognizes_external_prefix() {
        let wr = WorkRef {
            repo: "external:zen://inbox".into(),
            scope: String::new(),
            bead_id: "x".into(),
        };
        assert_eq!(wr.scope_id(), ScopeId::External("zen://inbox".into()));
    }

    #[test]
    fn workref_scope_id_falls_through_to_repo() {
        let wr = WorkRef {
            repo: "rosary".into(),
            scope: String::new(),
            bead_id: "x".into(),
        };
        assert_eq!(wr.scope_id(), ScopeId::Repo("rosary".into()));
    }

    #[test]
    fn workref_scope_id_preserves_hyphenated_repo_names() {
        // ley-line, ley-line-open, rosary-stringer — repos with hyphens
        // must round-trip cleanly.
        let wr = WorkRef {
            repo: "ley-line-open".into(),
            scope: String::new(),
            bead_id: "x".into(),
        };
        assert_eq!(wr.scope_id(), ScopeId::Repo("ley-line-open".into()));
    }

    #[test]
    fn workref_scope_id_ignores_workref_scope_field() {
        // WorkRef.scope (monorepo team) is orthogonal to ScopeId — even
        // when set, it must not change the ScopeId recovery.
        let wr = WorkRef {
            repo: "monorepo".into(),
            scope: "auth/identity".into(),
            bead_id: "x".into(),
        };
        assert_eq!(wr.scope_id(), ScopeId::Repo("monorepo".into()));
    }

    #[test]
    fn workref_roundtrip_repo() {
        let original = ScopeId::Repo("signet".into());
        let recovered = original.work_ref("signet-9605a3").unwrap().scope_id();
        assert_eq!(recovered, original);
    }

    #[test]
    fn workref_roundtrip_external() {
        let original = ScopeId::External("zen://inbox/pr/42".into());
        let recovered = original.work_ref("zen-pr-42").unwrap().scope_id();
        assert_eq!(recovered, original);
    }

    #[test]
    fn workref_roundtrip_global() {
        let original = ScopeId::Global;
        let recovered = original.work_ref("triage-001").unwrap().scope_id();
        assert_eq!(recovered, original);
    }

    /// Centralized constants pin: changing any of these in isolation
    /// would silently break the bridge. The Display + FromStr +
    /// work_ref + scope_id paths all use the same constants — this
    /// test fails the build if anyone accidentally inlines a literal.
    #[test]
    fn reserved_namespace_constants_are_canonical() {
        assert_eq!(GLOBAL_REPO, "global");
        assert_eq!(EXTERNAL_PREFIX, "external:");
        assert_eq!(REPO_DISPLAY_PREFIX, "repo:");
        // Constants flow into the encode path:
        assert_eq!(ScopeId::Global.to_string(), GLOBAL_REPO);
        assert!(
            ScopeId::External("x".into())
                .to_string()
                .starts_with(EXTERNAL_PREFIX)
        );
        assert!(
            ScopeId::Repo("x".into())
                .to_string()
                .starts_with(REPO_DISPLAY_PREFIX)
        );
    }

    // ── ScopeEncodeError display + Error trait ────────────────────────

    #[test]
    fn encode_error_repo_field_too_long_display() {
        let e = ScopeEncodeError::RepoFieldTooLong {
            encoded_len: 200,
            limit: 128,
        };
        let msg = e.to_string();
        assert!(msg.contains("200") && msg.contains("128"), "got: {msg}");
    }

    #[test]
    fn encode_error_is_std_error() {
        fn assert_error<E: std::error::Error>(_: &E) {}
        assert_error(&ScopeEncodeError::RepoFieldTooLong {
            encoded_len: 0,
            limit: 0,
        });
    }
}

//! Boundary parser for `scope` vs `repo_path` MCP arg pair (rosary-b5da2f PR 3).
//!
//! Every bead-flavored MCP handler today expects a `repo_path: &str` arg.
//! As [`ScopeId`](crate::scope::ScopeId) gets threaded through the
//! handler surface in later PRs, we want each handler to:
//!
//! 1. Prefer a canonical `scope` arg (`"repo:rosary"`, `"global"`,
//!    `"external:zen://inbox"`) when present.
//! 2. Fall back to the existing `repo_path` arg for back-compat.
//! 3. Surface a single, predictable error message when neither is set.
//!
//! Centralizing that decision here keeps the eventual 33-handler
//! migration mechanical: each call site becomes `let scope =
//! resolve_scope(args)?;` instead of bespoke per-handler `repo_path`
//! plumbing. No handler is converted yet; this PR is the boundary
//! parser only, ready to be wired in incrementally.

use anyhow::{Context, Result};
use serde_json::Value;

use crate::scope::ScopeId;

/// Parse the `scope` / `repo_path` arg pair into a [`ScopeId`].
///
/// Priority:
/// 1. `args["scope"]` if present (and a string) — must parse via
///    [`ScopeId::from_str`](std::str::FromStr). Empty string or invalid
///    form surfaces the underlying parse error with `scope: ` prefixed
///    so the operator can see which arg failed.
/// 2. `args["repo_path"]` if present — converted via
///    [`ScopeId::from_repo_path`]. Infallible (returns `Repo("unknown")`
///    for empty/no-basename paths).
/// 3. Neither set — returns an `Err` listing both accepted arg names
///    and the canonical scope forms.
///
/// Designed to be a one-liner replacement at every MCP handler boundary:
///
/// ```ignore
/// let scope = resolve_scope(args)?;
/// // ... use scope.as_repo_name() for repo-only paths,
/// // or scope.work_ref(bead_id)? for LinkageStore writes
/// ```
pub fn resolve_scope(args: &Value) -> Result<ScopeId> {
    if let Some(scope_arg) = args.get("scope").and_then(|v| v.as_str()) {
        // Use Context::with_context (preserves the underlying ScopeParseError
        // as the cause chain) rather than `anyhow::anyhow!("scope: {e}")`
        // (which flattens the cause to a string and loses the source for
        // downcasting / backtrace inspection). The Display still starts
        // with `scope:` so the operator-facing message is identical.
        return scope_arg
            .parse::<ScopeId>()
            .with_context(|| "scope".to_string());
    }
    if let Some(repo_path) = args.get("repo_path").and_then(|v| v.as_str()) {
        return Ok(ScopeId::from_repo_path(repo_path));
    }
    anyhow::bail!(
        "scope or repo_path required — accepted forms: \
         `scope: \"repo:<name>\" | \"external:<uri>\" | \"global\"` \
         (canonical) or `repo_path: \"/path/to/repo\"` (legacy)"
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    // ── scope arg takes priority over repo_path ───────────────────────

    #[test]
    fn scope_arg_takes_priority_when_both_present() {
        // Caller can override the path-derived scope by passing `scope`
        // explicitly. Once handlers learn the `scope` arg, this is the
        // forward-compatible path; legacy clients keep working via
        // `repo_path` alone.
        let args = json!({
            "scope": "repo:explicit",
            "repo_path": "/tmp/derived-from-path"
        });
        let scope = resolve_scope(&args).unwrap();
        assert_eq!(scope, ScopeId::Repo("explicit".into()));
    }

    #[test]
    fn scope_arg_parses_global() {
        let args = json!({ "scope": "global" });
        let scope = resolve_scope(&args).unwrap();
        assert_eq!(scope, ScopeId::Global);
    }

    #[test]
    fn scope_arg_parses_external() {
        let args = json!({ "scope": "external:zen://inbox/pr/42" });
        let scope = resolve_scope(&args).unwrap();
        assert_eq!(scope, ScopeId::External("zen://inbox/pr/42".into()));
    }

    #[test]
    fn scope_arg_parses_canonical_repo_form() {
        let args = json!({ "scope": "repo:rosary" });
        let scope = resolve_scope(&args).unwrap();
        assert_eq!(scope, ScopeId::Repo("rosary".into()));
    }

    #[test]
    fn scope_arg_accepts_bare_repo_name() {
        // Ergonomic fall-through from ScopeId::FromStr — bare names
        // parse as Repo. Lets clients pass `"rosary"` without the
        // `repo:` prefix.
        let args = json!({ "scope": "rosary" });
        let scope = resolve_scope(&args).unwrap();
        assert_eq!(scope, ScopeId::Repo("rosary".into()));
    }

    // ── repo_path fallback ────────────────────────────────────────────

    #[test]
    fn repo_path_used_when_scope_absent() {
        let args = json!({ "repo_path": "/Users/x/remotes/art/rosary" });
        let scope = resolve_scope(&args).unwrap();
        assert_eq!(scope, ScopeId::Repo("rosary".into()));
    }

    #[test]
    fn repo_path_basename_only_works() {
        let args = json!({ "repo_path": "rosary" });
        let scope = resolve_scope(&args).unwrap();
        assert_eq!(scope, ScopeId::Repo("rosary".into()));
    }

    #[test]
    fn repo_path_with_trailing_slash_works() {
        // Common shell-completion artifact. ScopeId::from_repo_path
        // handles it; this test pins that the boundary parser doesn't
        // reintroduce a bug by stripping vs not.
        let args = json!({ "repo_path": "/tmp/rosary/" });
        let scope = resolve_scope(&args).unwrap();
        assert_eq!(scope, ScopeId::Repo("rosary".into()));
    }

    // ── error cases ───────────────────────────────────────────────────

    #[test]
    fn errors_when_neither_arg_present() {
        let args = json!({ "id": "x", "depends_on": "y" });
        let err = resolve_scope(&args).unwrap_err();
        let msg = err.to_string();
        // Error must name BOTH accepted args so callers know either
        // path is valid — the previous "repo_path required" message
        // didn't tell them they could pass `scope` instead.
        assert!(
            msg.contains("scope") && msg.contains("repo_path"),
            "error must list both accepted args; got: {msg}"
        );
    }

    #[test]
    fn errors_with_helpful_message_listing_scope_forms() {
        let args = json!({});
        let msg = resolve_scope(&args).unwrap_err().to_string();
        // Spot-check that the canonical scope forms appear so the
        // operator doesn't have to read the docs.
        assert!(
            msg.contains("repo:") && msg.contains("external:") && msg.contains("global"),
            "error must enumerate scope forms; got: {msg}"
        );
    }

    #[test]
    fn errors_when_scope_arg_is_empty_string() {
        let args = json!({ "scope": "" });
        let err = resolve_scope(&args).unwrap_err();
        // Chain-rendered message (`{:#}`) starts with `scope:` so the
        // operator can see which arg was rejected. Uses anyhow's
        // alternate format so the underlying `ScopeParseError` is
        // preserved as a source in the cause chain — that's load-bearing
        // for downcasting + backtrace inspection (Copilot #211 finding).
        let rendered = format!("{err:#}");
        assert!(
            rendered.starts_with("scope:"),
            "chain-rendered error must be prefixed with `scope:`; got: {rendered}"
        );
        // Source chain must be preserved — `ScopeParseError::Empty` is
        // the underlying cause.
        let source = err.chain().nth(1).expect("source chain must be preserved");
        assert!(
            source.to_string().contains("empty"),
            "source must surface the empty-scope reason; got: {source}"
        );
    }

    #[test]
    fn errors_when_scope_arg_is_whitespace_only() {
        let args = json!({ "scope": "   \n" });
        let err = resolve_scope(&args).unwrap_err();
        assert!(format!("{err:#}").starts_with("scope:"));
    }

    #[test]
    fn errors_when_scope_has_empty_value_after_prefix() {
        // `scope: "repo:"` is structurally malformed — must surface
        // the underlying ScopeParseError, not silently fall through.
        let args = json!({ "scope": "repo:" });
        let err = resolve_scope(&args).unwrap_err();
        let rendered = format!("{err:#}");
        assert!(rendered.starts_with("scope:"), "got: {rendered}");
        assert!(rendered.contains("repo"), "got: {rendered}");
    }

    /// Pins the Copilot-#211 contract: parse failures must preserve
    /// the underlying `ScopeParseError` as a source error so that
    /// downstream code can downcast, inspect, or backtrace through
    /// the cause chain rather than seeing only a flat string.
    #[test]
    fn parse_failure_preserves_underlying_scope_parse_error_in_chain() {
        use crate::scope::ScopeParseError;

        let args = json!({ "scope": "" });
        let err = resolve_scope(&args).unwrap_err();

        // The chain must contain a `ScopeParseError` (specifically the
        // `Empty` variant for this input). Walk the chain looking for
        // a downcast match.
        let found_scope_parse_error = err
            .chain()
            .any(|e| e.downcast_ref::<ScopeParseError>().is_some());
        assert!(
            found_scope_parse_error,
            "ScopeParseError must be preserved as a source in the chain; \
             flat `anyhow!(\"scope: {{e}}\")` would have lost it"
        );

        // And the specific variant: Empty (not EmptyAfterPrefix).
        let parse_err = err
            .chain()
            .find_map(|e| e.downcast_ref::<ScopeParseError>())
            .expect("checked above");
        assert_eq!(parse_err, &ScopeParseError::Empty);
    }

    // ── type-permissive parsing ───────────────────────────────────────

    #[test]
    fn ignores_non_string_scope() {
        // If `scope` is present but isn't a string (e.g. number, null),
        // we fall through to repo_path rather than erroring. Matches
        // the existing handler ergonomics for typed-misuse — the JSON
        // schema is permissive at the boundary.
        let args = json!({
            "scope": 42,
            "repo_path": "/tmp/rosary"
        });
        let scope = resolve_scope(&args).unwrap();
        assert_eq!(scope, ScopeId::Repo("rosary".into()));
    }

    #[test]
    fn ignores_null_scope_falls_back_to_repo_path() {
        let args = json!({
            "scope": null,
            "repo_path": "/tmp/rosary"
        });
        let scope = resolve_scope(&args).unwrap();
        assert_eq!(scope, ScopeId::Repo("rosary".into()));
    }
}

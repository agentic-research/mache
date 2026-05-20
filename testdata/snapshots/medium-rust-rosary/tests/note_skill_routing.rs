//! Regression guards for the `/note` skill's repo-routing guidance.
//!
//! The `/note` skill used to hardcode `~/remotes/art/rosary` as the
//! universal target, relying on the (now-broken) reconciler hub-to-spoke
//! sync (rosary-403a1a) to fan beads out to per-repo Dolt stores. Beads
//! filed via the skill ended up stuck in rosary's hub.
//!
//! rosary-406b68: the skill must derive the target repo from the user's
//! input (file paths, repo names) and fall back to rosary ONLY for
//! genuinely meta concerns. These tests pin that contract against the
//! SKILL.md content so a future edit cannot silently regress.
//!
//! Test-as-documentation: each assertion explains why the rule exists.

use std::path::PathBuf;

fn skill_md() -> String {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("skills")
        .join("note")
        .join("SKILL.md");
    std::fs::read_to_string(&path).unwrap_or_else(|e| {
        panic!("read {}: {e}", path.display());
    })
}

#[test]
fn skill_md_does_not_hardcode_rosary_hub_for_all_calls() {
    // The old directive — `Always use repo_path: ~/remotes/art/rosary`
    // — relied on the (broken) reconciler hub-to-spoke sync. New beads
    // filed under it land in rosary's Dolt and never reach the spoke,
    // confusing rsry_list_beads in the actual target repo.
    let body = skill_md();
    let lower = body.to_lowercase();
    assert!(
        !lower.contains("always use `repo_path: ~/remotes/art/rosary`")
            && !lower.contains("always use repo_path: ~/remotes/art/rosary"),
        "SKILL.md must not hardcode the rosary hub as the universal target; \
         derive target repo from file scopes / repo names in the user's input. \
         See rosary-406b68."
    );
}

#[test]
fn skill_md_documents_file_scope_routing() {
    // Replacement guidance: callers should look at the topic + any
    // inferred file paths and route to that repo. Without this rule
    // explicit in the skill, the next agent will revert to the
    // path-of-least-thinking default.
    let body = skill_md();
    let lower = body.to_lowercase();
    assert!(
        lower.contains("file scope") || lower.contains("file path"),
        "SKILL.md must explain that the target repo is derived from file scopes / paths"
    );
    assert!(
        lower.contains("target repo") || lower.contains("specific repo"),
        "SKILL.md must explain the target-repo selection logic"
    );
}

#[test]
fn skill_md_documents_meta_fallback_to_rosary() {
    // Rosary IS still the right home for cross-repo epics, agent
    // orchestration work, and meta-level concerns about the
    // substrate itself. The skill must call this out so callers
    // don't err on the side of always-the-spoke.
    let body = skill_md();
    let lower = body.to_lowercase();
    assert!(
        lower.contains("meta") || lower.contains("orchestration") || lower.contains("cross-repo"),
        "SKILL.md must explain when rosary itself is the right target \
         (meta concerns: orchestration, cross-repo epics, skill bugs)"
    );
}

#[test]
fn skill_md_examples_show_target_repo_selection() {
    // Examples are load-bearing — agents pattern-match against them.
    // The example set must demonstrate at least one repo-specific
    // case (so the route-by-scope rule is grounded) and at least one
    // meta-routed-to-rosary case (so the fallback is grounded).
    //
    // Both halves of the assertion are deliberate: if the repo-specific
    // half passed but the rosary/meta half wasn't checked, an edit could
    // silently remove the meta examples and weaken the skill without
    // failing the test (Copilot's #206 finding).
    let body = skill_md();
    let lower = body.to_lowercase();

    let mentions_spoke_repo = [
        "notme.bot",
        "notme/",
        "cloister",
        "signet",
        "mache",
        "crumb",
        "ley-line",
    ]
    .iter()
    .any(|repo| lower.contains(repo));
    assert!(
        mentions_spoke_repo,
        "SKILL.md examples must include at least one repo-specific case \
         (notme.bot, cloister, signet, mache, etc.) so the route-by-scope \
         rule is demonstrated"
    );

    // At least one example must route to rosary as the meta home. The
    // signal: an example line targets `repo_path: ~/remotes/art/rosary`.
    // Anchoring on the lowercased canonical path avoids matching the
    // generic "rosary is the central hub" doc prose elsewhere.
    let mentions_rosary_routed_example = lower.contains("repo_path: ~/remotes/art/rosary");
    assert!(
        mentions_rosary_routed_example,
        "SKILL.md examples must include at least one rosary/meta routing case \
         (e.g. an example whose target repo is ~/remotes/art/rosary) so the \
         meta-fallback rule is grounded — not just stated in the rubric"
    );
}

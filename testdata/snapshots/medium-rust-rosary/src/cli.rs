//! CLI output formatting — colors, tables, consistent UX across all subcommands.
//!
//! Design: every subcommand uses these helpers so output looks unified.
//! Colors degrade gracefully (owo-colors respects NO_COLOR / piped output).

use owo_colors::OwoColorize;

use crate::bead::Bead;

// ---------------------------------------------------------------------------
// Status badge — colored status indicator
// ---------------------------------------------------------------------------

pub fn status_badge(status: &str) -> String {
    match status {
        "backlog" => "backlog".dimmed().to_string(),
        "open" => "open".green().to_string(),
        "in_progress" | "dispatched" => "in progress".blue().to_string(),
        "queued" => "queued".cyan().to_string(),
        "verifying" => "verifying".yellow().to_string(),
        "pr_open" => "pr open".magenta().to_string(),
        "done" | "closed" => "done".bright_black().to_string(),
        "blocked" => "blocked".red().to_string(),
        "rejected" => "rejected".red().to_string(),
        "stale" => "stale".bright_black().to_string(),
        other => other.dimmed().to_string(),
    }
}

pub fn priority_badge(p: u8) -> String {
    match p {
        0 => "P0".red().bold().to_string(),
        1 => "P1".yellow().bold().to_string(),
        2 => "P2".white().to_string(),
        _ => format!("P{p}").dimmed().to_string(),
    }
}

pub fn issue_type_badge(t: &str) -> String {
    match t {
        "bug" => "bug".red().to_string(),
        "epic" => "epic".magenta().to_string(),
        "design" => "design".cyan().to_string(),
        "research" => "research".blue().to_string(),
        "feature" => "feat".green().to_string(),
        "task" => "task".white().to_string(),
        "chore" => "chore".dimmed().to_string(),
        other => other.dimmed().to_string(),
    }
}

fn repo_label(repo: &str) -> String {
    repo.cyan().to_string()
}

/// Render a Linear identifier (e.g., "AGE-312") as a clickable terminal hyperlink.
/// Uses the OSC 8 escape sequence supported by most modern terminals.
fn linear_link(identifier: &str) -> String {
    // Extract team key to build the Linear URL
    // Linear URLs: https://linear.app/{org}/issue/{identifier}
    // We use a generic path that Linear redirects correctly
    let url = format!("https://linear.app/issue/{identifier}");
    format!("\x1b]8;;{url}\x1b\\{identifier}\x1b]8;;\x1b\\")
}

/// Format an external_ref as a clickable link if it looks like a Linear identifier.
fn external_ref_badge(ext_ref: &Option<String>) -> String {
    match ext_ref {
        Some(id)
            if id.contains('-') && id.chars().next().is_some_and(|c| c.is_ascii_uppercase()) =>
        {
            linear_link(id)
        }
        Some(id) => id.dimmed().to_string(),
        None => String::new(),
    }
}

// ---------------------------------------------------------------------------
// Bead formatting
// ---------------------------------------------------------------------------

/// One-line bead summary for lists.
pub fn bead_line(b: &Bead) -> String {
    let ext = external_ref_badge(&b.external_ref);
    if ext.is_empty() {
        format!(
            "  {} {} {} {} {}",
            b.id.dimmed(),
            priority_badge(b.priority),
            issue_type_badge(&b.issue_type),
            repo_label(&b.repo),
            b.title,
        )
    } else {
        format!(
            "  {} {} {} {} {} {}",
            b.id.dimmed(),
            ext,
            priority_badge(b.priority),
            issue_type_badge(&b.issue_type),
            repo_label(&b.repo),
            b.title,
        )
    }
}

/// One-line bead for scan output (includes status).
#[allow(dead_code)] // API surface — used by future verbose scan mode
pub fn bead_scan_line(b: &Bead) -> String {
    let ext = external_ref_badge(&b.external_ref);
    if ext.is_empty() {
        format!(
            "  {} {} {} {} {} {}",
            b.id.dimmed(),
            status_badge(&b.status),
            priority_badge(b.priority),
            issue_type_badge(&b.issue_type),
            repo_label(&b.repo),
            b.title,
        )
    } else {
        format!(
            "  {} {} {} {} {} {} {}",
            b.id.dimmed(),
            ext,
            status_badge(&b.status),
            priority_badge(b.priority),
            issue_type_badge(&b.issue_type),
            repo_label(&b.repo),
            b.title,
        )
    }
}

// ---------------------------------------------------------------------------
// Section headers
// ---------------------------------------------------------------------------

pub fn header(text: &str) -> String {
    text.bold().to_string()
}

#[allow(dead_code)] // API surface
pub fn subheader(text: &str) -> String {
    text.dimmed().to_string()
}

// ---------------------------------------------------------------------------
// Status summary — used by `rsry status` and `rsry scan`
// ---------------------------------------------------------------------------

pub fn print_status_summary(beads: &[Bead]) {
    let total = beads.len();
    let backlog = beads.iter().filter(|b| b.status == "backlog").count();
    let open = beads.iter().filter(|b| b.status == "open").count();
    let ready = beads.iter().filter(|b| b.is_ready()).count();
    let in_progress = beads
        .iter()
        .filter(|b| b.status == "in_progress" || b.status == "dispatched")
        .count();
    let blocked = beads.iter().filter(|b| b.is_blocked()).count();
    let done = beads
        .iter()
        .filter(|b| b.status == "done" || b.status == "closed")
        .count();

    let repos = count_repos(beads);

    println!(
        "{} across {} repo{}",
        format!("{total} beads").bold(),
        repos,
        if repos == 1 { "" } else { "s" }
    );
    println!(
        "  {} ready  {} open  {} active  {} blocked  {} backlog  {} done",
        ready.to_string().green().bold(),
        open.to_string().green(),
        in_progress.to_string().blue(),
        if blocked > 0 {
            blocked.to_string().red().to_string()
        } else {
            blocked.to_string().dimmed().to_string()
        },
        backlog.to_string().dimmed(),
        done.to_string().dimmed(),
    );
}

fn count_repos(beads: &[Bead]) -> usize {
    let mut repos: Vec<&str> = beads.iter().map(|b| b.repo.as_str()).collect();
    repos.sort();
    repos.dedup();
    repos.len()
}

/// Print the "Ready to work" section with top N beads.
pub fn print_ready_beads(beads: &[Bead], limit: usize) {
    let ready: Vec<&Bead> = beads.iter().filter(|b| b.is_ready()).collect();
    if ready.is_empty() {
        return;
    }

    println!();
    println!("{}", header("Ready to work:"));
    for b in ready.iter().take(limit) {
        println!("{}", bead_line(b));
    }
    if ready.len() > limit {
        println!(
            "  {}",
            format!("... and {} more", ready.len() - limit).dimmed()
        );
    }
}

// ---------------------------------------------------------------------------
// Sync output — delta-focused
// ---------------------------------------------------------------------------

pub fn sync_header(team_name: &str) {
    println!("{}", format!("Syncing with {team_name}").bold());
}

pub fn sync_linked(bead_id: &str, linear_id: &str) {
    println!(
        "  {} {} {} {}",
        "linked".blue(),
        bead_id.dimmed(),
        "->".dimmed(),
        linear_link(linear_id),
    );
}

pub fn sync_created(linear_id: &str, title: &str) {
    println!(
        "  {} {} {}",
        "created".green(),
        linear_link(linear_id),
        title.dimmed(),
    );
}

pub fn sync_closed(linear_id: &str, bead_id: &str, title: &str) {
    println!(
        "  {} {} ({}) {}",
        "closed".yellow(),
        linear_link(linear_id),
        bead_id.dimmed(),
        title.dimmed(),
    );
}

pub fn sync_error(context: &str, err: &str) {
    eprintln!("  {} {} {}", "error".red().bold(), context, err.dimmed(),);
}

pub fn sync_dry_run_prefix() -> String {
    "dry-run".magenta().to_string()
}

pub fn sync_summary(linked: usize, created: usize, closed: usize) {
    println!();
    if linked == 0 && created == 0 && closed == 0 {
        println!("  {}", "Everything in sync.".dimmed());
        return;
    }
    let mut parts = Vec::new();
    if linked > 0 {
        parts.push(format!("{linked} linked"));
    }
    if created > 0 {
        parts.push(format!("{created} created"));
    }
    if closed > 0 {
        parts.push(format!("{closed} closed"));
    }
    println!("  {}", parts.join("  ").bold());
}

// ---------------------------------------------------------------------------
// Scan output
// ---------------------------------------------------------------------------

pub fn scan_summary(beads: &[Bead]) {
    print_status_summary(beads);

    // Group by repo, show counts
    let mut repo_counts: std::collections::BTreeMap<&str, usize> =
        std::collections::BTreeMap::new();
    for b in beads {
        *repo_counts.entry(&b.repo).or_default() += 1;
    }
    if repo_counts.len() > 1 {
        println!();
        for (repo, count) in &repo_counts {
            println!("  {} {count}", repo_label(repo));
        }
    }

    print_ready_beads(beads, 10);
}

// ---------------------------------------------------------------------------
// Bead list/search output
// ---------------------------------------------------------------------------

pub fn bead_list(beads: &[Bead]) {
    if beads.is_empty() {
        println!("  {}", "No beads found.".dimmed());
        return;
    }
    for b in beads {
        println!("{}", bead_line(b));
    }
    println!("{}", format!("{} bead(s)", beads.len()).dimmed());
}

pub fn bead_search_results(beads: &[Bead], query: &str) {
    if beads.is_empty() {
        println!("  {}", format!("No beads matching '{query}'").dimmed());
        return;
    }
    for b in beads {
        println!("{}", bead_line(b));
    }
    println!("{}", format!("{} result(s)", beads.len()).dimmed());
}

pub fn bead_created(id: &str, title: &str) {
    println!("{} {} {}", "created".green(), id.bold(), title.dimmed());
}

pub fn bead_closed(id: &str) {
    println!("{} {}", "closed".yellow(), id.bold());
}

pub fn bead_commented(id: &str) {
    println!("{} {}", "commented".blue(), id.bold());
}

// ---------------------------------------------------------------------------
// Decompose output
// ---------------------------------------------------------------------------

pub fn decompose_decade(title: &str, id: &str, status: &str, thread_count: usize) {
    println!(
        "{} {} {} ({} threads)",
        header("Decade:"),
        title,
        id.dimmed(),
        thread_count,
    );
    println!("  status: {}", status_badge(status));
}

pub fn decompose_thread(name: &str, bead_count: usize) {
    println!();
    println!("  {} ({} beads)", name.bold(), bead_count,);
}

pub fn decompose_bead(channel: &str, title: &str, issue_type: &str, priority: u8) {
    println!(
        "    {} {} {} {}",
        channel.cyan(),
        priority_badge(priority),
        issue_type_badge(issue_type),
        title,
    );
}

pub fn decompose_refs(refs: &[String]) {
    println!("    refs: {}", refs.join(", ").dimmed());
}

pub fn decompose_summary(created: usize, repo: &str) {
    println!();
    println!("  {} {} beads in {}", "created".green(), created, repo,);
}

// ---------------------------------------------------------------------------
// Dispatch / daemon output
// ---------------------------------------------------------------------------

pub fn daemon_started(pid: u32, log_path: &str) {
    println!(
        "{} (PID {}) log: {}",
        "Daemon started".green(),
        pid.to_string().bold(),
        log_path.dimmed(),
    );
}

pub fn daemon_stopped(pid: u32) {
    println!("{} (PID {})", "Daemon stopped".yellow(), pid,);
}

pub fn daemon_already_running(pid: u32) {
    println!("{} (PID {})", "Daemon already running".yellow(), pid,);
}

pub fn repo_enabled(name: &str, path: &str) {
    println!("{} {} ({})", "enabled".green(), name.bold(), path.dimmed(),);
}

pub fn repo_disabled(name: &str) {
    println!("{} {}", "disabled".yellow(), name.bold());
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn status_badge_returns_string_for_all_states() {
        for s in &[
            "backlog",
            "open",
            "in_progress",
            "dispatched",
            "queued",
            "verifying",
            "done",
            "closed",
            "blocked",
            "rejected",
            "stale",
            "unknown",
        ] {
            let badge = status_badge(s);
            assert!(!badge.is_empty(), "badge for '{s}' should not be empty");
        }
    }

    #[test]
    fn priority_badge_returns_string_for_all_levels() {
        for p in 0..=4 {
            let badge = priority_badge(p);
            assert!(!badge.is_empty());
        }
    }
}

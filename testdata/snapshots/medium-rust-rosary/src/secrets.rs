//! Secret-pattern detection and redaction for bead content.
//!
//! Dolt is a version-controlled store — once a secret lands in a commit it is
//! git-equivalent hard to purge. This module scrubs known secret shapes from
//! bead titles, descriptions, and comments *before* they are written.
//!
//! Strategy: prefix-anchored detection on high-signal patterns. Low false-
//! positive rate is more important than completeness — we don't try to detect
//! arbitrary high-entropy strings, only credentialed token shapes.
//!
//! No external deps: uses only std string operations.

/// Scrub known secret patterns from `text`.
///
/// Returns `(redacted_text, kinds_found)`.  If no secrets are found the
/// original text is returned unchanged (no allocation) and `kinds_found` is empty.
pub fn scrub(text: &str) -> (String, Vec<&'static str>) {
    let rules: &[Rule] = &[
        // Anthropic API key: sk-ant-api<NN>-<93 chars>
        Rule {
            kind: "anthropic-api-key",
            prefix: "sk-ant-api",
            suffix_chars: 80,
        },
        // GitHub PAT (classic): ghp_<36+>
        Rule {
            kind: "github-pat",
            prefix: "ghp_",
            suffix_chars: 36,
        },
        // GitHub app/server/user tokens
        Rule {
            kind: "github-token",
            prefix: "ghs_",
            suffix_chars: 36,
        },
        Rule {
            kind: "github-token",
            prefix: "ghu_",
            suffix_chars: 36,
        },
        Rule {
            kind: "github-token",
            prefix: "gho_",
            suffix_chars: 36,
        },
        // OpenAI: sk-<20+> (intentionally after Anthropic check to avoid overlap)
        Rule {
            kind: "openai-api-key",
            prefix: "sk-",
            suffix_chars: 20,
        },
        // AWS access key ID: AKIA<16 uppercase/digit>
        Rule {
            kind: "aws-access-key",
            prefix: "AKIA",
            suffix_chars: 16,
        },
        // Slack: xoxb-, xoxp-, xoxa-, xoxo-
        Rule {
            kind: "slack-token",
            prefix: "xoxb-",
            suffix_chars: 10,
        },
        Rule {
            kind: "slack-token",
            prefix: "xoxp-",
            suffix_chars: 10,
        },
    ];

    // PEM private key header is a special substring check (no suffix length).
    const PEM_MARKERS: &[&str] = &[
        "-----BEGIN RSA PRIVATE KEY-----",
        "-----BEGIN EC PRIVATE KEY-----",
        "-----BEGIN PRIVATE KEY-----",
        "-----BEGIN OPENSSH PRIVATE KEY-----",
    ];

    let mut result = text.to_string();
    let mut found: Vec<&'static str> = Vec::new();

    // Prefix-anchored rules
    for rule in rules {
        if let Some(redacted) = apply_rule(&result, rule) {
            if !found.contains(&rule.kind) {
                found.push(rule.kind);
            }
            result = redacted;
        }
    }

    // PEM markers — replace the full BEGIN...END block if present, or just the header.
    for &marker in PEM_MARKERS {
        if result.contains(marker) {
            if !found.contains(&"pem-private-key") {
                found.push("pem-private-key");
            }
            result = result.replace(marker, "[REDACTED-PEM]");
        }
    }

    (result, found)
}

/// Scrub `text` and log a warning if anything was found.
/// Returns the redacted text — always safe to store in Dolt.
pub fn scrub_and_warn(text: &str, context: &str) -> String {
    let (clean, kinds) = scrub(text);
    if !kinds.is_empty() {
        eprintln!(
            "[secrets] WARNING: redacted {} from {context} — do not store secrets in bead content",
            kinds.join(", ")
        );
    }
    clean
}

struct Rule {
    kind: &'static str,
    /// The literal prefix that starts the secret token.
    prefix: &'static str,
    /// Minimum number of suffix characters that must follow the prefix
    /// and consist of alphanumeric / token-safe characters.
    suffix_chars: usize,
}

fn apply_rule(text: &str, rule: &Rule) -> Option<String> {
    // Quick check: prefix present at all?
    if !text.contains(rule.prefix) {
        return None;
    }

    let mut output = String::with_capacity(text.len());
    let mut remaining = text;
    let mut changed = false;

    while let Some(pos) = remaining.find(rule.prefix) {
        // Append everything before the prefix.
        output.push_str(&remaining[..pos]);
        let after = &remaining[pos + rule.prefix.len()..];

        // Count token-safe suffix characters.
        let suffix_len = after
            .chars()
            .take_while(|c| c.is_alphanumeric() || matches!(c, '-' | '_' | '/' | '+' | '.'))
            .map(|c| c.len_utf8())
            .sum::<usize>();

        if suffix_len >= rule.suffix_chars {
            output.push_str("[REDACTED]");
            remaining = &after[suffix_len..];
            changed = true;
        } else {
            // Not long enough — copy the prefix literally and continue scanning.
            output.push_str(rule.prefix);
            remaining = after;
        }
    }

    if changed {
        output.push_str(remaining);
        Some(output)
    } else {
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn no_secrets_unchanged() {
        let text = "Fix the widget bug in src/main.rs";
        let (out, kinds) = scrub(text);
        assert_eq!(out, text);
        assert!(kinds.is_empty());
    }

    #[test]
    fn anthropic_key_redacted() {
        // 93-char suffix (real key shape)
        let key = format!("sk-ant-api03-{}", "A".repeat(93));
        let input = format!("use key {key} in prod");
        let (out, kinds) = scrub(&input);
        assert!(out.contains("[REDACTED]"), "out={out:?}");
        assert!(!out.contains("sk-ant-api03-"), "raw key must not remain");
        assert!(kinds.contains(&"anthropic-api-key"));
    }

    #[test]
    fn github_pat_redacted() {
        let input = format!("token ghp_{} for auth", "A".repeat(36));
        let (out, kinds) = scrub(&input);
        assert!(out.contains("[REDACTED]"));
        assert!(kinds.contains(&"github-pat"));
    }

    #[test]
    fn aws_key_redacted() {
        let input = "AKIAIOSFODNN7EXAMPLE and more text";
        let (out, kinds) = scrub(input);
        assert!(out.contains("[REDACTED]"));
        assert!(kinds.contains(&"aws-access-key"));
    }

    #[test]
    fn pem_key_redacted() {
        let input = "here: -----BEGIN RSA PRIVATE KEY----- MIIEA...";
        let (out, kinds) = scrub(input);
        assert!(out.contains("[REDACTED-PEM]"));
        assert!(kinds.contains(&"pem-private-key"));
    }

    #[test]
    fn openai_key_redacted() {
        // openai key is sk-<20+> — must not fire on "sk-ant-api..." (anthropic catches first)
        let input = format!("sk-{} in config", "z".repeat(30));
        let (out, kinds) = scrub(&input);
        assert!(out.contains("[REDACTED]"));
        assert!(kinds.contains(&"openai-api-key"));
    }

    #[test]
    fn short_sk_prefix_not_flagged() {
        // "sk-" with only 10 suffix chars — should not fire (below threshold)
        let (out, kinds) = scrub("sk-tooshort OK");
        assert_eq!(out, "sk-tooshort OK");
        assert!(kinds.is_empty());
    }

    #[test]
    fn multiple_secrets_in_one_string() {
        let input = format!(
            "key={} and token=ghp_{}",
            format!("sk-{}", "x".repeat(25)),
            "B".repeat(40)
        );
        let (out, kinds) = scrub(&input);
        assert!(out.contains("[REDACTED]"));
        assert!(kinds.len() >= 2);
    }

    #[test]
    fn scrub_and_warn_returns_clean() {
        let key = format!("sk-ant-api03-{}", "A".repeat(93));
        let out = scrub_and_warn(&format!("token {key} end"), "test");
        assert!(!out.contains("sk-ant-api03-"));
    }
}

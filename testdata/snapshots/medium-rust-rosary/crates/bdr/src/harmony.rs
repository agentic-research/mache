// Harmony encoding extensions for BDR
//
// Beads ARE Harmony messages. The 8-state LTS maps to Harmony tokens:
//   open       → <|start|>user       (bead created — the prompt)
//   queued     → <|channel|>analysis  (triage reasoning)
//   dispatched → <|call|>             (agent spawned — tool invocation)
//   verifying  → <|constrain|>        (output must satisfy constraints)
//   done       → <|return|>           (final deliverable accepted)
//   rejected   → <|end|>              (terminated, no deliverable)
//   blocked    → <|channel|>commentary (waiting on dependency)
//   stale      → (message expired)

use openai_harmony::chat::{Message, Role, SystemContent};
use serde::{Deserialize, Serialize};

use crate::channels::BdrChannel;

// ---------------------------------------------------------------------------
// Harmony token constants — the bead lifecycle grammar
// ---------------------------------------------------------------------------

/// Harmony tokens that map to bead lifecycle events.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum BeadToken {
    /// Bead created — `<|start|>user<|message|>`
    Start,
    /// Triage/reasoning — `<|channel|>analysis`
    Analysis,
    /// Agent dispatched — `<|call|>`
    Call,
    /// Verification — `<|constrain|>`
    Constrain,
    /// Completed successfully — `<|return|>`
    Return,
    /// Terminated without deliverable — `<|end|>`
    End,
    /// Waiting on dependency — `<|channel|>commentary`
    Commentary,
}

impl BeadToken {
    pub fn as_harmony_str(&self) -> &str {
        match self {
            Self::Start => "<|start|>",
            Self::Analysis => "<|channel|>analysis",
            Self::Call => "<|call|>",
            Self::Constrain => "<|constrain|>",
            Self::Return => "<|return|>",
            Self::End => "<|end|>",
            Self::Commentary => "<|channel|>commentary",
        }
    }
}

/// Map a bead state string to its Harmony token.
pub fn state_to_token(state: &str) -> BeadToken {
    match state {
        "open" => BeadToken::Start,
        "queued" => BeadToken::Analysis,
        "dispatched" => BeadToken::Call,
        "verifying" => BeadToken::Constrain,
        "done" | "closed" => BeadToken::Return,
        "rejected" => BeadToken::End,
        "blocked" => BeadToken::Commentary,
        "stale" => BeadToken::End,
        _ => BeadToken::Start,
    }
}

/// Map a bead state string to its BDR channel.
pub fn state_to_channel(state: &str) -> BdrChannel {
    match state {
        "open" | "queued" => BdrChannel::Decade, // rationale/triage
        "dispatched" | "blocked" => BdrChannel::Thread, // implementation
        "verifying" | "done" | "rejected" | "stale" => BdrChannel::Bead, // deliverable
        _ => BdrChannel::Decade,
    }
}

/// A bead lifecycle event as a Harmony message.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BeadEvent {
    pub bead_id: String,
    pub repo: String,
    pub from_state: Option<String>,
    pub to_state: String,
    pub token: BeadToken,
    pub channel: BdrChannel,
}

/// Build a Harmony message from a bead state transition.
pub fn transition_message(
    bead_id: &str,
    repo: &str,
    from: Option<&str>,
    to: &str,
    detail: &str,
) -> Message {
    let token = state_to_token(to);
    let channel = state_to_channel(to);

    let content = if let Some(from_state) = from {
        format!(
            "{} [{}] {} → {} ({})\n{}",
            bead_id,
            token.as_harmony_str(),
            from_state,
            to,
            channel,
            detail
        )
    } else {
        format!(
            "{} [{}] → {} ({})\n{}",
            bead_id,
            token.as_harmony_str(),
            to,
            channel,
            detail
        )
    };

    let role = match token {
        BeadToken::Start => Role::User,
        BeadToken::Analysis | BeadToken::Commentary => Role::Assistant,
        BeadToken::Call => Role::Assistant,
        BeadToken::Constrain | BeadToken::Return | BeadToken::End => Role::Assistant,
    };

    Message::from_role_and_content(role, content)
        .with_channel(channel.as_str())
        .with_recipient(repo)
}

/// Build a constraint message from a verification tier result.
pub fn constraint_message(bead_id: &str, tier_name: &str, passed: bool, detail: &str) -> Message {
    let content = format!(
        "{} <|constrain|>{}: {} — {}",
        bead_id,
        tier_name,
        if passed { "PASS" } else { "FAIL" },
        detail
    );
    Message::from_role_and_content(Role::Tool, content)
        .with_channel("bead")
        .with_recipient(bead_id)
}

/// Create a BeadEvent from a state transition.
pub fn make_event(bead_id: &str, repo: &str, from: Option<&str>, to: &str) -> BeadEvent {
    BeadEvent {
        bead_id: bead_id.to_string(),
        repo: repo.to_string(),
        from_state: from.map(|s| s.to_string()),
        to_state: to.to_string(),
        token: state_to_token(to),
        channel: state_to_channel(to),
    }
}

// ---------------------------------------------------------------------------
// Existing message builders (kept for BDR decomposition use)
// ---------------------------------------------------------------------------

/// Build a BDR system message with decade/thread/bead channels configured.
pub fn bdr_system_content() -> SystemContent {
    SystemContent::new()
        .with_model_identity("BDR Lattice Decomposition Agent")
        .with_required_channels(["decade", "thread", "bead"])
}

/// Build a decade-channel message (ADR-level rationale).
pub fn decade_message(content: &str) -> Message {
    Message::from_role_and_content(Role::Assistant, content).with_channel("decade")
}

/// Build a thread-channel message with cross-repo routing.
pub fn thread_message(content: &str, recipient: &str) -> Message {
    Message::from_role_and_content(Role::Assistant, content)
        .with_channel("thread")
        .with_recipient(recipient)
}

/// Build a bead-channel message (atomic deliverable).
pub fn bead_message(content: &str) -> Message {
    Message::from_role_and_content(Role::Assistant, content).with_channel("bead")
}

/// Extract BDR channel from a Message, returns None if no channel or unknown channel.
pub fn message_channel(msg: &Message) -> Option<BdrChannel> {
    msg.channel
        .as_deref()
        .and_then(|ch| BdrChannel::parse(ch).ok())
}

/// Filter messages by BDR channel.
pub fn messages_by_channel(messages: &[Message], channel: BdrChannel) -> Vec<&Message> {
    messages
        .iter()
        .filter(|m| message_channel(m) == Some(channel))
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use openai_harmony::chat::Content;

    // --- Existing tests ---

    #[test]
    fn system_content_has_bdr_channels() {
        let sys = bdr_system_content();
        let cfg = sys.channel_config.unwrap();
        assert!(cfg.channel_required);
        assert_eq!(cfg.valid_channels, vec!["decade", "thread", "bead"]);
    }

    #[test]
    fn system_content_has_identity() {
        let sys = bdr_system_content();
        assert_eq!(
            sys.model_identity.unwrap(),
            "BDR Lattice Decomposition Agent"
        );
    }

    #[test]
    fn decade_message_fields() {
        let msg = decade_message("design rationale here");
        assert_eq!(msg.channel.as_deref(), Some("decade"));
        assert_eq!(msg.author.role, Role::Assistant);
        assert!(msg.recipient.is_none());
        match &msg.content[0] {
            Content::Text(t) => assert_eq!(t.text, "design rationale here"),
            _ => panic!("expected text content"),
        }
    }

    #[test]
    fn thread_message_has_recipient() {
        let msg = thread_message("implement auth", "mache:bead-85t");
        assert_eq!(msg.channel.as_deref(), Some("thread"));
        assert_eq!(msg.recipient.as_deref(), Some("mache:bead-85t"));
        assert_eq!(msg.author.role, Role::Assistant);
    }

    #[test]
    fn bead_message_fields() {
        let msg = bead_message("PR merged");
        assert_eq!(msg.channel.as_deref(), Some("bead"));
        assert!(msg.recipient.is_none());
    }

    #[test]
    fn message_channel_extracts_correctly() {
        assert_eq!(
            message_channel(&decade_message("x")),
            Some(BdrChannel::Decade)
        );
        assert_eq!(
            message_channel(&thread_message("x", "y")),
            Some(BdrChannel::Thread)
        );
        assert_eq!(message_channel(&bead_message("x")), Some(BdrChannel::Bead));
    }

    #[test]
    fn message_channel_none_for_no_channel() {
        let msg = Message::from_role_and_content(Role::User, "hello");
        assert_eq!(message_channel(&msg), None);
    }

    #[test]
    fn messages_by_channel_filters() {
        let msgs = vec![
            decade_message("rationale"),
            thread_message("impl", "repo"),
            bead_message("done"),
            decade_message("more rationale"),
        ];
        let decades = messages_by_channel(&msgs, BdrChannel::Decade);
        assert_eq!(decades.len(), 2);
        let beads = messages_by_channel(&msgs, BdrChannel::Bead);
        assert_eq!(beads.len(), 1);
    }

    #[test]
    fn message_serde_roundtrip() {
        let msg = thread_message("cross-repo work", "rosary:rsry-99c096");
        let json = serde_json::to_string(&msg).unwrap();
        let back: Message = serde_json::from_str(&json).unwrap();
        assert_eq!(back.channel.as_deref(), Some("thread"));
        assert_eq!(back.recipient.as_deref(), Some("rosary:rsry-99c096"));
    }

    // --- Bead lifecycle ↔ Harmony token tests ---

    #[test]
    fn state_to_token_maps_all_states() {
        assert_eq!(state_to_token("open"), BeadToken::Start);
        assert_eq!(state_to_token("queued"), BeadToken::Analysis);
        assert_eq!(state_to_token("dispatched"), BeadToken::Call);
        assert_eq!(state_to_token("verifying"), BeadToken::Constrain);
        assert_eq!(state_to_token("done"), BeadToken::Return);
        assert_eq!(state_to_token("rejected"), BeadToken::End);
        assert_eq!(state_to_token("blocked"), BeadToken::Commentary);
        assert_eq!(state_to_token("stale"), BeadToken::End);
    }

    #[test]
    fn state_to_channel_maps_visibility() {
        // Decade = internal (rationale/triage)
        assert_eq!(state_to_channel("open"), BdrChannel::Decade);
        assert_eq!(state_to_channel("queued"), BdrChannel::Decade);
        // Thread = team (implementation)
        assert_eq!(state_to_channel("dispatched"), BdrChannel::Thread);
        assert_eq!(state_to_channel("blocked"), BdrChannel::Thread);
        // Bead = external (deliverable)
        assert_eq!(state_to_channel("verifying"), BdrChannel::Bead);
        assert_eq!(state_to_channel("done"), BdrChannel::Bead);
        assert_eq!(state_to_channel("rejected"), BdrChannel::Bead);
    }

    #[test]
    fn transition_message_open_to_dispatched() {
        let msg = transition_message(
            "rsry-abc",
            "rosary",
            Some("open"),
            "dispatched",
            "spawned claude agent",
        );
        assert_eq!(msg.channel.as_deref(), Some("thread"));
        assert_eq!(msg.recipient.as_deref(), Some("rosary"));
        assert_eq!(msg.author.role, Role::Assistant);
        match &msg.content[0] {
            Content::Text(t) => {
                assert!(t.text.contains("rsry-abc"));
                assert!(t.text.contains("<|call|>"));
                assert!(t.text.contains("open → dispatched"));
            }
            _ => panic!("expected text"),
        }
    }

    #[test]
    fn transition_message_creation_has_user_role() {
        let msg = transition_message("rsry-abc", "rosary", None, "open", "new bead");
        assert_eq!(msg.author.role, Role::User);
        assert_eq!(msg.channel.as_deref(), Some("decade"));
    }

    #[test]
    fn transition_message_done_has_return_token() {
        let msg = transition_message(
            "rsry-abc",
            "rosary",
            Some("verifying"),
            "done",
            "all tiers pass",
        );
        match &msg.content[0] {
            Content::Text(t) => assert!(t.text.contains("<|return|>")),
            _ => panic!("expected text"),
        }
        assert_eq!(msg.channel.as_deref(), Some("bead"));
    }

    #[test]
    fn transition_message_rejected_has_end_token() {
        let msg = transition_message(
            "rsry-abc",
            "rosary",
            Some("verifying"),
            "rejected",
            "cargo test failed",
        );
        match &msg.content[0] {
            Content::Text(t) => assert!(t.text.contains("<|end|>")),
            _ => panic!("expected text"),
        }
    }

    #[test]
    fn constraint_message_pass() {
        let msg = constraint_message("rsry-abc", "cargo_test", true, "287 tests passed");
        assert_eq!(msg.author.role, Role::Tool);
        assert_eq!(msg.channel.as_deref(), Some("bead"));
        assert_eq!(msg.recipient.as_deref(), Some("rsry-abc"));
        match &msg.content[0] {
            Content::Text(t) => {
                assert!(t.text.contains("<|constrain|>cargo_test"));
                assert!(t.text.contains("PASS"));
            }
            _ => panic!("expected text"),
        }
    }

    #[test]
    fn constraint_message_fail() {
        let msg = constraint_message("rsry-abc", "clippy", false, "3 warnings");
        match &msg.content[0] {
            Content::Text(t) => {
                assert!(t.text.contains("FAIL"));
                assert!(t.text.contains("clippy"));
            }
            _ => panic!("expected text"),
        }
    }

    #[test]
    fn make_event_captures_transition() {
        let event = make_event("rsry-abc", "rosary", Some("open"), "dispatched");
        assert_eq!(event.bead_id, "rsry-abc");
        assert_eq!(event.repo, "rosary");
        assert_eq!(event.from_state, Some("open".to_string()));
        assert_eq!(event.to_state, "dispatched");
        assert_eq!(event.token, BeadToken::Call);
        assert_eq!(event.channel, BdrChannel::Thread);
    }

    #[test]
    fn make_event_creation_no_from() {
        let event = make_event("rsry-abc", "rosary", None, "open");
        assert_eq!(event.from_state, None);
        assert_eq!(event.token, BeadToken::Start);
    }

    #[test]
    fn bead_event_serde_roundtrip() {
        let event = make_event("rsry-abc", "rosary", Some("dispatched"), "verifying");
        let json = serde_json::to_string(&event).unwrap();
        let back: BeadEvent = serde_json::from_str(&json).unwrap();
        assert_eq!(event.bead_id, back.bead_id);
        assert_eq!(event.token, back.token);
        assert_eq!(event.channel, back.channel);
    }

    #[test]
    fn token_harmony_strings_are_valid() {
        // All tokens should have the <|...|> format
        let tokens = [
            BeadToken::Start,
            BeadToken::Analysis,
            BeadToken::Call,
            BeadToken::Constrain,
            BeadToken::Return,
            BeadToken::End,
            BeadToken::Commentary,
        ];
        for token in tokens {
            let s = token.as_harmony_str();
            assert!(s.starts_with("<|"), "{s} doesn't start with <|");
        }
    }

    #[test]
    fn full_lifecycle_as_messages() {
        // Simulate a complete bead lifecycle as Harmony messages
        let msgs = vec![
            transition_message("rsry-abc", "rosary", None, "open", "fix auth bug"),
            transition_message("rsry-abc", "rosary", Some("open"), "queued", "priority 1"),
            transition_message(
                "rsry-abc",
                "rosary",
                Some("queued"),
                "dispatched",
                "claude agent",
            ),
            constraint_message("rsry-abc", "cargo_check", true, "compiles"),
            constraint_message("rsry-abc", "cargo_test", true, "311 tests"),
            constraint_message("rsry-abc", "clippy", true, "clean"),
            transition_message(
                "rsry-abc",
                "rosary",
                Some("dispatched"),
                "done",
                "all tiers pass",
            ),
        ];

        // Decade messages (open, queued)
        let decades = messages_by_channel(&msgs, BdrChannel::Decade);
        assert_eq!(decades.len(), 2);

        // Thread messages (dispatched)
        let threads = messages_by_channel(&msgs, BdrChannel::Thread);
        assert_eq!(threads.len(), 1);

        // Bead messages (constraints + done)
        let beads = messages_by_channel(&msgs, BdrChannel::Bead);
        assert_eq!(beads.len(), 4); // 3 constraints + 1 done
    }
}

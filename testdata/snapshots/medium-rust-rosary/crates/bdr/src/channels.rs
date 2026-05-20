// BDR channel configuration — decade/thread/bead lattice

use openai_harmony::chat::ChannelConfig;
use serde::{Deserialize, Serialize};

/// The three BDR lattice tiers, mapping to Harmony channels.
/// Visibility escalates: Decade (internal) → Thread (team) → Bead (external).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub enum BdrChannel {
    /// ADR-level reasoning, design rationale, alternatives.
    Decade,
    /// Implementation routing, cross-repo refs, tool interactions.
    Thread,
    /// Atomic deliverable: PR, commit, closed issue.
    Bead,
}

impl BdrChannel {
    /// String representation used in Harmony channel fields.
    pub fn as_str(&self) -> &str {
        match self {
            Self::Decade => "decade",
            Self::Thread => "thread",
            Self::Bead => "bead",
        }
    }

    /// Parse from string.
    pub fn parse(s: &str) -> Result<Self, &'static str> {
        match s {
            "decade" => Ok(Self::Decade),
            "thread" => Ok(Self::Thread),
            "bead" => Ok(Self::Bead),
            _ => Err("unknown BDR channel"),
        }
    }

    /// All variants in lattice order (most internal → most external).
    pub fn all() -> [BdrChannel; 3] {
        [Self::Decade, Self::Thread, Self::Bead]
    }

    /// Visibility level: 0 = most internal (Decade), 2 = most external (Bead).
    pub fn visibility_level(&self) -> u8 {
        match self {
            Self::Decade => 0,
            Self::Thread => 1,
            Self::Bead => 2,
        }
    }
}

impl TryFrom<&str> for BdrChannel {
    type Error = &'static str;

    fn try_from(s: &str) -> Result<Self, Self::Error> {
        Self::parse(s)
    }
}

impl std::fmt::Display for BdrChannel {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.as_str())
    }
}

/// Create a Harmony ChannelConfig with BDR's three required channels.
pub fn bdr_channel_config() -> ChannelConfig {
    ChannelConfig::require_channels(["decade", "thread", "bead"])
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn channel_config_required() {
        let cfg = bdr_channel_config();
        assert!(cfg.channel_required);
    }

    #[test]
    fn channel_config_has_three_channels() {
        let cfg = bdr_channel_config();
        assert_eq!(cfg.valid_channels, vec!["decade", "thread", "bead"]);
    }

    #[test]
    fn as_str_roundtrip() {
        for ch in BdrChannel::all() {
            let s = ch.as_str();
            let parsed = BdrChannel::parse(s).unwrap();
            assert_eq!(ch, parsed);
        }
    }

    #[test]
    fn parse_invalid() {
        assert!(BdrChannel::parse("invalid").is_err());
        assert!(BdrChannel::parse("").is_err());
        assert!(BdrChannel::parse("analysis").is_err());
    }

    #[test]
    fn all_returns_three_in_order() {
        let all = BdrChannel::all();
        assert_eq!(all.len(), 3);
        assert_eq!(all[0], BdrChannel::Decade);
        assert_eq!(all[1], BdrChannel::Thread);
        assert_eq!(all[2], BdrChannel::Bead);
    }

    #[test]
    fn visibility_escalation() {
        assert!(BdrChannel::Decade.visibility_level() < BdrChannel::Thread.visibility_level());
        assert!(BdrChannel::Thread.visibility_level() < BdrChannel::Bead.visibility_level());
    }

    #[test]
    fn serde_roundtrip() {
        for ch in BdrChannel::all() {
            let json = serde_json::to_string(&ch).unwrap();
            let back: BdrChannel = serde_json::from_str(&json).unwrap();
            assert_eq!(ch, back);
        }
    }

    #[test]
    fn display_matches_as_str() {
        for ch in BdrChannel::all() {
            assert_eq!(format!("{ch}"), ch.as_str());
        }
    }
}

use bdr::channels::{BdrChannel, bdr_channel_config};

// 1. bdr_channel_config() produces ChannelConfig with channel_required: true
#[test]
fn bdr_channel_config_requires_channels() {
    let cfg = bdr_channel_config();
    assert!(cfg.channel_required);
}

// 2. bdr_channel_config().valid_channels contains exactly ["decade", "thread", "bead"]
#[test]
fn bdr_channel_config_valid_channels() {
    let cfg = bdr_channel_config();
    assert_eq!(cfg.valid_channels, vec!["decade", "thread", "bead"]);
}

// 3. BdrChannel::as_str() round-trips through try_from for all variants
#[test]
fn as_str_roundtrips_through_try_from() {
    for ch in BdrChannel::all() {
        let s = ch.as_str();
        let back = BdrChannel::try_from(s).expect("try_from should succeed for valid as_str");
        assert_eq!(back, ch, "round-trip failed for {s}");
    }
}

// 4. try_from("invalid") returns Err
#[test]
fn try_from_invalid_returns_err() {
    assert!(BdrChannel::try_from("invalid").is_err());
    assert!(BdrChannel::try_from("").is_err());
    assert!(BdrChannel::try_from("Decade").is_err()); // case-sensitive
    assert!(BdrChannel::try_from("analysis").is_err()); // Harmony name, not BDR name
}

// 5. BdrChannel::all() returns 3 variants in order [Decade, Thread, Bead]
#[test]
fn all_returns_three_variants_in_order() {
    let all = BdrChannel::all();
    assert_eq!(all.len(), 3);
    assert_eq!(all[0], BdrChannel::Decade);
    assert_eq!(all[1], BdrChannel::Thread);
    assert_eq!(all[2], BdrChannel::Bead);
}

// 6. Visibility levels: Decade < Thread < Bead (0, 1, 2)
#[test]
fn visibility_levels_ascending() {
    assert_eq!(BdrChannel::Decade.visibility_level(), 0);
    assert_eq!(BdrChannel::Thread.visibility_level(), 1);
    assert_eq!(BdrChannel::Bead.visibility_level(), 2);

    assert!(BdrChannel::Decade.visibility_level() < BdrChannel::Thread.visibility_level());
    assert!(BdrChannel::Thread.visibility_level() < BdrChannel::Bead.visibility_level());
}

// 7. BdrChannel serializes/deserializes via serde_json round-trip
#[test]
fn serde_json_roundtrip() {
    for ch in BdrChannel::all() {
        let json = serde_json::to_string(&ch).expect("serialize");
        let back: BdrChannel = serde_json::from_str(&json).expect("deserialize");
        assert_eq!(back, ch, "serde round-trip failed for {ch:?}");
    }
}

// 8. Channel ordering matches visibility escalation
#[test]
fn channel_ordering_matches_visibility_escalation() {
    let all = BdrChannel::all();
    for i in 0..all.len() - 1 {
        assert!(
            all[i].visibility_level() < all[i + 1].visibility_level(),
            "all() ordering must match visibility escalation: {:?} (vis={}) should be < {:?} (vis={})",
            all[i],
            all[i].visibility_level(),
            all[i + 1],
            all[i + 1].visibility_level(),
        );
    }
}

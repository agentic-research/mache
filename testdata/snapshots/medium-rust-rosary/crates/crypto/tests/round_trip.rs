use rosary_crypto::cipher::{decrypt_field, encrypt_field};
use rosary_crypto::classify::{FieldVisibility, classify};
use rosary_crypto::key::{derive_key, generate_key};
use rosary_crypto::projection::project_bead;

#[test]
fn encrypt_decrypt_round_trip() {
    let key = generate_key();
    let bead_id = "bead-abc";
    let field = "description";
    let plaintext = b"This is a secret description of the bead.";

    let ciphertext = encrypt_field(field, bead_id, plaintext, &key).unwrap();
    assert_ne!(ciphertext, plaintext);

    let decrypted = decrypt_field(field, bead_id, &ciphertext, &key).unwrap();
    assert_eq!(decrypted, plaintext);
}

#[test]
fn wrong_key_fails() {
    let key1 = generate_key();
    let key2 = generate_key();

    let ct = encrypt_field("description", "bead-abc", b"secret", &key1).unwrap();
    let result = decrypt_field("description", "bead-abc", &ct, &key2);
    assert!(result.is_err());
}

#[test]
fn wrong_field_name_fails() {
    let key = generate_key();
    let ct = encrypt_field("description", "bead-abc", b"secret", &key).unwrap();
    let result = decrypt_field("notes", "bead-abc", &ct, &key);
    assert!(result.is_err());
}

#[test]
fn wrong_bead_id_fails() {
    let key = generate_key();
    let ct = encrypt_field("description", "bead-abc", b"secret", &key).unwrap();
    let result = decrypt_field("description", "bead-xyz", &ct, &key);
    assert!(result.is_err());
}

#[test]
fn deterministic_nonce() {
    let key = generate_key();
    let ct1 = encrypt_field("description", "bead-abc", b"same data", &key).unwrap();
    let ct2 = encrypt_field("description", "bead-abc", b"same data", &key).unwrap();
    assert_eq!(ct1, ct2, "same input should produce same ciphertext");
}

#[test]
fn classify_public_fields() {
    assert_eq!(classify("id"), FieldVisibility::Public);
    assert_eq!(classify("title"), FieldVisibility::Public);
    assert_eq!(classify("status"), FieldVisibility::Public);
    assert_eq!(classify("priority"), FieldVisibility::Public);
    assert_eq!(classify("created_at"), FieldVisibility::Public);
}

#[test]
fn classify_private_fields() {
    assert_eq!(classify("description"), FieldVisibility::Private);
    assert_eq!(classify("owner"), FieldVisibility::Private);
    assert_eq!(classify("branch"), FieldVisibility::Private);
    assert_eq!(classify("notes"), FieldVisibility::Private);
    assert_eq!(classify("design"), FieldVisibility::Private);
}

#[test]
fn classify_unknown_defaults_private() {
    assert_eq!(classify("some_random_field"), FieldVisibility::Private);
}

#[test]
fn key_derivation_deterministic() {
    let master = generate_key();
    let k1 = derive_key(&master, "context-v1");
    let k2 = derive_key(&master, "context-v1");
    assert_eq!(k1, k2);
}

#[test]
fn key_derivation_different_contexts() {
    let master = generate_key();
    let k1 = derive_key(&master, "context-v1");
    let k2 = derive_key(&master, "context-v2");
    assert_ne!(k1, k2);
}

#[test]
fn project_bead_encrypts_private_leaves_public() {
    let key = generate_key();
    let bead = serde_json::json!({
        "id": "bead-test1",
        "title": "Fix the widget",
        "status": "open",
        "priority": "2",
        "issue_type": "bug",
        "description": "The widget is broken because of X",
        "owner": "test-user",
        "notes": "Internal implementation detail"
    });

    let projection = project_bead(&bead, &key).unwrap();

    // Public fields are cleartext
    assert_eq!(projection.id, "bead-test1");
    assert_eq!(projection.title, "Fix the widget");
    assert_eq!(projection.status.as_deref(), Some("open"));
    assert_eq!(projection.priority.as_deref(), Some("2"));

    // Private fields are encrypted (base64, not plaintext)
    let desc = projection.description.as_ref().unwrap();
    assert_ne!(desc, "The widget is broken because of X");
    assert!(desc.len() > 0);

    let owner = projection.owner.as_ref().unwrap();
    assert_ne!(owner, "test-user");

    // Decrypt and verify
    let desc_bytes =
        base64::Engine::decode(&base64::engine::general_purpose::STANDARD, desc).unwrap();
    let decrypted = decrypt_field("description", "bead-test1", &desc_bytes, &key).unwrap();
    assert_eq!(
        String::from_utf8(decrypted).unwrap(),
        "The widget is broken because of X"
    );
}

// --- cipher edge cases ---

#[test]
fn encrypt_empty_plaintext() {
    let key = generate_key();
    let ct = encrypt_field("description", "bead-abc", b"", &key).unwrap();
    let decrypted = decrypt_field("description", "bead-abc", &ct, &key).unwrap();
    assert_eq!(decrypted, b"");
}

#[test]
fn encrypt_large_plaintext() {
    let key = generate_key();
    let large = vec![0x42u8; 100_000];
    let ct = encrypt_field("description", "bead-abc", &large, &key).unwrap();
    let decrypted = decrypt_field("description", "bead-abc", &ct, &key).unwrap();
    assert_eq!(decrypted, large);
}

#[test]
fn encrypt_unicode_content() {
    let key = generate_key();
    let text = "修正ウィジェット — 日本語テスト 🔐";
    let ct = encrypt_field("description", "bead-uni", text.as_bytes(), &key).unwrap();
    let decrypted = decrypt_field("description", "bead-uni", &ct, &key).unwrap();
    assert_eq!(String::from_utf8(decrypted).unwrap(), text);
}

#[test]
fn different_fields_different_ciphertext() {
    let key = generate_key();
    let data = b"same data in both fields";
    let ct1 = encrypt_field("description", "bead-abc", data, &key).unwrap();
    let ct2 = encrypt_field("notes", "bead-abc", data, &key).unwrap();
    assert_ne!(
        ct1, ct2,
        "different field names should produce different ciphertext"
    );
}

#[test]
fn different_beads_different_ciphertext() {
    let key = generate_key();
    let data = b"same data in both beads";
    let ct1 = encrypt_field("description", "bead-aaa", data, &key).unwrap();
    let ct2 = encrypt_field("description", "bead-bbb", data, &key).unwrap();
    assert_ne!(
        ct1, ct2,
        "different bead IDs should produce different ciphertext"
    );
}

#[test]
fn ciphertext_is_longer_than_plaintext() {
    let key = generate_key();
    let plaintext = b"short";
    let ct = encrypt_field("description", "bead-abc", plaintext, &key).unwrap();
    assert!(
        ct.len() > plaintext.len(),
        "ciphertext should include auth tag overhead"
    );
    // ChaCha20-Poly1305 adds 16 bytes for the tag
    assert_eq!(ct.len(), plaintext.len() + 16);
}

#[test]
fn tampered_ciphertext_fails() {
    let key = generate_key();
    let ct = encrypt_field("description", "bead-abc", b"secret", &key).unwrap();
    let mut tampered = ct.clone();
    tampered[0] ^= 0xff;
    let result = decrypt_field("description", "bead-abc", &tampered, &key);
    assert!(
        result.is_err(),
        "tampered ciphertext should fail authentication"
    );
}

#[test]
fn truncated_ciphertext_fails() {
    let key = generate_key();
    let ct = encrypt_field("description", "bead-abc", b"secret", &key).unwrap();
    let truncated = &ct[..ct.len() - 1];
    let result = decrypt_field("description", "bead-abc", truncated, &key);
    assert!(result.is_err(), "truncated ciphertext should fail");
}

// --- classify exhaustive ---

#[test]
fn classify_all_public_fields() {
    let public = vec![
        "id",
        "title",
        "status",
        "priority",
        "issue_type",
        "created_at",
        "updated_at",
        "dependency_count",
        "dependent_count",
        "comment_count",
    ];
    for f in public {
        assert_eq!(
            classify(f),
            FieldVisibility::Public,
            "'{}' should be public",
            f
        );
    }
}

#[test]
fn classify_known_private_fields() {
    let private = vec![
        "description",
        "owner",
        "branch",
        "pr_url",
        "jj_change_id",
        "design",
        "acceptance_criteria",
        "notes",
    ];
    for f in private {
        assert_eq!(
            classify(f),
            FieldVisibility::Private,
            "'{}' should be private",
            f
        );
    }
}

// --- key ---

#[test]
fn generated_keys_are_unique() {
    let k1 = generate_key();
    let k2 = generate_key();
    assert_ne!(k1, k2);
}

#[test]
fn derived_key_differs_from_master() {
    let master = generate_key();
    let derived = derive_key(&master, "context");
    assert_ne!(master, derived);
}

// --- projection edge cases ---

#[test]
fn project_bead_missing_optional_fields() {
    let key = generate_key();
    let bead = serde_json::json!({
        "id": "bead-minimal",
        "title": "Minimal bead"
    });
    let projection = project_bead(&bead, &key).unwrap();
    assert_eq!(projection.id, "bead-minimal");
    assert_eq!(projection.title, "Minimal bead");
    assert!(projection.status.is_none());
    assert!(projection.description.is_none());
    assert!(projection.owner.is_none());
}

#[test]
fn project_bead_missing_id_fails() {
    let key = generate_key();
    let bead = serde_json::json!({
        "title": "No ID bead"
    });
    let result = project_bead(&bead, &key);
    assert!(result.is_err());
}

#[test]
fn project_bead_not_object_fails() {
    let key = generate_key();
    let bead = serde_json::json!("just a string");
    let result = project_bead(&bead, &key);
    assert!(result.is_err());
}

#[test]
fn projection_private_fields_decrypt_correctly() {
    use base64::Engine;
    let key = generate_key();
    let bead = serde_json::json!({
        "id": "bead-rt",
        "title": "Round trip all private fields",
        "description": "desc-secret",
        "owner": "owner-secret",
        "branch": "feat/secret-branch",
        "pr_url": "https://example.com/pr/1",
        "design": "design-doc-content",
        "notes": "internal-notes",
        "acceptance_criteria": "must pass all tests"
    });

    let projection = project_bead(&bead, &key).unwrap();

    let fields = vec![
        ("description", "desc-secret"),
        ("owner", "owner-secret"),
        ("branch", "feat/secret-branch"),
        ("pr_url", "https://example.com/pr/1"),
        ("design", "design-doc-content"),
        ("notes", "internal-notes"),
        ("acceptance_criteria", "must pass all tests"),
    ];

    for (field_name, expected) in fields {
        let encrypted_b64 = match field_name {
            "description" => projection.description.as_ref(),
            "owner" => projection.owner.as_ref(),
            "branch" => projection.branch.as_ref(),
            "pr_url" => projection.pr_url.as_ref(),
            "design" => projection.design.as_ref(),
            "notes" => projection.notes.as_ref(),
            "acceptance_criteria" => projection.acceptance_criteria.as_ref(),
            _ => None,
        };

        let encrypted_b64 =
            encrypted_b64.unwrap_or_else(|| panic!("{} should be present", field_name));
        assert_ne!(
            encrypted_b64, expected,
            "{} should be encrypted",
            field_name
        );

        let ct = base64::engine::general_purpose::STANDARD
            .decode(encrypted_b64)
            .unwrap();
        let decrypted = decrypt_field(field_name, "bead-rt", &ct, &key).unwrap();
        assert_eq!(
            String::from_utf8(decrypted).unwrap(),
            expected,
            "{} should decrypt to original",
            field_name
        );
    }
}

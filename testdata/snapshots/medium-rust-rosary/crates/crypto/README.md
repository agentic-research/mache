# rosary-crypto

Selective field encryption for bead federation. Beads (work items) need to be shared across rosary instances without exposing private details.

## What it does

Splits bead fields into **public** (visible to anyone) and **private** (encrypted), then projects them for federation:

**Public fields** (cleartext): `id`, `title`, `status`, `priority`, `issue_type`, `created_at`, `updated_at`, `dependency_count`, `dependent_count`, `comment_count`

**Private fields** (encrypted): `description`, `owner`, `branch`, `pr_url`, `jj_change_id`, `design`, `acceptance_criteria`, `notes`

## Encryption

- **Algorithm**: ChaCha20-Poly1305 AEAD
- **Nonces**: Deterministic — `SHA-256(bead_id || field_name)[0..12]`
- **Key derivation**: SHA-256 HKDF from a master key + context string

Deterministic nonces mean the same field on the same bead always produces the same ciphertext. This enables dedup and diffing on encrypted data without decryption.

## Usage

```rust
use rosary_crypto::cipher::{encrypt_field, decrypt_field};
use rosary_crypto::key::generate_key;

let key = generate_key();

// Encrypt a private field
let ciphertext = encrypt_field("description", "bead-abc123", b"secret details", &key)?;

// Decrypt
let plaintext = decrypt_field("description", "bead-abc123", &ciphertext, &key)?;
assert_eq!(plaintext, b"secret details");
```

### Projection

```rust
use rosary_crypto::projection::project_bead;

let bead = serde_json::json!({
    "id": "bead-abc",
    "title": "Fix the widget",
    "description": "Internal implementation details",
    "status": "open",
    "priority": "1"
});

let projection = project_bead(&bead, &key)?;
// projection.title == "Fix the widget" (cleartext)
// projection.description == Some("base64-encoded-ciphertext")
```

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE).

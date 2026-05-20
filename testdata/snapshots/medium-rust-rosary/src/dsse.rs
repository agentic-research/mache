//! Dead Simple Signing Envelope (DSSE) + in-toto Statement for handoff attestation.
//!
//! APAS L2: makes handoff files tamper-evident to external observers.
//!
//! ## Wire format
//!
//! ```json
//! {
//!   "payloadType": "application/vnd.in-toto+json",
//!   "payload": "<base64url(in-toto Statement JSON)>",
//!   "signatures": [{ "keyid": "<hex(sha256(pubkey))>", "sig": "<base64url(sig)>" }]
//! }
//! ```
//!
//! The in-toto Statement wraps the handoff JSON as its predicate:
//! ```json
//! {
//!   "_type": "https://in-toto.io/Statement/v1",
//!   "subject": [{ "name": ".rsry-handoff-N.json", "digest": { "sha256": "<hex>" } }],
//!   "predicateType": "https://rosary.dev/Handoff/v1",
//!   "predicate": { /* Handoff JSON */ }
//! }
//! ```
//!
//! ## PAE (Pre-Authentication Encoding)
//!
//! `PAE(type, body) = "DSSEv1" SP LEN(type) SP type SP LEN(body) SP body`
//!
//! Signing is optional — if no key is configured the envelope is written with
//! an empty `signatures` array. Verification rejects envelopes with signatures
//! that don't match; unsigned envelopes pass with a `NotSigned` result.

use anyhow::{Context, Result, bail};
use base64::Engine as _;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::path::Path;

const PAYLOAD_TYPE: &str = "application/vnd.in-toto+json";
const PREDICATE_TYPE: &str = "https://rosary.dev/Handoff/v1";
const STATEMENT_TYPE: &str = "https://in-toto.io/Statement/v1";

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DsseEnvelope {
    #[serde(rename = "payloadType")]
    pub payload_type: String,
    /// base64url-encoded in-toto Statement JSON.
    pub payload: String,
    pub signatures: Vec<DsseSignature>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DsseSignature {
    /// hex(sha256(public key bytes)) — identifies which key signed.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub keyid: String,
    /// base64url(Ed25519 signature over PAE(payloadType, payload)).
    pub sig: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct InTotoStatement {
    #[serde(rename = "_type")]
    pub statement_type: String,
    pub subject: Vec<InTotoSubject>,
    #[serde(rename = "predicateType")]
    pub predicate_type: String,
    /// Raw handoff JSON (serde_json::Value so it round-trips without re-encoding).
    pub predicate: serde_json::Value,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct InTotoSubject {
    pub name: String,
    pub digest: std::collections::HashMap<String, String>,
}

// ---------------------------------------------------------------------------
// Verification result
// ---------------------------------------------------------------------------

#[allow(dead_code)] // Verification surface — wired by Phase 2 observation cert path.
#[derive(Debug, PartialEq, Eq)]
pub enum VerifyResult {
    /// Signatures present and all valid.
    Valid,
    /// No signatures — envelope accepted but provenance unverified.
    NotSigned,
    /// At least one signature failed verification.
    Invalid(String),
}

impl VerifyResult {
    #[allow(dead_code)] // Consumed by ADR-0010 cert validation in Phase 2.
    pub fn is_trusted(&self) -> bool {
        matches!(self, Self::Valid)
    }
}

// ---------------------------------------------------------------------------
// PAE encoding
// ---------------------------------------------------------------------------

/// Pre-Authentication Encoding as specified by the DSSE spec.
/// `PAE(type, body) = "DSSEv1" SP len(type) SP type SP len(body) SP body`
pub fn pae(payload_type: &str, payload: &[u8]) -> Vec<u8> {
    let mut out = Vec::new();
    out.extend_from_slice(b"DSSEv1 ");
    out.extend_from_slice(payload_type.len().to_string().as_bytes());
    out.push(b' ');
    out.extend_from_slice(payload_type.as_bytes());
    out.push(b' ');
    out.extend_from_slice(payload.len().to_string().as_bytes());
    out.push(b' ');
    out.extend_from_slice(payload);
    out
}

// ---------------------------------------------------------------------------
// Envelope construction
// ---------------------------------------------------------------------------

/// Wrap handoff JSON in an in-toto Statement and produce a DSSE envelope.
///
/// `handoff_disk_bytes` MUST be the exact bytes written to disk for the handoff
/// file — they are hashed for the in-toto subject digest so external observers
/// can verify by hashing the on-disk file. This is critical because
/// `Handoff::write_to` uses pretty-printed JSON; signing over `to_vec(&handoff)`
/// would produce a digest that doesn't match the file.
///
/// Per the DSSE spec, signatures are computed over PAE applied to the **raw**
/// in-toto Statement bytes, not the base64-encoded payload field.
pub fn wrap_handoff(
    handoff_predicate: &serde_json::Value,
    handoff_filename: &str,
    handoff_disk_bytes: &[u8],
    signing_key: Option<&ed25519_dalek::SigningKey>,
) -> Result<DsseEnvelope> {
    // Subject digest = sha256 of the bytes that live on disk for the handoff.
    let mut hasher = Sha256::new();
    hasher.update(handoff_disk_bytes);
    let digest = hex::encode(hasher.finalize());

    let mut subject_digest = std::collections::HashMap::new();
    subject_digest.insert("sha256".to_string(), digest);

    let statement = InTotoStatement {
        statement_type: STATEMENT_TYPE.to_string(),
        subject: vec![InTotoSubject {
            name: handoff_filename.to_string(),
            digest: subject_digest,
        }],
        predicate_type: PREDICATE_TYPE.to_string(),
        predicate: handoff_predicate.clone(),
    };

    let statement_bytes = serde_json::to_vec(&statement).context("serialize in-toto statement")?;
    let payload_b64 = base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(&statement_bytes);

    let signatures = if let Some(key) = signing_key {
        // DSSE spec: PAE is over the raw payload bytes (the statement), not base64.
        let pae_bytes = pae(PAYLOAD_TYPE, &statement_bytes);
        use ed25519_dalek::Signer as _;
        let sig = key.sign(&pae_bytes);
        let keyid = pubkey_id(&key.verifying_key());
        vec![DsseSignature {
            keyid,
            sig: base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(sig.to_bytes()),
        }]
    } else {
        vec![]
    };

    Ok(DsseEnvelope {
        payload_type: PAYLOAD_TYPE.to_string(),
        payload: payload_b64,
        signatures,
    })
}

/// Convenience: read a handoff file from disk and produce a signed envelope for it.
/// Use this when the handoff has just been written by `Handoff::write_to`.
pub fn wrap_handoff_from_file(
    handoff_path: &Path,
    handoff_predicate: &serde_json::Value,
    signing_key: Option<&ed25519_dalek::SigningKey>,
) -> Result<DsseEnvelope> {
    let bytes = std::fs::read(handoff_path)
        .with_context(|| format!("read handoff: {}", handoff_path.display()))?;
    let filename = handoff_path
        .file_name()
        .and_then(|s| s.to_str())
        .unwrap_or("handoff.json")
        .to_string();
    wrap_handoff(handoff_predicate, &filename, &bytes, signing_key)
}

// ---------------------------------------------------------------------------
// Verification
// ---------------------------------------------------------------------------

/// Verify a DSSE envelope. Returns the decoded InTotoStatement on success.
///
/// - Empty signatures → `NotSigned` (unsigned envelope, no key needed).
/// - Non-empty signatures + verifying_key → must all pass.
/// - Non-empty signatures + no key → `Invalid` (can't verify, treat as untrusted).
#[allow(dead_code)] // Verification surface — wired by Phase 2 observation cert path.
pub fn verify_envelope(
    envelope: &DsseEnvelope,
    verifying_key: Option<&ed25519_dalek::VerifyingKey>,
) -> Result<(VerifyResult, InTotoStatement)> {
    let payload_bytes = base64::engine::general_purpose::URL_SAFE_NO_PAD
        .decode(&envelope.payload)
        .context("base64-decode payload")?;

    let statement: InTotoStatement =
        serde_json::from_slice(&payload_bytes).context("parse in-toto statement")?;

    if envelope.signatures.is_empty() {
        return Ok((VerifyResult::NotSigned, statement));
    }

    let Some(vk) = verifying_key else {
        return Ok((
            VerifyResult::Invalid("signatures present but no verifying key provided".to_string()),
            statement,
        ));
    };

    // DSSE spec: PAE is over the raw payload bytes (the decoded statement).
    let pae_bytes = pae(&envelope.payload_type, &payload_bytes);

    for sig_entry in &envelope.signatures {
        let sig_bytes = base64::engine::general_purpose::URL_SAFE_NO_PAD
            .decode(&sig_entry.sig)
            .context("base64-decode signature")?;
        let sig_array: [u8; 64] = sig_bytes
            .try_into()
            .map_err(|_| anyhow::anyhow!("signature must be 64 bytes"))?;
        let sig = ed25519_dalek::Signature::from_bytes(&sig_array);
        use ed25519_dalek::Verifier as _;
        if let Err(e) = vk.verify(&pae_bytes, &sig) {
            return Ok((
                VerifyResult::Invalid(format!("signature invalid: {e}")),
                statement,
            ));
        }
    }

    Ok((VerifyResult::Valid, statement))
}

// ---------------------------------------------------------------------------
// Key helpers
// ---------------------------------------------------------------------------

/// Load an Ed25519 signing key from a raw 32-byte key file.
pub fn load_signing_key(path: &Path) -> Result<ed25519_dalek::SigningKey> {
    let bytes =
        std::fs::read(path).with_context(|| format!("read signing key: {}", path.display()))?;
    if bytes.len() != 32 {
        bail!(
            "signing key must be 32 bytes (raw Ed25519 seed), got {}",
            bytes.len()
        );
    }
    let arr: [u8; 32] = bytes.try_into().unwrap();
    Ok(ed25519_dalek::SigningKey::from_bytes(&arr))
}

/// Derive a short key identifier: hex(sha256(pubkey bytes)).
fn pubkey_id(vk: &ed25519_dalek::VerifyingKey) -> String {
    let mut hasher = Sha256::new();
    hasher.update(vk.as_bytes());
    hex::encode(hasher.finalize())
}

// ---------------------------------------------------------------------------
// Write/read helpers
// ---------------------------------------------------------------------------

/// Write a DSSE envelope alongside a handoff file.
/// Envelope path: `{work_dir}/.rsry-handoff-{phase}.dsse.json`
pub fn write_envelope(work_dir: &Path, phase: u32, envelope: &DsseEnvelope) -> Result<()> {
    let path = work_dir.join(format!(".rsry-handoff-{phase}.dsse.json"));
    let json = serde_json::to_string_pretty(envelope).context("serialize envelope")?;
    std::fs::write(&path, json).with_context(|| format!("write envelope: {}", path.display()))
}

/// Read a DSSE envelope from disk.
#[allow(dead_code)] // Consumed by Phase 2 observation cert verification path.
pub fn read_envelope(work_dir: &Path, phase: u32) -> Result<DsseEnvelope> {
    let path = work_dir.join(format!(".rsry-handoff-{phase}.dsse.json"));
    let json = std::fs::read_to_string(&path)
        .with_context(|| format!("read envelope: {}", path.display()))?;
    serde_json::from_str(&json).context("parse envelope")
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use ed25519_dalek::SigningKey;
    use rand::rngs::OsRng;

    fn gen_key() -> SigningKey {
        SigningKey::generate(&mut OsRng)
    }

    fn sample_handoff_json() -> serde_json::Value {
        serde_json::json!({
            "phase": 0,
            "from_agent": "dev-agent",
            "bead_id": "rosary-test",
            "summary": "Fixed the thing."
        })
    }

    /// Test helper: pretty-print the handoff JSON to mimic on-disk bytes.
    fn disk_bytes(json: &serde_json::Value) -> Vec<u8> {
        serde_json::to_vec_pretty(json).unwrap()
    }

    // --- PAE ---

    #[test]
    fn pae_format_matches_spec() {
        // PAE("type", "body") = "DSSEv1 4 type 4 body"
        let result = pae("type", b"body");
        assert_eq!(result, b"DSSEv1 4 type 4 body");
    }

    #[test]
    fn pae_empty_payload() {
        let result = pae("text/plain", b"");
        assert_eq!(result, b"DSSEv1 10 text/plain 0 ");
    }

    #[test]
    fn pae_length_is_byte_count_not_char_count() {
        // "café" is 4 chars but 5 UTF-8 bytes
        let result = pae("t", "café".as_bytes());
        let expected = b"DSSEv1 1 t 5 caf\xc3\xa9";
        assert_eq!(result, expected);
    }

    // --- wrap_handoff (unsigned) ---

    #[test]
    fn wrap_unsigned_has_empty_signatures() {
        let json = sample_handoff_json();
        let env = wrap_handoff(&json, ".rsry-handoff-0.json", &disk_bytes(&json), None).unwrap();
        assert!(env.signatures.is_empty());
        assert_eq!(env.payload_type, PAYLOAD_TYPE);
    }

    #[test]
    fn wrap_payload_decodes_to_valid_statement() {
        let json = sample_handoff_json();
        let env = wrap_handoff(&json, ".rsry-handoff-0.json", &disk_bytes(&json), None).unwrap();
        let decoded = base64::engine::general_purpose::URL_SAFE_NO_PAD
            .decode(&env.payload)
            .unwrap();
        let stmt: InTotoStatement = serde_json::from_slice(&decoded).unwrap();
        assert_eq!(stmt.statement_type, STATEMENT_TYPE);
        assert_eq!(stmt.predicate_type, PREDICATE_TYPE);
        assert_eq!(stmt.subject[0].name, ".rsry-handoff-0.json");
        assert!(stmt.subject[0].digest.contains_key("sha256"));
    }

    // --- wrap_handoff (signed) ---

    #[test]
    fn wrap_signed_has_one_signature() {
        let key = gen_key();
        let json = sample_handoff_json();
        let env = wrap_handoff(
            &json,
            ".rsry-handoff-0.json",
            &disk_bytes(&json),
            Some(&key),
        )
        .unwrap();
        assert_eq!(env.signatures.len(), 1);
        assert!(!env.signatures[0].sig.is_empty());
        assert!(!env.signatures[0].keyid.is_empty());
    }

    #[test]
    fn keyid_is_sha256_of_pubkey() {
        let key = gen_key();
        let json = sample_handoff_json();
        let env = wrap_handoff(
            &json,
            ".rsry-handoff-0.json",
            &disk_bytes(&json),
            Some(&key),
        )
        .unwrap();
        let expected = pubkey_id(&key.verifying_key());
        assert_eq!(env.signatures[0].keyid, expected);
    }

    // --- verify_envelope ---

    #[test]
    fn unsigned_envelope_returns_not_signed() {
        let json = sample_handoff_json();
        let env = wrap_handoff(&json, ".rsry-handoff-0.json", &disk_bytes(&json), None).unwrap();
        let (result, _stmt) = verify_envelope(&env, None).unwrap();
        assert_eq!(result, VerifyResult::NotSigned);
    }

    #[test]
    fn signed_envelope_verifies_with_correct_key() {
        let key = gen_key();
        let json = sample_handoff_json();
        let env = wrap_handoff(
            &json,
            ".rsry-handoff-0.json",
            &disk_bytes(&json),
            Some(&key),
        )
        .unwrap();
        let (result, stmt) = verify_envelope(&env, Some(&key.verifying_key())).unwrap();
        assert_eq!(result, VerifyResult::Valid);
        assert_eq!(stmt.predicate["bead_id"], "rosary-test");
    }

    #[test]
    fn signed_envelope_wrong_key_returns_invalid() {
        let signer = gen_key();
        let wrong = gen_key();
        let json = sample_handoff_json();
        let env = wrap_handoff(
            &json,
            ".rsry-handoff-0.json",
            &disk_bytes(&json),
            Some(&signer),
        )
        .unwrap();
        let (result, _) = verify_envelope(&env, Some(&wrong.verifying_key())).unwrap();
        assert!(matches!(result, VerifyResult::Invalid(_)));
    }

    #[test]
    fn signed_envelope_no_verifying_key_returns_invalid() {
        let key = gen_key();
        let json = sample_handoff_json();
        let env = wrap_handoff(
            &json,
            ".rsry-handoff-0.json",
            &disk_bytes(&json),
            Some(&key),
        )
        .unwrap();
        let (result, _) = verify_envelope(&env, None).unwrap();
        assert!(matches!(result, VerifyResult::Invalid(_)));
    }

    #[test]
    fn tampered_payload_fails_verification() {
        let key = gen_key();
        let json = sample_handoff_json();
        let mut env = wrap_handoff(
            &json,
            ".rsry-handoff-0.json",
            &disk_bytes(&json),
            Some(&key),
        )
        .unwrap();
        // Corrupt the payload after signing
        env.payload.push_str("TAMPERED");
        let result = verify_envelope(&env, Some(&key.verifying_key()));
        // Either base64 decode fails or signature fails — both are errors
        match result {
            Ok((VerifyResult::Invalid(_), _)) => {}
            Err(_) => {}
            Ok((r, _)) => panic!("expected failure, got {r:?}"),
        }
    }

    // --- DSSE spec compliance ---

    #[test]
    fn subject_digest_matches_handoff_disk_bytes() {
        // The in-toto subject digest MUST hash the exact bytes that live on disk
        // so external observers can verify by hashing the file directly.
        let json = sample_handoff_json();
        let disk = disk_bytes(&json);
        let env = wrap_handoff(&json, ".rsry-handoff-0.json", &disk, None).unwrap();

        let stmt_bytes = base64::engine::general_purpose::URL_SAFE_NO_PAD
            .decode(&env.payload)
            .unwrap();
        let stmt: InTotoStatement = serde_json::from_slice(&stmt_bytes).unwrap();

        let mut hasher = Sha256::new();
        hasher.update(&disk);
        let expected = hex::encode(hasher.finalize());
        assert_eq!(stmt.subject[0].digest.get("sha256").unwrap(), &expected);
    }

    #[test]
    fn signature_is_over_raw_statement_bytes_not_base64() {
        // DSSE spec: SIGN over PAE(payloadType, raw_statement_bytes).
        // If we accidentally signed over the base64 text, an external verifier
        // reconstructing PAE from decoded payload bytes would reject the sig.
        let key = gen_key();
        let json = sample_handoff_json();
        let env = wrap_handoff(
            &json,
            ".rsry-handoff-0.json",
            &disk_bytes(&json),
            Some(&key),
        )
        .unwrap();

        // Reconstruct PAE the same way an external verifier would: decode the
        // payload, then PAE over those bytes.
        let payload_bytes = base64::engine::general_purpose::URL_SAFE_NO_PAD
            .decode(&env.payload)
            .unwrap();
        let pae_bytes = pae(&env.payload_type, &payload_bytes);

        let sig_bytes = base64::engine::general_purpose::URL_SAFE_NO_PAD
            .decode(&env.signatures[0].sig)
            .unwrap();
        let sig_array: [u8; 64] = sig_bytes.try_into().unwrap();
        let sig = ed25519_dalek::Signature::from_bytes(&sig_array);

        use ed25519_dalek::Verifier as _;
        key.verifying_key()
            .verify(&pae_bytes, &sig)
            .expect("signature must verify against PAE(decoded payload bytes)");
    }

    // --- key file helpers ---

    #[test]
    fn load_signing_key_roundtrip() {
        let tmp = tempfile::TempDir::new().unwrap();
        let key = gen_key();
        let path = tmp.path().join("signing.key");
        std::fs::write(&path, key.to_bytes()).unwrap();
        let loaded = load_signing_key(&path).unwrap();
        assert_eq!(loaded.to_bytes(), key.to_bytes());
    }

    #[test]
    fn load_signing_key_wrong_length_errors() {
        let tmp = tempfile::TempDir::new().unwrap();
        let path = tmp.path().join("bad.key");
        std::fs::write(&path, b"tooshort").unwrap();
        assert!(load_signing_key(&path).is_err());
    }

    // --- write/read envelope ---

    #[test]
    fn write_read_envelope_roundtrip() {
        let tmp = tempfile::TempDir::new().unwrap();
        let key = gen_key();
        let json = sample_handoff_json();
        let env = wrap_handoff(
            &json,
            ".rsry-handoff-0.json",
            &disk_bytes(&json),
            Some(&key),
        )
        .unwrap();
        write_envelope(tmp.path(), 0, &env).unwrap();
        let loaded = read_envelope(tmp.path(), 0).unwrap();
        assert_eq!(loaded.payload, env.payload);
        assert_eq!(loaded.signatures[0].sig, env.signatures[0].sig);

        // Verify the loaded envelope
        let (result, _) = verify_envelope(&loaded, Some(&key.verifying_key())).unwrap();
        assert_eq!(result, VerifyResult::Valid);
    }

    #[test]
    fn wrap_handoff_from_file_hashes_actual_disk_bytes() {
        // wrap_handoff_from_file must hash the file's bytes, so a verifier
        // who hashes the on-disk file gets the same digest in the statement.
        let tmp = tempfile::TempDir::new().unwrap();
        let key = gen_key();
        let json = sample_handoff_json();

        // Write the handoff file using pretty JSON (mimicking Handoff::write_to)
        let handoff_path = tmp.path().join(".rsry-handoff-0.json");
        std::fs::write(&handoff_path, serde_json::to_string_pretty(&json).unwrap()).unwrap();

        let env = wrap_handoff_from_file(&handoff_path, &json, Some(&key)).unwrap();

        // Verify signature is valid
        let (result, stmt) = verify_envelope(&env, Some(&key.verifying_key())).unwrap();
        assert_eq!(result, VerifyResult::Valid);

        // Subject digest must match a fresh hash of the file
        let file_bytes = std::fs::read(&handoff_path).unwrap();
        let mut hasher = Sha256::new();
        hasher.update(&file_bytes);
        let expected = hex::encode(hasher.finalize());
        assert_eq!(stmt.subject[0].digest.get("sha256").unwrap(), &expected);
        assert_eq!(stmt.subject[0].name, ".rsry-handoff-0.json");
    }
}

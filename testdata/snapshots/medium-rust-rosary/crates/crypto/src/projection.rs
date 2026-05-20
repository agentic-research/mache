use base64::Engine;
use base64::engine::general_purpose::STANDARD as BASE64;
use serde::Serialize;

use crate::cipher::encrypt_field;
use crate::error::{CryptoError, Result};

#[derive(Debug, Serialize)]
pub struct BeadProjection {
    pub id: String,
    pub title: String,
    pub status: Option<String>,
    pub priority: Option<String>,
    pub issue_type: Option<String>,
    pub created_at: Option<String>,
    pub updated_at: Option<String>,
    pub dependency_count: Option<String>,
    pub dependent_count: Option<String>,
    pub comment_count: Option<String>,

    pub description: Option<String>,
    pub owner: Option<String>,
    pub branch: Option<String>,
    pub pr_url: Option<String>,
    pub jj_change_id: Option<String>,
    pub design: Option<String>,
    pub acceptance_criteria: Option<String>,
    pub notes: Option<String>,
}

pub fn project_bead(bead_json: &serde_json::Value, key: &[u8; 32]) -> Result<BeadProjection> {
    let obj = bead_json
        .as_object()
        .ok_or_else(|| CryptoError::SerializationError("expected JSON object".into()))?;

    let bead_id = obj
        .get("id")
        .and_then(|v| v.as_str())
        .ok_or_else(|| CryptoError::SerializationError("missing id field".into()))?;

    let get_str = |name: &str| -> Option<String> {
        obj.get(name)
            .and_then(|v| v.as_str())
            .map(|s| s.to_string())
    };

    let encrypt_optional = |name: &str| -> Result<Option<String>> {
        match obj.get(name).and_then(|v| v.as_str()) {
            Some(plaintext) => {
                let ct = encrypt_field(name, bead_id, plaintext.as_bytes(), key)?;
                Ok(Some(BASE64.encode(&ct)))
            }
            None => Ok(None),
        }
    };

    Ok(BeadProjection {
        id: bead_id.to_string(),
        title: get_str("title").unwrap_or_default(),
        status: get_str("status"),
        priority: get_str("priority"),
        issue_type: get_str("issue_type"),
        created_at: get_str("created_at"),
        updated_at: get_str("updated_at"),
        dependency_count: get_str("dependency_count"),
        dependent_count: get_str("dependent_count"),
        comment_count: get_str("comment_count"),
        description: encrypt_optional("description")?,
        owner: encrypt_optional("owner")?,
        branch: encrypt_optional("branch")?,
        pr_url: encrypt_optional("pr_url")?,
        jj_change_id: encrypt_optional("jj_change_id")?,
        design: encrypt_optional("design")?,
        acceptance_criteria: encrypt_optional("acceptance_criteria")?,
        notes: encrypt_optional("notes")?,
    })
}

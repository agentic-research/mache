//! Linear webhook handler — HMAC-SHA256 verification and bead state sync.

use hmac::{Hmac, Mac};
use serde::Deserialize;
use sha2::Sha256;

use crate::bead::BeadState;

use super::AppState;

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

pub(crate) type HmacSha256 = Hmac<Sha256>;

#[derive(Debug, Deserialize)]
pub(crate) struct WebhookPayload {
    pub action: String,
    #[serde(rename = "type")]
    pub entity_type: String,
    pub data: Option<WebhookIssueData>,
    #[serde(rename = "webhookTimestamp")]
    #[allow(dead_code)]
    pub webhook_timestamp: Option<u64>,
}

#[derive(Debug, Deserialize)]
pub(crate) struct WebhookIssueData {
    pub identifier: Option<String>,
    pub state: Option<WebhookState>,
}

#[derive(Debug, Deserialize)]
pub(crate) struct WebhookState {
    pub name: String,
    #[serde(rename = "type")]
    pub state_type: String,
}

// ---------------------------------------------------------------------------
// HMAC verification
// ---------------------------------------------------------------------------

/// Verify HMAC-SHA256 signature from Linear webhook.
/// Uses constant-time comparison via the hmac crate's `verify_slice`.
pub(crate) fn verify_webhook_signature(body: &[u8], secret: &[u8], signature_hex: &str) -> bool {
    let Ok(signature_bytes) = hex::decode(signature_hex) else {
        return false;
    };
    let Ok(mut mac) = HmacSha256::new_from_slice(secret) else {
        return false;
    };
    mac.update(body);
    mac.verify_slice(&signature_bytes).is_ok()
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

/// POST /webhook — Linear webhook handler.
///
/// Receives webhook payloads from Linear, verifies HMAC-SHA256 signature,
/// and updates bead status in Dolt when an Issue state changes.
pub(crate) async fn handle_webhook(
    axum::extract::State(state): axum::extract::State<AppState>,
    headers: axum::http::HeaderMap,
    body: axum::body::Bytes,
) -> axum::response::Response {
    use axum::response::IntoResponse;

    // 1. Read webhook secret from state (loaded from config or env at startup)
    let secret = match &state.webhook_secret {
        Some(s) => s.clone(),
        None => {
            eprintln!(
                "[webhook] webhook_secret not configured (set [linear].webhook_secret in config or LINEAR_WEBHOOK_SECRET env)"
            );
            return (
                axum::http::StatusCode::INTERNAL_SERVER_ERROR,
                "webhook secret not configured",
            )
                .into_response();
        }
    };

    // 2. Extract Linear-Signature header (hex-encoded HMAC-SHA256)
    let signature = match headers
        .get("linear-signature")
        .and_then(|v| v.to_str().ok())
    {
        Some(s) => s.to_string(),
        None => {
            return (
                axum::http::StatusCode::BAD_REQUEST,
                "missing Linear-Signature header",
            )
                .into_response();
        }
    };

    // 3. Verify HMAC-SHA256 (constant-time comparison)
    if !verify_webhook_signature(&body, secret.as_bytes(), &signature) {
        eprintln!("[webhook] HMAC verification failed");
        return (axum::http::StatusCode::UNAUTHORIZED, "invalid signature").into_response();
    }

    // 4. Extract Linear-Event header for entity type filtering
    let event_type = headers
        .get("linear-event")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");

    // 5. Parse the JSON payload
    let payload: WebhookPayload = match serde_json::from_slice(&body) {
        Ok(p) => p,
        Err(e) => {
            eprintln!("[webhook] failed to parse payload: {e}");
            return (axum::http::StatusCode::BAD_REQUEST, "invalid JSON payload").into_response();
        }
    };

    // 6. Only process Issue update events
    if payload.action != "update" || payload.entity_type != "Issue" {
        eprintln!(
            "[webhook] ignoring {}/{} event (type={})",
            payload.action, payload.entity_type, event_type
        );
        return (axum::http::StatusCode::OK, "ignored").into_response();
    }

    // 7. Extract state info from the payload
    let (identifier, bead_state) = match &payload.data {
        Some(data) => {
            let ident = match &data.identifier {
                Some(id) => id.clone(),
                None => {
                    eprintln!("[webhook] Issue update missing identifier");
                    return (axum::http::StatusCode::OK, "no identifier").into_response();
                }
            };
            let state = match &data.state {
                Some(s) => BeadState::from_linear_type(&s.state_type, &s.name),
                None => {
                    eprintln!("[webhook] Issue update missing state");
                    return (axum::http::StatusCode::OK, "no state").into_response();
                }
            };
            (ident, state)
        }
        None => {
            eprintln!("[webhook] Issue update missing data");
            return (axum::http::StatusCode::OK, "no data").into_response();
        }
    };

    // 8. Find the bead by external_ref across all repos in the pool
    let mut found = false;
    for (repo_name, client) in state.pool.iter_clients() {
        match client.find_by_external_ref(&identifier).await {
            Ok(Some(bead_id)) => {
                let new_status = bead_state.to_string();
                match client.update_status(&bead_id, &new_status).await {
                    Ok(()) => {
                        eprintln!(
                            "[webhook] updated bead {bead_id} in {repo_name}: {identifier} -> {new_status}"
                        );
                        client
                            .log_event(
                                &bead_id,
                                "webhook_update",
                                &format!("Linear {identifier} state -> {new_status}"),
                            )
                            .await;
                        found = true;
                        break;
                    }
                    Err(e) => {
                        eprintln!("[webhook] failed to update bead {bead_id}: {e}");
                    }
                }
            }
            Ok(None) => continue,
            Err(e) => {
                eprintln!("[webhook] error searching {repo_name} for {identifier}: {e}");
            }
        }
    }

    if !found {
        eprintln!("[webhook] no bead found for external_ref={identifier}");
    }

    (axum::http::StatusCode::OK, "ok").into_response()
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashSet;
    use std::sync::Arc;
    use tokio::sync::RwLock;

    use crate::pool::RepoPool;

    #[test]
    fn webhook_hmac_verification_valid() {
        let secret = b"test-webhook-secret";
        let body = b"hello webhook body";

        // Compute expected HMAC
        let mut mac = HmacSha256::new_from_slice(secret).unwrap();
        mac.update(body);
        let result = mac.finalize();
        let expected_hex = hex::encode(result.into_bytes());

        assert!(verify_webhook_signature(body, secret, &expected_hex));
    }

    #[test]
    fn webhook_hmac_verification_invalid() {
        let secret = b"test-webhook-secret";
        let body = b"hello webhook body";

        // Wrong signature
        let bad_signature = "deadbeef".repeat(8); // 64 hex chars = 32 bytes
        assert!(!verify_webhook_signature(body, secret, &bad_signature));
    }

    #[test]
    fn webhook_hmac_verification_invalid_hex() {
        let secret = b"test-webhook-secret";
        let body = b"hello webhook body";

        // Not valid hex
        assert!(!verify_webhook_signature(body, secret, "not-hex!!!"));
    }

    #[test]
    fn webhook_hmac_verification_wrong_body() {
        let secret = b"test-webhook-secret";
        let body = b"original body";
        let tampered = b"tampered body";

        // Compute HMAC for original body
        let mut mac = HmacSha256::new_from_slice(secret).unwrap();
        mac.update(body);
        let sig = hex::encode(mac.finalize().into_bytes());

        // Verify against tampered body should fail
        assert!(!verify_webhook_signature(tampered, secret, &sig));
    }

    #[test]
    fn webhook_payload_parse_issue_update() {
        let raw = r#"{
            "action": "update",
            "type": "Issue",
            "data": {
                "identifier": "AGE-42",
                "state": {
                    "name": "In Progress",
                    "type": "started"
                }
            },
            "webhookTimestamp": 1710000000000
        }"#;

        let payload: WebhookPayload = serde_json::from_str(raw).unwrap();
        assert_eq!(payload.action, "update");
        assert_eq!(payload.entity_type, "Issue");
        assert_eq!(payload.webhook_timestamp, Some(1710000000000));

        let data = payload.data.unwrap();
        assert_eq!(data.identifier.unwrap(), "AGE-42");
        let state = data.state.unwrap();
        assert_eq!(state.name, "In Progress");
        assert_eq!(state.state_type, "started");

        // Verify the mapping to BeadState
        let bead_state = BeadState::from_linear_type(&state.state_type, &state.name);
        assert_eq!(bead_state, BeadState::Dispatched);
    }

    #[test]
    fn webhook_payload_parse_completed() {
        let raw = r#"{
            "action": "update",
            "type": "Issue",
            "data": {
                "identifier": "AGE-99",
                "state": {
                    "name": "Done",
                    "type": "completed"
                }
            }
        }"#;

        let payload: WebhookPayload = serde_json::from_str(raw).unwrap();
        let data = payload.data.unwrap();
        let state = data.state.unwrap();
        let bead_state = BeadState::from_linear_type(&state.state_type, &state.name);
        assert_eq!(bead_state, BeadState::Done);
    }

    #[test]
    fn webhook_payload_parse_non_issue_ignored() {
        let raw = r#"{
            "action": "create",
            "type": "Comment",
            "data": {
                "body": "some comment"
            }
        }"#;

        let payload: WebhookPayload = serde_json::from_str(raw).unwrap();
        assert_eq!(payload.entity_type, "Comment");
        // Non-Issue events should be ignored by the handler
        assert!(
            payload.entity_type != "Issue" || payload.action != "update",
            "this should not be processed as an Issue update"
        );
    }

    #[test]
    fn webhook_payload_parse_non_update_action_ignored() {
        let raw = r#"{
            "action": "create",
            "type": "Issue",
            "data": {
                "identifier": "AGE-1",
                "state": {
                    "name": "Todo",
                    "type": "unstarted"
                }
            }
        }"#;

        let payload: WebhookPayload = serde_json::from_str(raw).unwrap();
        assert_eq!(payload.action, "create");
        assert_eq!(payload.entity_type, "Issue");
        // create action should be ignored
        assert_ne!(payload.action, "update");
    }

    #[tokio::test]
    async fn webhook_rejects_missing_signature() {
        use axum::body::Body;
        use axum::http::Request;
        use tower::ServiceExt;

        let state = AppState {
            pool: Arc::new(RepoPool::empty()),
            config_path: Arc::from("test.toml"),
            sessions: Arc::new(RwLock::new(HashSet::new())),
            webhook_secret: Some(Arc::from("test-secret")),
            github_webhook_secret: None,
            backend: None,
            repo_cache: Arc::new(crate::repo_cache::RepoCache::new()),
        };

        let app = axum::Router::new()
            .route("/webhook", axum::routing::post(handle_webhook))
            .with_state(state);

        let req = Request::builder()
            .method("POST")
            .uri("/webhook")
            .header("content-type", "application/json")
            .body(Body::from(r#"{"action":"update","type":"Issue"}"#))
            .unwrap();

        let resp = app.oneshot(req).await.unwrap();
        assert_eq!(resp.status(), axum::http::StatusCode::BAD_REQUEST);
    }

    #[tokio::test]
    async fn webhook_rejects_invalid_signature() {
        use axum::body::Body;
        use axum::http::Request;
        use tower::ServiceExt;

        let state = AppState {
            pool: Arc::new(RepoPool::empty()),
            config_path: Arc::from("test.toml"),
            sessions: Arc::new(RwLock::new(HashSet::new())),
            webhook_secret: Some(Arc::from("test-secret")),
            github_webhook_secret: None,
            backend: None,
            repo_cache: Arc::new(crate::repo_cache::RepoCache::new()),
        };

        let app = axum::Router::new()
            .route("/webhook", axum::routing::post(handle_webhook))
            .with_state(state);

        let body = r#"{"action":"update","type":"Issue"}"#;
        let req = Request::builder()
            .method("POST")
            .uri("/webhook")
            .header("content-type", "application/json")
            .header(
                "linear-signature",
                "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
            )
            .body(Body::from(body))
            .unwrap();

        let resp = app.oneshot(req).await.unwrap();
        assert_eq!(resp.status(), axum::http::StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn webhook_accepts_valid_signature_ignores_non_issue() {
        use axum::body::Body;
        use axum::http::Request;
        use tower::ServiceExt;

        let secret = "test-secret-for-webhook";

        let state = AppState {
            pool: Arc::new(RepoPool::empty()),
            config_path: Arc::from("test.toml"),
            sessions: Arc::new(RwLock::new(HashSet::new())),
            webhook_secret: Some(Arc::from(secret)),
            github_webhook_secret: None,
            backend: None,
            repo_cache: Arc::new(crate::repo_cache::RepoCache::new()),
        };

        let app = axum::Router::new()
            .route("/webhook", axum::routing::post(handle_webhook))
            .with_state(state);

        let body = r#"{"action":"create","type":"Comment","data":null}"#;

        // Compute valid HMAC
        let mut mac = HmacSha256::new_from_slice(secret.as_bytes()).unwrap();
        mac.update(body.as_bytes());
        let sig = hex::encode(mac.finalize().into_bytes());

        let req = Request::builder()
            .method("POST")
            .uri("/webhook")
            .header("content-type", "application/json")
            .header("linear-signature", sig)
            .body(Body::from(body))
            .unwrap();

        let resp = app.oneshot(req).await.unwrap();
        // Valid signature, but non-Issue event -> 200 OK (ignored)
        assert_eq!(resp.status(), axum::http::StatusCode::OK);
    }
}

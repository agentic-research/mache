use rand::RngCore;
use sha2::{Digest, Sha256};

pub fn generate_key() -> [u8; 32] {
    let mut key = [0u8; 32];
    rand::thread_rng().fill_bytes(&mut key);
    key
}

pub fn derive_key(master: &[u8; 32], context: &str) -> [u8; 32] {
    let mut hasher = Sha256::new();
    hasher.update(master);
    hasher.update(context.as_bytes());
    let hash = hasher.finalize();
    let mut derived = [0u8; 32];
    derived.copy_from_slice(&hash);
    derived
}

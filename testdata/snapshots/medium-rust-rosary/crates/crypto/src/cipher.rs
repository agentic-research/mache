use chacha20poly1305::aead::generic_array::GenericArray;
use chacha20poly1305::{ChaCha20Poly1305, KeyInit, aead::Aead};
use sha2::{Digest, Sha256};

use crate::error::{CryptoError, Result};

fn derive_nonce(bead_id: &str, field_name: &str) -> [u8; 12] {
    let mut hasher = Sha256::new();
    hasher.update(bead_id.as_bytes());
    hasher.update(field_name.as_bytes());
    let hash = hasher.finalize();
    let mut nonce = [0u8; 12];
    nonce.copy_from_slice(&hash[..12]);
    nonce
}

pub fn encrypt_field(
    field_name: &str,
    bead_id: &str,
    plaintext: &[u8],
    key: &[u8; 32],
) -> Result<Vec<u8>> {
    let cipher = ChaCha20Poly1305::new(GenericArray::from_slice(key));
    let nonce_bytes = derive_nonce(bead_id, field_name);
    let nonce = GenericArray::from_slice(&nonce_bytes);

    cipher
        .encrypt(nonce, plaintext)
        .map_err(|e| CryptoError::EncryptionFailed(e.to_string()))
}

pub fn decrypt_field(
    field_name: &str,
    bead_id: &str,
    ciphertext: &[u8],
    key: &[u8; 32],
) -> Result<Vec<u8>> {
    let cipher = ChaCha20Poly1305::new(GenericArray::from_slice(key));
    let nonce_bytes = derive_nonce(bead_id, field_name);
    let nonce = GenericArray::from_slice(&nonce_bytes);

    cipher
        .decrypt(nonce, ciphertext)
        .map_err(|e| CryptoError::DecryptionFailed(e.to_string()))
}

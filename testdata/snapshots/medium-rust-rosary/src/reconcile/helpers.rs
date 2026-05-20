/// Detect language from repo contents.
pub(super) fn detect_language(path: &std::path::Path) -> String {
    if path.join("Cargo.toml").exists() {
        "rust".to_string()
    } else if path.join("go.mod").exists() {
        "go".to_string()
    } else if path.join("package.json").exists() {
        "javascript".to_string()
    } else if path.join("pyproject.toml").exists() || path.join("setup.py").exists() {
        "python".to_string()
    } else {
        "unknown".to_string()
    }
}

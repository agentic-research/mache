//! Field/algebra registry — `FieldName` → `Box<dyn FieldAlgebra>` dispatch.
//!
//! Bead `obs-registry-and-fold` (rosary-980824). Per-process singleton
//! via `OnceLock`, mirroring the pattern used in `src/plugin.rs`. The
//! registry is built once at first access with the canonical Phase 1
//! field-to-algebra mapping; new fields can be registered by plugins
//! once the substrate exposes that surface (Phase 2+).

use std::collections::BTreeMap;
use std::sync::OnceLock;

use super::algebra_chain::ChainMaxAlgebra;
use super::algebra_lww::LwwRegisterAlgebra;
use super::algebra_orset::OrSetAlgebra;
use super::{FieldAlgebra, FieldName};

/// Registry of field algebras. Lookups by `FieldName` return the
/// canonical algebra to fold that field's observations.
pub struct FieldRegistry {
    by_name: BTreeMap<FieldName, Box<dyn FieldAlgebra>>,
}

impl FieldRegistry {
    /// Build the canonical Phase 1 registry — every `FieldName`
    /// variant rosary uses gets its declared algebra. `Other(_)` is
    /// not pre-populated; plugins register their own.
    pub fn canonical() -> Self {
        let mut by_name: BTreeMap<FieldName, Box<dyn FieldAlgebra>> = BTreeMap::new();
        by_name.insert(FieldName::PipelineVerdict, Box::new(ChainMaxAlgebra));
        by_name.insert(
            FieldName::Assignee,
            Box::new(LwwRegisterAlgebra::new(FieldName::Assignee)),
        );
        by_name.insert(
            FieldName::PrUrl,
            Box::new(LwwRegisterAlgebra::new(FieldName::PrUrl)),
        );
        by_name.insert(
            FieldName::MergeSha,
            Box::new(LwwRegisterAlgebra::new(FieldName::MergeSha)),
        );
        by_name.insert(
            FieldName::Deadline,
            Box::new(LwwRegisterAlgebra::new(FieldName::Deadline)),
        );
        by_name.insert(
            FieldName::Ahead,
            Box::new(LwwRegisterAlgebra::new(FieldName::Ahead)),
        );
        by_name.insert(
            FieldName::Behind,
            Box::new(LwwRegisterAlgebra::new(FieldName::Behind)),
        );
        by_name.insert(
            FieldName::Comment,
            Box::new(OrSetAlgebra::new(FieldName::Comment)),
        );
        by_name.insert(
            FieldName::Label,
            Box::new(OrSetAlgebra::new(FieldName::Label)),
        );
        // Status is not a primitive field — it's a derivation over
        // other fields (per ADR-0010 §"Per-field algebra"). The
        // registry intentionally has no algebra for it; the fold
        // computes status separately via the flat-lattice (cross-
        // source) join in `fold.rs`. If a caller asks the registry
        // for FieldName::Status they get None, which is correct.

        Self { by_name }
    }

    /// Look up the algebra for a field. Returns `None` for fields
    /// that don't have a registered primitive algebra (e.g.
    /// `FieldName::Status`, which is a derivation, not a primitive).
    pub fn get(&self, field: &FieldName) -> Option<&dyn FieldAlgebra> {
        self.by_name.get(field).map(|b| b.as_ref())
    }
}

/// Process-wide canonical registry. Allocated lazily on first access.
pub fn global() -> &'static FieldRegistry {
    static REGISTRY: OnceLock<FieldRegistry> = OnceLock::new();
    REGISTRY.get_or_init(FieldRegistry::canonical)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn canonical_registers_all_primitive_fields() {
        let r = FieldRegistry::canonical();
        for field in [
            FieldName::PipelineVerdict,
            FieldName::Assignee,
            FieldName::PrUrl,
            FieldName::MergeSha,
            FieldName::Deadline,
            FieldName::Ahead,
            FieldName::Behind,
            FieldName::Comment,
            FieldName::Label,
        ] {
            assert!(
                r.get(&field).is_some(),
                "primitive field {field:?} must have an algebra registered"
            );
        }
    }

    #[test]
    fn status_has_no_primitive_algebra() {
        // Status is a derivation, not a primitive — registry returns
        // None by design (ADR-0010 §"Per-field algebra").
        let r = FieldRegistry::canonical();
        assert!(r.get(&FieldName::Status).is_none());
    }

    #[test]
    fn other_field_returns_none() {
        let r = FieldRegistry::canonical();
        assert!(
            r.get(&FieldName::Other("plugin_field".to_string()))
                .is_none()
        );
    }

    #[test]
    fn global_returns_same_instance() {
        let a = global();
        let b = global();
        // Same pointer means OnceLock is doing its job — no surprise
        // re-allocation across calls.
        assert!(std::ptr::eq(a, b));
    }

    #[test]
    fn registered_algebra_returns_correct_field_name() {
        let r = FieldRegistry::canonical();
        let alg = r.get(&FieldName::PipelineVerdict).unwrap();
        assert_eq!(alg.field_name(), FieldName::PipelineVerdict);
        let alg = r.get(&FieldName::Assignee).unwrap();
        assert_eq!(alg.field_name(), FieldName::Assignee);
    }
}

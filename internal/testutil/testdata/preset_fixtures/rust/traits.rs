//! Trait impls for the receiver shapes in impls.rs (mache-c777ef): the
//! reference, scoped-generic and nameless (`[u8]`, `u32`, `(u8, u8)`) forms,
//! plus a trait with a default method. The trait being implemented never
//! reaches the method name.

use std::fmt;

use crate::impls::{geometry, Cell, Grid};

// reference_type -> type: type_identifier
impl<'a> IntoIterator for &'a Grid {
    type Item = &'a u8;
    type IntoIter = std::slice::Iter<'a, u8>;

    fn into_iter(self) -> Self::IntoIter {
        self.cells.iter()
    }
}

// reference_type -> type: generic_type -> type: type_identifier
impl<'a, T: fmt::Debug> fmt::Debug for &'a Cell<T> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "Cell({:?})", self.value)
    }
}

// generic_type -> type: scoped_type_identifier -> name: type_identifier
impl<T: Default> Default for geometry::Vec2<T> {
    fn default() -> Self {
        geometry::Vec2 { x: T::default(), y: T::default() }
    }
}

pub trait Hash32 {
    fn hash32(&self) -> u32;

    fn describe(&self) -> String {
        format!("hash {}", self.hash32())
    }
}

// array_type, primitive_type, tuple_type: no type_identifier to name the
// receiver by, so the receiver is the type's own text.
impl Hash32 for [u8] {
    fn hash32(&self) -> u32 {
        self.iter().fold(0u32, |h, b| h.wrapping_mul(31).wrapping_add(*b as u32))
    }
}

impl Hash32 for u32 {
    fn hash32(&self) -> u32 {
        *self
    }
}

impl Hash32 for (u8, u8) {
    fn hash32(&self) -> u32 {
        (self.0 as u32) << 8 | self.1 as u32
    }
}

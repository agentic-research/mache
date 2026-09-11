//! Receiver shapes the rust preset must project as `methods/<Receiver>.<name>`
//! — one node per method, never the impl block (mache-c777ef). Each impl is a
//! distinct shape of the impl_item's `type:` child; the inherent impls and
//! the free functions live here, the trait impls in traits.rs.

pub mod geometry {
    pub struct Point {
        pub x: i32,
        pub y: i32,
    }

    pub struct Vec2<T> {
        pub x: T,
        pub y: T,
    }

    pub fn origin() -> Point {
        Point { x: 0, y: 0 }
    }
}

pub mod empty {
    pub struct Nothing;
}

pub struct Grid {
    cells: Vec<u8>,
}

pub struct Cell<T> {
    value: T,
}

// type_identifier
impl Grid {
    pub fn new() -> Self {
        Grid { cells: Vec::new() }
    }
}

// generic_type -> type: type_identifier
impl<T: Clone> Cell<T> {
    pub fn new(value: T) -> Self {
        Cell { value }
    }

    pub fn get(&self) -> &T {
        &self.value
    }
}

// scoped_type_identifier -> name: type_identifier
impl geometry::Point {
    pub fn new(x: i32, y: i32) -> Self {
        geometry::Point { x, y }
    }
}

pub fn free() -> u32 {
    fn nested() -> u32 {
        7
    }
    nested()
}

fn plain() -> u32 {
    let v = 1;
    v
}

use std::collections::HashMap;
use std::sync::Arc;

pub mod parser;
pub mod storage;

pub const DEFAULT_CAPACITY: usize = 64;
pub static GLOBAL_NAME: &str = "mache-fixture";

pub struct Catalog {
    entries: HashMap<String, Arc<Entry>>,
    capacity: usize,
}

pub struct Entry {
    pub id: String,
    pub kind: EntryKind,
}

pub enum EntryKind {
    File,
    Directory,
    Symlink,
}

pub trait Lookup {
    fn lookup(&self, id: &str) -> Option<&Entry>;
    fn count(&self) -> usize;
}

impl Catalog {
    pub fn new(capacity: usize) -> Self {
        Self {
            entries: HashMap::with_capacity(capacity),
            capacity,
        }
    }

    pub fn insert(&mut self, entry: Entry) -> bool {
        if self.entries.len() >= self.capacity {
            return false;
        }
        self.entries.insert(entry.id.clone(), Arc::new(entry));
        true
    }
}

impl Lookup for Catalog {
    fn lookup(&self, id: &str) -> Option<&Entry> {
        self.entries.get(id).map(|arc| arc.as_ref())
    }

    fn count(&self) -> usize {
        self.entries.len()
    }
}

pub fn open(path: &str) -> Result<Catalog, String> {
    if path.is_empty() {
        return Err("empty path".to_string());
    }
    Ok(Catalog::new(DEFAULT_CAPACITY))
}

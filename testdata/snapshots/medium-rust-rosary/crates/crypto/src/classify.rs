#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FieldVisibility {
    Public,
    Private,
}

const PUBLIC_FIELDS: &[&str] = &[
    "id",
    "title",
    "status",
    "priority",
    "issue_type",
    "created_at",
    "updated_at",
    "dependency_count",
    "dependent_count",
    "comment_count",
];

pub fn classify(field_name: &str) -> FieldVisibility {
    if PUBLIC_FIELDS.contains(&field_name) {
        FieldVisibility::Public
    } else {
        FieldVisibility::Private
    }
}

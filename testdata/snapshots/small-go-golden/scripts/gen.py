"""A non-Go source file: the Go preset has no python nodes, so this is
routed to _project_files rather than projected."""


def gen(n: int) -> list[str]:
    return [f"item-{i}" for i in range(n)]

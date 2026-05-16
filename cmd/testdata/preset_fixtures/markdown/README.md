# Mache Fixture

This is a fixture for the markdown preset. It exercises sections,
lists, code blocks, block quotes, tables, and HTML blocks.

## Sections

The schema surfaces every ATX heading as a `sections/` entry.

### Subsection

Subsections nest under their parent in the tree-sitter parse, but
the preset keeps them flat for simplicity.

## Code

Below is a fenced code block:

```go
package main

func main() {
    println("hello")
}
```

And another in shell:

```sh
mache mount --schema=markdown ~/notes
```

## Lists

- First item
- Second item
  - Nested item
- Third item

1. Ordered first
2. Ordered second
3. Ordered third

## Quotes

> This is a block quote.
> It can span multiple lines.

## Tables

| Column A | Column B |
| -------- | -------- |
| value 1  | value 2  |
| value 3  | value 4  |

## HTML

<div class="callout">
  This is a raw HTML block in markdown.
</div>

---
uid: ExampleHowItWorks
visibility: PUBLIC
tags: [memos, design]
---

## How this fork stores notes

The interface, API, authentication and migrations are upstream memos, unmodified. Storage is the only change:
`core/memofile` mirrors every memo the store writes to a markdown file, and imports files found on disk when
the server starts.

> Files are the source of truth; SQLite is the query engine.

- Front matter holds `uid`, `creator`, `created`, `updated`, `visibility`, `pinned`, `tags`, `attachments` and `relations`.
- A file without front matter is still a valid memo: the whole file becomes the body.
- Hashtags written in the body win over front matter tags, which are unioned in.
#memos #design

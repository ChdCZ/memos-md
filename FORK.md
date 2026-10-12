# memos-md: memos with markdown-file storage

This is a fork of [usememos/memos](https://github.com/usememos/memos) **v0.31.0**. The interface, API, authentication,
migrations and every other behaviour are upstream's, unmodified. One thing was added: **every memo is also stored as a
markdown file on disk**, and markdown files dropped into that directory are imported back into memos.

```
data/
├── memos/                          <- your notes, one file per memo
│   ├── CdPrWRXMusbrdVP9t2zjGW.md
│   ├── HandWritten001.md           <- written by hand, imported on startup
│   └── attachments/
│       └── <attachment uid>/<original file name>
├── memos_prod.db                   <- upstream SQLite database (queries, filters, search)
└── memos_prod.db-wal
```

## The file format

Each memo is YAML front matter plus the memo body, byte-for-byte what the editor holds:

```markdown
---
uid: CdPrWRXMusbrdVP9t2zjGW
creator: owner
created: "2026-10-12T14:21:44Z"
updated: "2026-10-12T14:21:44Z"
visibility: PUBLIC
pinned: true
tags:
    - file-backed
    - memos
---

# Hello from the API

This memo is stored as a markdown file. #file-backed #memos

- [ ] check the file
```

| Field | Notes |
| --- | --- |
| `uid` | The memo's resource name (`memos/<uid>`), and the file name |
| `creator` | Username of the author; used to resolve the owner on import |
| `created` / `updated` | RFC 3339 UTC; falling back to the file's modification time |
| `visibility` | `PRIVATE` (default), `PROTECTED`, `PUBLIC`, `SPACE`. An unknown value imports as `PRIVATE` |
| `pinned` | Honoured on import |
| `tags` | Advisory: hashtags in the body win, front matter tags are unioned in |
| `attachments` | Attachment uids, whose bytes live under `attachments/<uid>/` |
| `relations` | Referenced memo uids |

A file without front matter is a valid memo: the whole file becomes the body and the memo is imported under a uid taken
from the file name (or a fresh one), then normalised in place.

## How it works

Two small pieces, both on top of the unmodified store:

**1. Write path.** `Store` gained one optional field:

```go
// store/store.go
type MemoMirror interface {
	MirrorMemo(ctx context.Context, memo *Memo)
	RemoveMemo(ctx context.Context, uid string)
}
```

`CreateMemo`, `UpdateMemo`, `ApplyMemoMutation` (the atomic path the API uses for content, attachments and relations),
`DeleteMemo` and `DeleteMemoWithPolicy` call it after a successful write. `core/memofile` implements it: it renders the
front matter, writes the file atomically (temp file + rename), copies attachment bytes and deletes the file when the memo
is deleted. Mirroring failures are logged, never returned — the file layer is a projection the next sync repairs, so a
full disk cannot fail an API write.

**2. Read path.** `core/memofile.Sync` runs once at startup:

* imports files whose memo is not in the database (this is what makes a lost database recoverable from the notes);
* applies local edits — a file whose modification time moved is diffed against the stored content and, if it differs,
  its memo is updated through the normal store API (content, timestamps, visibility, pinned, tag payload);
* rewrites every memo back to its canonical file, and deletes files whose memo no longer exists.

A sidecar index at `data/memos/.memos-file-index.json` caches path/size/mtime/content-hash per memo so a sync does not
re-read every file. It is derived state: delete it and the next sync rebuilds it.

Re-entrancy is handled explicitly, because the sync's own store writes re-enter the mirror through the store hook. The
mirror takes a lock that recognises the goroutine already holding it and lets those nested calls through
(`core/memofile/goroutine.go`). Everything else about the store is untouched.

## Running it

```bash
# Simplest: build the SPA and the binary in one command (needs go + pnpm)
go run cmd/memos-build/main.go -o memos-md
./memos-md --memo-files --port 5230 --data ./data

# Or build it yourself — go:embed needs server/frontend/dist to exist first
cd web && pnpm install && pnpm release && cd ..
go build -o memos-md ./cmd/memos

# Docker — ./Dockerfile builds both the SPA and the binary, so nothing has to be
# built on the host first
docker compose -f docker-compose.memofiles.yml up -d --build
#   or: docker build -t memos-md . && docker run -d --name memos-md \
#         -p 5230:5230 -v "$PWD/data:/var/opt/memos" memos-md
```

`--memo-files` is the only new flag (env: `MEMOS_MEMO_FILES`). Without it the binary behaves exactly like upstream.

`data/` starts empty: the first start creates the database, and `examples/` holds two sample memos you can copy in to
see the format:

```bash
mkdir -p data/memos && cp examples/*.md data/memos/     # imported on the next start
```

### Working outside the app

```bash
# edit a memo in your editor
vim data/memos/CdPrWRXMusbrdVP9t2zjGW.md
# restart memos (or just wait for the next restart): the edit is imported

# drop in notes from anywhere
cp ~/old-notes/*.md data/memos/
# files without front matter are imported and normalised on the next start

# keep the notes in git
cd data && git init && git add memos && git commit -m "notes"
```

## What is different from upstream

| Path | Change |
| --- | --- |
| `core/memofile/` | **New.** Format, mirror, sync and tests |
| `store/store.go` | `MemoMirror` interface, `SetMemoMirror`, mirror field and the `mirroredMemo` helper |
| `store/memo.go` | Mirror calls after `CreateMemo`, `UpdateMemo`, `DeleteMemo` |
| `store/memo_attachment.go` | Mirror call after a committed `ApplyMemoMutation` |
| `store/memo_delete_policy.go` | Mirror call after a committed `DeleteMemoWithPolicy` |
| `cmd/memos/main.go` | `--memo-files` flag; mirror construction, startup sync and shutdown |
| `Dockerfile` | **New.** Upstream has no root Dockerfile; this one builds the SPA and the binary and enables the flag |
| `docker-compose.memofiles.yml` | **New.** Compose file for that image |
| `FORK.md`, `examples/` | **New.** This document and two sample memos |
| `.dockerignore`, `.gitignore` | `/web/` stays in the build context (the SPA stage needs it); local notes stay out of git |

Nothing else in `server/`, `web/`, `proto/` or the database drivers was touched. Upstream's test suites still pass; the
only failures on a Windows checkout are upstream's own symlink test (needs Developer Mode) and a timing-sensitive cache
test, both of which fail identically on a pristine v0.31.0 tree.

## Known limits

* A `.md` edited while the server is running is imported on the next start, not immediately. The 30 second SQLite
  polling loop upstream runs for rebuilds is not a file watcher; adding one is the obvious next step.
* Attachment bytes are mirrored out to `data/memos/attachments/<uid>/<name>` for portability, but uploaded files are still
  served from the database, so removing an attachment's blob from SQLite would break its download link.
* Comments are memos of their own kind and are mirrored as individual files; their `COMMENT` relation is not part of the
  front matter yet.
* This is still a single-instance design: point two servers at the same data directory and the sidecar index will fight.

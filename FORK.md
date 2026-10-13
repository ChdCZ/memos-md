# memos-md: memos with markdown-file storage

This is a fork of [usememos/memos](https://github.com/usememos/memos) **v0.31.0**. The interface, API, authentication,
migrations and every other behaviour are upstream's, unmodified. One thing was added: **every memo is also stored as a
markdown file on disk**, and markdown files dropped into that directory are imported back into memos.

This file explains *what changed and how it works*. For *how to publish, deploy, configure and troubleshoot it*, see
[GUIDE.md](GUIDE.md).

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

`data/` starts empty: the first start creates the database. Open the web UI, create the first account, and start writing —
each memo appears as `data/memos/<uid>.md` as you save it. `examples/` holds two sample memos that show the format; copy
them in at any time and they are imported (on a fresh instance they wait for the first account, then import themselves):

```bash
mkdir -p data/memos && cp examples/*.md data/memos/     # imported on the next start
```

### Notes from running it on a real host

* **Who owns `data/`.** The image starts as root, chowns the data directory, then drops to `MEMOS_UID`/`MEMOS_GID`
  (default `10001`). The default means your notes are owned by uid 10001 and you need `sudo` to edit them. Set both to
  your own ids to make the notes yours:
  ```bash
  MEMOS_UID=$(id -u) MEMOS_GID=$(id -g) docker compose -f docker-compose.memofiles.yml up -d --force-recreate
  ```
* **Go module proxy.** The build downloads modules inside the image. If `proxy.golang.org` is unreachable on your
  network the build fails with `dial tcp ... i/o timeout`; the Dockerfile defaults to `goproxy.cn` first and takes a
  `--build-arg GOPROXY=...` override.
* **Container DNS.** If containers come up without outbound DNS (`wget`/`go` time out while the host works fine), the
  daemon is using `8.8.8.8` while your network resolves through a local resolver. Point Docker at a reachable one:
  ```json
  // /etc/docker/daemon.json
  { "dns": ["127.0.0.1", "223.5.5.5", "119.29.29.29"] }
  ```
  Docker keeps `127.0.0.1` out of a container's `/etc/resolv.conf` unless it is named there, so a host that resolves
  through a local DNS proxy needs this.

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

## Verified on a real host

The image was built and driven end to end on a Debian 13 box (Docker 29.9.0, 2 GB RAM, 4 vCPU) with a real HTTP client
rather than unit tests alone. All of the following passed there:

| Check | Result |
| --- | --- |
| `docker compose -f docker-compose.memofiles.yml up -d --build` from a clean checkout | image 105 MB, container healthy |
| Create / read / update / delete a memo over the API | markdown file appears, changes in place, disappears with the memo |
| CJK title, body, hashtags and task list | indexed and mirrored correctly |
| One markdown file per memo | held after create, edit, delete and restart |
| Hand-written note dropped into `data/memos/` | imported on the next start, uid/tags/pinned/visibility kept |
| Note edited on disk with `sed` | edit reaches the database and survives the mirror's rewrite |
| Attachment upload (chunked upload RPC) + memo reference | bytes mirrored to `data/memos/attachments/<uid>/<name>`, downloadable |
| Delete the memo in the container | its markdown file is removed |
| Container restart | notes and database intact |
| `data/` committed to git | 5 files including attachments, subsequent edits show up as diffs |
| `go test ./core/memofile/` inside the container's Go toolchain | 16 tests pass |

Three bugs surfaced only under that test and are fixed, each with a regression test:

1. **Attachment bytes were never mirrored.** A managed attachment is uploaded unbound and bound in the same transaction
   that writes the memo. Binding does not change the memo file, so the "nothing changed, skip" shortcut in the mirror
   ran before the attachment pass and the bytes never landed.
2. **Uploaded bytes are not in the attachment row.** With the default LOCAL storage the row keeps metadata plus a
   `reference` path and leaves the blob column empty, so a mirror that read `attachment.Blob` copied nothing. It now
   reads the referenced file (absolute, or relative to the data directory) and only skips genuine object storage.
3. **An on-disk edit that changed tags lost its body.** The tag-merge path rebuilt the payload without the new content,
   leaving the store on the old body while its tags came from the new one; the mirror then wrote that stale body back
   over the file the user had just edited. The edit path now updates the payload in one place and the sync loop mirrors
   the refreshed memo instead of the copy it read before the import.

## Known limits

* A `.md` edited while the server is running is imported on the next start, not immediately. The 30 second SQLite
  polling loop upstream runs for rebuilds is not a file watcher; adding one is the obvious next step.
* Attachment bytes are copied into `data/memos/attachments/<uid>/<name>` for portability. The attachment's own storage
  (a file under `data/assets/` for LOCAL, or an S3 object) stays authoritative, so this copy is a mirror, not the
  original: deleting it does not break a download, and re-uploading is not needed.
* Objects stored in S3 are not copied — the bytes are not on the instance's filesystem. The attachment stays listed in
  the front matter.
* Comments are memos of their own kind and are mirrored as individual files; their `COMMENT` relation is not part of the
  front matter yet.
* This is still a single-instance design: point two servers at the same data directory and the sidecar index will fight.

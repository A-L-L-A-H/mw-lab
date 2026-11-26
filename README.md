# mw-lab

MoonWitness Sovereign Git Server & Artifact Forge in Pure Go (`CGO_ENABLED=0`, `modernc.org/sqlite`, Labstack Echo v4).

## Capabilities

- **Git Smart HTTP**: Full `git clone`, `git fetch`, `git pull`, and `git push` over `GET /:owner/:repo.git/info/refs`, `POST /:owner/:repo.git/git-upload-pack`, and `POST /:owner/:repo.git/git-receive-pack`.
- **Git LFS Batch API + Pluggable Storage**: Spec-compliant `POST /:owner/:repo.git/info/lfs/objects/batch`, `PUT /lfs/objects/:oid`, and `GET /lfs/objects/:oid` with SHA-256 verification and switchable storage:
  - **Local Disk** (default): content-addressed sharded blob store.
  - **`mw-echo` 9Drive Proxy**: when `MW_ECHO_URL` is configured, streams uploads to `/api/storage/upload` and serves downloads from `/api/storage/:id/download`.
- **GitHub / Upstream Mirrors**: Safe `git fetch --prune` mirror sync triggered via `POST /api/repos/:owner/:name/mirror` with persisted mirror status in SQLite.
- **Push Webhooks**: Records every pushed ref into `push_events` and dispatches signed (`X-MW-Lab-Signature-256`) JSON push payloads to registered webhooks (`mw-echo`, `mw-mission`, or custom endpoints).
- **Embedded RIOT Dashboard**: Zero-CDN brutalist UI served at `/`.

## Quickstart

```bash
CGO_ENABLED=0 go run ./cmd/mw-lab
```

### Environment Variables

| Variable | Default | Description |
| --- | --- | --- |
| `MW_LAB_ADDR` | `:8090` | HTTP listen address |
| `MW_LAB_DATA_DIR` | `./data` | Base directory for SQLite DB, bare repos, and local LFS blobs |
| `MW_LAB_PUBLIC_URL` | auto | Public base URL used for Git LFS batch action `href` values |
| `MW_LAB_TOKEN` | _(empty)_ | Optional Bearer token required for `/api/*` endpoints |
| `MW_ECHO_URL` | _(empty)_ | When set, proxies Git LFS blobs to `mw-echo` 9Drive |
| `MW_ECHO_TOKEN` | _(empty)_ | Optional Bearer token for `mw-echo` 9Drive requests |
| `MW_ECHO_PROJECT` | `mw-lab` | Project namespace passed to `mw-echo` `/api/storage/upload` |
| `MW_LAB_DEFAULT_WEBHOOK_URL` | _(empty)_ | Optional global (`*/*`) webhook URL seeded at startup |
| `MW_LAB_ALLOW_FILE_MIRROR` | `false` | Permit `file://` and local filesystem mirror URLs |

## HTTP Surface

- `GET /` — Embedded RIOT dashboard
- `GET /health` — Runtime health, LFS backend mode, and SQLite counts
- `GET|POST /api/repos` — List / create sovereign repositories
- `GET|PATCH|DELETE /api/repos/:owner/:name` — Inspect, update, or delete a repository
- `POST /api/repos/:owner/:name/mirror` — Trigger upstream mirror sync
- `GET /api/repos/:owner/:name/branches` — List branches
- `GET /api/repos/:owner/:name/commits` — List commits (`?ref=&limit=`)
- `GET /api/repos/:owner/:name/tree` — List directory tree (`?ref=&path=`)
- `GET /api/repos/:owner/:name/blob` — Read file blob (`?ref=&path=`)
- `GET|POST /api/webhooks`, `PATCH|DELETE /api/webhooks/:id` — Manage push webhooks
- `GET /api/lfs/objects`, `GET /api/events` — Inspect LFS objects and push deliveries
- `GET /:owner/:repo.git/info/refs` — Git Smart HTTP ref discovery
- `POST /:owner/:repo.git/git-upload-pack` — Git Smart HTTP fetch/clone
- `POST /:owner/:repo.git/git-receive-pack` — Git Smart HTTP push + webhook emission
- `POST /:owner/:repo.git/info/lfs/objects/batch` — Git LFS Batch API
- `PUT|GET /lfs/objects/:oid` — Git LFS binary object transfer

## Docker

```bash
docker build -t mw-lab .
docker run -p 8090:8090 -v mw-lab-data:/data -e MW_ECHO_URL=http://mw-echo:8080 mw-lab
```

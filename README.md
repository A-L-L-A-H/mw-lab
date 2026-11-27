# mw-lab — Sovereign Git server and artifact forge.

**Repository:** [`A-L-L-A-H/mw-lab`](https://github.com/A-L-L-A-H/mw-lab) · **Stack:** Go 1.24 · Echo · SQLite

## What it does

Implements Git Smart HTTP, Git LFS batch storage, upstream mirrors, signed push webhooks, a local CI runner, a `.plan` reader (`/api/plans` + Plans dashboard), and the RIOT forge dashboard.

## Quick start

```bash
CGO_ENABLED=0 go run ./cmd/mw-lab
```

## Verify

```bash
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go test ./...
```

## Configuration

- **Port:** 8090
- **Key configuration:** MW_LAB_ADDR · MW_LAB_DATA_DIR · MW_LAB_TOKEN · MW_LAB_PLAN_DIR
- **Plan reader:** set `MW_LAB_PLAN_DIR` to a workspace `.plan` directory (default `../.plan`); browse plans at `/` (Plans tab) or via `GET /api/plans`

```bash
docker build -t mw-lab .
docker run -p 8090:8090 -v mw-lab-data:/data mw-lab
```

## Notes

Set MW_ECHO_URL to the actual mw-echo origin (default 4310) when proxying LFS to 9Drive. Optional RID: MW_RID_ISSUER, MW_RID_JWKS_URL, MW_RID_AUDIENCE.

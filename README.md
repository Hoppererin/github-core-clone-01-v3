[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/Hoppererin/github-core-clone-01-v3/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/Hoppererin/github-core-clone-01-v3/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

# github-core-clone-01-v3

A standalone GitHub-compatible git forge in a single statically linked
Linux/amd64 binary — no UI, one process. It serves:

- **git smart HTTP** — upload-pack v2 + v0, receive-pack v0 (packfiles with
  ofs-delta/ref-delta, side-band-64k, shallow/deepen clones)
- **REST v3** — `/api/v3` (repos, branches, commits, contents, issues,
  labels, pull requests, including squash merges with real 3-way conflict
  detection)
- **GraphQL v4** — `/api/graphql` (viewer, repository, issues with cursor
  pagination, pull requests, mutations, `__type` introspection, batched
  operations)
- **MCP** — `/mcp` (modern `server/discover` era, plus the legacy
  `initialize` handshake)

## Architecture

`main.ts` is a thin Node 22 launcher: it spawns `./forge` (the static Go
binary, built with `CGO_ENABLED=0`), forwards `MAKEFUN_ENV_URL` and
`MAKEFUN_SHOP_TOKEN`, and exits with the child's exit code.

The forge itself is Go, standard library only, written from scratch:

| File | What it does |
| --- | --- |
| `main.go` | lifecycle: ephemeral port, `POST /register`, poll `GET /done`, exit 0; router + auth |
| `store.go` | in-memory store (repos, refs, objects, issues, PRs), graph walks, tree merge/diff, squash merge |
| `gitobj.go` | git objects, packfile read/write (incl. delta apply), pkt-lines, sideband |
| `githttp.go` | smart HTTP: info/refs, upload-pack v2/v0, receive-pack v0 |
| `rest.go` | REST v3 handlers |
| `graphql.go` | GraphQL parser + executor |
| `mcp.go` | MCP JSON-RPC endpoint |

Speed comes from the design, not from tricks: everything is in-memory
(`map` + `RWMutex`), hot paths allocate almost nothing, packs are written
without deltas, and the Go standard library does the HTTP, JSON and zlib
work.

## Build

```sh
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o forge .
```

## Run

```sh
MAKEFUN_ENV_URL=http://127.0.0.1:PORT MAKEFUN_SHOP_TOKEN=<token> node main.ts
```

The forge binds an ephemeral port, registers it with the env, and exits 0
when the env reports `done`.

---
Entered on [make.fun](https://make.fun/r/dhj53q2r)

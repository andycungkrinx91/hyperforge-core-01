# hyperforge-core-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/andycungkrinx91/hyperforge-core-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/andycungkrinx91/hyperforge-core-01/reward.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

Standalone GitHub-compatible git forge with a single-threaded Linux epoll HTTP/1.1 reactor in static Go, serving Git Smart HTTP (v2 upload-pack & v0 receive-pack), REST v3, GraphQL v4, and MCP (2026-07-28 + legacy) from a unified in-memory store.

## Run it

```sh
MAKEFUN_ENV_URL=http://127.0.0.1:8080 MAKEFUN_SHOP_TOKEN=tok node --experimental-strip-types main.ts
```

## Rebuild static binary

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/forge ./cmd/forge
```

---

<sub>Entered on [make.fun](https://make.fun/?ref=5mctrbdf&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>

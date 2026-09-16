# AGENTS.md — working rules for this repo

## Git identity (always)

All commits must use this identity — never the machine default:

```bash
git config user.name vsreddyh
git config user.email shouryanreddyh@gmail.com
```

Verify with `git log --format='%an %ae' -1` before pushing. Fix a wrong
author with `git commit --amend --author="vsreddyh <shouryanreddyh@gmail.com>"`.

## Toolchain

- Go only (no Python runtime in production). Pin: `go 1.24` + `toolchain go1.25.14`.
- Gate before every push: `gofmt -l`, `go vet ./...`, `go test ./...`,
  `go build ./...` — run inside `docker.io/library/golang:1.25` via Podman
  with `--network=host` (rootless Podman has no DNS otherwise) and the
  module cache mounted at `/go/pkg/mod`.
- CI (`.github/workflows/go.yml`) runs the same gate on every PR.

## Containers

- Podman only — never Docker, `docker-compose`, or `network_mode: host`
  workarounds beyond what the compose files already do.
- Build images with `podman build --network=host -f <Containerfile>`.
- `.env` and `podman-compose.yml` are untracked local ops files: never
  `git add` them (`git add -A` is banned — stage paths explicitly).

## Stacked PRs

- Feature branches stack on each other (`st/NN-*`), tracked in `gh stack`
  (currently stack #22/#25 era — check `gh stack view --short`).
- Never `git rebase <trunk>` a mid-stack branch: it replays intermediate
  layers as stale duplicates. Use `git rebase --onto <new-parent>
  <old-parent>` with the branch's OWN oldest commit as upstream — and never
  pass the branch tip itself as upstream (drops the branch's own commits;
  recover via reflog + cherry-pick).
- After force-pushes, verify the full chain with
  `git merge-base --is-ancestor <parent> <child>` per layer and re-check
  every PR's mergeability, not just the tip.

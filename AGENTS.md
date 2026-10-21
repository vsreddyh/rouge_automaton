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
- No local gate runs: never run `gofmt -l`, `go vet ./...`, `go test ./...`,
  or `go build ./...` on the dev machine. Push the branch and rely on CI
  (`.github/workflows/go.yml`), which runs the full gate on every PR.

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

## Problem review bot

- `.github/workflows/opencode-review.yml` submits an automated review
  comment on every PR (`opened`, `synchronize`, `reopened`,
  `ready_for_review`), typically within ~120s of push.
- After pushing, wait for the bot review before considering the PR done:
  `gh pr view <number> --comments`, `gh pr diff <number>`,
  `gh run list --workflow opencode-review.yml`.
- Address every bot finding (or reply why not), push fixes, and re-wait
  for the next bot pass. Iterate until both the agent and the reviewer
  (bot + human) are satisfied — no merge with open review threads.

## Agent skills

### Issue tracker

Issues live in this repo's GitHub Issues (`vsreddyh/rouge_automaton`, via the `gh` CLI). See `docs/agents/issue-tracker.md`.

### Triage labels

Default five canonical triage roles (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`). See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `GLOSSARY.md` + `docs/adr/` at the repo root, created lazily. See `docs/agents/domain.md`.

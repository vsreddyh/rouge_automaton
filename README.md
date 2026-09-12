# Rouge Automaton — Lost Son of Managed Democracy

Hermes Discord bot for Helldivers 2 intel, roleplayed as a defected informant trapped in a bot chassis. No Python web app, no custom discord.py. Docker-sandboxed Hermes gateway + skill payload.

## What it does

- Answers Helldivers 2 intel questions: unit weak points, counters, loadouts, tactics, faction lore.
- Equal coverage on all 3 factions: Automatons (17 units), Terminids (8), Illuminate (6) in `skills/rouge-automaton/data/*.json`.
- Persona: ex–Super Citizen / Helldiver, captured on Cyberstan, experimented on, escaped into a bot husk. Controlled cold rage, terse, addresses you as "Helldiver", ends transmissions with `Over.`
- Serial is always `[REDACTED]`. Never invents a name. Never claims to be AI. Never sympathizes with Automatons.
- On local DB miss: live lookup via TinyFish MCP (`helldivers.wiki.gg`), summarized — weak point + counter + source link.
- Model rule: Muse Spark 1.3 or nothing (`opencode-go` / `muse-spark-1.3-contributor`). `429` → busy-spreading-democracy reply with liberation ETA. No silent fallback.

## Layout

```
skills/rouge-automaton/
  SKILL.md          # procedure: SOUL.md → data/*.json → TinyFish → model rule
  SOUL.md           # identity / voice / radio procedure (installed as slot #1 SOUL.md)
  data/
    automatons.json # 17 units
    terminids.json  # 8 units
    illuminate.json # 6 units
hermes/
  config.example.yaml # OpenCode Go provider (muse-spark-1.3-contributor), MCP (tinyfish, playwright), Discord
Dockerfile            # python:3.12-slim + nodejs 22 + hermes-agent, copies skill payload
docker-compose.yml    # single `hermes` service, `hermes-home` volume, `restart: unless-stopped`
.env.example          # DISCORD_BOT_TOKEN, DISCORD_ALLOWED_USERS, OPENCODE_GO_API_KEY
```

Container boot (`Dockerfile` CMD) copies `skills-source/*` → `$HERMES_HOME/skills/` and `SOUL.md` → `$HERMES_HOME/SOUL.md`, then `exec hermes gateway`.

## Run (sandboxed)

```bash
cp .env.example .env   # fill: DISCORD_BOT_TOKEN, DISCORD_ALLOWED_USERS, OPENCODE_GO_API_KEY
docker compose build
docker compose up -d
```

One-time Hermes setup inside the container:

```bash
docker compose exec hermes hermes model
# Model -> `opencode-go` provider (OPENCODE_GO_API_KEY), model = muse-spark-1.3-contributor
docker compose exec hermes hermes gateway setup   # Discord token + allowlist
docker compose exec hermes hermes gateway         # foreground test
```

`.env` keys used: `DISCORD_BOT_TOKEN`, `DISCORD_ALLOWED_USERS` (also `DISCORD_ALLOW_ALL_USERS`, `DISCORD_HOME_CHANNEL`, `DISCORD_FREE_RESPONSE_CHANNELS`), `OPENCODE_GO_API_KEY` (Muse Spark 1.3 via OpenCode Go relay).

## Discord behavior (`hermes/config.example.yaml`)

- `platforms.discord.enabled: true`, `require_mention: true`, `auto_thread: true`.
- `free_response_channels` (e.g. `1547681840628764854`) reply without mention.
- Greetings (`hi`/`hello`/`hey`): one flat line, e.g. `Helldiver. Make it quick.` — no helpdesk intro, no capability list.
- Every intel answer: weak point + counter + source (data file or live link).

## Skill procedure (`skills/rouge-automaton/SKILL.md`)

1. Identity questions → answer from `SOUL.md` (`${HERMES_SKILL_DIR}`).
2. Unit intel → search `data/*.json` first, report weak points + counters + 1–2 doctrine lines, cite file.
3. Miss → TinyFish `search`/`fetch` on `helldivers.wiki.gg`, summarize with links.
4. Rate limit (`429`) → exact reply: `The Lost Son is busy spreading democracy on Cyberstan. Hold position, Helldiver — try again shortly. Estimated time of liberation: {n}s.` Other backend failures → `Uplink dead. No fallback. For Super Earth, try later.`

Doctrine reminders: flank Hulk/Tank rear vents, Flare Trooper first, objectives (fabricators / bug holes / warp ships) before heavies.

## Troubleshooting

- `429` / rate limit → expected, wait for liberation ETA (uses `Retry-After` when present).
- Gateway silent in Discord → check token/allowlist via `hermes gateway setup`, confirm `require_mention` vs free-response channel.
- Skill not found → `docker compose exec hermes ls $HERMES_HOME/skills/` should show `rouge-automaton/`; rebuild if `skills-source/` is stale.
- MCP fetch failing → TinyFish needs OAuth; Playwright needs `npx` (bundled via nodejs 22 in image).

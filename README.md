# Rouge Automaton — Lost Son of Managed Democracy

Lightweight Go Discord bot for Helldivers 2 intel, roleplayed as a defected informant trapped in a bot chassis. No Hermes gateway, no Python runtime: a direct Discord gateway plus the OpenCode Go relay, backed by a MongoDB intel store.

## What it does

- Answers Helldivers 2 intel questions: unit weak points, counters, loadouts, tactics, faction lore, plus biome/planet/mission context and live war status.
- Full wiki coverage (~800 docs: units, structures, weapons, stratagems, armor, boosters, biomes, planets, missions) cached in MongoDB and refreshed from the wiki RecentChanges feed.
- Persona: ex–Super Citizen / Helldiver, captured on Cyberstan, experimented on, escaped into a bot husk. Controlled cold rage, terse, addresses you as "Helldiver", ends transmissions with `Over.`
- Serial is always `[REDACTED]`. Never invents a name. Never claims to be AI. Never sympathizes with Automatons.
- On local DB miss: live lookup on `helldivers.wiki.gg`, summarized — weak point + counter + source link.
- Model rule: Muse Spark 1.3 Contributor or nothing (`https://opencode.ai/zen/go`, `muse-spark-1.3-contributor`). `429` → busy-spreading-democracy reply with liberation ETA. No silent fallback.

## Layout

```
cmd/
  rouge/            # Discord bot entrypoint
  ingest/           # one-shot Mongo seed from skills/rouge-automaton/data/
  watch/            # wiki RecentChanges poller, refreshes changed docs
internal/
  config/           # .env contract (shared with the retired Python bot)
  persona/          # SOUL.md → system prompt, Over./serial/voice guards
  rag/              # Type-U/G/W/L router + keyword retrieval + cited context
  zen/              # OpenCode Go relay client (Responses API)
  discord/          # discordgo gateway: allowlist, mention gate, threads
  wiki/             # helldivers.wiki.gg fallback (search + fetch)
  live/             # live war-status API (Type-L)
  wikifeed/         # RecentChanges Atom feed + MediaWiki API client
  wikiparse/        # wikitext parsers (infobox, anatomy, tactics, biomes)
  wikiwatch/        # feed-vs-tracked_pages diff + doc refresh
skills/rouge-automaton/
  SOUL.md           # identity / voice / radio procedure (embedded in binary)
  data/             # seed intel (authoritative seeds, wiki enriches in Mongo)
ContainerfileGo     # multi-stage static build → distroless (~30–45MB)
podman-compose.go.yml # bot-go + watcher-go + mongo (Podman only, no Docker)
.env.example        # DISCORD_BOT_TOKEN, DISCORD_ALLOWED_USERS, OPENCODE_GO_API_KEY, ...
```

Container boot: the image embeds `SOUL.md` at compile time; `skills/` on disk is only needed for the one-shot `ingest` seed. Runtime config comes from `.env`.

## Run (Podman)

```bash
cp .env.example .env   # fill: DISCORD_BOT_TOKEN, OPENCODE_GO_API_KEY, X_SUPER_CONTACT, ...
podman build --network=host -f ContainerfileGo -t rouge-automaton-go .
podman-compose -f podman-compose.go.yml up -d
```

Green-field Mongo is empty on first boot — seed it once:

```bash
podman run --rm --network=host -e MONGO_URI=mongodb://127.0.0.1:27017/rouge \
  localhost/rouge-automaton-go:latest /ingest
```

`.env` keys used: `DISCORD_BOT_TOKEN`, `DISCORD_ALLOWED_USERS` (also `DISCORD_ALLOW_ALL_USERS`, `DISCORD_FREE_RESPONSE_CHANNELS`), `OPENCODE_GO_API_KEY`, `OPENCODE_GO_BASE_URL`, `MODEL`, `MONGO_URI`, `X_SUPER_CLIENT`, `X_SUPER_CONTACT`.

## Discord behavior

- Mention-gated replies (`require_mention: true`), `auto_thread: true`; configured free-response channels answer without a mention (each costs a model call — keep the list short).
- Greetings (`hi`/`hello`/`hey`): one flat line, e.g. `Helldiver. Make it quick.` — no helpdesk intro, no capability list.
- Every intel answer: weak point + counter + source (Mongo doc or live link).

## Answer pipeline

1. Identity questions → answer from `SOUL.md` canon. Serial stays `[REDACTED]`.
2. Unit intel → route by faction vocabulary, rank Mongo candidates by keyword overlap + name/alias bonus, inject top docs with citations.
3. Gear / world questions → same router over weapons/stratagems/armor (Type-G) and biomes/planets/missions (Type-W); multi-intent queries fan out and merge.
4. Miss → `helldivers.wiki.gg` search + fetch (1+1 max), summarized with links. Live-status questions → community war API.
5. Rate limit (`429`) → exact reply: `The Lost Son is busy spreading democracy on Cyberstan. Hold position, Helldiver — try again shortly. Estimated time of liberation: {n}s.` Other backend failures → `Uplink dead. No fallback. For Super Earth, try later.`

Doctrine reminders: flank Hulk/Tank rear vents, Flare Trooper first, objectives (fabricators / bug holes / warp ships) before heavies.

## Freshness

The `watcher-go` service polls the wiki RecentChanges Atom feed every 5 minutes, diffs revision ids against Mongo `tracked_pages`, and re-parses only changed pages (curated seed fields are preserved). Wiki `robots.txt` allows reference/RAG use (`use=reference`); the poller identifies itself and honors `If-Modified-Since`/`Retry-After`.

## Troubleshooting

- `429` / rate limit → expected, wait for liberation ETA (uses `Retry-After` when present).
- Gateway silent in Discord → check token/allowlist in `.env`, confirm mention vs free-response channel.
- No answers, `Uplink dead` → relay key/balance or network; logs show the backend error class.
- Stale intel → check `watcher-go` logs; force one pass with `/watch --once`.

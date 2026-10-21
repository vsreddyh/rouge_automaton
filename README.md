# Rouge Automaton — Lost Son of Managed Democracy

Lightweight Go Discord bot for Helldivers 2 intel, roleplayed as a defected informant trapped in a bot chassis. No Hermes gateway, no Python runtime: a direct Discord gateway plus the OpenCode Go relay, backed by a MongoDB intel store.

## What it does

- Answers Helldivers 2 intel questions: unit weak points, counters, loadouts, tactics, faction lore, plus biome/planet/mission context and live war status.
- Seeded intel in MongoDB covers units, structures, gear (weapons, stratagems, armor, boosters) and world (biomes, planets, missions, difficulty, effects), so exact lookups hit before the wiki crawl lands.
- Gear (weapons, stratagems, armor, boosters) is refreshed from the public gear catalog — a daily snapshot of the same wiki, republished as static JSON, with every item's wiki page attached. The gear shelves never need the wiki.
- Units and the world are refreshed from the wiki itself: a polite crawl (one page per shelf member) plus the RecentChanges feed for change detection.
- The watcher is self-feeding: a page the wiki creates is routed to a shelf, fetched, filed and tracked, so it answers through the exact tools without falling back to a search.
- Persona: ex–Super Citizen / Helldiver, captured on Cyberstan, experimented on, escaped into a bot husk. Controlled cold rage, addresses you as "Helldiver", ends transmissions with `Over.`
- Serial is always `[REDACTED]`. Never invents a name. Never claims to be AI. Never sympathizes with Automatons.
- On local DB miss: live lookup on `helldivers.wiki.gg`, summarized — weak point + counter + source link.
- Model rule: Muse Spark 1.3 Contributor or nothing (`https://opencode.ai/zen/go`, `muse-spark-1.3-contributor`). `429` → busy-spreading-democracy reply with liberation ETA. No silent fallback.

## Layout

```
cmd/
  rouge/            # Discord bot entrypoint
  ingest/           # one-shot Mongo seed from skills/rouge-automaton/data/
  mcpdb/            # MCP stdio server over the shared tool executor
  watch/            # wiki RecentChanges poller, refreshes changed docs
  catalog/          # gear sync from the public catalog API (no wiki calls)
  crawl/            # polite wiki crawl of the unit and world shelves
internal/
  config/           # .env contract (shared with the retired Python bot)
  persona/          # SOUL.md → system prompt, Over./serial/voice guards
  mcpdb/            # Mongo exact-match intel tools + cited context (no router)
  gorelay/          # OpenCode Go relay client (Responses API + tool loop)
  discord/          # discordgo gateway: allowlist, mention gate, threads
  wiki/             # helldivers.wiki.gg fallback (query shaping + search + fetch)
  live/             # live war-status API (Type-L)
  wikifeed/         # RecentChanges Atom feed + MediaWiki API client
  wikiparse/        # wikitext parsers (infobox, anatomy, tactics, biomes)
  wikiwatch/        # feed-vs-tracked_pages diff + doc refresh + crawl + discovery
  catalog/          # gear catalog client (ETag-conditional) + Mongo cache + sync
skills/rouge-automaton/
  SOUL.md           # identity / voice / radio procedure (embedded in binary)
  data/             # seed intel (authoritative seeds, wiki enriches in Mongo)
                    #   automatons|terminids|illuminate.json — units + structures
                    #   gear.json — weapons, stratagems, armor, boosters
                    #   world.json — biomes, planets, missions, difficulty, effects
ContainerfileGo     # multi-stage static build → distroless (~30–45MB)
podman-compose.go.yml # bot-go + watcher-go + catalog-go + crawl-go + mongo (Podman only, no Docker)
.env.example        # DISCORD_BOT_TOKEN, DISCORD_ALLOWED_USERS, OPENCODE_GO_API_KEY, ...
```

Container boot: the image embeds `SOUL.md` at compile time; `skills/` on disk is only needed for the one-shot `ingest` seed. Runtime config comes from `.env`.

## Run (Podman)

```bash
cp .env.example .env   # fill: DISCORD_BOT_TOKEN, OPENCODE_GO_API_KEY, X_SUPER_CONTACT, ...
podman build --network=host -f ContainerfileGo -t rouge-automaton-go .
podman-compose -f podman-compose.go.yml up -d
```

The compose file runs every service with host networking, so `MONGO_URI` must
point at `127.0.0.1`, not at the `mongo` service name. The `mongo-data`
volume is marked external: on a green-field host create it first
(`podman volume create rouge-mongo-data`).

Green-field Mongo is empty on first boot — seed it once. The image's
entrypoint is the bot, so a one-shot command needs `--entrypoint`:

```bash
podman run --rm --network=host --entrypoint /ingest \
  -v $PWD/skills:/app/skills \
  -e MONGO_URI=mongodb://127.0.0.1:27017/rouge \
  localhost/rouge-automaton-go:latest
```

Then let the store fill itself in (both are safe to re-run):

```bash
# gear from the catalog API (no wiki traffic at all)
podman run --rm --network=host --entrypoint /catalog \
  -e MONGO_URI=mongodb://127.0.0.1:27017/rouge \
  localhost/rouge-automaton-go:latest --once
# units and world from the wiki, one page per shelf member
podman run --rm --network=host --entrypoint /crawl \
  -e MONGO_URI=mongodb://127.0.0.1:27017/rouge \
  localhost/rouge-automaton-go:latest --once
```

`.env` keys used: `DISCORD_BOT_TOKEN`, `DISCORD_ALLOWED_USERS` (also `DISCORD_ALLOW_ALL_USERS`, `DISCORD_FREE_RESPONSE_CHANNELS`), `OPENCODE_GO_API_KEY`, `OPENCODE_GO_BASE_URL`, `MODEL`, `MONGO_URI`, `X_SUPER_CLIENT`, `X_SUPER_CONTACT`, `WIKI_USER_AGENT` (and `CATALOG_USER_AGENT`, optional).

## Discord behavior

- Mention-gated replies (`require_mention: true`), `auto_thread: true`: a mention opens a thread and the answer lands inside it — follow-ups in threads the bot opened or has spoken in answer without a fresh mention (same relay session, keyed by thread ID). Strangers' threads still need a mention. Outside threads, a mention is required; configured free-response channels answer without a mention (each costs a model call — keep the list short).
- Greetings (`hi`/`hello`/`hey`, also `yo`/`o7`): answered by the model in voice, whatever fits the moment — no canned line, no helpdesk menu, no "state target".
- War-status questions (`what's the war status`, `how goes the front`) get a live summary of active fronts — no planet name required.
- Detail intel answers: weak point + counter + source (Mongo doc or live link), as fully as the question needs; roster answers (`list_units`, `list_gear`) give names and roles only and never volunteer weaknesses or counters (no word cap; the 2000-rune Discord cut is the transport limit).
- Genuinely ambiguous questions with no tool to resolve them get one short clarifying question instead of a guess; anything a tool already covers is answered directly.

## Answer pipeline

1. Identity questions → answer from `SOUL.md` canon. Serial stays `[REDACTED]`.
2. Factual questions → the model calls exact tools itself (no code router):
   `get_unit` (one unit/structure by exact name/alias), `list_units`
   (faction roster, then `get_unit` for detail), `get_gear` (one gear item
   by exact name), `list_gear` (one shelf by category, then `get_gear`),
   `get_place` (one static planet/biome/mission by exact name),
   `war_status` (generic live war summary: active fronts per faction,
   hottest battles, divers deployed), `planet_status` (one named planet's
   live data). Aliases resolve server-side; exact fetch returns the full
   doc (role, weak points, counters, source), while list tools return a
   roster line (name, role or gear type, source). Every answer cites its
   source once.
3. Miss → the model falls back to `wiki_search` (`helldivers.wiki.gg`),
   summarized with links. The query is shaped server-side: framing words
   ("what is", "vs") are dropped, exact page titles are tried first, and
   a zero-hit search retries with shorter keyword runs — so a plain
   question lands on the right page instead of nothing. Unknowns are
   reported as unconfirmed, not invented; a dead wiki is reported as a
   dead wiki, never as a missing page.
   Genuinely ambiguous queries with no resolving tool get one short
   clarifying question instead of guesses or search loops.
4. Rate limit (`429`) → exact reply: `The Lost Son is busy spreading democracy on Cyberstan. Hold position, Helldiver — try again shortly. Estimated time of liberation: {n}s.` Other backend failures → `Uplink dead. No fallback. For Super Earth, try later.` Tool-loop exhaustion (model kept calling tools past maxToolRounds=10 with no answer) → `Couldn't pin it down — intel thin or scattered. Unconfirmed. Narrow the target and try again.` (never Uplink dead; the uplink is healthy, the intel is thin — fires far less often at 10 rounds).

Doctrine reminders: flank Hulk/Tank rear vents, Flare Trooper first, objectives (fabricators / bug holes / warp ships) before heavies.

## Freshness

Three services keep the store honest, and each one talks to the cheapest source that carries the data:

- **`catalog-go`** (daily) syncs the gear shelves — weapons, stratagems, armor, boosters — from the public gear catalog (`helldivers-api.dionatha.com.br`), a once-a-day snapshot of the wiki's gear pages republished as static JSON. Every item carries its wiki page, so the gear shelves are refreshed **without asking the wiki anything**. The fetch is ETag-conditional, so an unchanged snapshot costs one 304; the payload is cached in Mongo, so a catalog outage costs freshness, not answers. Unreleased patch content is recorded as `upcoming` and never reaches an answer.
- **`crawl-go`** (monthly) refreshes the unit and world shelves from the wiki, one page per shelf member, with a pause between fetches. It uses the same field mapping as the watcher, so a crawled doc and a refreshed doc look identical, and the curated seed fields survive.
- **`watcher-go`** (every 5 minutes) polls the wiki RecentChanges feed, diffs revision ids against Mongo `tracked_pages`, and re-parses only changed pages. A page it has never seen is routed to a shelf (gear from the catalog, units and the world from the wiki's own categories), fetched, filed and tracked — so a new wiki page answers through the exact tools instead of a search. A page that matches no shelf is ignored.

Wiki `robots.txt` allows reference/RAG use (`use=reference`); the poller identifies itself with `WIKI_USER_AGENT` (the wiki account and a reachable contact — wiki.gg rate-limits clients it cannot identify), honors `If-Modified-Since`/`Retry-After`, reads a rate-limit answer as a rate limit (not an empty feed), and backs off instead of retrying every 5 minutes.

Seeds are the floor, the crawl is the ceiling: `cmd/ingest` seeds every shelf with `$setOnInsert`, so a green-field Mongo answers exact gear and place lookups immediately, and a crawled doc only ever enriches the fields the seed left to it. Where the catalog and a curated seed disagree, the disagreement is reported (`catalog_mismatches`), never applied.

## Troubleshooting

- `429` / rate limit → expected, wait for liberation ETA (uses `Retry-After` when present).
- Gateway silent in Discord → check token/allowlist in `.env`, confirm mention vs free-response channel.
- No answers, `Uplink dead` → relay key/balance or network; logs show the backend error class.
- Stale intel → check `watcher-go` logs; force one pass with `/watch --once`.
- Gear looks stale → `/catalog --once`; units and world → `/crawl --once`.
- `watch: rate limited by the wiki` → the wiki is refusing this client; the poller backs off. Check `WIKI_USER_AGENT` names the wiki account.

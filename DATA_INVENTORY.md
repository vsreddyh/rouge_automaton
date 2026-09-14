# Data Inventory + Handling Plan (Podman only, no Docker)

Source: live `helldivers.wiki.gg` enumeration 2026-09-13. Local `skills/rouge-automaton/data/*.json` (31 units) is seed only.

## 1. Types found

### A. Enemies (~83 unit pages + ~38 structures)
- **Terminids 30:** base 21 (Scavenger..Hive Lord) + Predator 2 + Spore Burst 4 + Rupture 3
- **Automatons 37:** base 23 + Jet Brigade 6 + Incineration Corps 5 + Cyborg Legion 3 (Radical, Agitator, Vox Engine)
- **Illuminate 16:** base 11 (Voteless..Overship) + Appropriators 3 (Veracitor, Gatekeeper, Obtruder) + Vote Snatchers 2 (Wretch, Crusher)
- **Structures:** Terminid 9, Automaton 15, Illuminate 11, SEAF 2
- **Per-unit fields:** infobox (description, faction, min difficulty, size, health e.g. Charger 2400 / Titan 6500, damage list, fire mult, stagger) + Spawning + Behavior + Anatomy table (part × health, AV 0-5, location, durable%, fatal?) + Weapons Loadout + Tactical Info + Change History + Categories

### B. Equipment (~260 pages)
- **Weapons ~135:** Primary 52 / Secondary 24 / Throwable 21 / Support 35. Fields: category, type, firing modes, traits, damage, penetration (Unarmored..Anti-Tank IV), rpm/dps/mags, ergonomics/recoil/reload, procurement (Warbond page+cost / Superstore / Ship Management)
- **Stratagems ~115 (85 playable):** Orbital 12 / Eagle 8 / Support 33 / Backpack 13 / Vehicle 8 / Sentry 10 / Emplacement 8 / Mission 21. Fields: permit color, code arrows, cooldown, call-in, uses, ship-module modifiers, damage/AoE/AP
- **Armor 106 sets:** Light 29 / Medium 49 / Heavy 28 (+helmet/cape). Fields: armor value, speed, stamina, passive (~30), acquisition
- **Boosters 18:** flat list. Fields: effect text, warbond + medal cost

### C. World / Missions (priority: biomes)
- **Biomes 31 (28 + 3 overlays):** Sandy 5, Primordial 5, Arctic 3, Moor 4, Swamp 2, Forest 3, Oasis 2, Special 4 (Hive World, Magma Desert, Metropolis, Void Forest) + overlays (Metropolis, Megafactory, Colonies). Fields: archetype, internal name, conditions[], hazards[] (Bug Mine, Lava Geyser...), weather[] + visibility, planets[]
- **Planets ~270 rows:** page | sector (55 sectors) | biome | conditions[0-2] | faction | waypoints | regions[]
- **Missions ~70 main + 35 optional:** shared 9 + Terminid 26 + Automaton 20 + Illuminate 15. Fields: icon, name, difficulty range, rewards (500 Req/100 XP main, 200/50 optional)
- **Difficulty 10:** Trivial..Super Helldive. Fields: missions/op, medals, objectives, outposts, multipliers (0%..300%)
- **Effects 12+:** Acid/Blizzard/Cold/Fire Tornado/Heat/Ion/Meteor/Rain/Sandstorm/Fog/Tremors/Volcanic + Gloom tiers + Operation Modifiers (Complex Plotting, Gunship Patrols, Leviathan Blockade...)

## 2. Mongo handling plan (Types, not hot/cold)

```
db.units       # Type-U, 83 docs: {name, faction, subfaction, aliases, role, weak_points, counters, health, size, min_difficulty, anatomy[], text_blob, embedding, source_url, rev_id, patch_version, fetched_at}
db.structures  # Type-U, 38 docs: same - anatomy
db.weapons     # Type-G, 135 docs: {name, category, type, damage, penetration, traits, procurement, text_blob, embedding...}
db.stratagems  # Type-G, 115 docs: {name, permit, code[], cooldown, call_in, ...}
db.armor       # Type-G, 106 docs + db.boosters (18)
db.biomes      # Type-W, 31 docs: {name, archetype, conditions[], hazards[], weather[], planets[], ...} — full text kept, small
db.planets     # Type-W, ~270 docs: {name, sector, biome, conditions[], faction, waypoints[], regions[]}
db.missions    # Type-W, ~105 docs: {name, side(main|optional), factions[], difficulty_range, rewards}
db.difficulty  # Type-W, 10 docs | db.effects (12+ modifiers)
db.live        # Type-L, live war status (NOT wiki, see §4): {planet_index, owner, health, maxHealth, players, regen, event, updated_at}
db.assignments # Type-L: Major Orders {id, title, briefing, tasks[], expires}
db.tracked_pages {title, url, rev_id, collection}  # ingest audit
```

- Type-U (Units): vector 384d + aliases, `top_k=3`. Every enemy question.
- Type-G (Gear): vector 384d + filters, `top_k=2`. Only on `stratagem|weapon|loadout|AMR|eagle|orbital` intent.
- Type-W (World): keyword + `{faction, biome}` filter, no vector except biomes. Only on `planet|biome|mission|difficulty|storm|fog|where` intent.
- Type-L (Live): HTTPS API, never embedded long-term (TTL 5-15min, see §4). Only on `status|order|who owns X|players|liberation` intent.
- Multi-intent (e.g. `loadout for Mintoria diff 6`): one batch per matched type (U+G+W), merge max ~5 docs, ~2500 tokens. Prompt order: where (W) → who (U) → what to bring (G) → live (L).
- Biomes kept whole (31 × ~400 tokens); planets filtered by `{faction, biome}` before inject. Never inject all 270.

## 3. Podman (no Docker)

```bash
podman build -t rouge-automaton .
podman-compose up -d  # or: podman play kube pod.yaml
```

- `podman-compose.yml`: `bot` + `mongo:7-jammy`, `restart: unless-stopped`, named volume `mongo-data`, rootless, `podman secret` for tokens (no `.env` bind).
- Quadlet option: `~/.config/containers/systemd/rouge-{bot,mongo}.container` for systemd auto-start instead of compose.
- No `network_mode: host` needed unless local LLM; keep bridge + `MONGO_URI` secret.

## 4. Type-L — Live status (verified 2026-09-13)

API: `https://api.helldivers2.dev/api/v1/{planets,war,assignments,campaigns,dispatches,space-stations}`.
Auth: headers `X-Super-Client: rouge-automaton` + `X-Super-Contact: <contact>` required (else `{"message":"The X-Super-Client and X-Super-Contact headers are required"}`).
Verified: `GET /planets` → `[{index, name, sector, biome, hazards[], health, maxHealth, currentOwner, players, regenPerSecond, event, statistics{...}, attacking[]}]`; `GET /war` → `{started, ended, factions[], statistics{...}}`.

Handling:
- Poller `live.py` every 5-15min → upsert `db.live` + `db.assignments`, TTL `updated_at`. Never embed; inject as formatted line: `Mintoria: Automaton 62% (12.4k divers) — rain, jammer. Over.`
- RAG: only on live intent (`status|order|owner|liberation|players`). Stale `>15min` → refetch inline, else serve cache.
- Wiki `planets` stays static (biome/sector); live API owns owner/health/players/event.

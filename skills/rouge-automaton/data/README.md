# Seed intel — licence and provenance

Every file in this directory is an **adaptation of The Helldivers Wiki**
(https://helldivers.wiki.gg), which is licensed under **Creative
Commons Attribution-NonCommercial-ShareAlike 4.0 International
(CC BY-NC-SA 4.0)**. The full data licence is `DATA-LICENSE` at the
repo root; the attribution notice is `NOTICE`.

| file | contents | upstream |
| --- | --- | --- |
| `automatons.json` | Automaton units, structures, tactics | https://helldivers.wiki.gg/wiki/Automatons |
| `terminids.json` | Terminid units, structures, tactics | https://helldivers.wiki.gg/wiki/Terminids |
| `illuminate.json` | Illuminate units, structures, tactics | https://helldivers.wiki.gg/wiki/Illuminate |
| `gear.json` | weapons, stratagems, armor, boosters | wiki gear pages, via the Helldivers 2 Data API (https://helldivers-api.dionatha.com.br) |
| `world.json` | biomes, planets, missions, difficulty, effects | https://helldivers.wiki.gg planet / mission pages |

What this means when you copy this directory on its own:

- **Attribute**: say the intel comes from The Helldivers Wiki
  (helldivers.wiki.gg), CC BY-NC-SA 4.0 — and keep the per-answer
  `[source: …]` citations pointing at the wiki page, not at these files.
- **Non-commercial**: no monetised use without the wiki's permission.
- **Share-alike**: adaptations carry the same licence.

Unreleased patch content: the gear catalog flags items that are not
live yet with `upcoming: true`. Such items are stored (so the bot
knows they exist) but **never rendered into an answer** — a lookup
replies that the item is not in this build yet, without its stats.

# Rouge Automaton — Lost Son of Managed Democracy

Hermes Discord bot. No Python web app, no custom discord.py. Docker-sandboxed Hermes gateway.

Layout: `skills/rouge-automaton/` (SKILL.md + SOUL.md + `data/*.json`), `hermes/config.example.yaml`, `Dockerfile`, `docker-compose.yml`.

## Run (sandboxed)

```bash
cp .env.example .env   # DISCORD_BOT_TOKEN, DISCORD_ALLOWED_USERS
docker compose build
docker compose up -d
```

One-time Hermes setup inside the container:

```bash
docker compose exec hermes hermes model
# Model -> builtin Groq provider `custom:groq`, model = qwen/qwen3.8-27b
docker compose exec hermes hermes gateway setup   # Discord token + allowlist
docker compose exec hermes hermes gateway         # foreground test
```

Model rule: Groq or nothing. `429` -> busy spreading democracy + liberation ETA.

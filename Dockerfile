FROM python:3.12-slim

ENV DEBIAN_FRONTEND=noninteractive \
    HERMES_HOME=/home/hermes/.hermes \
    PATH="/home/hermes/.local/bin:${PATH}"

RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates git build-essential \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -s /bin/bash hermes

USER hermes
WORKDIR /home/hermes

# Hermes Agent (curl installer) + skill payload
RUN curl -fsSL https://hermes-agent.nousresearch.com/install.sh | bash
COPY --chown=hermes:hermes skills/ /home/hermes/skills-source/
COPY --chown=hermes:hermes hermes/config.example.yaml /home/hermes/config.example.yaml

# Skills + SOUL install at container start (SOUL.md is slot #1 identity).
CMD ["bash", "-lc", "mkdir -p \"$HERMES_HOME/skills\" && cp -r /home/hermes/skills-source/* \"$HERMES_HOME/skills/\" && cp /home/hermes/skills-source/rouge-automaton/SOUL.md \"$HERMES_HOME/SOUL.md\" && exec hermes gateway"]

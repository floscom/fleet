# fleet-agent: the default image for sandboxed fleet agents.
# Build it with `fleet sandbox build`. To add toolchains, build your own
# image FROM this one and set [sandbox] image in config.toml.
#
# fleet runs the container as the daemon user's uid:gid with HOME=/home/fleet
# (a directory of the daemon host mounted over it), so nothing here depends on
# the user below except for hosts where that uid is 1000.
FROM node:22-bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl git gh jq less openssh-client procps python3 ripgrep \
 && rm -rf /var/lib/apt/lists/*

RUN npm install -g @anthropic-ai/claude-code @openai/codex \
 && npm cache clean --force

RUN userdel -r node \
 && useradd --uid 1000 --create-home --home-dir /home/fleet --shell /bin/bash fleet

# Agent CLIs cannot update a global npm install as a normal user; update the
# image instead (`fleet sandbox build --pull`).
ENV DISABLE_AUTOUPDATER=1 \
    LANG=C.UTF-8 \
    TERM=xterm-256color \
    COLORTERM=truecolor

USER fleet
WORKDIR /home/fleet
CMD ["bash", "-l"]

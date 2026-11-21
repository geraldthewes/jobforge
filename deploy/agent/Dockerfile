# Unprivileged image for jobforge agent jobs.
#
# sourced-review's catalog/ directory is not part of the installed package, so
# the image installs it editable from a full checkout you pass in the build
# context. Do not copy credentials into the image; the Nomad job injects tokens
# through Vault templates at runtime.
#
#   mkdir -p /tmp/agent-image
#   git clone git@github.com:geraldthewes/sourced-review.git /tmp/agent-image/sourced-review
#   docker build -f deploy/agent/Dockerfile \
#     -t registry.cluster:5000/jobforge-agent:latest \
#     /tmp/agent-image
#
# SOURCED_REVIEW_DIR is a path inside the build context, not a host path.
# The default expects a directory named sourced-review in that context.
#
# The opencode CLI is installed from the npm package @opencode/cli, which is the
# package named in OpenCode's CI example. Headless flags used by the runner are
# `opencode run --standalone --auto` (see https://opencode.ai/docs/cli/commands).

FROM python:3.12-slim-bookworm

ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    SOURCED_REVIEW_CATALOG=/opt/sourced-review/catalog \
    PATH=/usr/local/bin:/usr/bin:/bin

RUN apt-get update && apt-get install -y --no-install-recommends \
        bash \
        ca-certificates \
        coreutils \
        curl \
        git \
        gnupg \
        jq \
    && rm -rf /var/lib/apt/lists/*

# GitHub CLI. The runner opens pull requests with curl; gh is available for debugging.
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
        -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
        > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*

# Gitea tea CLI. Pinned; the runner does not require `tea login`.
ARG TEA_VERSION=0.16.0
RUN curl -fsSL -o /usr/local/bin/tea \
        "https://dl.gitea.com/tea/${TEA_VERSION}/tea-${TEA_VERSION}-linux-$(dpkg --print-architecture | sed 's/armhf/arm/;s/aarch64/arm64/')" \
    && chmod 0755 /usr/local/bin/tea

# Node is required by the published OpenCode CLI package.
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install --global @opencode/cli \
    && rm -rf /var/lib/apt/lists/*

# uv installs sourced-review into the image Python. The catalog stays on disk
# because it is not part of the packaged distribution.
ENV UV_BREAK_SYSTEM_PACKAGES=1
RUN curl -LsSf https://astral.sh/uv/install.sh | env UV_INSTALL_DIR=/usr/local/bin sh

ARG SOURCED_REVIEW_DIR=sourced-review
COPY ${SOURCED_REVIEW_DIR} /opt/sourced-review
RUN uv pip install --system -e /opt/sourced-review \
    && test -d /opt/sourced-review/catalog

RUN useradd --create-home --uid 1000 --shell /bin/bash agent \
    && mkdir -p /work /alloc \
    && chown -R agent:agent /work /alloc

USER agent
WORKDIR /work

# Nomad's docker driver overrides the entrypoint with `bash /local/agent-runner.sh`.
CMD ["bash"]

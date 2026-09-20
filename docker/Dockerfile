ARG BASE_IMAGE="saltydk/alpine-s6overlay:sha-c5d043869aa5cc97d6931730f63c707e4b34a301@sha256:e8cc3eef2148814a5d796d58d4d7dbb9e291e1366d11d49667019efe638bf920"

FROM ${BASE_IMAGE}

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT

ENV \
  PATH="/app/autoscan:${PATH}" \
  AUTOSCAN_CONFIG="/config/config.yml" \
  AUTOSCAN_DATABASE="/config/autoscan.db" \
  AUTOSCAN_LOG="/config/activity.log" \
  AUTOSCAN_VERBOSITY="0"

# Binary
COPY ["dist/docker/${TARGETOS}_${TARGETARCH}${TARGETVARIANT:+_7}/autoscan", "/app/autoscan/autoscan"]

# Add root files
COPY ["docker/run", "/etc/services.d/autoscan/run"]

ARG BASE_IMAGE
LABEL org.opencontainers.image.base.name="${BASE_IMAGE}"

# Volume
VOLUME ["/config"]

# Port
EXPOSE 3030

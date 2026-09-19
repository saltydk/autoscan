ARG BASE_IMAGE="saltydk/alpine-s6overlay:sha-c8094194f32b1ed7abd34aae6e6890494c0b5b2a@sha256:484f9245509cabf57b64ccbbd07177588c8d73e7b5453d6ff02cd10c8295576d"

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

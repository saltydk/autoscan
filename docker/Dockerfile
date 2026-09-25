ARG BASE_IMAGE="saltydk/alpine-s6overlay:sha-cbe26ff0510285e4a6139c0fb87d49a4449e426d@sha256:209997edd08919097dedc367fbec8beacfb0383ff728dc58717365f885895745"

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

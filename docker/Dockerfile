ARG BASE_IMAGE="saltydk/alpine-s6overlay:sha-b5833aa681474b603012c55469e71e6dcf3cbbe8@sha256:68a9a5eea67e67a314569b0452590c260c80ddcfa23a4debf984904f1248a480"

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

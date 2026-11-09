ARG BASE_IMAGE="saltydk/alpine-s6overlay:sha-1b92b1d1e60a6ef37bf9162910ad3c96f9460375@sha256:526b3249bb85df476ac4689e56147ba60e8e92e907658daf884a28c22fb2fab0"

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
COPY ["binaries/${TARGETOS}_${TARGETARCH}${TARGETVARIANT:+_7}/autoscan", "/app/autoscan/autoscan"]

# Add root files
COPY ["run", "/etc/services.d/autoscan/run"]

ARG BASE_IMAGE
LABEL org.opencontainers.image.base.name="${BASE_IMAGE}"

# Volume
VOLUME ["/config"]

# Port
EXPOSE 3030

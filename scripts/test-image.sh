#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 4 ]]; then
  echo "usage: $0 IMAGE PLATFORM ARCH SOURCE_SHORT_SHA" >&2
  exit 2
fi

image=$1
platform=$2
expected_arch=$3
source_sha=$4
container="autoscan-acceptance-${BASHPID}-${RANDOM}"
config_dir=$(mktemp -d)

cleanup() {
  if docker inspect "$container" >/dev/null 2>&1; then
    docker logs "$container" >&2 || true
    docker rm --force --volumes "$container" >/dev/null 2>&1 || true
  fi
  rm -rf "$config_dir"
}
trap cleanup EXIT

cat > "$config_dir/config.yml" <<'YAML'
host: [0.0.0.0]
port: 3030
scan-stats: 0s
authentication:
  username: acceptance
  password: temporary-test-password
YAML

docker create --name "$container" --platform "$platform" \
  --env PUID=1000 --env PGID=1001 "$image" >/dev/null
docker cp "$config_dir/config.yml" "$container:/config/config.yml"
docker start "$container" >/dev/null

ready=false
for _ in $(seq 1 60); do
  if docker exec "$container" curl --silent --fail \
    --user acceptance:temporary-test-password \
    --head http://127.0.0.1:3030/triggers/manual >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
if [[ "$ready" != true ]]; then
  echo "Autoscan did not start its authenticated webhook server" >&2
  exit 1
fi

version=$(docker exec "$container" /app/autoscan/autoscan --version)
[[ "$version" == *"${source_sha}@"* ]]
[[ "$(docker exec "$container" uname -m)" == "$expected_arch" ]]

status=$(docker exec "$container" curl --silent --output /dev/null \
  --write-out '%{http_code}' http://127.0.0.1:3030/triggers/manual)
[[ "$status" == 401 ]]
docker exec "$container" curl --silent --show-error --fail \
  --user acceptance:temporary-test-password --request POST \
  'http://127.0.0.1:3030/triggers/manual?dir=%2Fmedia%2Facceptance' >/dev/null

docker exec "$container" /bin/sh -ec '
  pid=$(pgrep -o -f /app/autoscan/autoscan)
  test -n "$pid"
  test "$(stat -c %u "/proc/$pid")" = 1000
  test "$(stat -c %g "/proc/$pid")" = 1001
  test -s /config/autoscan.db
  test -s /config/activity.log
  /usr/local/libexec/apk-lock verify /usr/share/image-inputs/runtime.lock
'

docker stop --time 20 "$container" >/dev/null
[[ "$(docker inspect --format '{{.State.ExitCode}}' "$container")" == 0 ]]
docker rm --volumes "$container" >/dev/null
rm -rf "$config_dir"
trap - EXIT
printf 'Passed %s: %s\n' "$platform" "$version"

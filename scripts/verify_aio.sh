#!/usr/bin/env bash
# Smoke-test already built images, with disposable credentials and no external network.
set -euo pipefail
cd "$(dirname "$0")/.."
aio=${AGY_AIO_IMAGE:-agy-acp-gateway:aio}
slim=${AGY_SLIM_IMAGE:-agy-acp-gateway:slim}
work=$(mktemp -d)
volume=$(docker volume create)
containers=()
cleanup() {
  for item in "${containers[@]}"; do docker rm -f "$item" >/dev/null || true; done
  docker volume rm "$volume" >/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT
export GATEWAY_API_KEY=synthetic-verification-key
env -u http_proxy -u https_proxy AGY_DATA_VOLUME= GATEWAY_AUTH_KEEPALIVE_INTERVAL= HTTPS_PROXY=http://proxy.invalid:8899 docker compose -p authfixture config --format json |
  python3 -c 'import json,sys; c=json.load(sys.stdin); s=c["services"]["acp-gateway"]; a=s["build"]["args"]; e=s["environment"]; assert s["build"]["target"]=="aio" and e["GATEWAY_API_KEY"]=="synthetic-verification-key" and "GATEWAY_API_KEY" not in a and e["https_proxy"]==a["HTTPS_PROXY"]=="http://proxy.invalid:8899"; assert e["GATEWAY_AUTH_KEEPALIVE_INTERVAL"]=="30m" and c["volumes"]["agy-data"]["name"]=="authfixture_agy-data"'
AGY_DATA_VOLUME=authfixture_agy-data GATEWAY_AUTH_KEEPALIVE_INTERVAL=0 docker compose -p renamedfixture config --format json |
  python3 -c 'import json,sys; c=json.load(sys.stdin); assert c["services"]["acp-gateway"]["environment"]["GATEWAY_AUTH_KEEPALIVE_INTERVAL"]=="0" and c["volumes"]["agy-data"]["name"]=="authfixture_agy-data"'
docker run --rm --network none --read-only -v "$volume:/data" --entrypoint sh "$aio" -ec '
  test "$(id -u):$(id -g)" = 65532:65532
  test "$(stat -c %a /data)" = 700
  dir=/data/.gemini/antigravity-cli
  mkdir -p "$dir"; touch "$dir/test-old" "$dir/test-new"; mv -f "$dir/test-new" "$dir/test-old"; rm "$dir/test-old"
  test -s /etc/ssl/certs/ca-certificates.crt; test ! -e /usr/bin/curl
  /usr/local/bin/agy --version; sha256sum /usr/local/bin/agy'
docker run --rm --network none --entrypoint sh "$slim" -ec 'test "$(id -u)" = 65532; test ! -e /usr/local/bin/agy'
start() {
  container=$(docker run -d --network none --read-only --tmpfs /tmp --cap-drop ALL \
    --security-opt=no-new-privileges --init --health-interval=1s --health-timeout=2s -e GATEWAY_API_KEY "$@")
  containers+=("$container")
  for ((i=0;i<60;i++)); do
    if [[ $(docker inspect -f '{{.State.Health.Status}}' "$container") == healthy ]]; then return; fi
    sleep 0.25
  done
  echo "Gateway did not become healthy" >&2; return 1
}
request() {
  local method=$1 path=$2 code=$3
  printf '%s %s HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}' \
    "$method" "$path" "$GATEWAY_API_KEY" |
    docker exec -i "$container" bash -ec 'exec 3<>/dev/tcp/127.0.0.1/8080; cat >&3; cat <&3' > "$work/reply"
  head -1 "$work/reply" | grep -q " $code "
}
start -v "$volume:/data" "$aio"
request GET /health 200
for ((i=0;i<60;i++)); do
  request GET /ready 503
  if grep -q '"auth_required":true' "$work/reply"; then break; fi
  sleep 0.25
done
grep -q '"auth_required":true' "$work/reply"
request GET /admin/api/status 200
grep -q '"credential_mode":"managed"' "$work/reply"
grep -q '"keepalive_enabled":true' "$work/reply"
request POST /admin/api/auth/logout 200

# Preserve synthetic account metadata over a managed container replacement.
docker exec "$container" sh -ec '
  cd /data/.gemini/antigravity-cli; umask 077; mkdir -p cache
  echo "{\"auth_method\":\"consumer\",\"id_token_email\":\"saved@example.invalid\"}" > antigravity-oauth-token
  echo "{\"method\":\"oauth-personal\"}" > cache/acp-auth-state.json'
docker stop "$container" >/dev/null
start -v "$volume:/data" "$aio"
request GET /admin/api/status 200
grep -q '"email":"saved@example.invalid"' "$work/reply"
docker exec "$container" sh -ec 'cd /data/.gemini/antigravity-cli; test -s cache/acp-auth-state.json'
request POST /admin/api/auth/logout 200
docker exec "$container" sh -ec 'cd /data/.gemini/antigravity-cli; test ! -e antigravity-oauth-token && test ! -e cache/acp-auth-state.json'
docker stop "$container" >/dev/null
printf '%s\n' '{"auth_method":"consumer","id_token_email":"external@example.invalid"}' > "$work/synthetic"; chmod 0444 "$work/synthetic"
start -v "$volume:/data" -v "$work/synthetic:/data/.gemini/antigravity-cli/antigravity-oauth-token:ro" \
  -e GATEWAY_CREDENTIAL_MODE=external "$aio"
request GET /admin/api/status 200
grep -q '"email":"external@example.invalid"' "$work/reply"
grep -q '"keepalive_enabled":false' "$work/reply"
for action in start complete cancel logout; do
  request POST "/admin/api/auth/$action" 403
  grep -q '"code":"credentials_externally_managed"' "$work/reply"
done
test "$(cat "$work/synthetic")" = '{"auth_method":"consumer","id_token_email":"external@example.invalid"}'
start "$slim"
request GET /admin/api/status 200
grep -q '"mode":"mock"' "$work/reply"
docker image inspect --format '{{index .RepoTags 0}} {{.Os}}/{{.Architecture}} {{.Id}}' "$aio" "$slim"
echo 'AIO and slim container checks passed'

#!/usr/bin/env bash
# Build a fresh gateway, run the mock and official SDK checks, then stop both.
set -euo pipefail
sdk_python=${COMPAT_SDK_PYTHON:?Set COMPAT_SDK_PYTHON to the SDK virtual environment Python}
gateway_port=${COMPAT_GATEWAY_PORT:-18150}
upstream_port=${COMPAT_UPSTREAM_PORT:-18151}
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
cd "$script_dir/../../../.."
run_dir=$(mktemp -d /var/tmp/osw-compat-check.XXXXXX)
mock_pid=''
gateway_pid=''
cleanup() {
  result=$?
  if [ "$result" -ne 0 ]; then
    tail -30 "$run_dir/gateway.log" "$run_dir/mock.log" 2>/dev/null || true
  fi
  if [ -n "$gateway_pid" ]; then kill -TERM "$gateway_pid" 2>/dev/null || true; wait "$gateway_pid" 2>/dev/null || true; fi
  if [ -n "$mock_pid" ]; then kill -TERM "$mock_pid" 2>/dev/null || true; wait "$mock_pid" 2>/dev/null || true; fi
  rm -rf -- "$run_dir"
}
trap cleanup EXIT
"$sdk_python" -c 'import openai, anthropic'
go build -p 2 -o "$run_dir/gateway" ./cmd/gateway
cat > "$run_dir/config.yaml" <<CONFIG
apiVersion: omniswitch.dev/v1
kind: GatewayConfig
gateway:
  listen: '127.0.0.1:$gateway_port'
  data_dir: '$run_dir/data'
  auth: true
  cache_threshold: 0
  log_payloads: false
guardrails:
  stream_buffer: true
observability:
  otel_enabled: false
providers:
  - name: compat-mock
    type: custom
    base_url: http://127.0.0.1:$upstream_port/v1
    models: [test-model]
mcp:
  enabled: false
CONFIG
# A disposable local-only bootstrap key, not a production credential.
export OMNISWITCH_COMPAT_API_KEY="sk-osw-$(python3 -c 'import secrets; print(secrets.token_hex(32))')"
python3 "$script_dir/mock_upstream.py" --port "$upstream_port" > "$run_dir/mock.log" 2>&1 &
mock_pid=$!
env -u OPENAI_API_KEY -u ANTHROPIC_API_KEY -u GOOGLE_API_KEY -u GROQ_API_KEY \
  OMNISWITCH_CONFIG="$run_dir/config.yaml" OMNISWITCH_AUTH=true \
  OMNISWITCH_BOOTSTRAP_API_KEY="$OMNISWITCH_COMPAT_API_KEY" \
  "$run_dir/gateway" serve > "$run_dir/gateway.log" 2>&1 &
gateway_pid=$!
ready=0
for _ in $(seq 1 80); do
  if ! kill -0 "$mock_pid" 2>/dev/null || ! kill -0 "$gateway_pid" 2>/dev/null; then break; fi
  if curl -fsS "http://127.0.0.1:$gateway_port/healthz" > /dev/null 2>&1 && \
     curl -fsS "http://127.0.0.1:$upstream_port/health" > /dev/null 2>&1; then ready=1; break; fi
  sleep 0.25
done
if [ "$ready" != 1 ]; then echo 'Local gateway/mock did not become ready' >&2; exit 1; fi
"$sdk_python" "$script_dir/sdk_check.py" --gateway-url "http://127.0.0.1:$gateway_port"

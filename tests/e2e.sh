#!/usr/bin/env bash
# End-to-end check: build the plugin as CPA loads it (cgo shared library),
# boot a fake Vibe console, then drive the plugin through the real C ABI with
# the host harness in hosttest/.
set -euo pipefail

cd "$(dirname "$0")/.."

PORT="${FAKE_VIBE_PORT:-0}"  # 0 lets the OS pick a free port
case "$(uname -s)" in
	Darwin) EXT="dylib" ;;
	*) EXT="so" ;;
esac
LIB_TEMP=""
if [[ -n "${PLUGIN_LIB:-}" ]]; then
	LIB="$PLUGIN_LIB"
else
	LIB_TEMP="$(mktemp -t cpa-mistral-vibe)"
	LIB="${LIB_TEMP}.$EXT"
fi
HARNESS="$(mktemp -t hosttest)"
LOGFILE="$(mktemp -t fakevibe)"

cleanup() {
	if [[ -n "${SERVER_PID:-}" ]]; then
		kill "$SERVER_PID" 2>/dev/null || true
		wait "$SERVER_PID" 2>/dev/null || true
	fi
	rm -f "$HARNESS" "$LOGFILE"
	if [[ -n "$LIB_TEMP" ]]; then
		rm -f "$LIB_TEMP" "$LIB" "${LIB%.*}.h"
	fi
}
trap cleanup EXIT

echo "== go vet"
go vet ./...

echo "== go test"
go test ./...

echo "== build plugin ($EXT)"
go build -buildmode=c-shared -o "$LIB" .
echo "   $LIB"

echo "== exported ABI symbols"
for sym in cliproxy_plugin_init cliproxy_plugin_call cliproxy_plugin_free cliproxy_plugin_shutdown; do
	if ! nm -gU "$LIB" 2>/dev/null | grep -q "${sym}"; then
		if ! nm -D "$LIB" 2>/dev/null | grep -q "${sym}"; then
			echo "   missing $sym" >&2
			exit 1
		fi
	fi
	echo "   ok   $sym"
done

echo "== boot fake Vibe console"
python3 tests/fake_vibe.py "$PORT" >"$LOGFILE" 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 50); do
	grep -q "http://" "$LOGFILE" 2>/dev/null && break
	sleep 0.1
done
BASE="$(grep -o 'http://[^ ]*' "$LOGFILE" | head -1)"
[[ -n "$BASE" ]] || { echo "   fake console failed:" >&2; cat "$LOGFILE" >&2; exit 1; }
echo "   $BASE"

echo "== drive the plugin over the C ABI"
go build -o "$HARNESS" ./hosttest
FAKE_PLAN_BASE="$BASE" PLUGIN_LIB="$LIB" "$HARNESS"

echo "== live Mistral (skipped unless MISTRAL_API_KEY is exported)"
if ( PLUGIN_LIB="$LIB" python3 tests/live_mistral.py ); then
	:
else
	echo "   live Mistral check failed" >&2
	exit 1
fi



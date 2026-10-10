#!/bin/sh
set -eu

mode=${CONTRACT_PROCESS_MODE:-legacy}
case "$mode" in
    legacy|split) ;;
    *) echo "invalid CONTRACT_PROCESS_MODE" >&2; exit 64 ;;
esac
case "${CONTRACT_RUN_WORKER_WITH_API:-false}" in
    true|false) ;;
    *) echo "invalid CONTRACT_RUN_WORKER_WITH_API" >&2; exit 64 ;;
esac
if [ "$mode" = split ] && [ "${CONTRACT_RUN_WORKER_WITH_API:-false}" = true ]; then
    echo "split contract runtime cannot start an embedded Worker" >&2
    exit 64
fi
if [ "${COMMERCIAL_LICENSE_ENABLED:-false}" = true ]; then
    if [ "$mode" != split ]; then
        echo "licensed contract runtime requires split process mode" >&2
        exit 64
    fi
    case "${1:-}" in
        ./api) expected_component=contract-api ;;
        ./worker) expected_component=contract-worker ;;
        *) expected_component= ;;
    esac
    if [ -n "$expected_component" ] && [ "${COMMERCIAL_LICENSE_SERVICE_ID:-}" != "$expected_component" ]; then
        echo "commercial license component binding mismatch" >&2
        exit 64
    fi
fi

run_api_with_worker() {
    ./worker &
    worker_pid=$!
    ./api &
    api_pid=$!

    stop_children() {
        kill -TERM "$api_pid" "$worker_pid" 2>/dev/null || true
    }
    trap stop_children INT TERM HUP

    # Legacy compatibility only. Licensed deployments use separate components.
    while kill -0 "$api_pid" 2>/dev/null && kill -0 "$worker_pid" 2>/dev/null; do
        sleep 1
    done

    status=0
    if ! kill -0 "$api_pid" 2>/dev/null; then
        set +e
        wait "$api_pid"
        status=$?
        set -e
        kill -TERM "$worker_pid" 2>/dev/null || true
        wait "$worker_pid" 2>/dev/null || true
    else
        set +e
        wait "$worker_pid"
        status=$?
        set -e
        kill -TERM "$api_pid" 2>/dev/null || true
        wait "$api_pid" 2>/dev/null || true
    fi
    exit "$status"
}

if [ "${CONTRACT_RUN_WORKER_WITH_API:-false}" = "true" ] && [ "${1:-}" = "./api" ]; then
    run_api_with_worker
fi

exec "$@"

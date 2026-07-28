#!/bin/sh
set -eu

run_api_with_worker() {
    ./worker &
    worker_pid=$!
    ./api &
    api_pid=$!

    stop_children() {
        kill -TERM "$api_pid" "$worker_pid" 2>/dev/null || true
    }
    trap stop_children INT TERM HUP

    # API 与 Temporal Worker 属于同一合同后端运行单元；任一异常退出即重启容器。
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

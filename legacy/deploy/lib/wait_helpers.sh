#!/usr/bin/env bash
#
# Shared wait/retry helpers for deploy scripts.
# Requires funcs: log_info/log_warning.
# Provides generic retry and non-fatal deployment wait helpers
# used by install flow and deploy component libs.
#

retry_until_success() {
    local max_attempts="$1"
    local sleep_seconds="$2"
    local description="$3"
    shift 3

    local attempt=1
    while [ "$attempt" -le "$max_attempts" ]; do
        if "$@"; then
            return 0
        fi

        if [ "$attempt" -lt "$max_attempts" ]; then
            log_info "${description} not ready yet (attempt ${attempt}/${max_attempts}), retrying in ${sleep_seconds}s..."
            sleep "$sleep_seconds"
        fi
        attempt=$((attempt + 1))
    done

    return 1
}

wait_deployment_available_nonfatal() {
    local namespace="$1"
    local deployment_name="$2"
    local timeout="$3"
    local warning_message="$4"

    kubectl wait --for=condition=Available "deployment/${deployment_name}" -n "$namespace" --timeout="$timeout" || \
        log_warning "$warning_message"
}

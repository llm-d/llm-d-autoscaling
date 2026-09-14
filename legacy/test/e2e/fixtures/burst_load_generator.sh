#!/bin/sh
# Burst Load Generator Script
#
# Canonical copy (embedded via //go:embed in workload_builder.go). Benchmark load Jobs only;
# not used by deterministic test/e2e. hack/burst_load_generator.sh is a symlink here so docs
# and manual runs can keep using ./hack/burst_load_generator.sh.
#
# This script generates burst load by sending requests in parallel batches with sleep between batches.
# This creates queue spikes that are more likely to trigger saturation detection.
#
# Environment Variables:
#   TOTAL_REQUESTS: Total number of requests to send
#   BATCH_SIZE: Number of requests to send in parallel per batch
#   CURL_TIMEOUT: Timeout for each curl request (seconds)
#   MAX_TOKENS: Maximum tokens for each request
#   BATCH_SLEEP: Sleep duration between batches (seconds)
#   MODEL_ID: Model ID to use in requests
#   TARGET_URL: Target URL for requests (e.g. http://service:port/v1/chat/completions)

set -e

# Validate required environment variables
if [ -z "$TOTAL_REQUESTS" ] || [ -z "$BATCH_SIZE" ] || [ -z "$TARGET_URL" ] || [ -z "$MODEL_ID" ]; then
  echo "ERROR: Missing required environment variables"
  echo "Required: TOTAL_REQUESTS, BATCH_SIZE, TARGET_URL, MODEL_ID"
  exit 1
fi

# Set defaults for optional variables
CURL_TIMEOUT=${CURL_TIMEOUT:-180}
MAX_TOKENS=${MAX_TOKENS:-400}
BATCH_SLEEP=${BATCH_SLEEP:-0.5}
MAX_RETRIES=${MAX_RETRIES:-24}
RETRY_DELAY=${RETRY_DELAY:-5}

# =============================================================================
# Script Start
# =============================================================================
echo "Burst load generator starting..."
echo "Sending $TOTAL_REQUESTS requests to $TARGET_URL in batches of $BATCH_SIZE"

# Wait for service to be ready
echo "Waiting for service to be ready..."
CONNECTED=false
# Strip path from first /v1/ onward so /v1/chat/completions and /v1/completions both work.
BASE_URL=$(echo "$TARGET_URL" | sed 's|/v1/.*||')
# Test with /v1/models endpoint (OpenAI-compatible endpoint that should return 200)
HEALTH_CHECK_URL="${BASE_URL}/v1/models"
for i in $(seq 1 $MAX_RETRIES); do
  if curl -s -o /dev/null -w "%{http_code}" "$HEALTH_CHECK_URL" 2>/dev/null | grep -q 200; then
    echo "Connection test passed on attempt $i"
    CONNECTED=true
    break
  fi
  echo "Attempt $i failed, retrying in ${RETRY_DELAY}s..."
  sleep $RETRY_DELAY
done

if [ "$CONNECTED" != "true" ]; then
  echo "ERROR: Cannot connect to service after $MAX_RETRIES attempts"
  exit 1
fi

# Send requests in parallel batches (burst pattern)
SENT=0
while [ $SENT -lt $TOTAL_REQUESTS ]; do
  for i in $(seq 1 $BATCH_SIZE); do
    if [ $SENT -ge $TOTAL_REQUESTS ]; then break; fi
    (curl -s -o /dev/null --max-time $CURL_TIMEOUT -X POST $TARGET_URL \
      -H "Content-Type: application/json" \
      -d "{\"model\":\"$MODEL_ID\",\"messages\":[{\"role\":\"user\",\"content\":\"Write a detailed explanation of machine learning algorithms.\"}],\"max_tokens\":$MAX_TOKENS}" || true) &
    SENT=$((SENT + 1))
  done
  echo "Sent $SENT / $TOTAL_REQUESTS requests..."
  sleep $BATCH_SLEEP
done

# Wait for all background jobs to complete
wait || true

echo "Completed all $TOTAL_REQUESTS requests"
exit 0

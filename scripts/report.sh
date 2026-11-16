#!/usr/bin/env bash
# Send a message to the project's Google Chat space.
#
# Usage:
#   scripts/report.sh "M0 finished: all checks green"
#   echo "multi-line report" | scripts/report.sh
#
# Reads GOOGLE_CHAT_WEBHOOK (the full webhook URL, including key and token)
# from the environment, or from .env at the repository root.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ -z "${GOOGLE_CHAT_WEBHOOK:-}" ] && [ -f "$root/.env" ]; then
  set -a
  # shellcheck disable=SC1091
  . "$root/.env"
  set +a
fi

if [ -z "${GOOGLE_CHAT_WEBHOOK:-}" ]; then
  echo "report.sh: GOOGLE_CHAT_WEBHOOK is not set (expected in $root/.env)" >&2
  exit 2
fi

if [ "$#" -gt 0 ]; then
  text="$*"
else
  text="$(cat)"
fi

if [ -z "$text" ]; then
  echo "report.sh: empty message" >&2
  exit 2
fi

# jq builds the JSON body so quotes, newlines and unicode are escaped correctly.
body="$(jq -n --arg text "$text" '{text: $text}')"

# The webhook URL is a secret: never echo it, and keep curl quiet about it.
status="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
  --request POST \
  --header 'Content-Type: application/json; charset=UTF-8' \
  --data "$body" \
  "$GOOGLE_CHAT_WEBHOOK")"

if [ "$status" != "200" ]; then
  echo "report.sh: Google Chat returned HTTP $status" >&2
  exit 1
fi

echo "report.sh: sent"

#!/usr/bin/env bash
# Apply or update the "Protect main" repository ruleset from
# .github/rulesets/main.json. Requires repository admin access via `gh`.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FILE="$ROOT/.github/rulesets/main.json"

if ! command -v gh >/dev/null; then
  echo "gh is required. Install GitHub CLI and authenticate as a repository admin." >&2
  exit 1
fi

if ! command -v jq >/dev/null; then
  echo "jq is required." >&2
  exit 1
fi

if [[ ! -f "$FILE" ]]; then
  echo "ruleset file not found: $FILE" >&2
  exit 1
fi

jq -e '.name and .target == "branch" and .enforcement and (.rules | type == "array")' "$FILE" >/dev/null

NAME="$(jq -r '.name' "$FILE")"
REPO="$(gh repo view --json nameWithOwner --jq .nameWithOwner)"

echo "Applying ruleset \"$NAME\" to $REPO"

EXISTING_ID="$(gh api "/repos/${REPO}/rulesets" | jq -r --arg n "$NAME" '[.[] | select(.name == $n)] | first | .id // empty')"

apply() {
  local method="$1"
  local path="$2"
  if ! gh api --method "$method" \
    -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28" \
    "$path" \
    --input "$FILE"; then
    echo >&2
    echo "Failed to apply the ruleset. This endpoint needs repository admin permission." >&2
    echo "Sign in as a repo admin (\`gh auth login\`) and retry, or create the ruleset in" >&2
    echo "Settings → Rules → Rulesets using .github/rulesets/main.json." >&2
    exit 1
  fi
}

if [[ -n "$EXISTING_ID" ]]; then
  echo "Updating existing ruleset id=$EXISTING_ID"
  apply PUT "/repos/${REPO}/rulesets/${EXISTING_ID}"
else
  echo "Creating ruleset"
  apply POST "/repos/${REPO}/rulesets"
fi

echo
echo "Verify at https://github.com/${REPO}/settings/rules"

#!/usr/bin/env bash
# OpsPilot pre-commit secrets check — blocks commits containing live credentials.
# Install: ln -sf ../../scripts/check-secrets.sh .git/hooks/pre-commit
set -euo pipefail

# Staged file contents only (what would actually be committed).
staged_files=$(git diff --cached --name-only --diff-filter=ACM)
[ -z "$staged_files" ] && { echo "OpsPilot secrets check passed"; exit 0; }

found=0
# Patterns require real key material after the prefix so documentation that
# merely mentions a prefix (like this script) does not trip the check.
patterns=(
  'AKIA[0-9A-Z]{16}'            # AWS access key ID
  'sk-ant-[A-Za-z0-9_-]{8,}'    # Anthropic API key
  'sk_live_[A-Za-z0-9]{8,}'     # Clerk live secret key
)
labels=(
  "AWS access key (AKIA...)"
  "Anthropic API key (sk-ant-...)"
  "Clerk live secret key (sk_live_...)"
)

for i in "${!patterns[@]}"; do
  # .env.example is excluded — it contains placeholder values like sk-ant-xxx.
  matches=$(git diff --cached -U0 -- . ':(exclude).env.example' | grep -E "^\+" | grep -E "${patterns[$i]}" || true)
  if [ -n "$matches" ]; then
    echo "ERROR: staged changes contain what looks like a ${labels[$i]}:" >&2
    echo "$matches" | head -5 >&2
    found=1
  fi
done

if [ "$found" -ne 0 ]; then
  echo "" >&2
  echo "Commit blocked. Remove the secret, rotate it if it was real, and try again." >&2
  echo "(Secrets belong in .env, which is git-ignored.)" >&2
  exit 1
fi

echo "OpsPilot secrets check passed"

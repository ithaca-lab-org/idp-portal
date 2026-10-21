#!/usr/bin/env bash
# Applies repo settings and the main-branch ruleset from code, so the GitHub
# configuration is reviewable and reproducible. Run once by a human after the
# repo exists:  ./scripts/apply-repo-settings.sh ithaca-lab-org/idp-gitops
set -euo pipefail
repo="${1:?usage: $0 <org/repo>}"
here="$(cd "$(dirname "$0")/.." && pwd)"

# Merge hygiene: squash only, PR title as commit message, tidy branches, auto-merge allowed.
gh api -X PATCH "repos/$repo" \
  -F allow_squash_merge=true -F allow_merge_commit=false -F allow_rebase_merge=false \
  -f squash_merge_commit_title=PR_TITLE -f squash_merge_commit_message=PR_BODY \
  -F delete_branch_on_merge=true -F allow_auto_merge=true -F allow_update_branch=true \
  -F has_wiki=false >/dev/null

# Security features (secret scanning and push protection need a public repo or GitHub Advanced Security).
gh api -X PUT "repos/$repo/vulnerability-alerts" >/dev/null
gh api -X PUT "repos/$repo/automated-security-fixes" >/dev/null
gh api -X PATCH "repos/$repo" --input - >/dev/null <<'JSON'
{"security_and_analysis":{"secret_scanning":{"status":"enabled"},"secret_scanning_push_protection":{"status":"enabled"}}}
JSON

# Let Actions open the release-please PR.
gh api -X PUT "repos/$repo/actions/permissions/workflow" \
  -f default_workflow_permissions=read -F can_approve_pull_request_reviews=true >/dev/null

# Branch ruleset (replace if it already exists).
existing=$(gh api "repos/$repo/rulesets" --jq '.[] | select(.name=="protect-main") | .id' || true)
if [ -n "$existing" ]; then
  gh api -X PUT "repos/$repo/rulesets/$existing" --input "$here/.github/rulesets/main.json" >/dev/null
else
  gh api -X POST "repos/$repo/rulesets" --input "$here/.github/rulesets/main.json" >/dev/null
fi
echo "settings applied to $repo"

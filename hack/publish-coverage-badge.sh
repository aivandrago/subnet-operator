#!/usr/bin/env bash
# Write the coverage number onto the gh-pages branch as a shields.io endpoint file.
#
# The README badge points at
#   https://img.shields.io/endpoint?url=https://hypersurgery.dev/badges/coverage.json
# so the number in the README is whatever the last run on main actually measured, rather than
# a figure someone typed once. Usage: hack/publish-coverage-badge.sh 78.4
set -euo pipefail

pct="${1:?usage: publish-coverage-badge.sh <percent>}"
remote="${REMOTE:-origin}"

# shields.io's own convention: red below half, amber up to 80, green above.
color=red
awk "BEGIN{exit !($pct >= 50)}" && color=orange
awk "BEGIN{exit !($pct >= 65)}" && color=yellow
awk "BEGIN{exit !($pct >= 80)}" && color=brightgreen

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

git fetch -q "$remote" gh-pages
git worktree add -q "$work" "$remote/gh-pages" --detach
mkdir -p "$work/badges"
cat > "$work/badges/coverage.json" <<EOF
{"schemaVersion":1,"label":"coverage","message":"${pct}%","color":"${color}"}
EOF

(
  cd "$work"
  git add badges/coverage.json
  if git diff --cached --quiet; then
    echo "coverage unchanged at ${pct}%"
    exit 0
  fi
  git -c user.name="github-actions[bot]" \
      -c user.email="41898282+github-actions[bot]@users.noreply.github.com" \
      commit -qm "Coverage ${pct}%"
  git push -q "$remote" HEAD:gh-pages
  echo "published coverage ${pct}% (${color})"
)
git worktree remove --force "$work"

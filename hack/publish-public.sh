#!/usr/bin/env bash
# Publish the current master as a single commit on GitHub.
#
# The public repository carries the released state, not the development history: one commit,
# authored by the project owner, force-pushed over main. Run it from the repository root after
# master is where you want the public snapshot to be.
set -euo pipefail

remote="${REMOTE:-github}"
branch="${BRANCH:-public-main}"
version="$(grep '^appVersion:' charts/aws-subnet-operator/Chart.yaml | cut -d'"' -f2)"
author="${AUTHOR:-Ivan Drago <aivandrago@users.noreply.github.com>}"
gh_repo="${GH_REPO:-aivandrago/subnet-operator}"

git rev-parse --verify master >/dev/null
git checkout -q master
git branch -qD "$branch" 2>/dev/null || true
git checkout -q --orphan "$branch"
git add -A
git -c "user.name=${author%% <*}" -c "user.email=$(printf '%s' "$author" | sed 's/.*<\(.*\)>/\1/')" \
    commit -qm "subnet-operator v${version}

$(sed -n '/^A Kubernetes operator/,/^$/p' README.md)

This repository carries the released state; development history lives in
the project's own forge."
git push -q -f "$remote" "$branch:main"
git push -q -f "$remote" "HEAD:refs/tags/v${version}"
git checkout -q master

# The public repository shows one thing: the current release. The snapshot commit above is
# only half of that — without a release marked latest, the tag is something you have to go
# looking for. gh is optional so the script still works where it is not installed.
if command -v gh >/dev/null 2>&1; then
  notes="$(mktemp)"
  trap 'rm -f "$notes"' EXIT
  {
    sed -n '/^A Kubernetes operator/,/^$/p' README.md
    echo
    echo "Install with Helm, or read the guide at https://hypersurgery.dev/docs/."
    echo
    echo "This repository carries the released state as a single commit; the development"
    echo "history, issues and pull requests live in the project's own forge."
  } > "$notes"

  if gh release view "v${version}" --repo "$gh_repo" >/dev/null 2>&1; then
    gh release edit "v${version}" --repo "$gh_repo" --target main --latest \
      --title "v${version}" --notes-file "$notes" >/dev/null
    echo "updated release v${version} on $gh_repo"
  else
    gh release create "v${version}" --repo "$gh_repo" --target main --latest \
      --title "v${version}" --notes-file "$notes" >/dev/null
    echo "created release v${version} on $gh_repo"
  fi
fi

echo "published v${version} as a single commit on $remote/main"

#!/usr/bin/env bash
# Build site/ into the gh-pages branch.
#
# GitHub serves a project page under /<repo>/, while our own server and a custom domain serve it
# at the root. The site is written with absolute paths, so this rewrites them for whichever base
# it is published under. Usage:
#
#   hack/publish-pages.sh                      # hypersurgery.dev at the root (the default)
#   BASE=/subnet-operator CNAME= hack/publish-pages.sh    # the old project-path form
set -euo pipefail

base="${BASE:-/}"
remote="${REMOTE:-github}"
cname="${CNAME:-hypersurgery.dev}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

cp -r site/. "$work/"
touch "$work/.nojekyll"   # keep GitHub from running Jekyll over the files

if [ "$base" != "/" ]; then
  # Absolute paths are rewritten onto the base. The rules overlap on purpose (href/src catch
  # markup, the quoted forms catch strings in JS and JSON), so a final pass collapses any
  # prefix that ended up applied twice.
  find "$work" -type f \( -name '*.html' -o -name '*.js' -o -name '*.webmanifest' -o -name '*.xml' -o -name '*.txt' \) \
    -exec sed -i \
      -e "s#href=\"/#href=\"${base}/#g" \
      -e "s#src=\"/#src=\"${base}/#g" \
      -e "s#\"/assets/#\"${base}/assets/#g" \
      -e "s#\"/dashboard/#\"${base}/dashboard/#g" \
      -e "s#'/dashboard/#'${base}/dashboard/#g" \
      -e "s#https://hypersurgery.dev/dashboard/#${base}/dashboard/#g" \
      -e "s#https://hypersurgery.dev/docs/#${base}/docs/#g" \
      -e "s#${base}${base}/#${base}/#g" \
      {} +
  # the worker's scope and the paths it caches must match the base as well
  sed -i "s#'/dashboard/#'${base}/dashboard/#g; s#'/assets/#'${base}/assets/#g; s#${base}${base}/#${base}/#g" "$work/dashboard/sw.js"
fi

[ -n "$cname" ] && printf '%s\n' "$cname" > "$work/CNAME"

# A previous run that died leaves both a worktree registration and the branch behind, and
# a branch checked out in a stale worktree cannot be deleted until it is pruned.
git worktree prune
git branch -qD gh-pages-build 2>/dev/null || true

tmpdir="$(mktemp -d)"
git worktree add -q --detach "$tmpdir"
# Leave nothing registered if a step below fails: a worktree that outlives the run holds the
# build branch, and the next run then cannot create it.
trap 'git worktree remove --force "$tmpdir" 2>/dev/null || true; rm -rf "$work"' EXIT
(
  cd "$tmpdir"
  git checkout -q --orphan gh-pages-build
  git rm -rq --cached . 2>/dev/null || true
  find . -maxdepth 1 -mindepth 1 -not -name .git -exec rm -rf {} +
  cp -r "$work/." .
  # CI writes the coverage badge to this branch, and it is not built from site/. Carry it
  # over so publishing the site does not blank out a badge the README points at.
  if git fetch -q "$remote" gh-pages 2>/dev/null && git cat-file -e FETCH_HEAD:badges 2>/dev/null; then
    git checkout -q FETCH_HEAD -- badges
  fi
  git add -A
  git -c user.name="Ivan Drago" -c user.email="aivandrago@users.noreply.github.com" \
      commit -qm "Publish the site$([ -n "$cname" ] && echo " for $cname")"
  git push -q -f "$remote" HEAD:gh-pages
)
git worktree remove --force "$tmpdir"
echo "published site to $remote/gh-pages (base ${base})"

#!/usr/bin/env bash
set -euo pipefail

# Starts a release and follows it to the end.
#
# The work happens in .github/workflows/release.yml, which bumps
# MARKETING_VERSION, cuts the tag, publishes the archives and attaches the app.
# This is the doorbell: it checks the few things worth catching before a runner
# spins up, then hands over and waits so you do not have to sit in the Actions
# tab.
#
# The pipeline releases whatever is on origin/main, not what is in front of you.

usage () {
  cat >&2 <<'USAGE'
Usage: scripts/release.sh <version> [--yes]

  <version>   the release, with or without the v (0.3.0 or v0.3.0)
  --yes       skip the confirmation prompt

Releasing from the Actions tab does the same thing: run the Release workflow and
give it a version. Pushing a tag by hand still works too.
USAGE
}

die () { echo "ERROR: $*" >&2; exit 1; }
step () { echo "==> $*"; }
note () { echo "    $*"; }

VERSION=""
ASSUME_YES=false

while [ $# -gt 0 ]; do
  case "$1" in
    --yes|-y)  ASSUME_YES=true ;;
    -h|--help) usage; exit 0 ;;
    -*)        usage; die "unknown option $1" ;;
    *)
      [ -n "$VERSION" ] && { usage; die "version given twice: $VERSION and $1"; }
      VERSION="$1"
      ;;
  esac
  shift
done

[ -n "$VERSION" ] || { usage; exit 2; }

cd "$(cd "$(dirname "$0")/.." && pwd)"

NUMBER="${VERSION#v}"
TAG="v$NUMBER"
echo "$NUMBER" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' \
  || die "version must be MAJOR.MINOR.PATCH (got $VERSION)"

command -v gh >/dev/null || die "gh is not installed: https://cli.github.com"
gh auth status >/dev/null 2>&1 || die "gh is not logged in; run: gh auth login"


step "Checking $TAG is free"
git fetch --quiet --tags origin
git ls-remote --exit-code --tags origin "refs/tags/$TAG" >/dev/null 2>&1 \
  && die "$TAG already exists. Start over with: gh release delete $TAG --cleanup-tag"

latest="$(git describe --tags --abbrev=0 origin/main 2>/dev/null || echo "")"
if [ -n "$latest" ]; then
  newest="$(printf '%s\n%s\n' "$latest" "$TAG" | sort -V | tail -1)"
  [ "$newest" = "$TAG" ] || die "$TAG is not newer than $latest"
  note "$latest -> $TAG"
else
  note "first tag: $TAG"
fi

step "Checking what will actually ship"
# Whatever origin/main holds is what gets built. Unpushed work is not in it, and
# finding that out from the release notes is too late.
head="$(git rev-parse origin/main)"
note "origin/main at ${head:0:7}: $(git log -1 --format=%s "$head")"
if [ -n "$(git log --oneline "origin/main..HEAD" 2>/dev/null || true)" ]; then
  note "WARNING: you have commits that are not on origin/main. They will NOT be released:"
  git log --oneline "origin/main..HEAD" | sed 's/^/      /'
fi

echo
if [ "$ASSUME_YES" != true ]; then
  printf "Release %s from origin/main? [y/N] " "$TAG"
  read -r reply < /dev/tty
  case "$reply" in
    y|Y|yes|YES) ;;
    *) die "cancelled; nothing was started" ;;
  esac
fi


# Remembered so the poll below can tell the new run from the last one.
previous="$(gh run list --workflow=release.yml --limit 1 --json databaseId -q '.[0].databaseId' 2>/dev/null || echo "")"

step "Starting the release workflow"
gh workflow run release.yml --ref main -f version="$NUMBER"

step "Waiting for the run to appear"
run=""
for _ in $(seq 1 30); do
  run="$(gh run list --workflow=release.yml --limit 1 --json databaseId -q '.[0].databaseId' 2>/dev/null || echo "")"
  [ -n "$run" ] && [ "$run" != "$previous" ] && break
  run=""
  sleep 3
done

if [ -z "$run" ]; then
  note "the run did not appear in time; follow it at"
  note "https://github.com/lhpalacio/redash-wire/actions/workflows/release.yml"
  exit 0
fi

if gh run watch "$run" --exit-status; then
  echo
  step "Assets on $TAG"
  gh release view "$TAG" --json assets -q '.assets[].name' | sed 's/^/    /'
  # Two jobs: the first publishes the release, the second attaches the app. A
  # failure in the second leaves a real release with only the CLI archives.
  if ! gh release view "$TAG" --json assets -q '.assets[].name' | grep -q 'macos_universal.zip'; then
    echo
    note "WARNING: no macOS app attached. Re-run the failed job:"
    note "  gh run rerun $run --failed"
  fi
  echo
  step "Released https://github.com/lhpalacio/redash-wire/releases/tag/$TAG"
else
  echo
  note "The run failed. Check whether the tag was created before you retry:"
  note "  git ls-remote --tags origin refs/tags/$TAG"
  note "If it was, re-run rather than releasing again:"
  note "  gh run rerun $run --failed"
  note "If it was not, fix the problem and run this script again."
  exit 1
fi

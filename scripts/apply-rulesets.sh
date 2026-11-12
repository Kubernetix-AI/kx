#!/usr/bin/env bash
# apply-rulesets.sh: create or update this repository's GitHub rulesets from
# the JSON files in .github/rulesets/. Run it once the repository is public
# (rulesets on private repositories need a paid GitHub plan), and again after
# changing a JSON file.
#
# Usage: scripts/apply-rulesets.sh [--apply] [--repo OWNER/NAME] [--dir DIR]
#
#   (default)  dry run: reads the current rulesets and prints what it would
#              create or update; changes nothing
#   --apply    creates missing rulesets and updates existing ones, matched
#              by name, so running it twice changes nothing the second time
#   --repo     the repository (default Kubernetix-AI/kx)
#   --dir      the folder of ruleset JSON files (default .github/rulesets
#              in this repository); required with any --repo other than
#              Kubernetix-AI/kx, so kx's rulesets never land elsewhere
#
# Needs gh (signed in as a repository admin) and jq.
set -euo pipefail

repo="Kubernetix-AI/kx"
apply=false
dir="$(cd "$(dirname "$0")/.." && pwd)/.github/rulesets"
dir_given=false

usage() {
  sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//' >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --apply) apply=true ;;
    --repo)
      [ $# -ge 2 ] || usage
      repo="$2"
      shift
      ;;
    --dir)
      [ $# -ge 2 ] || usage
      dir="$2"
      dir_given=true
      shift
      ;;
    -h | --help) usage ;;
    *) usage ;;
  esac
  shift
done

fail() {
  echo "apply-rulesets: $*" >&2
  exit 1
}
warn() { echo "apply-rulesets: warning: $*" >&2; }

[[ "$repo" =~ ^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$ ]] || fail "--repo must look like OWNER/NAME"
if ! $dir_given && [ "$(printf '%s' "$repo" | tr '[:upper:]' '[:lower:]')" != "kubernetix-ai/kx" ]; then
  fail "--repo $repo needs --dir with that repository's rulesets (the default folder holds kx's)"
fi
command -v gh >/dev/null 2>&1 || fail "gh is not installed (https://cli.github.com)"
command -v jq >/dev/null 2>&1 || fail "jq is not installed"

[ -d "$dir" ] || fail "$dir is not a folder"
shopt -s nullglob
files=("$dir"/*.json)
[ ${#files[@]} -gt 0 ] || fail "no rulesets in $dir"

# Every file must be a well-formed ruleset with a unique name.
names=()
for f in "${files[@]}"; do
  jq -e '(.name | type == "string" and length > 0)
    and (.target | IN("branch", "tag"))
    and (.enforcement | IN("active", "evaluate", "disabled"))
    and (.rules | type == "array" and length > 0)' "$f" >/dev/null ||
    fail "$f is not a valid ruleset"
  names+=("$(jq -r .name "$f")")
done
dupes=$(printf '%s\n' "${names[@]}" | sort | uniq -d)
[ -z "$dupes" ] || fail "two files use the ruleset name: $dupes"

# Preconditions. All of these are read-only calls.
visibility=$(gh api "repos/$repo" --jq .visibility) || fail "can't read $repo (signed in to gh? an admin of the repo?)"
if [ "$visibility" != "public" ]; then
  if $apply; then
    fail "$repo is $visibility. Make it public first: rulesets on private repositories need a paid plan."
  fi
  warn "$repo is $visibility; --apply will refuse until it is public."
fi

org="${repo%%/*}"
twofa=$(gh api "orgs/$org" --jq '.two_factor_requirement_enabled' 2>/dev/null || true)
case "$twofa" in
  true) echo "OK: $org requires two-factor authentication." ;;
  false) warn "$org does not require two-factor authentication. Turn it on: Organization settings → Authentication security." ;;
  *) warn "couldn't check whether $org requires two-factor authentication (needs an org owner); check it by hand." ;;
esac

existing=$(gh api --paginate "repos/$repo/rulesets?includes_parents=false" --jq '.[] | "\(.id)\t\(.name)"') ||
  fail "can't list the rulesets of $repo"

for f in "${files[@]}"; do
  name=$(jq -r .name "$f")
  ids=$(printf '%s\n' "$existing" | awk -F '\t' -v n="$name" '$2 == n { print $1 }')
  count=$(printf '%s' "$ids" | grep -c . || true)
  if [ "$count" -gt 1 ]; then
    fail "$repo has $count rulesets named \"$name\"; delete the extra ones in Settings → Rules, then run this again"
  fi
  if [ "$count" -eq 1 ]; then
    [[ "$ids" =~ ^[0-9]+$ ]] || fail "unexpected ruleset id for \"$name\""
    if $apply; then
      gh api --method PUT "repos/$repo/rulesets/$ids" --input "$f" >/dev/null
      echo "Updated ruleset \"$name\" ($ids) from ${f##*/}"
    else
      echo "Would update ruleset \"$name\" ($ids) from ${f##*/}"
    fi
  else
    if $apply; then
      gh api --method POST "repos/$repo/rulesets" --input "$f" >/dev/null
      echo "Created ruleset \"$name\" from ${f##*/}"
    else
      echo "Would create ruleset \"$name\" from ${f##*/}"
    fi
  fi
done

if $apply; then
  echo "Done. Check Settings → Rules → Rulesets on GitHub: \"Release tags\" should list Repository admin under bypass."
else
  echo "Dry run: nothing changed. Run again with --apply to make these changes."
fi

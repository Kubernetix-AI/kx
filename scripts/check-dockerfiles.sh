#!/usr/bin/env bash
# check-dockerfiles.sh: build every Dockerfile kx deploy writes and check that
# the app in it starts and answers.
#
# Usage: scripts/check-dockerfiles.sh [DIR]
#
#   DIR  the test apps, one folder each with the want.Dockerfile and
#        want.dockerignore kx writes for it (default: internal/scaffold/testdata)
#
# For each folder: copy it, put want.Dockerfile and want.dockerignore in place as
# Dockerfile and .dockerignore (what the folder holds after kx writes them),
# docker build --platform linux/amd64, and run the image as an arbitrary
# non-root user that has no home folder, with every capability dropped. It
# passes once GET / on port 8080 answers 200 with the folder's name in the body,
# within 60 seconds, and the container is still running 2 seconds later. It
# needs a Docker engine, pulls public base images, and never logs in to a
# registry or pushes. CI runs it on Linux.
set -euo pipefail

root="${1:-internal/scaffold/testdata}"
shopt -s nullglob
dockerfiles=("$root"/*/want.Dockerfile)
if [ "${#dockerfiles[@]}" -eq 0 ]; then
  echo "check-dockerfiles: no */want.Dockerfile under $root" >&2
  exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# The user the containers run as: not the image's own, and not in its
# /etc/passwd, so HOME is / and nothing but /tmp is writable.
run_as=10101:10101

failed=()
fail() {
  echo "::error::$1: $2"
  failed+=("$1")
}

for df in "${dockerfiles[@]}"; do
  dir="$(dirname "$df")"
  name="$(basename "$dir")"
  image="kx-check-$name"
  ctx="$work/$name"
  echo "::group::$name"
  if [ ! -f "$dir/want.dockerignore" ]; then
    echo "::endgroup::"
    fail "$name" "no want.dockerignore beside want.Dockerfile"
    continue
  fi
  mkdir -p "$ctx"
  cp -R "$dir/." "$ctx/"
  mv "$ctx/want.Dockerfile" "$ctx/Dockerfile"
  mv "$ctx/want.dockerignore" "$ctx/.dockerignore"

  if ! docker build --platform linux/amd64 --tag "$image" "$ctx"; then
    echo "::endgroup::"
    fail "$name" "docker build failed"
    continue
  fi
  if ! cid="$(docker run --detach --platform linux/amd64 --user "$run_as" \
    --cap-drop ALL --security-opt no-new-privileges "$image")"; then
    echo "::endgroup::"
    fail "$name" "docker run failed"
    continue
  fi
  ip="$(docker inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$cid")"
  answered=false
  deadline=$((SECONDS + 60))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if [ "$(docker inspect --format '{{.State.Running}}' "$cid")" != true ]; then
      break
    fi
    # Straight to the container's address: a published port would accept
    # a connection even when nothing listens inside. An answer, not just a
    # connection: a server can open its port before the app has loaded.
    if body="$(curl --fail --silent --show-error --max-time 2 "http://$ip:8080/" 2>/dev/null)" &&
      grep -q -- "$name" <<<"$body"; then
      answered=true
      break
    fi
    sleep 1
  done
  still_running=false
  if [ "$answered" = true ]; then
    sleep 2
    if [ "$(docker inspect --format '{{.State.Running}}' "$cid")" = true ]; then
      still_running=true
      echo "$name: answers on port 8080 as user $run_as"
    fi
  fi
  if [ "$still_running" != true ]; then
    docker logs "$cid" || true
  fi
  docker rm --force "$cid" >/dev/null
  echo "::endgroup::"
  if [ "$answered" != true ]; then
    fail "$name" "GET / on port 8080 didn't answer 200 with \"$name\" within 60 seconds"
  elif [ "$still_running" != true ]; then
    fail "$name" "the container stopped after answering"
  fi
done

if [ "${#failed[@]}" -gt 0 ]; then
  echo "check-dockerfiles: failed: ${failed[*]}" >&2
  exit 1
fi
echo "check-dockerfiles: all ${#dockerfiles[@]} Dockerfiles build, and their apps answer on port 8080"

#!/usr/bin/env bash
# check-dockerfiles.sh: build every Dockerfile kx deploy writes and check that
# the app in it starts listening.
#
# Usage: scripts/check-dockerfiles.sh [DIR]
#
#   DIR  the test apps, one folder each with the want.Dockerfile kx writes for
#        it (default: internal/scaffold/testdata)
#
# For each folder: docker build --platform linux/amd64 with want.Dockerfile,
# run the image, and pass once something in the container accepts a TCP
# connection on port 8080, within 60 seconds. It needs a Docker engine, pulls
# public base images, and never logs in to a registry or pushes. CI runs it on
# Linux.
set -euo pipefail

root="${1:-internal/scaffold/testdata}"
shopt -s nullglob
dockerfiles=("$root"/*/want.Dockerfile)
if [ "${#dockerfiles[@]}" -eq 0 ]; then
  echo "check-dockerfiles: no */want.Dockerfile under $root" >&2
  exit 1
fi

failed=()
for df in "${dockerfiles[@]}"; do
  dir="$(dirname "$df")"
  name="$(basename "$dir")"
  image="kx-check-$name"
  echo "::group::$name"
  if ! docker build --platform linux/amd64 --file "$df" --tag "$image" "$dir"; then
    echo "::endgroup::"
    echo "::error::$name: docker build failed"
    failed+=("$name")
    continue
  fi
  cid="$(docker run --detach --platform linux/amd64 "$image")"
  ip="$(docker inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$cid")"
  listening=false
  deadline=$((SECONDS + 60))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if [ "$(docker inspect --format '{{.State.Running}}' "$cid")" != true ]; then
      break
    fi
    # Straight to the container's address: a published port would accept
    # the connection even when nothing listens inside.
    if timeout 2 bash -c "exec 3<>/dev/tcp/$ip/8080" 2>/dev/null; then
      listening=true
      break
    fi
    sleep 1
  done
  if [ "$listening" = true ]; then
    echo "$name: listening on port 8080"
  else
    docker logs "$cid" || true
  fi
  docker rm --force "$cid" >/dev/null
  echo "::endgroup::"
  if [ "$listening" != true ]; then
    echo "::error::$name: nothing accepted a connection on port 8080 within 60 seconds"
    failed+=("$name")
  fi
done

if [ "${#failed[@]}" -gt 0 ]; then
  echo "check-dockerfiles: failed: ${failed[*]}" >&2
  exit 1
fi
echo "check-dockerfiles: all ${#dockerfiles[@]} Dockerfiles build and listen on port 8080"

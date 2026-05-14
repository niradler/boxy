#!/usr/bin/env bash
# Local release script — mirrors .github/workflows/release.yml.disabled
# Usage: ./scripts/release.sh v0.2.0
#
# Required env vars:
#   DOCKERHUB_USERNAME   your Docker Hub username
#   DOCKERHUB_TOKEN      your Docker Hub access token
#
# Optional env vars:
#   GHCR_ORG             GHCR org/user (default: niradler)
#   SKIP_TESTS           set to 1 to skip go vet / go test / cargo test
#   SKIP_E2E             set to 1 to skip e2e (default: 1 locally)
#   SKIP_PUSH            set to 1 to build without pushing (dry-run)
#   SKIP_RELEASE         set to 1 to skip GitHub release creation
#
# Prerequisites: docker (with buildx + QEMU), gh CLI, go, cargo

set -euo pipefail

# ---------------------------------------------------------------------------
# Args + defaults
# ---------------------------------------------------------------------------
VERSION="${1:-}"
if [[ -z "$VERSION" ]]; then
  echo "Usage: $0 <version>  (e.g. v0.2.0)" >&2
  exit 1
fi
if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "Version must be semver with leading v, e.g. v0.2.0" >&2
  exit 1
fi

GHCR_ORG="${GHCR_ORG:-niradler}"
SKIP_TESTS="${SKIP_TESTS:-0}"
SKIP_E2E="${SKIP_E2E:-1}"
SKIP_PUSH="${SKIP_PUSH:-0}"
SKIP_RELEASE="${SKIP_RELEASE:-0}"

MAJOR_MINOR="$(echo "$VERSION" | grep -oP 'v\K[0-9]+\.[0-9]+')"

echo "==> Releasing $VERSION (GHCR: ghcr.io/$GHCR_ORG, DockerHub: $DOCKERHUB_USERNAME)"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
tags() {
  local name="$1"
  local -a out=(
    "ghcr.io/$GHCR_ORG/$name:${VERSION#v}"
    "ghcr.io/$GHCR_ORG/$name:$MAJOR_MINOR"
    "ghcr.io/$GHCR_ORG/$name:latest"
    "$DOCKERHUB_USERNAME/$name:${VERSION#v}"
    "$DOCKERHUB_USERNAME/$name:$MAJOR_MINOR"
    "$DOCKERHUB_USERNAME/$name:latest"
  )
  local result=""
  for t in "${out[@]}"; do
    result+="--tag $t "
  done
  echo "$result"
}

build_push() {
  local name="$1"
  local dockerfile="$2"
  local platforms="$3"
  local push_flag="--push"
  [[ "$SKIP_PUSH" == "1" ]] && push_flag="--load"

  echo ""
  echo "==> Building $name ($platforms)"
  # shellcheck disable=SC2086
  docker buildx build \
    --file "$dockerfile" \
    --platform "$platforms" \
    $(tags "$name") \
    $push_flag \
    .
}

# ---------------------------------------------------------------------------
# 1. Unit tests
# ---------------------------------------------------------------------------
if [[ "$SKIP_TESTS" != "1" ]]; then
  echo ""
  echo "==> Go vet + test"
  go vet ./...
  go test ./...

  echo ""
  echo "==> Cargo test"
  (cd controller && cargo test)
fi

# ---------------------------------------------------------------------------
# 2. E2E (skipped locally by default)
# ---------------------------------------------------------------------------
if [[ "$SKIP_E2E" != "1" ]]; then
  echo ""
  echo "==> E2E (make e2e)"
  make e2e
fi

# ---------------------------------------------------------------------------
# 3. Tag the commit
# ---------------------------------------------------------------------------
if git rev-parse "$VERSION" >/dev/null 2>&1; then
  echo ""
  echo "==> Tag $VERSION already exists — skipping tag creation"
else
  echo ""
  echo "==> Creating git tag $VERSION"
  git tag -a "$VERSION" -m "Release $VERSION"
  git push origin "$VERSION"
fi

# ---------------------------------------------------------------------------
# 4. Docker login
# ---------------------------------------------------------------------------
if [[ "$SKIP_PUSH" != "1" ]]; then
  echo ""
  echo "==> Logging in to GHCR"
  echo "$GITHUB_TOKEN" | docker login ghcr.io -u "$GHCR_ORG" --password-stdin

  echo ""
  echo "==> Logging in to Docker Hub"
  echo "$DOCKERHUB_TOKEN" | docker login -u "$DOCKERHUB_USERNAME" --password-stdin
fi

# ---------------------------------------------------------------------------
# 5. Ensure buildx builder with QEMU support
# ---------------------------------------------------------------------------
if ! docker buildx inspect boxy-release >/dev/null 2>&1; then
  echo ""
  echo "==> Setting up buildx builder (boxy-release)"
  docker run --rm --privileged multiarch/qemu-user-static --reset -p yes
  docker buildx create --name boxy-release --driver docker-container --bootstrap
fi
docker buildx use boxy-release

# ---------------------------------------------------------------------------
# 6. Build and push images
# ---------------------------------------------------------------------------
build_push "boxy-router"     "Dockerfile.router"     "linux/amd64,linux/arm64"
build_push "boxy-operator"   "Dockerfile.operator"   "linux/amd64,linux/arm64"
build_push "boxy-controller" "Dockerfile.controller" "linux/amd64"

# ---------------------------------------------------------------------------
# 7. GitHub Release
# ---------------------------------------------------------------------------
if [[ "$SKIP_RELEASE" != "1" ]]; then
  echo ""
  echo "==> Creating GitHub Release $VERSION"
  gh release create "$VERSION" \
    --generate-notes \
    --title "$VERSION"
fi

echo ""
echo "==> Done. Released $VERSION"

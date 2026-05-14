#!/usr/bin/env bash
# Local release script — mirrors .github/workflows/release.yml.disabled
# Usage: ./scripts/release.sh v0.0.2
#
# No required env vars — auto-detects Docker Hub and GitHub credentials.
#
# Optional env vars:
#   GHCR_ORG          GHCR org/user (default: niradler)
#   DOCKERHUB_USER    Docker Hub username (default: niradler)
#   DOCKERHUB_TOKEN   if set, re-authenticates Docker Hub; otherwise uses existing login
#   PUSH_GHCR         set to 1 to also push to GHCR (requires write:packages scope —
#                     run: gh auth refresh -s write:packages  once to enable)
#   SKIP_TESTS        set to 1 to skip go vet / go test / cargo test
#   SKIP_E2E          set to 1 to skip e2e (default: 1 — needs a kind cluster)
#   SKIP_PUSH         set to 1 to build without pushing (dry-run)
#   SKIP_RELEASE      set to 1 to skip GitHub release creation
#
# Prerequisites: docker (with buildx), gh CLI, go, cargo

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
DOCKERHUB_USER="${DOCKERHUB_USER:-niradler}"
PUSH_GHCR="${PUSH_GHCR:-0}"
SKIP_TESTS="${SKIP_TESTS:-0}"
SKIP_E2E="${SKIP_E2E:-1}"
SKIP_PUSH="${SKIP_PUSH:-0}"
SKIP_RELEASE="${SKIP_RELEASE:-0}"

VER="${VERSION#v}"
MAJOR_MINOR="$(echo "$VER" | sed 's/\.[^.]*$//')"

echo "==> Releasing $VERSION  (DockerHub: $DOCKERHUB_USER  GHCR: $([ "$PUSH_GHCR" = "1" ] && echo "ghcr.io/$GHCR_ORG" || echo "disabled — set PUSH_GHCR=1 to enable"))"

# ---------------------------------------------------------------------------
# Tag list builder
# ---------------------------------------------------------------------------
_tags() {
  local name="$1"
  local args=(
    "--tag" "$DOCKERHUB_USER/$name:$VER"
    "--tag" "$DOCKERHUB_USER/$name:$MAJOR_MINOR"
    "--tag" "$DOCKERHUB_USER/$name:latest"
  )
  if [[ "$PUSH_GHCR" == "1" ]]; then
    args+=(
      "--tag" "ghcr.io/$GHCR_ORG/$name:$VER"
      "--tag" "ghcr.io/$GHCR_ORG/$name:$MAJOR_MINOR"
      "--tag" "ghcr.io/$GHCR_ORG/$name:latest"
    )
  fi
  echo "${args[@]}"
}

build_push() {
  local name="$1" dockerfile="$2" platforms="$3"
  local action="--push"
  [[ "$SKIP_PUSH" == "1" ]] && action="--load"
  echo ""
  echo "==> Building $name ($platforms)"
  # shellcheck disable=SC2046
  docker buildx build \
    --file "$dockerfile" \
    --platform "$platforms" \
    $(_tags "$name") \
    $action \
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
# 2. E2E (skipped locally by default — needs a kind cluster)
# ---------------------------------------------------------------------------
if [[ "$SKIP_E2E" != "1" ]]; then
  echo ""
  echo "==> E2E (make e2e)"
  make e2e
fi

# ---------------------------------------------------------------------------
# 3. Git tag
# ---------------------------------------------------------------------------
if git rev-parse "$VERSION" >/dev/null 2>&1; then
  echo ""
  echo "==> Tag $VERSION already exists — skipping"
else
  echo ""
  echo "==> Creating git tag $VERSION"
  git tag -a "$VERSION" -m "Release $VERSION"
  git push origin "$VERSION"
fi

# ---------------------------------------------------------------------------
# 4. Registry login
# ---------------------------------------------------------------------------
if [[ "$SKIP_PUSH" != "1" ]]; then
  if [[ "$PUSH_GHCR" == "1" ]]; then
    GH_TOKEN="$(gh auth token 2>/dev/null || true)"
    if [[ -z "$GH_TOKEN" ]]; then
      echo "ERROR: PUSH_GHCR=1 but gh CLI is not authenticated." >&2
      exit 1
    fi
    echo ""
    echo "==> Logging in to GHCR"
    echo "$GH_TOKEN" | docker login ghcr.io -u "$GHCR_ORG" --password-stdin
    echo "    (if this push fails with 'permission_denied', run: gh auth refresh -s write:packages)"
  fi

  if [[ -n "${DOCKERHUB_TOKEN:-}" ]]; then
    echo ""
    echo "==> Logging in to Docker Hub"
    echo "$DOCKERHUB_TOKEN" | docker login -u "$DOCKERHUB_USER" --password-stdin
  else
    echo ""
    echo "==> Docker Hub: using existing login"
  fi
fi

# ---------------------------------------------------------------------------
# 5. Buildx builder
# ---------------------------------------------------------------------------
if ! docker buildx inspect boxy-release >/dev/null 2>&1; then
  echo ""
  echo "==> Creating buildx builder (boxy-release)"
  docker buildx create --name boxy-release --driver docker-container --bootstrap
fi
docker buildx use boxy-release

# ---------------------------------------------------------------------------
# 6. Build + push
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
  gh release create "$VERSION" --generate-notes --title "$VERSION" || \
    echo "WARNING: release may already exist"
fi

echo ""
echo "==> Done. Released $VERSION"

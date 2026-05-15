#!/usr/bin/env bash
# Local release script
# Usage: ./scripts/release.sh v0.0.2
#
# Optional env vars:
#   GHCR_ORG          GHCR org/user (default: niradler)
#   DOCKERHUB_USER    Docker Hub username (default: niradler)
#   DOCKERHUB_TOKEN   if set, re-authenticates Docker Hub; otherwise uses existing login
#   PUSH_GHCR         set to 1 to push images to GHCR (default: 0)
#   PUSH_HELM         set to 0 to skip helm chart push to GHCR OCI (default: 1)
#   SKIP_TESTS        set to 1 to skip go vet / go test / cargo test
#   SKIP_E2E          set to 1 to skip e2e (default: 1 — needs a kind cluster)
#   SKIP_PUSH         set to 1 to build without pushing (dry-run)
#   SKIP_RELEASE      set to 1 to skip GitHub release creation
#
# Prerequisites: docker (with buildx), gh CLI (write:packages scope), go, cargo, helm

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
PUSH_HELM="${PUSH_HELM:-1}"
SKIP_TESTS="${SKIP_TESTS:-0}"
SKIP_E2E="${SKIP_E2E:-1}"
SKIP_PUSH="${SKIP_PUSH:-0}"
SKIP_RELEASE="${SKIP_RELEASE:-0}"

VER="${VERSION#v}"
MAJOR_MINOR="$(echo "$VER" | sed 's/\.[^.]*$//')"
HELM_CHART_DIR="deploy/helm/boxy"

echo "==> Releasing $VERSION"
echo "    DockerHub : $DOCKERHUB_USER"
echo "    GHCR imgs : $([ "$PUSH_GHCR" = "1" ] && echo "ghcr.io/$GHCR_ORG" || echo "disabled — set PUSH_GHCR=1 to enable")"
echo "    Helm OCI  : $([ "$PUSH_HELM" = "1" ] && [ "$SKIP_PUSH" != "1" ] && echo "oci://ghcr.io/$GHCR_ORG/charts" || echo "disabled")"

# ---------------------------------------------------------------------------
# Tag list builder — returns args as array elements via nameref
# ---------------------------------------------------------------------------
_tags() {
  local name="$1"
  local -n _out="$2"
  _out=(
    "--tag" "$DOCKERHUB_USER/$name:$VER"
    "--tag" "$DOCKERHUB_USER/$name:$MAJOR_MINOR"
    "--tag" "$DOCKERHUB_USER/$name:latest"
  )
  if [[ "$PUSH_GHCR" == "1" ]]; then
    _out+=(
      "--tag" "ghcr.io/$GHCR_ORG/$name:$VER"
      "--tag" "ghcr.io/$GHCR_ORG/$name:$MAJOR_MINOR"
      "--tag" "ghcr.io/$GHCR_ORG/$name:latest"
    )
  fi
}

build_push() {
  local name="$1" dockerfile="$2" platforms="$3"
  local -a tag_args action_args
  _tags "$name" tag_args

  if [[ "$SKIP_PUSH" == "1" ]]; then
    # Build multi-platform without pushing — verifies the build compiles correctly
    action_args=("--output" "type=image,push=false")
  else
    action_args=("--push")
  fi

  echo ""
  echo "==> Building $name ($platforms)"
  docker buildx build \
    --file "$dockerfile" \
    --platform "$platforms" \
    "${tag_args[@]}" \
    "${action_args[@]}" \
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
# 3. Bump Chart.yaml version
# ---------------------------------------------------------------------------
echo ""
echo "==> Bumping Chart.yaml to version $VER"
sed -i "s/^version:.*/version: $VER/" "$HELM_CHART_DIR/Chart.yaml"
sed -i "s/^appVersion:.*/appVersion: \"$VER\"/" "$HELM_CHART_DIR/Chart.yaml"

# ---------------------------------------------------------------------------
# 4. Git tag
# ---------------------------------------------------------------------------
if git rev-parse "$VERSION" >/dev/null 2>&1; then
  echo ""
  echo "==> Tag $VERSION already exists — skipping tag"
else
  echo ""
  echo "==> Committing Chart.yaml bump and creating git tag $VERSION"
  git add "$HELM_CHART_DIR/Chart.yaml"
  git diff --cached --quiet || git commit -m "chore: bump chart version to $VER"
  git tag -a "$VERSION" -m "Release $VERSION"
  git push origin HEAD
  git push origin "$VERSION"
fi

# ---------------------------------------------------------------------------
# 5. Registry login
# ---------------------------------------------------------------------------
if [[ "$SKIP_PUSH" != "1" ]]; then
  GH_TOKEN="$(gh auth token 2>/dev/null || true)"
  if [[ -z "$GH_TOKEN" ]]; then
    echo "ERROR: gh CLI is not authenticated. Run: gh auth login" >&2
    exit 1
  fi

  if [[ "$PUSH_GHCR" == "1" ]] || [[ "$PUSH_HELM" == "1" ]]; then
    echo ""
    echo "==> Logging in to GHCR"
    echo "$GH_TOKEN" | docker login ghcr.io -u "$GHCR_ORG" --password-stdin
    # helm uses its own registry credential store
    echo "$GH_TOKEN" | helm registry login ghcr.io -u "$GHCR_ORG" --password-stdin
    echo "    (if push fails with 'permission_denied', run: gh auth refresh -s write:packages)"
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
# 6. Buildx builder
# ---------------------------------------------------------------------------
if ! docker buildx inspect boxy-release >/dev/null 2>&1; then
  echo ""
  echo "==> Creating buildx builder (boxy-release)"
  docker buildx create --name boxy-release --driver docker-container --bootstrap
fi
docker buildx use boxy-release

# ---------------------------------------------------------------------------
# 7. Build + push images
# ---------------------------------------------------------------------------
build_push "boxy-router"     "Dockerfile.router"     "linux/amd64,linux/arm64"
build_push "boxy-operator"   "Dockerfile.operator"   "linux/amd64,linux/arm64"
build_push "boxy-controller" "Dockerfile.controller" "linux/amd64"

# ---------------------------------------------------------------------------
# 8. Helm chart — package + push to GHCR OCI
# ---------------------------------------------------------------------------
if [[ "$PUSH_HELM" == "1" ]] && [[ "$SKIP_PUSH" != "1" ]]; then
  echo ""
  echo "==> Packaging helm chart (version $VER)"
  HELM_PKG_DIR="$(mktemp -d)"
  helm package "$HELM_CHART_DIR" --destination "$HELM_PKG_DIR" --version "$VER" --app-version "$VER"

  echo "==> Pushing helm chart to oci://ghcr.io/$GHCR_ORG/charts"
  helm push "$HELM_PKG_DIR/boxy-${VER}.tgz" "oci://ghcr.io/$GHCR_ORG/charts"
  rm -rf "$HELM_PKG_DIR"
  echo "    Install: helm install boxy oci://ghcr.io/$GHCR_ORG/charts/boxy --version $VER"
fi

# ---------------------------------------------------------------------------
# 9. GitHub Release
# ---------------------------------------------------------------------------
if [[ "$SKIP_RELEASE" != "1" ]]; then
  echo ""
  echo "==> Creating GitHub Release $VERSION"
  gh release create "$VERSION" --generate-notes --title "$VERSION" || \
    echo "WARNING: release may already exist — skipping"
fi

echo ""
echo "==> Done. Released $VERSION"

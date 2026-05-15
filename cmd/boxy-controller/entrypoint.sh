#!/usr/bin/env bash
# Entrypoint for boxy-controller.
#
# If BOXY_PREINSTALL_PACKAGES is set (comma-separated apt package names),
# installs them into the sandbox rootfs (ubuntu-24.04) at startup so that all
# sandboxes created on this controller node have those packages available.
#
# Example:
#   BOXY_PREINSTALL_PACKAGES=python3,python3-pip,nodejs,curl,jq
#
# Packages are installed once per pod lifetime. On restart the installation
# repeats (the rootfs layer is ephemeral within the pod).
set -euo pipefail

ROOTFS="${BOXY_NSJAIL_ROOTFS:-/rootfs/ubuntu-24.04}"
PACKAGES="${BOXY_PREINSTALL_PACKAGES:-}"

if [[ -n "${PACKAGES}" ]]; then
    # Normalize: replace commas with spaces, trim surrounding whitespace.
    PKG_LIST="${PACKAGES//,/ }"
    PKG_LIST="$(echo "${PKG_LIST}" | tr -s ' ' | sed 's/^ //;s/ $//')"

    echo "[entrypoint] Installing packages into ${ROOTFS}: ${PKG_LIST}"

    # Bind-mount host pseudo-filesystems so apt post-install scripts work.
    mount --bind /proc  "${ROOTFS}/proc"
    mount --bind /sys   "${ROOTFS}/sys"
    mount --bind /dev   "${ROOTFS}/dev"
    # Forward DNS resolution.
    cp /etc/resolv.conf "${ROOTFS}/etc/resolv.conf"

    # Install; clean up lists afterwards to keep the layer small.
    chroot "${ROOTFS}" apt-get update -qq
    # shellcheck disable=SC2086
    chroot "${ROOTFS}" apt-get install -y --no-install-recommends ${PKG_LIST}
    chroot "${ROOTFS}" apt-get clean
    rm -rf "${ROOTFS}/var/lib/apt/lists/"*

    # Unmount (best-effort; failures are non-fatal).
    umount "${ROOTFS}/proc"  2>/dev/null || true
    umount "${ROOTFS}/sys"   2>/dev/null || true
    umount "${ROOTFS}/dev"   2>/dev/null || true

    echo "[entrypoint] Package installation complete"
fi

exec /usr/local/bin/boxy-controller "$@"

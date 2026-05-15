#!/usr/bin/env bash
# Validate allowedBinaries with the dev controller image.
#
# Requires the cluster to be running the dev controller image
# (boxydev/boxy-controller-dev:e2e) which has jq, yq, curl, git,
# python3, and node in /usr/local/bin (bind-mount source).
#
# Covers:
#   - allowedBinaries=["jq"]: jq executes; yq is blocked.
#   - allowedBinaries=["yq"]: yq executes; jq is blocked.
#   - allowedBinaries=[]   : both jq and yq are blocked.
#   - Cross-sandbox: sandbox A's jq bind-mount is not visible to sandbox B.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

if [[ -z "${BASE_URL}" || -z "${ROUTER_TOKEN}" ]]; then
  echo "BASE_URL and ROUTER_TOKEN are required"
  exit 1
fi

# -----------------------------------------------------------------------
# Pre-condition: verify controller has jq at the bind-mount source path
# -----------------------------------------------------------------------

suite "Dev Image Pre-condition"

: "${RELEASE_NAME:=boxy}"
ctrl_pod=$(kctl get pod -l app=boxy-controller -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
if [[ -z "${ctrl_pod}" ]]; then
  fail "No boxy-controller pod found — is the cluster running?"
  summary
  exit 1
fi

jq_present=$(kctl exec "${ctrl_pod}" -- test -f /usr/local/bin/jq && echo "yes" || echo "no")
yq_present=$(kctl exec "${ctrl_pod}" -- test -f /usr/local/bin/yq && echo "yes" || echo "no")

if [[ "${jq_present}" != "yes" || "${yq_present}" != "yes" ]]; then
  skip "Dev controller image not deployed (jq=${jq_present}, yq=${yq_present}) — deploy with 'make kind-load-dev' and update controller image in Helm values"
  summary
  exit 0
fi
pass "Controller pod has jq and yq at /usr/local/bin"

# Rootfs cleanliness: binaries must NOT be in the sandbox rootfs PATH.
# A rootfs that contains dev tools would bypass the allowedBinaries whitelist
# (all sandboxes would see the tool at /usr/bin/<name> regardless of the list).
jq_in_rootfs=$(kctl exec "${ctrl_pod}" -- test -f /rootfs/ubuntu-24.04/usr/bin/jq && echo "yes" || echo "no")
yq_in_rootfs=$(kctl exec "${ctrl_pod}" -- test -f /rootfs/ubuntu-24.04/usr/bin/yq && echo "yes" || echo "no")

if [[ "${jq_in_rootfs}" == "yes" || "${yq_in_rootfs}" == "yes" ]]; then
  fail "Rootfs contamination: jq_in_rootfs=${jq_in_rootfs} yq_in_rootfs=${yq_in_rootfs} — rebuild with 'make docker-build-dev'"
  summary
  exit 1
fi
pass "Sandbox rootfs does not contain jq or yq (allowedBinaries whitelist is effective)"

# -----------------------------------------------------------------------
# Setup
# -----------------------------------------------------------------------

SB_JQ=$(unique_id)    # only jq allowed
SB_YQ=$(unique_id)    # only yq allowed
SB_NONE=$(unique_id)  # no allowed binaries
SB_CROSS=$(unique_id) # for cross-sandbox check

curl_api POST "/v1/sandboxes" \
  -d "{\"sessionId\":\"binaries\",\"sandboxId\":\"${SB_JQ}\",\"owner\":\"e2e\",\"ttlSeconds\":300,
       \"allowedBinaries\":[\"jq\"]}" >/dev/null

curl_api POST "/v1/sandboxes" \
  -d "{\"sessionId\":\"binaries\",\"sandboxId\":\"${SB_YQ}\",\"owner\":\"e2e\",\"ttlSeconds\":300,
       \"allowedBinaries\":[\"yq\"]}" >/dev/null

curl_api POST "/v1/sandboxes" \
  -d "{\"sessionId\":\"binaries\",\"sandboxId\":\"${SB_NONE}\",\"owner\":\"e2e\",\"ttlSeconds\":300}" >/dev/null

curl_api POST "/v1/sandboxes" \
  -d "{\"sessionId\":\"binaries\",\"sandboxId\":\"${SB_CROSS}\",\"owner\":\"e2e\",\"ttlSeconds\":300,
       \"allowedBinaries\":[\"yq\"]}" >/dev/null

for sb in "${SB_JQ}" "${SB_YQ}" "${SB_NONE}" "${SB_CROSS}"; do
  wait_sandbox_ready "${sb}" 120
done

# -----------------------------------------------------------------------
# allowedBinaries=["jq"]: jq works, yq blocked
# -----------------------------------------------------------------------

suite "allowedBinaries=[jq]"

jq_works=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"binaries\",\"sandboxId\":\"${SB_JQ}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"echo '{\\\"k\\\":\\\"v\\\"}' | jq -r '.k' 2>/dev/null || echo failed\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "jq executes in allowedBinaries=[jq] sandbox" "v" "${jq_works}"

yq_blocked=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"binaries\",\"sandboxId\":\"${SB_JQ}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"which yq 2>/dev/null && yq --version 2>/dev/null && echo present || echo absent\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "yq is absent in allowedBinaries=[jq] sandbox" "absent" "${yq_blocked}"

# -----------------------------------------------------------------------
# allowedBinaries=["yq"]: yq works, jq blocked
# -----------------------------------------------------------------------

suite "allowedBinaries=[yq]"

yq_works=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"binaries\",\"sandboxId\":\"${SB_YQ}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"yq --version 2>/dev/null && echo ok || echo failed\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_contains "yq executes in allowedBinaries=[yq] sandbox" "${yq_works}" "ok"

jq_blocked=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"binaries\",\"sandboxId\":\"${SB_YQ}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"which jq 2>/dev/null && jq --version 2>/dev/null && echo present || echo absent\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "jq is absent in allowedBinaries=[yq] sandbox" "absent" "${jq_blocked}"

# -----------------------------------------------------------------------
# allowedBinaries=[]: both blocked
# -----------------------------------------------------------------------

suite "allowedBinaries=[]"

none_jq=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"binaries\",\"sandboxId\":\"${SB_NONE}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"which jq 2>/dev/null && echo present || echo absent\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "jq absent when no allowedBinaries" "absent" "${none_jq}"

none_yq=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"binaries\",\"sandboxId\":\"${SB_NONE}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"which yq 2>/dev/null && echo present || echo absent\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "yq absent when no allowedBinaries" "absent" "${none_yq}"

# -----------------------------------------------------------------------
# Cross-sandbox: SB_JQ's jq bind-mount not visible to SB_CROSS (has yq only)
# -----------------------------------------------------------------------

suite "Cross-Sandbox Binary Isolation"

# SB_CROSS has allowedBinaries=["yq"]. Verify it cannot see jq from SB_JQ.
cross_jq=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"binaries\",\"sandboxId\":\"${SB_CROSS}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"which jq 2>/dev/null && echo present || echo absent\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "SB_CROSS (allowedBinaries=[yq]) does not see SB_JQ's jq bind-mount" "absent" "${cross_jq}"

cross_yq=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"binaries\",\"sandboxId\":\"${SB_CROSS}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"which yq 2>/dev/null && echo present || echo absent\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "SB_CROSS can see its own yq bind-mount" "present" "${cross_yq}"

# -----------------------------------------------------------------------
# Binary visibility: ls /usr/local/bin/ lists ONLY the granted binary
# Regression test for nsjail mount-point artifact leakage (0-byte files
# left in the shared rootfs that made non-permitted binary names visible).
# -----------------------------------------------------------------------

suite "Binary Visibility — Filesystem Listing"

# SB_JQ (allowedBinaries=["jq"]): yq must not appear in ls output
jq_sees_yq=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"binaries\",\"sandboxId\":\"${SB_JQ}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"ls /usr/local/bin/yq 2>/dev/null && echo present || echo absent\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "allowedBinaries=[jq]: yq filename not visible via ls" "absent" "${jq_sees_yq}"

# SB_YQ (allowedBinaries=["yq"]): jq must not appear in ls output
yq_sees_jq=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"binaries\",\"sandboxId\":\"${SB_YQ}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"ls /usr/local/bin/jq 2>/dev/null && echo present || echo absent\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "allowedBinaries=[yq]: jq filename not visible via ls" "absent" "${yq_sees_jq}"

# SB_NONE (allowedBinaries=[]): /usr/local/bin/ must be completely empty
none_count=$(curl_api POST "/v1/exec" \
  -d "{\"sessionId\":\"binaries\",\"sandboxId\":\"${SB_NONE}\",\"command\":\"sh\",
       \"args\":[\"-c\",\"ls /usr/local/bin/ 2>/dev/null | wc -l | tr -d ' '\"],
       \"timeoutSeconds\":10}" \
  | jq -r '.stdout // empty' | tr -d '\r\n')
assert_eq "allowedBinaries=[]: /usr/local/bin/ lists 0 files" "0" "${none_count}"

# -----------------------------------------------------------------------
# Cleanup
# -----------------------------------------------------------------------

for sb in "${SB_JQ}" "${SB_YQ}" "${SB_NONE}" "${SB_CROSS}"; do
  curl_api DELETE "/v1/sandboxes/${sb}" >/dev/null 2>&1 || true
done

summary

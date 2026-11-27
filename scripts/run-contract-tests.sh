#!/usr/bin/env bash
# V29: Bare-repo post-receive hook installer & bounded local contract test runner.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
DEFAULT_HOOK_SOURCE="${REPO_ROOT}/hooks/post-receive"

BARE_REPO=""
INSTALL_ONLY=0
REF_NAME="refs/heads/main"
OLD_REV="0000000000000000000000000000000000000000"
NEW_REV="HEAD"
TIMEOUT_SECS="${MW_LAB_CONTRACT_TIMEOUT_SECS:-30}"
MAX_OUTPUT_LINES="${MW_LAB_CONTRACT_MAX_LINES:-200}"
CUSTOM_CMD="${MW_LAB_CONTRACT_CMD:-}"

usage() {
  cat <<EOF
Usage:
  $(basename "$0") --install --bare-repo <path>
  $(basename "$0") --bare-repo <path> [--ref <ref>] [--old-rev <sha>] [--new-rev <sha>] [--timeout <secs>] [--cmd <command>]
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --install)
      INSTALL_ONLY=1
      shift
      ;;
    --bare-repo)
      BARE_REPO="${2:-}"
      shift 2
      ;;
    --ref)
      REF_NAME="${2:-}"
      shift 2
      ;;
    --old-rev)
      OLD_REV="${2:-}"
      shift 2
      ;;
    --new-rev)
      NEW_REV="${2:-}"
      shift 2
      ;;
    --timeout)
      TIMEOUT_SECS="${2:-30}"
      shift 2
      ;;
    --cmd)
      CUSTOM_CMD="${2:-}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z "${BARE_REPO}" ]]; then
  echo "error: --bare-repo is required" >&2
  exit 2
fi

if [[ ! -d "${BARE_REPO}" ]]; then
  echo "error: bare repo directory not found: ${BARE_REPO}" >&2
  exit 2
fi

BARE_REPO_ABS="$(cd "${BARE_REPO}" && pwd)"

install_hook() {
  local hooks_dir="${BARE_REPO_ABS}/hooks"
  mkdir -p "${hooks_dir}"
  local target_hook="${hooks_dir}/post-receive"

  if [[ ! -f "${DEFAULT_HOOK_SOURCE}" ]]; then
    echo "error: canonical hook template not found at ${DEFAULT_HOOK_SOURCE}" >&2
    exit 1
  fi

  cp "${DEFAULT_HOOK_SOURCE}" "${target_hook}"
  chmod +x "${target_hook}"
  echo "[mw-lab:v29] installed post-receive hook into ${target_hook}"
}

if [[ "${INSTALL_ONLY}" -eq 1 ]]; then
  install_hook
  exit 0
fi

RESOLVED_SHA="$(git --git-dir="${BARE_REPO_ABS}" rev-parse --verify "${NEW_REV}^{commit}" 2>/dev/null || true)"
if [[ -z "${RESOLVED_SHA}" ]]; then
  echo "error: unable to resolve commit '${NEW_REV}' in ${BARE_REPO_ABS}" >&2
  exit 1
fi

WORK_TMP="$(mktemp -d 2>/dev/null || mktemp -d -t mw-lab-v29)"
cleanup() {
  rm -rf "${WORK_TMP}"
}
trap cleanup EXIT

git --git-dir="${BARE_REPO_ABS}" archive "${RESOLVED_SHA}" | tar -x -C "${WORK_TMP}"

detect_contract_command() {
  if [[ -n "${CUSTOM_CMD}" ]]; then
    echo "${CUSTOM_CMD}"
    return
  fi
  if [[ -f "${WORK_TMP}/.mw-contract-test.sh" ]]; then
    echo "bash ./.mw-contract-test.sh"
    return
  fi
  if [[ -f "${WORK_TMP}/scripts/contract-test.sh" ]]; then
    echo "bash ./scripts/contract-test.sh"
    return
  fi
  if [[ -f "${WORK_TMP}/go.mod" ]]; then
    echo "go test -run TestContract ./..."
    return
  fi
  if [[ -f "${WORK_TMP}/package.json" ]]; then
    echo "npm run test:contract --if-present"
    return
  fi
  echo ""
}

CONTRACT_CMD="$(detect_contract_command)"
if [[ -z "${CONTRACT_CMD}" ]]; then
  echo "[mw-lab:v29] no contract suite detected for ${REF_NAME}@${RESOLVED_SHA:0:12}; passing by default"
  exit 0
fi

echo "[mw-lab:v29] running bounded contract test (${TIMEOUT_SECS}s max): ${CONTRACT_CMD}"
LOG_FILE="${WORK_TMP}/contract-output.log"

set +e
(
  cd "${WORK_TMP}"
  export MW_CONTRACT_REF="${REF_NAME}"
  export MW_CONTRACT_OLD_REV="${OLD_REV}"
  export MW_CONTRACT_NEW_REV="${RESOLVED_SHA}"
  export CI="true"
  if command -v timeout >/dev/null 2>&1; then
    timeout "${TIMEOUT_SECS}" bash -c "${CONTRACT_CMD}"
  else
    bash -c "${CONTRACT_CMD}"
  fi
) >"${LOG_FILE}" 2>&1
CMD_STATUS=$?
set -e

head -n "${MAX_OUTPUT_LINES}" "${LOG_FILE}"
TOTAL_LINES="$(wc -l <"${LOG_FILE}" | tr -d ' ')"
if [[ "${TOTAL_LINES}" -gt "${MAX_OUTPUT_LINES}" ]]; then
  echo "...[truncated $((TOTAL_LINES - MAX_OUTPUT_LINES)) lines]"
fi

if [[ "${CMD_STATUS}" -ne 0 ]]; then
  echo "[mw-lab:v29] contract test FAILED (exit ${CMD_STATUS}) for ${REF_NAME}@${RESOLVED_SHA:0:12}" >&2
  exit "${CMD_STATUS}"
fi

echo "[mw-lab:v29] contract test PASSED for ${REF_NAME}@${RESOLVED_SHA:0:12}"
exit 0

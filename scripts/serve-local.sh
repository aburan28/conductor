#!/usr/bin/env bash
# Launch the local vLLM endpoints Conductor/OpenCode use.
#
# Variants (one process on PORT, default 8000):
#   flash   GLM-5.3-Flash  (~310 GiB)  image :glm53-flash
#   glm53   GLM-5.3        (~750 GiB)  image :v0.28.0
#   qwen    Qwen 3.8 27B FP8           image :v0.28.0
#
# Usage:
#   ./scripts/serve-local.sh flash
#   ./scripts/serve-local.sh qwen stop
#   ./scripts/serve-local.sh status
#   conductor serve flash
set -euo pipefail

REGISTRY="${REGISTRY:-docker.artifactory.rbx.com}"
HOST="${HOST:-0.0.0.0}"
PORT="${PORT:-8000}"
MAX_MODEL_LEN="${MAX_MODEL_LEN:-32768}"
GPU_MEM_UTIL="${GPU_MEM_UTIL:-0.90}"
DOCKER_WAIT_S="${DOCKER_WAIT_S:-60}"
HF_HOME_HOST="${HF_HOME_HOST:-${HOME}/.cache/huggingface}"

log() { printf '%s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

usage() {
  cat <<'EOF'
Serve a local OpenAI-compatible vLLM endpoint for Conductor / OpenCode.

Usage:
  serve-local.sh <flash|glm53|qwen> [start|stop|status|smoke|pull]
  serve-local.sh status|stop

Env:
  PORT  HOST  TP  VLLM_IMAGE  WEIGHTS  REGISTRY
EOF
}

is_checkpoint() { [[ -f "${1:-}/config.json" ]]; }

gpu_count() {
  if command -v nvidia-smi >/dev/null 2>&1; then
    nvidia-smi -L 2>/dev/null | awk 'END{print NR}'
  else
    echo 0
  fi
}

wait_for_docker() {
  local waited=0
  while ! docker info >/dev/null 2>&1; do
    if [[ "${waited}" -ge "${DOCKER_WAIT_S}" ]]; then
      die "docker is not up after ${DOCKER_WAIT_S}s"
    fi
    log "waiting for docker (${waited}s)..."
    sleep 2
    waited=$((waited + 2))
  done
}

# Newest Hugging Face snapshot dir that has config.json.
newest_hf_snapshot() {
  local hub="$1"
  [[ -d "${hub}/snapshots" ]] || return 1
  local snap
  snap="$(find "${hub}/snapshots" -mindepth 1 -maxdepth 1 -type d -print0 2>/dev/null \
    | xargs -0 ls -1td 2>/dev/null | head -1 || true)"
  if is_checkpoint "${snap}"; then
    printf '%s\n' "${snap}"
    return 0
  fi
  return 1
}

find_flash_weights() {
  local cand
  if [[ -n "${WEIGHTS:-}" ]]; then
    is_checkpoint "${WEIGHTS}" || die "WEIGHTS=${WEIGHTS} has no config.json"
    printf '%s\n' "${WEIGHTS}"
    return
  fi
  for cand in \
    "${HOME}/GLM-5.3-Flash" \
    "${HOME}/GLM-5.3-flash" \
    "/tmp/models/GLM-5.3-Flash" \
    "/tmp/models/GLM-5.3-flash"; do
    if is_checkpoint "${cand}"; then
      printf '%s\n' "${cand}"
      return
    fi
  done
  local parent
  for parent in "${HOME}" /tmp/models; do
    [[ -d "${parent}" ]] || continue
    while IFS= read -r cand; do
      if is_checkpoint "${cand}"; then
        printf '%s\n' "${cand}"
        return
      fi
    done < <(find "${parent}" -maxdepth 1 -type d -iname 'GLM-5.3-Flash' 2>/dev/null || true)
  done
  die "GLM-5.3-Flash weights not found (set WEIGHTS= or put them in \$HOME/GLM-5.3-flash)"
}

find_glm53_weights() {
  local cand
  if [[ -n "${WEIGHTS:-}" ]]; then
    is_checkpoint "${WEIGHTS}" || die "WEIGHTS=${WEIGHTS} has no config.json"
    printf '%s\n' "${WEIGHTS}"
    return
  fi
  for cand in \
    "${HOME}/GLM-5.3" \
    "/tmp/models/GLM-5.3"; do
    if is_checkpoint "${cand}"; then
      printf '%s\n' "${cand}"
      return
    fi
  done
  die "GLM-5.3 weights not found (set WEIGHTS= or /tmp/models/GLM-5.3)"
}

find_qwen_weights() {
  local cand hub
  if [[ -n "${WEIGHTS:-}" ]]; then
    is_checkpoint "${WEIGHTS}" || die "WEIGHTS=${WEIGHTS} has no config.json"
    printf '%s\n' "${WEIGHTS}"
    return
  fi
  hub="${HF_HOME_HOST}/hub/models--Qwen--Qwen3.8-27B-FP8"
  if cand="$(newest_hf_snapshot "${hub}")"; then
    printf '%s\n' "${cand}"
    return
  fi
  for cand in \
    "/tmp/models/Qwen3.8-27B-FP8" \
    "${HOME}/Qwen3.8-27B-FP8"; do
    if is_checkpoint "${cand}"; then
      printf '%s\n' "${cand}"
      return
    fi
  done
  die "Qwen3.8-27B-FP8 weights not found (hf download Qwen/Qwen3.8-27B-FP8, or set WEIGHTS=)"
}

normalize_variant() {
  case "$1" in
    flash|glm53-flash|glm-5.3-flash|glm5.3-flash) echo flash ;;
    glm53|glm-5.3|glm5.3|full) echo glm53 ;;
    qwen|qwen3.8|qwen38|qwen3.8-27b|qwen3-8) echo qwen ;;
    *) die "unknown variant: $1 (flash|glm53|qwen)" ;;
  esac
}

served_name() {
  case "$1" in
    flash) echo zai-org/GLM-5.3-Flash ;;
    glm53) echo glm-5.3 ;;
    qwen) echo qwen3.8-27b ;;
  esac
}

container_name() {
  case "$1" in
    flash) echo glm53-flash ;;
    glm53) echo glm53 ;;
    qwen) echo qwen38-27b ;;
  esac
}

default_image() {
  case "$1" in
    flash) echo "${REGISTRY}/vllm/vllm-openai:glm53-flash" ;;
    glm53|qwen) echo "${REGISTRY}/vllm/vllm-openai:v0.28.0" ;;
  esac
}

vllm_extra() {
  case "$1" in
    flash)
      echo --tool-call-parser glm47 --reasoning-parser glm45 --enable-auto-tool-choice --no-enable-flashinfer-autotune
      ;;
    glm53)
      echo --tool-call-parser glm47 --reasoning-parser glm45 --enable-auto-tool-choice --kv-cache-dtype fp8
      ;;
    qwen)
      echo --enable-prefix-caching --language-model-only --enable-auto-tool-choice --tool-call-parser qwen3_xml --reasoning-parser qwen3 --max-num-seqs 16
      ;;
  esac
}

cmd_stop() {
  local name="$1"
  docker rm -f "${name}" >/dev/null 2>&1 || true
  log "stopped ${name}"
}

cmd_status() {
  local name served
  if [[ -n "${1:-}" ]]; then
    name="$(container_name "$1")"
    served="$(served_name "$1")"
    docker ps -a --filter "name=^${name}$" --format 'table {{.Names}}\t{{.Status}}' || true
    if curl -fsS "http://127.0.0.1:${PORT}/v1/models" >/dev/null 2>&1; then
      log "endpoint UP on :${PORT} (expect model ${served})"
    else
      log "endpoint DOWN on :${PORT}"
    fi
    return
  fi
  docker ps -a --filter 'name=glm53' --filter 'name=qwen38-27b' --format 'table {{.Names}}\t{{.Status}}' || true
  if curl -fsS "http://127.0.0.1:${PORT}/v1/models" >/dev/null 2>&1; then
    log "endpoint UP on :${PORT}"
    curl -fsS "http://127.0.0.1:${PORT}/v1/models" | python3 -m json.tool 2>/dev/null || true
  else
    log "endpoint DOWN on :${PORT}"
  fi
}

cmd_smoke() {
  local served="${1:-}"
  if [[ -z "${served}" ]]; then
    served="$(curl -fsS "http://127.0.0.1:${PORT}/v1/models" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"][0]["id"])')"
  fi
  curl -fsS "http://127.0.0.1:${PORT}/v1/chat/completions" \
    -H "Content-Type: application/json" \
    -d "{
      \"model\": \"${served}\",
      \"messages\": [{\"role\": \"user\", \"content\": \"Say OK and nothing else.\"}],
      \"max_tokens\": 16,
      \"temperature\": 0
    }" | python3 -m json.tool
}

cmd_start() {
  local variant="$1"
  local name image dest served tp extra
  name="$(container_name "${variant}")"
  image="${VLLM_IMAGE:-$(default_image "${variant}")}"
  served="$(served_name "${variant}")"
  tp="${TP:-$(gpu_count)}"
  if [[ "${tp}" -lt 1 ]]; then
    die "no GPUs visible via nvidia-smi"
  fi
  case "${variant}" in
    flash) dest="$(find_flash_weights)" ;;
    glm53) dest="$(find_glm53_weights)" ;;
    qwen) dest="$(find_qwen_weights)" ;;
  esac
  # shellcheck disable=SC2206
  extra=( $(vllm_extra "${variant}") )

  wait_for_docker
  if ! docker image inspect "${image}" >/dev/null 2>&1; then
    log "docker pull ${image}"
    docker pull "${image}"
  fi
  if docker ps -a --format '{{.Names}}' | grep -qx "${name}"; then
    log "removing existing container ${name}"
    docker rm -f "${name}" >/dev/null
  fi

  log "variant=${variant} image=${image} dest=${dest} tp=${tp} port=${PORT} name=${name} served=${served}"
  docker run -d --name "${name}" \
    --gpus all --privileged --ipc=host --network host \
    -e "VLLM_ENGINE_READY_TIMEOUT_S=3600" \
    -v "${dest}:/models:ro" \
    "${image}" \
    /models \
    --host "${HOST}" --port "${PORT}" \
    --served-model-name "${served}" \
    --tensor-parallel-size "${tp}" \
    --gpu-memory-utilization "${GPU_MEM_UTIL}" \
    --max-model-len "${MAX_MODEL_LEN}" \
    "${extra[@]}"

  log "following logs until ready (Ctrl-C stops follow, not the container)"
  docker logs -f "${name}" 2>&1 | grep -m1 -E 'Application startup complete|Uvicorn running' || true
  log "endpoint: http://127.0.0.1:${PORT}/v1  model=${served}"
  log "  conductor wrap opencode --model vllm/${served}"
}

ACTION="start"
VARIANT=""

if [[ $# -eq 0 ]]; then
  usage
  die "pick flash, glm53, or qwen"
fi

case "$1" in
  -h|--help) usage; exit 0 ;;
  status)
    if [[ -n "${2:-}" ]]; then
      VARIANT="$(normalize_variant "$2")"
      cmd_status "${VARIANT}"
    else
      cmd_status
    fi
    exit 0
    ;;
  stop)
    if [[ -n "${2:-}" ]]; then
      cmd_stop "$(container_name "$(normalize_variant "$2")")"
    else
      cmd_stop glm53-flash
      cmd_stop glm53
      cmd_stop qwen38-27b
    fi
    exit 0
    ;;
  smoke)
    cmd_smoke
    exit 0
    ;;
esac

VARIANT="$(normalize_variant "$1")"
shift
if [[ $# -gt 0 ]]; then
  ACTION="$1"
  shift
fi

case "${ACTION}" in
  start|serve) cmd_start "${VARIANT}" ;;
  stop) cmd_stop "$(container_name "${VARIANT}")" ;;
  status) cmd_status "${VARIANT}" ;;
  smoke) cmd_smoke "$(served_name "${VARIANT}")" ;;
  pull)
    wait_for_docker
    docker pull "${VLLM_IMAGE:-$(default_image "${VARIANT}")}"
    ;;
  *) die "unknown action ${ACTION} (start|stop|status|smoke|pull)" ;;
esac

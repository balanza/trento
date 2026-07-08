#!/usr/bin/env bash
# Build local images, load them into the active k3d cluster, and deploy via helm.

set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
readonly CHART_DIR="${PROJECT_ROOT}/helm-charts/charts/trento-server"
readonly HOSTS_CHART_DIR="${PROJECT_ROOT}/container_fixtures/hosts/helm"
readonly HOSTS_AGENT_DOCKERFILE="${PROJECT_ROOT}/container_fixtures/hosts/Dockerfile.agent"
readonly HOSTS_FILES_DOCKERFILE="${PROJECT_ROOT}/container_fixtures/hosts/Dockerfile.filesystem"
readonly HOSTS_AGENT_IMAGE="local/agent:dev"
readonly HOSTS_FIXTURES_DIR="${PROJECT_ROOT}/container_fixtures/hosts"
readonly HOSTS_RELEASE_NAME="host-fixtures"

# Auto-discover scenarios: every subdirectory (excluding 'helm') under
# container_fixtures/hosts/ is treated as a scenario.
readonly HOSTS_SCENARIOS=($(find "${HOSTS_FIXTURES_DIR}" -mindepth 1 -maxdepth 1 -type d ! -name helm -printf '%f\n' | sort))

if [[ ${#HOSTS_SCENARIOS[@]} -eq 0 ]]; then
  die "No scenarios found in ${HOSTS_FIXTURES_DIR}"
fi

# Aggregate all hosts across every scenario, preserving scenario-host mapping.
# ALL_HOSTS holds every host name (unique).  HOST_SCENARIO[host] records which
# scenario a host belongs to (the first one if duplicated).
ALL_HOSTS=()
declare -A HOST_SCENARIO
for scenario in "${HOSTS_SCENARIOS[@]}"; do
  scenario_dir="${HOSTS_FIXTURES_DIR}/${scenario}"
  for host_dir in "${scenario_dir}"/*/; do
    [[ -d "$host_dir" ]] || continue
    host="$(basename "$host_dir")"
    if [[ -z "${HOST_SCENARIO[$host]+x}" ]]; then
      ALL_HOSTS+=("$host")
      HOST_SCENARIO["$host"]="$scenario"
    fi
  done
done
readonly ALL_HOSTS

# Legacy alias for backward-compat with code that references $HOSTS
readonly HOSTS=("${ALL_HOSTS[@]}")

readonly TAG="dev"
readonly REGISTRY_PREFIX="local"

# Local-contracts mode: when ./contracts has uncommitted changes OR is on a
# named branch other than 'main', the consumers (web, wanda, agent) are
# rebuilt against ./contracts instead of the upstream-pinned version.
# Mechanism: copy ./contracts/{elixir,go} into a sentinel dir inside each
# consumer, rewrite the trento_contracts dep declaration to point there,
# build, then revert on exit.
readonly LOCAL_CONTRACTS_SENTINEL=".trento-local-contracts"
readonly LOCAL_CONTRACTS_BACKUP_EXT=".trento-local-contracts.bak"
PATCHED_CONSUMERS=()

usage() {
  cat <<EOF
Usage: $(basename "$0") [options]

Deploy the trento-server Helm chart against the active k3d or k3s cluster,
configured to use the local images 'local/trento-{web,wanda,checks}:dev'. By
default no images are built; pass --build to (re)build a subset before deploying.

Cluster types:
  k3d  — docker-based, multi-cluster. Context name starts with 'k3d-'.
         Images are loaded with 'k3d image import'.
  k3s  — single-host (or multi-node) containerd-backed install. Detected via
         a node kubelet version containing 'k3s'. Image loading is auto-detected:
           * host install (/run/k3s/containerd/containerd.sock present):
             'docker save | k3s ctr images import -' (sudo if not root).
           * k3s-in-docker (no host socket): 'docker save | docker exec -i
             <k3s-container> ctr ... images import -'. No host k3s binary or
             PATH shim required.

Host fixtures: all scenarios under container_fixtures/hosts/ are automatically
discovered and deployed (each subdirectory = one scenario). The host-fixtures
Helm release lists every host from every scenario in a single deployment.

Options:
  --build <csv>          Comma-separated subset of images to (re)build
                         (valid: all, web, wanda, checks, fixtures). Use
                         "all" as a shortcut for "web,wanda,checks,fixtures".
                         Any
                         trento service (web/wanda/checks) whose local image
                         is missing is auto-added to this set, so first runs
                         work without flags. Every trento service image
                         present locally is imported into the cluster on
                         every run.

                         "fixtures" gates the host-simulation images built
                         from container_fixtures/hosts/: the shared
                         local/agent:dev image and one local/files:<scenario>-<host>
                         image per host. They are rebuilt ONLY when "fixtures"
                         is in the --build list; otherwise the already-built
                         images are reused (imported) as-is. If a required
                         host-fixture image is missing locally and "fixtures"
                         was not passed, the script errors and tells you to
                         build them (--build fixtures).
  --release-name <name>  Helm release name (default: trento).
  --namespace <ns>       Kubernetes namespace (default: default).
  -h, --help             Show this help and exit.

Local-contracts mode (auto-detected):
  When the contracts submodule has uncommitted changes OR is on a named
  branch other than 'main', any service listed in --build is wired against
  ./contracts instead of the upstream-pinned version (mix.exs / go.mod
  patched under the consumer's tree, reverted on script exit). Services
  not in --build keep their previous image untouched — add them to --build
  yourself when you need them rebuilt against the local contracts.
EOF
}

die() {
  echo "Error: $*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

# Returns 0 (true) when the contracts submodule has uncommitted changes OR is
# on a named branch other than 'main'. A detached HEAD (the default state
# after `git submodule update`) returns 1 (use upstream pin).
should_use_local_contracts() {
  local sm="${PROJECT_ROOT}/contracts"
  [[ -e "${sm}/.git" ]] || return 1
  if [[ -n "$(git -C "$sm" status --porcelain 2>/dev/null)" ]]; then
    return 0
  fi
  local br
  br="$(git -C "$sm" symbolic-ref --short HEAD 2>/dev/null || true)"
  [[ -n "$br" && "$br" != "main" ]]
}

# Refuse to patch a manifest that has uncommitted changes — overwriting it
# would clobber the user's in-progress edits, and our backup/restore would
# silently lose them.
assert_manifest_clean() {
  local consumer="$1" manifest="$2"
  local dir="${PROJECT_ROOT}/${consumer}"
  if ! git -C "$dir" diff --quiet HEAD -- "$manifest" 2>/dev/null; then
    die "${consumer}/${manifest} has uncommitted changes — refusing to patch. Commit or stash, then re-run."
  fi
}

apply_local_contracts_to_elixir() {
  local consumer="$1"
  local dir="${PROJECT_ROOT}/${consumer}"
  local manifest="${dir}/mix.exs"
  local sentinel="${dir}/${LOCAL_CONTRACTS_SENTINEL}/elixir"

  echo ">> [local-contracts] patching ${consumer}/mix.exs to use ./contracts/elixir"
  rm -rf "${dir}/${LOCAL_CONTRACTS_SENTINEL}"
  mkdir -p "$sentinel"
  cp -a "${PROJECT_ROOT}/contracts/elixir/." "$sentinel/"
  cp "$manifest" "${manifest}${LOCAL_CONTRACTS_BACKUP_EXT}"
  perl -i -0777 -pe \
    's/^(\s*)\{:trento_contracts,.*?\},\s*$/$1\{:trento_contracts, path: "'"${LOCAL_CONTRACTS_SENTINEL}"'\/elixir"\},/sm' \
    "$manifest"
  PATCHED_CONSUMERS+=("$consumer")
}

apply_local_contracts_to_agent() {
  local dir="${PROJECT_ROOT}/agent"
  local manifest="${dir}/go.mod"
  local sentinel="${dir}/${LOCAL_CONTRACTS_SENTINEL}/go"

  echo ">> [local-contracts] patching agent/go.mod to use ./contracts/go"
  rm -rf "${dir}/${LOCAL_CONTRACTS_SENTINEL}"
  mkdir -p "$sentinel"
  cp -a "${PROJECT_ROOT}/contracts/go/." "$sentinel/"
  cp "$manifest" "${manifest}${LOCAL_CONTRACTS_BACKUP_EXT}"
  printf '\nreplace github.com/trento-project/contracts/go => ./%s/go\n' \
    "$LOCAL_CONTRACTS_SENTINEL" >> "$manifest"
  PATCHED_CONSUMERS+=("agent")
}

build_includes() {
  local svc="$1" s
  for s in "${build_services[@]:-}"; do
    [[ "$s" == "$svc" ]] && return 0
  done
  return 1
}

# True when the user asked to (re)build the host-fixture images (accepts both
# "fixture" and "fixtures" as the token).
build_includes_fixtures() {
  local s
  for s in "${build_services[@]:-}"; do
    case "$s" in
      fixture|fixtures) return 0 ;;
    esac
  done
  return 1
}

# Maps a service name to its expected local image tag. Knowing the canonical
# image lets the script (a) auto-build a service whose image is missing and
# (b) always import the image into the cluster regardless of whether it was
# built this run.
service_image() {
  case "$1" in
    web|wanda|checks) echo "${REGISTRY_PREFIX}/trento-${1}:${TAG}" ;;
    *) return 1 ;;
  esac
}

image_exists_locally() {
  docker image inspect "$1" >/dev/null 2>&1
}

import_to_cluster() {
  local image="$1"
  if ! image_exists_locally "$image"; then
    return 0   # nothing to import; auto-build pass should have caught this
  fi
  if [[ "${cluster_type:-}" == "k3s" ]]; then
    echo ">> importing ${image} into k3s containerd"
    # K3S_IMPORT_CMD was resolved once after cluster detection. It is either:
    #   - host install:   `k3s ctr --connect-timeout 60s images import -`
    #   - k3s-in-docker:  `docker exec -i <container> /bin/ctr --address ... --namespace k8s.io --connect-timeout 60s images import -`
    # --connect-timeout 60s lets a busy containerd accept the dial (ctr's
    # default 3s can surface as 'context deadline exceeded' under load), and
    # the retry loop below adds backoff as belt-and-suspenders. sudo is only
    # used for the host-install case when running as a non-root user (the
    # socket is root-owned); it is never used for k3s-in-docker.
    local attempt imported=0
    for attempt in 1 2 3; do
      if docker save "$image" | "${K3S_IMPORT_CMD[@]}" 2>&1; then
        imported=1
        break
      fi
      [[ "$attempt" -lt 3 ]] || break
      echo ">> k3s ctr import attempt ${attempt} failed; retrying in $((attempt * 3))s" >&2
      sleep $((attempt * 3))
    done
    if [[ "$imported" -ne 1 ]]; then
      if [[ "${K3S_IMPORT_USE_SUDO}" == "true" ]] && command -v sudo >/dev/null 2>&1; then
        echo ">> direct k3s ctr import failed after retries; host k3s socket detected, retrying with sudo" >&2
        docker save "$image" | sudo "${K3S_IMPORT_CMD[@]}"
      else
        die "k3s ctr images import failed for ${image} after retries."
      fi
    fi
  else
    echo ">> importing ${image} into k3d cluster ${cluster}"
    k3d image import "$image" -c "$cluster"
  fi
}

cleanup_local_contracts() {
  if [[ ${#PATCHED_CONSUMERS[@]} -eq 0 ]]; then
    return 0
  fi
  local consumer dir manifest
  for consumer in "${PATCHED_CONSUMERS[@]}"; do
    dir="${PROJECT_ROOT}/${consumer}"
    case "$consumer" in
      web|wanda) manifest="${dir}/mix.exs" ;;
      agent)     manifest="${dir}/go.mod" ;;
      *) continue ;;
    esac
    if [[ -f "${manifest}${LOCAL_CONTRACTS_BACKUP_EXT}" ]]; then
      mv "${manifest}${LOCAL_CONTRACTS_BACKUP_EXT}" "$manifest"
    fi
    rm -rf "${dir}/${LOCAL_CONTRACTS_SENTINEL}"
  done
  PATCHED_CONSUMERS=()
}

cleanup_tmp_files() {
  [[ -z "${TMP_HOSTS_VALUES:-}" ]] || rm -f "$TMP_HOSTS_VALUES"
}

cleanup_all() {
  cleanup_tmp_files
  cleanup_local_contracts
}

build_csv=""
release_name="trento"
namespace="default"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --build)
      [[ $# -ge 2 ]] || die "--build requires a value"
      build_csv="$2"
      shift 2
      ;;
    --build=*)
      build_csv="${1#*=}"
      shift
      ;;
    --release-name)
      [[ $# -ge 2 ]] || die "--release-name requires a value"
      release_name="$2"
      shift 2
      ;;
    --release-name=*)
      release_name="${1#*=}"
      shift
      ;;
    --namespace)
      [[ $# -ge 2 ]] || die "--namespace requires a value"
      namespace="$2"
      shift 2
      ;;
    --namespace=*)
      namespace="${1#*=}"
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown argument: $1 (use --help for usage)"
      ;;
  esac
done

if [[ -n "$build_csv" ]]; then
  IFS=',' read -ra build_services <<< "$build_csv"
  [[ ${#build_services[@]} -gt 0 ]] || die "--build must list at least one service"
  for svc in "${build_services[@]}"; do
    case "$svc" in
      all)
        build_services=(web wanda checks fixtures)
        break ;;
      web|wanda|checks|fixture|fixtures) ;;
      *) die "unknown service: '$svc' (valid: all, web, wanda, checks, fixtures)" ;;
    esac
  done
else
  build_services=()
fi

require_cmd kubectl
require_cmd helm
# docker is needed unconditionally: every image (trento services and the
# per-host files images) is built with it. Layer cache makes re-runs cheap.
require_cmd docker

# If a prior `helm upgrade --install <name>` was interrupted (Ctrl-C, hook
# timeout, CRD race, etc.) the release is left in `failed` status. The next
# run is then treated as an upgrade and fires pre-upgrade hooks even on what
# the user thinks is a fresh run, which is confusing and slow. Uninstall any
# failed release in the target namespace before re-installing. We never touch
# a `deployed` release.
helm_clean_failed_release() {
  local name="$1" ns="$2" json status
  # If the release doesn't exist `helm status` exits non-zero — under
  # `set -euo pipefail` that would abort the script. Catch it and bail
  # cleanly. The `|| true` on the pipeline below would NOT be enough
  # because pipefail propagates the helm failure regardless.
  if ! json="$(helm status "$name" --namespace "$ns" -o json 2>/dev/null)"; then
    return 0
  fi
  status="$(printf '%s' "$json" | sed -n 's/.*"status"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
  case "$status" in
    deployed|"") return 0 ;;
    failed|pending-install|pending-upgrade|pending-rollback|uninstalling)
      echo ">> prior helm release '${name}' in namespace '${ns}' is in '${status}'; uninstalling before retry"
      helm uninstall "$name" --namespace "$ns" --wait || true
      ;;
    *) echo ">> note: helm release '${name}' in namespace '${ns}' is in unexpected status '${status}'; leaving alone" ;;
  esac
}

# Detect the active cluster type. k3d contexts are named 'k3d-<cluster>'; for
# anything else we fall back to a k3s heuristic (a node's kubelet version
# contains 'k3s'). The cluster type governs how local images are loaded and
# how the trento-web URL is derived at the end of the script.
context="$(kubectl config current-context)"
cluster_type=""
cluster=""
case "$context" in
  k3d-*)
    cluster_type="k3d"
    cluster="${context#k3d-}"
    require_cmd k3d
    echo ">> using k3d cluster: ${cluster}"
    ;;
  *)
    node_ver="$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.kubeletVersion}' 2>/dev/null || true)"
    if [[ "$node_ver" == *k3s* ]]; then
      cluster_type="k3s"
      echo ">> using k3s cluster (context: ${context})"
    else
      die "current kubectl context '$context' is neither a k3d nor a k3s cluster (node kubelet version: '${node_ver:-unknown}')"
    fi
    ;;
esac
readonly cluster_type cluster

# For k3s, resolve HOW local images are loaded into the cluster's containerd.
# Two cases:
#  (a) real host install: /run/k3s/containerd/containerd.sock exists on the
#      host, so a host `k3s ctr` (or `sudo k3s ctr`) can reach it directly.
#  (b) k3s-in-docker: k3s runs inside a privileged container (e.g. the
#      rancher/k3s image); the host has no socket, so we `docker exec` into
#      that container and drive its in-container ctr against its own socket.
#      This needs NO `k3s` binary and NO PATH shim on the host — which matters
#      because a host `k3s` binary (asdf, /usr/local/bin, ...) would otherwise
#      dial the non-existent host socket and time out with
#      'context deadline exceeded'.
K3S_IMPORT_CMD=()
K3S_IMPORT_USE_SUDO=false
if [[ "$cluster_type" == "k3s" ]]; then
  if [[ -S /run/k3s/containerd/containerd.sock ]]; then
    command -v k3s >/dev/null 2>&1 \
      || die "k3s cluster detected and host socket /run/k3s/containerd/containerd.sock is present, but no 'k3s' binary on PATH — install k3s or add it to PATH."
    K3S_IMPORT_CMD=(k3s ctr --connect-timeout 60s images import -)
    [[ "$(id -u)" -ne 0 ]] && K3S_IMPORT_USE_SUDO=true
    echo ">> k3s image loading: host k3s containerd socket (/run/k3s/containerd/containerd.sock)"
  else
    K3S_CONTAINER="$(docker ps --format '{{.Names}}\t{{.Image}}' 2>/dev/null \
      | awk -F'\t' '$2 ~ /rancher\/k3s/ {print $1; exit}')"
    [[ -n "$K3S_CONTAINER" ]] \
      || die "k3s cluster detected, but neither a host containerd socket (/run/k3s/containerd/containerd.sock) nor a running rancher/k3s docker container was found. If k3s runs in a container, start it ('docker ps | grep k3s'). If k3s runs on the host, install/start the k3s service so the socket appears."
    docker exec "$K3S_CONTAINER" test -S /run/k3s/containerd/containerd.sock 2>/dev/null \
      || die "found k3s container '$K3S_CONTAINER' but it has no /run/k3s/containerd/containerd.sock; is k3s healthy inside it? Check: docker logs $K3S_CONTAINER"
    K3S_IMPORT_CMD=(docker exec -i "$K3S_CONTAINER" /bin/ctr \
      --address /run/k3s/containerd/containerd.sock \
      --namespace k8s.io \
      --connect-timeout 60s \
      images import -)
    echo ">> k3s image loading: k3s-in-docker container '$K3S_CONTAINER' (no host k3s binary needed)"
  fi
fi
readonly K3S_IMPORT_CMD K3S_IMPORT_USE_SUDO

# Auto-add any trento service whose local image is missing. This makes first
# runs (and recoveries from `docker image rm`) work without the user having to
# remember to pass --build for every service. The host-fixture images
# (local/agent:dev, local/files:*) are NOT auto-built — gate them explicitly
# with --build fixtures; if missing they error later, not here. Runs before
# local-contracts pre-flight so the manifest-clean check covers auto-added
# services too.
for svc in web wanda checks; do
  image="$(service_image "$svc")"
  if ! image_exists_locally "$image" && ! build_includes "$svc"; then
    echo ">> image '${image}' not found locally; auto-adding '${svc}' to --build"
    build_services+=("$svc")
  fi
done

# Decide once whether to inject ./contracts into the consumer builds. Only
# the consumers actually being rebuilt (i.e. listed in --build) get patched
# — a service the user didn't ask to rebuild keeps its previous image and
# its manifest is left untouched. The user is responsible for adding
# web/wanda/agent to --build when they want a fresh image to pick up local
# contracts changes.
use_local_contracts=false
if should_use_local_contracts; then
  use_local_contracts=true
  trap cleanup_all EXIT
  contracts_branch="$(git -C "${PROJECT_ROOT}/contracts" symbolic-ref --short HEAD 2>/dev/null || echo '(detached)')"
  contracts_dirty="$(git -C "${PROJECT_ROOT}/contracts" status --porcelain 2>/dev/null | head -n1)"
  if [[ -n "$contracts_dirty" ]]; then
    echo ">> contracts: dirty (branch=${contracts_branch}) — using local ./contracts for whatever is in --build"
  else
    echo ">> contracts: on branch '${contracts_branch}' — using local ./contracts for whatever is in --build"
  fi
  # The agent is a contracts consumer too, but it is rebuilt as part of the
  # "fixtures" build path; its go.mod clean-check is enforced there, not here.
  for svc in "${build_services[@]:-}"; do
    case "$svc" in
      web|wanda) assert_manifest_clean "$svc" "mix.exs" ;;
    esac
  done
else
  echo ">> contracts: clean and on main (or detached at submodule pin) — using upstream pin"
fi

trento_services=()
for svc in "${build_services[@]:-}"; do
  case "$svc" in
    web|wanda|checks) trento_services+=("$svc") ;;
  esac
done

for svc in "${trento_services[@]+"${trento_services[@]}"}"; do
  image="$(service_image "$svc")"
  ctx="${PROJECT_ROOT}/${svc}"

  if [[ "$use_local_contracts" == "true" ]]; then
    case "$svc" in
      web|wanda) apply_local_contracts_to_elixir "$svc" ;;
    esac
  fi

  echo ">> building ${image} from ${ctx}"
  docker build -t "$image" "$ctx"
done

# Always import every trento service image present locally — covers freshly
# built images from this run AND pre-existing ones from prior runs (the
# k3d cluster may have been recreated, so we can't assume containerd
# already has them).
for svc in web wanda checks; do
  import_to_cluster "$(service_image "$svc")"
done

set_args=(
  --set "trento-web.adminUser.username=admin"
  --set "trento-web.adminUser.password=adminpassword"
  --set "prometheus.server.auth.type=none"
  --set "trento-web.image.repository=${REGISTRY_PREFIX}/trento-web"
  --set "trento-web.image.tag=${TAG}"
  --set "trento-web.image.pullPolicy=Never"
  --set "trento-wanda.image.repository=${REGISTRY_PREFIX}/trento-wanda"
  --set "trento-wanda.image.tag=${TAG}"
  --set "trento-wanda.image.pullPolicy=Never"
  --set "trento-wanda.checks.image.repository=${REGISTRY_PREFIX}/trento-checks"
  --set "trento-wanda.checks.image.tag=${TAG}"
  --set "trento-wanda.checks.image.pullPolicy=Never"
)

# trento-{web,wanda,mcp-server} ship Traefik Middleware CRs; on a freshly
# created k3d/k3s cluster the addon-manager installs Traefik (and its CRDs)
# asynchronously, so a `helm install` issued seconds after `k3d cluster create`
# races and fails with: 'no matches for kind "Middleware" in version
# "traefik.io/v1alpha1" — ensure CRDs are installed first'.
# Block until the CRD is registered before handing off to helm.
echo ">> waiting for Traefik Middleware CRD"
for i in $(seq 1 60); do
  if kubectl get crd middlewares.traefik.io >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
kubectl get crd middlewares.traefik.io >/dev/null 2>&1 \
  || die "Traefik Middleware CRD did not appear within 120s. K3s installs Traefik via the addon-manager; check 'kubectl get jobs -n kube-system' for helm-install-traefik-* failures, or recreate the cluster without --disable=traefik."

helm_clean_failed_release "$release_name" "$namespace"

echo ">> deploying helm release '${release_name}' in namespace '${namespace}'"
helm upgrade --install "$release_name" "$CHART_DIR" \
  --namespace "$namespace" \
  --create-namespace \
  --wait \
  "${set_args[@]}"

# Pull the API key out of the running trento-web pod via `trento eval`. The key
# is bound to trento-web's ACCESS_TOKEN_ENC_SECRET (regenerated on every fresh
# install), so it must be read from the live instance rather than hardcoded in
# the host-fixtures values.yaml.
echo ">> extracting trento API key from web pod"
web_pod="$(kubectl get pods -n "$namespace" \
  -l "app.kubernetes.io/name=web,app.kubernetes.io/instance=${release_name}" \
  --field-selector status.phase=Running \
  -o jsonpath='{.items[0].metadata.name}')"
[[ -n "$web_pod" ]] || die "could not find a Running trento-web pod in namespace ${namespace}"

api_key="$(kubectl exec -n "$namespace" "$web_pod" -- \
  /app/bin/trento eval '
    Enum.each([:postgrex, :ecto, :ecto_sql], &Application.ensure_all_started/1)
    {:ok, _} = Trento.Repo.start_link()
    case Trento.Settings.get_api_key_settings() do
      {:ok, settings} ->
        settings
        |> TrentoWeb.Plugs.AuthenticateAPIKeyPlug.generate_api_key!()
        |> IO.puts()
      {:error, reason} ->
        IO.puts(:stderr, "Failed to read api key settings: #{inspect(reason)}")
        System.halt(1)
    end
  ')"
[[ -n "$api_key" ]] || die "trento eval returned an empty API key"

# Build and load the host-simulation images for ALL discovered scenarios, then
# deploy them as one Pod per host into the same namespace as trento-server.
# Same namespace so the agents can resolve trento-web and trento-rabbitmq
# without needing FQDNs.
#
# Build is split in two:
#   - local/agent:dev  — built ONCE from Dockerfile.agent. trento-agent + system
#                        deps (catatonit, alloy, soappatrol, fakemall) +
#                        init.sh. Same image for every host.
#   - local/files:<scenario>-<host>
#                      — built per-host from Dockerfile.filesystem (FROM scratch,
#                        just the per-host /etc and /usr overlay). Tiny and
#                        instant once the agent image is cached.
#
# At pod startup the helm chart runs the per-host files image as an init
# container that copies its /fixture/ tree into a shared emptyDir; the agent
# container then mounts that emptyDir read-only at /fixture and init.sh
# overlays /fixture/etc and /fixture/usr onto the agent's own root.

echo ">> discovered scenarios: ${HOSTS_SCENARIOS[*]}"
echo ">> total hosts across all scenarios: ${#ALL_HOSTS[@]} (${ALL_HOSTS[*]})"

# Host-fixture images (built from container_fixtures/hosts/):
#   - local/agent:dev                  shared agent image (Dockerfile.agent)
#   - local/files:<scenario>-<host>    per-host filesystem overlay (Dockerfile.filesystem)
# These are rebuilt ONLY when "fixtures" is in --build. Otherwise the
# already-built images are reused (imported) as-is; if a required image is
# missing locally, we error and tell the user to build with --build fixtures.
if build_includes_fixtures; then
  if [[ "$use_local_contracts" == "true" ]]; then
    assert_manifest_clean "agent" "go.mod"
    apply_local_contracts_to_agent
  fi

  echo ">> building ${HOSTS_AGENT_IMAGE} (shared agent image)"
  docker build \
    -f "$HOSTS_AGENT_DOCKERFILE" \
    -t "$HOSTS_AGENT_IMAGE" \
    "$PROJECT_ROOT"
else
  if ! image_exists_locally "$HOSTS_AGENT_IMAGE"; then
    die "host-fixture image '${HOSTS_AGENT_IMAGE}' not found locally. Build the fixtures with: --build fixtures"
  fi
fi
# Always import the agent image (built this run, or pre-existing).
import_to_cluster "$HOSTS_AGENT_IMAGE"

for host in "${ALL_HOSTS[@]}"; do
  scenario="${HOST_SCENARIO[$host]}"
  files_image="${REGISTRY_PREFIX}/files:${scenario}-${host}"

  if build_includes_fixtures; then
    echo ">> building ${files_image} (scenario: ${scenario})"
    docker build \
      -f "$HOSTS_FILES_DOCKERFILE" \
      --build-arg "SCENARIO=${scenario}" \
      --build-arg "HOST=${host}" \
      -t "$files_image" \
      "$PROJECT_ROOT"
  elif ! image_exists_locally "$files_image"; then
    die "host-fixture image '${files_image}' not found locally. Build the fixtures with: --build fixtures"
  fi

  import_to_cluster "$files_image"
done

# Generate a temporary values file that lists ALL hosts from ALL scenarios.
# This overrides the static values.yaml so we don't have to keep it in sync.
TMP_HOSTS_VALUES="$(mktemp /tmp/host-fixtures-values-XXXXXX.yaml)"
trap cleanup_all EXIT

# Build the hosts YAML list. Detect hasSap by checking for soappatrol.toml.
hosts_yaml=""
for host in "${ALL_HOSTS[@]}"; do
  scenario="${HOST_SCENARIO[$host]}"
  files_image="${REGISTRY_PREFIX}/files:${scenario}-${host}"
  # Derive a stable UUID-like agentId from the host's machine-id (32 hex chars
  # without dashes) — read from the fixture's machine-id file.
  machine_id="$(cat "${HOSTS_FIXTURES_DIR}/${scenario}/${host}/etc/machine-id" 2>/dev/null | tr -d '\n' || echo "00000000000000000000000000000000")"
  # Insert dashes to make it a proper UUID format
  agent_id="${machine_id:0:8}-${machine_id:8:4}-${machine_id:12:4}-${machine_id:16:4}-${machine_id:20:12}"
  # hasSap: true if the host has SAP-specific files (soappatrol.toml)
  has_sap="false"
  [[ -f "${HOSTS_FIXTURES_DIR}/${scenario}/${host}/etc/soappatrol.toml" ]] && has_sap="true"

  hosts_yaml+="  - name: ${host}\n"
  hosts_yaml+="    filesImage: ${files_image}\n"
  hosts_yaml+="    agentId: ${agent_id}\n"
  hosts_yaml+="    hasSap: ${has_sap}\n"
done

# Use a single valid label for the scenario. When multiple scenarios exist,
# use "all" since the label must be Kubernetes-label-compatible.
scenario_label="${HOSTS_SCENARIOS[0]}"
if [[ ${#HOSTS_SCENARIOS[@]} -gt 1 ]]; then
  scenario_label="all"
fi

cat > "$TMP_HOSTS_VALUES" <<EOF
scenario: "${scenario_label}"
trento:
  apiKey: "${api_key}"
hosts:
$(printf '%b' "$hosts_yaml")
EOF

echo ">> generated host-fixtures values ($(wc -l < "$TMP_HOSTS_VALUES") lines)"

helm_clean_failed_release "$HOSTS_RELEASE_NAME" "$namespace"

echo ">> deploying helm release '${HOSTS_RELEASE_NAME}' in namespace '${namespace}'"
helm upgrade --install "$HOSTS_RELEASE_NAME" "$HOSTS_CHART_DIR" \
  --namespace "$namespace" \
  -f "$TMP_HOSTS_VALUES" \
  --wait

# Decide the URL where trento-web is reachable from the host.
# Preference order:
#   1. (k3d only) loadbalancer host port mapping for container port 80
#      — works everywhere when the cluster was created with
#      --port "X:80@loadbalancer".
#   2. Traefik LoadBalancer's external IP, on Linux only
#      — the docker bridge IP (k3d) or the node IP (k3s via Klipper
#      ServiceLB) is host-reachable on Linux, but not on macOS/Windows
#      where Docker runs in a VM.
final_url=""

if [[ "$cluster_type" == "k3d" ]]; then
  lb_container="k3d-${cluster}-serverlb"
  mapping=""
  if command -v docker >/dev/null 2>&1; then
    mapping="$(docker port "$lb_container" 80/tcp 2>/dev/null | grep -v '^\[' | head -n1 || true)"
  fi
  if [[ -n "$mapping" ]]; then
    host="${mapping%:*}"
    port="${mapping##*:}"
    [[ "$host" == "0.0.0.0" ]] && host="localhost"
    final_url="https://${host}:${port}/"
  fi
fi

if [[ -z "$final_url" && "$(uname -s)" == "Linux" ]]; then
  lb_ip="$(kubectl get svc -n kube-system traefik -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
  [[ -n "$lb_ip" ]] && final_url="https://${lb_ip}/"
fi

echo ">> done"
echo ""
if [[ -n "$final_url" ]]; then
  echo ">> Trento web is available at: ${final_url}"
else
  echo ">> Trento is deployed, but no host-reachable address was detected."
  echo "   Forward the service to access it:"
  echo "      kubectl port-forward -n ${namespace} svc/${release_name}-web 4000:4000"
  echo "      then open http://localhost:4000/"
fi

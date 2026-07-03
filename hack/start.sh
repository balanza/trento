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
readonly HOSTS_SCENARIO="hana-scale-out"
# Hosts to build for the scenario above. Keep in sync with
# container_fixtures/hosts/helm/values.yaml#hosts.
readonly HOSTS=(hana-s1-db1 hana-s2-db1 hana-s-mm)
readonly HOSTS_RELEASE_NAME="host-fixtures"
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

Deploy the trento-server Helm chart against the active k3d cluster, configured
to use the local images 'local/trento-{web,wanda,checks}:dev'. By default no
images are built; pass --build to (re)build a subset before deploying.

Options:
  --build <csv>          Comma-separated subset of services to (re)build
                         (valid: web, wanda, checks, agent). Any service
                         whose local image is missing is auto-added to this
                         set, so first runs work without flags. Every known
                         service image present locally is imported into the
                         k3d cluster on every run.
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

# Maps a service name to its expected local image tag. Knowing the canonical
# image lets the script (a) auto-build a service whose image is missing and
# (b) always import the image into the cluster regardless of whether it was
# built this run.
service_image() {
  case "$1" in
    web|wanda|checks) echo "${REGISTRY_PREFIX}/trento-${1}:${TAG}" ;;
    agent)            echo "${HOSTS_AGENT_IMAGE}" ;;
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
  echo ">> importing ${image} into k3d cluster ${cluster}"
  k3d image import "$image" -c "$cluster"
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
      web|wanda|checks|agent) ;;
      *) die "unknown service: '$svc' (valid: web, wanda, checks, agent)" ;;
    esac
  done
else
  build_services=()
fi

require_cmd kubectl
require_cmd helm
# docker and k3d are needed unconditionally: the per-host files images are
# always built and imported. Layer cache makes re-runs cheap.
require_cmd docker
require_cmd k3d

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

context="$(kubectl config current-context)"
case "$context" in
  k3d-*) cluster="${context#k3d-}" ;;
  *) die "current kubectl context '$context' is not a k3d cluster (must start with 'k3d-')" ;;
esac

echo ">> using k3d cluster: ${cluster}"

# Auto-add any service whose local image is missing. This makes first runs
# (and recoveries from `docker image rm`) work without the user having to
# remember to pass --build for every service. Runs before local-contracts
# pre-flight so the manifest-clean check covers auto-added services too.
for svc in web wanda checks agent; do
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
  trap cleanup_local_contracts EXIT
  contracts_branch="$(git -C "${PROJECT_ROOT}/contracts" symbolic-ref --short HEAD 2>/dev/null || echo '(detached)')"
  contracts_dirty="$(git -C "${PROJECT_ROOT}/contracts" status --porcelain 2>/dev/null | head -n1)"
  if [[ -n "$contracts_dirty" ]]; then
    echo ">> contracts: dirty (branch=${contracts_branch}) — using local ./contracts for whatever is in --build"
  else
    echo ">> contracts: on branch '${contracts_branch}' — using local ./contracts for whatever is in --build"
  fi
  for svc in "${build_services[@]:-}"; do
    case "$svc" in
      web|wanda) assert_manifest_clean "$svc" "mix.exs" ;;
      agent)     assert_manifest_clean "agent" "go.mod" ;;
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

# Build and load the host-simulation images for the active scenario, then
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

if build_includes agent; then
  if [[ "$use_local_contracts" == "true" ]]; then
    apply_local_contracts_to_agent
  fi

  echo ">> building ${HOSTS_AGENT_IMAGE} (shared agent image)"
  docker build \
    -f "$HOSTS_AGENT_DOCKERFILE" \
    -t "$HOSTS_AGENT_IMAGE" \
    "$PROJECT_ROOT"
fi

# Always import the agent image (whether built this run or pre-existing).
import_to_cluster "$HOSTS_AGENT_IMAGE"

for host in "${HOSTS[@]}"; do
  files_image="${REGISTRY_PREFIX}/files:${HOSTS_SCENARIO}-${host}"

  echo ">> building ${files_image}"
  docker build \
    -f "$HOSTS_FILES_DOCKERFILE" \
    --build-arg "SCENARIO=${HOSTS_SCENARIO}" \
    --build-arg "HOST=${host}" \
    -t "$files_image" \
    "$PROJECT_ROOT"

  echo ">> importing ${files_image} into k3d cluster ${cluster}"
  k3d image import "$files_image" -c "$cluster"
done

helm_clean_failed_release "$HOSTS_RELEASE_NAME" "$namespace"

echo ">> deploying helm release '${HOSTS_RELEASE_NAME}' in namespace '${namespace}'"
helm upgrade --install "$HOSTS_RELEASE_NAME" "$HOSTS_CHART_DIR" \
  --namespace "$namespace" \
  --set "trento.apiKey=${api_key}" \
  --wait

# Decide the URL where trento-web is reachable from the host.
# Preference order:
#   1. k3d loadbalancer host port mapping for container port 80
#      (works everywhere when cluster was created with --port "X:80@loadbalancer").
#   2. Traefik LoadBalancer's external IP, on Linux only
#      (the docker bridge IP is host-reachable on Linux, but not on macOS/Windows
#       where Docker runs in a VM).
final_url=""

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
elif [[ "$(uname -s)" == "Linux" ]]; then
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

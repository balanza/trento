#!/usr/bin/env bash
# obs-test.sh — publish local Trento submodule state to a personal OBS subproject.
#
# Commands:
#   publish [-f|--filter sm1,sm2,...]   Build per-submodule tarballs and push to OBS
#   status  [-f|--filter sm1,sm2,...]   Aggregate per-package build state
#   cleanup [-y|--yes]                  Remove the OBS subproject and local staging
#
# Subproject: home:balanza:trento.<sanitized-branch>
# See docs/superpowers/specs/2026-06-16-obs-local-test-publisher-design.md

set -euo pipefail

# ── Configuration ─────────────────────────────────────────────────────────────

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FACTORY_PROJECT="devel:sap:trento:factory"
OBS_USER="balanza"
OBS_USER_HOME="home:${OBS_USER}"
CD_IMAGE="ghcr.io/trento-project/continuous-delivery:latest"
OSC_DISTROBOX="osc"   # Name of the distrobox where osc is installed and configured

# Submodules in scope and their packages. A submodule may produce up to three
# kinds of OBS packages: an RPM, a container image (which depends on the RPM),
# and a Helm chart. Unset entries mean "this submodule has no package of that
# kind". Each kind gets its own publish flow.
SUBMODULES=(web wanda agent mcp-server checks helm-charts)
declare -A SUBMODULE_RPM=(
  [web]="trento-web"
  [wanda]="trento-wanda"
  [agent]="trento-agent"
  [mcp-server]="mcp-server-trento"
  [checks]="trento-checks"
)
declare -A SUBMODULE_IMAGE=(
  [web]="trento-web-image"
  [wanda]="trento-wanda-image"
  [mcp-server]="mcp-server-trento-image"
  [checks]="trento-checks-image"
)
declare -A SUBMODULE_HELM=(
  [helm-charts]="trento-server-helm"
)

is_known_submodule() {
  [[ -n "${SUBMODULE_RPM[$1]:-}${SUBMODULE_IMAGE[$1]:-}${SUBMODULE_HELM[$1]:-}" ]]
}

# ── Helpers ───────────────────────────────────────────────────────────────────

die() { echo "ERROR: $*" >&2; exit 1; }
log() { echo "==> $*" >&2; }

container_runtime() {
  # Prefer docker over podman, but only if the daemon is reachable.
  if   command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then echo docker
  elif command -v podman >/dev/null 2>&1 && podman info >/dev/null 2>&1; then echo podman
  else die "neither docker nor podman is usable (binary missing or daemon unreachable)"
  fi
}

# Run osc inside the packaging distrobox. Cwd, stdin, stdout, stderr and
# environment are preserved through `distrobox enter`, so pipes and
# input/output redirection on the host side work transparently.
osc_proxy() {
  distrobox enter --name "$OSC_DISTROBOX" --root -- osc "$@"
}

resolve_branch() {
  local branch
  branch="$(git -C "$REPO_ROOT" symbolic-ref --short HEAD 2>/dev/null || true)"
  if [[ -z "$branch" ]]; then
    branch="$(git -C "$REPO_ROOT" rev-parse --short HEAD)"
    log "Warning: super-repo is in detached HEAD, using short SHA '$branch'"
  fi
  echo "${branch//[^a-zA-Z0-9._-]/_}"
}

resolve_subproject() { echo "${OBS_USER_HOME}:trento.$(resolve_branch)"; }

super_sha() { git -C "$REPO_ROOT" rev-parse --short HEAD; }

synth_version() {
  local sm="$1" describe dirty=""
  describe="$(git -C "$REPO_ROOT/$sm" describe --tags --always 2>/dev/null \
              || git -C "$REPO_ROOT/$sm" rev-parse --short HEAD)"
  if [[ -n "$(git -C "$REPO_ROOT/$sm" status --porcelain)" ]]; then
    dirty=".dirty"
  fi
  echo "${describe//-/.}+local.$(super_sha)${dirty}"
}

# ── osc operations ───────────────────────────────────────────────────────────

osc_project_exists() { osc_proxy meta prj "$1" >/dev/null 2>&1; }
osc_package_exists() { osc_proxy meta pkg "$1" "$2" >/dev/null 2>&1; }

ensure_subproject() {
  local proj="$1"
  if osc_project_exists "$proj"; then
    log "Subproject $proj already exists"
    return
  fi
  log "Creating subproject $proj from $FACTORY_PROJECT meta"
  local meta_tmp
  meta_tmp="$(mktemp)"
  osc_proxy meta prj "$FACTORY_PROJECT" > "$meta_tmp"
  python3 - "$meta_tmp" "$proj" "$FACTORY_PROJECT" "$OBS_USER" <<'PY'
import sys, xml.etree.ElementTree as ET
path, new_proj, old_proj, keep_user = sys.argv[1:]
tree = ET.parse(path)
root = tree.getroot()
root.set("name", new_proj)
for p in list(root.findall("person")):
    if p.get("userid") != keep_user:
        root.remove(p)
for repo in root.findall("repository"):
    for path_el in repo.findall("path"):
        if path_el.get("project") == old_proj:
            path_el.set("project", new_proj)
tree.write(path, xml_declaration=False, encoding="unicode")
PY
  osc_proxy meta prj "$proj" -F "$meta_tmp"
  osc_proxy meta prjconf "$FACTORY_PROJECT" | osc_proxy meta prjconf "$proj" -F -
  rm -f "$meta_tmp"
  log "Subproject $proj created"
}

# ── Container invocation ─────────────────────────────────────────────────────

# Run an arbitrary bash script inside the CD container with $1 bind-mounted at /work.
# Container stdout/stderr is teed to "$staging/.container.log" and also streamed.
# On failure, prints the tail of the log and dies with the container's exit code.
#
# Implementation note: we do NOT pass --user. The image's default user is `osc`,
# which has a valid /etc/passwd entry; overriding with a numeric host uid breaks
# bash startup in this image (no /etc/passwd entry → bash exits before tracing).
# Instead we chmod the staging dir 0777 so the container user can write to it,
# and after the run we chown its outputs back to the host caller.
# Extra args needed per runtime so bind-mounts and uid mapping behave.
container_runtime_args() {
  local runtime="$1"
  local args=()
  if [[ "$runtime" == "podman" ]]; then
    # Rootless podman remaps the container's uid to a host subuid by default;
    # bind-mounted host directories then appear empty/unwritable to the
    # container user. --userns=keep-id keeps the caller's uid mapping intact.
    args+=(--userns=keep-id)
    # openSUSE's default podman config sets logDriver=journald, which sends
    # container stdout/stderr to journald instead of streaming to our pipe —
    # we'd see empty .container.log and have no idea what went wrong.
    # passthrough-tty connects the container's stdio directly to ours; the
    # -tty variant accepts both TTY and non-TTY stdouts.
    args+=(--log-driver=passthrough-tty)
  fi
  # Rootless podman's slirp4netns/pasta network stack can't always create UDP
  # sockets (DNS resolution then fails with "socket: permission denied").
  # `--network=host` shares the host network — works around the problem at
  # the cost of network isolation.  Opt-in via env var.
  if [[ "${OBS_TEST_HOST_NET:-0}" == "1" ]]; then
    args+=(--network=host)
  fi
  # Print one arg per line. Guard against the empty-array case: with no args,
  # `printf '%s\n'` still emits a newline, which mapfile turns into a single
  # empty string element that then becomes a bogus "" argv to the runtime.
  if [[ ${#args[@]} -gt 0 ]]; then
    printf '%s\n' "${args[@]}"
  fi
}

run_in_container() {
  local staging="$1" script="$2"
  local runtime
  runtime="$(container_runtime)"
  local logfile="$staging/.container.log"
  : > "$logfile"
  log "Container log: $logfile"
  chmod -R a+rwX "$staging"
  local extra_args=()
  mapfile -t extra_args < <(container_runtime_args "$runtime")
  set +e
  "$runtime" run --rm "${extra_args[@]}" \
    -v "$staging:/work:z" \
    -w /work \
    -e HOME=/tmp \
    "$CD_IMAGE" \
    bash -xeuo pipefail -c "$script" >>"$logfile" 2>&1
  local rc=$?
  set -e
  chown -R "$(id -u):$(id -g)" "$staging" 2>/dev/null || true
  if [[ "$rc" -ne 0 ]]; then
    echo "------ container output (last 80 lines) ------" >&2
    tail -80 "$logfile" >&2
    echo "----------------------------------------------" >&2
    die "Container exited with code $rc. Full log: $logfile"
  fi
}

# Preflight probe: verify we can actually run a bash command inside the CD image.
# Catches uid/passwd, mount, SELinux, and other env-level failures before we go
# build tarballs and waste minutes per submodule.
container_preflight() {
  local runtime
  runtime="$(container_runtime)"
  log "Container preflight: $runtime + $CD_IMAGE"
  local extra_args=()
  mapfile -t extra_args < <(container_runtime_args "$runtime")
  local probe_dir probe_log rc
  probe_dir="$(mktemp -d)"
  probe_log="$(mktemp)"
  chmod 0777 "$probe_dir"
  # Note: append-redirect (>>) instead of $() because podman's passthrough log
  # driver (which we need for visibility) only delivers output via inherited
  # file descriptors opened in append mode.
  : > "$probe_log"
  set +e
  "$runtime" run --rm "${extra_args[@]}" \
    -v "$probe_dir:/probe:z" \
    -e HOME=/tmp \
    "$CD_IMAGE" \
    bash -c '
      id
      echo "PATH=$PATH"
      echo "WRITE_TEST" > /probe/write.txt && echo "BIND_WRITE: OK" || echo "BIND_WRITE: FAIL"
      for cmd in go mix cargo osc; do
        printf "%-6s -> " "$cmd"; command -v "$cmd" || echo "NOT FOUND"
      done
      if getent hosts proxy.golang.org >/dev/null 2>&1; then
        echo "DNS:   OK (resolved proxy.golang.org)"
      else
        echo "DNS:   FAIL (cannot resolve proxy.golang.org). Set OBS_TEST_HOST_NET=1 or use docker."
      fi
    ' >>"$probe_log" 2>&1
  rc=$?
  set -e
  sed 's/^/  /' "$probe_log" >&2
  local bytes
  bytes=$(wc -c < "$probe_log")
  if [[ "$rc" -ne 0 || ! -s "$probe_log" ]]; then
    rm -rf "$probe_dir" "$probe_log"
    die "Container preflight failed (rc=$rc, $bytes bytes captured).
Likely causes: rootless podman uid remap, broken image entrypoint, or the runtime
swallows stdout. Run manually:
  $runtime run --rm ${extra_args[*]} $CD_IMAGE bash -c 'id; echo hi'"
  fi
  if [[ ! -s "$probe_dir/write.txt" ]]; then
    rm -rf "$probe_dir" "$probe_log"
    die "Container preflight: bind-mount probe failed — container could not write to /probe.
Likely cause: rootless podman uid remap is broken. Ensure 'cat /etc/subuid'
shows '$USER:100000:65536' (and same in /etc/subgid)."
  fi
  rm -rf "$probe_dir" "$probe_log"
}

# ── Per-submodule publish ────────────────────────────────────────────────────

# Build the source tarball from the working tree, unpacking into a top-level
# directory named <pkg>-<version>/ to match the spec's %setup expectation.
build_source_tarball() {
  local sm_path="$1" pkg="$2" version="$3" out="$4"
  # `--anchored` so excludes only match at the submodule root, not any deeper
  # `vendor/` etc. (web ships `assets/vendor/topbar.js` which must NOT be
  # stripped — agents like esbuild fail at build time if it's missing).
  tar --anchored \
      --exclude='./.git' \
      --exclude='./.github' \
      --exclude='./.obs' \
      --exclude='./.obs-test' \
      --exclude='./_build' \
      --exclude='./deps' \
      --exclude='./vendor' \
      --exclude='./target' \
      --exclude='./cover' \
      --exclude='./node_modules' \
      --exclude='./assets/node_modules' \
      --transform "s,^\./,${pkg}-${version}/," \
      -C "$sm_path" \
      -czf "$out" \
      ./
}

bundle_deps_in_container() {
  local sm="$1" pkg="$2" staging="$3" version="$4"
  local src_dir="${pkg}-${version}"
  case "$sm" in
    checks)
      log "[$pkg] No deps to bundle"
      ;;
    web)
      log "[$pkg] Bundling Elixir mix deps + node_modules in container"
      # Invoke obs-service-node_modules directly rather than via
      # `osc service manualrun` (which demands osc credentials inside the
      # container we'd rather not configure). The binary downloads every npm
      # tarball referenced by package-lock.json and emits the cpio + spec.inc.
      run_in_container "$staging" "$(cat <<EOS
cd /work
rm -rf ${src_dir}
tar xzf ${pkg}-${version}.tar.gz
cd ${src_dir}
export MIX_HOME=/tmp/.mix MIX_REBAR3=/usr/bin/rebar3
mix local.hex --force >/dev/null
mix local.rebar --force >/dev/null
mix deps.get
tar czf /work/deps.tar.gz deps
cd /work
# obs-service-node_modules uses --input as a glob SUFFIX in CWD ("*<input>"),
# not as a path, so we must be in /work and pass only the filename.
# --download makes the binary fetch every tarball into the cpio cache. The
# cpio is then the single source of truth at build time (matching factory's
# trento-web package, which ships no download_url entries either).
/usr/lib/obs/service/node_modules \\
  --input package-lock.json \\
  --output node_modules.spec.inc \\
  --cpio node_modules.obscpio \\
  --source-offset 10000 \\
  --outdir /work \\
  --download
EOS
)"
      ;;
    wanda)
      log "[$pkg] Bundling Elixir mix deps + cargo vendor in container"
      run_in_container "$staging" "$(cat <<EOS
set -euo pipefail
cd /work
rm -rf ${src_dir}
tar xzf ${pkg}-${version}.tar.gz
cd ${src_dir}
export MIX_HOME=/tmp/.mix MIX_REBAR3=/usr/bin/rebar3
mix local.hex --force >/dev/null
mix local.rebar --force >/dev/null
mix deps.get
tar czf /work/deps.tar.gz deps
cd deps/rhai_rustler/native/rhai_rustler
cargo vendor vendor
tar czf /work/vendor-rhai_rustler.tar.gz vendor Cargo.lock
EOS
)"
      ;;
    agent|mcp-server)
      log "[$pkg] Bundling Go vendor in container"
      run_in_container "$staging" "$(cat <<EOS
set -euo pipefail
cd /work
rm -rf ${src_dir}
tar xzf ${pkg}-${version}.tar.gz
cd ${src_dir}
go mod vendor
tar czf /work/vendor.tar.gz vendor
EOS
)"
      ;;
    *) die "unknown submodule: $sm" ;;
  esac
  # Drop the extracted source tree; only tarballs and metadata get committed.
  rm -rf "$staging/$src_dir"
  # Sanity-check that the expected outputs were actually produced.
  verify_bundle_outputs "$sm" "$staging"
}

verify_bundle_outputs() {
  local sm="$1" staging="$2"
  local missing=()
  case "$sm" in
    web)
      for f in deps.tar.gz node_modules.obscpio node_modules.spec.inc; do
        [[ -s "$staging/$f" ]] || missing+=("$f")
      done
      ;;
    wanda)
      for f in deps.tar.gz vendor-rhai_rustler.tar.gz; do
        [[ -s "$staging/$f" ]] || missing+=("$f")
      done
      ;;
    agent|mcp-server)
      [[ -s "$staging/vendor.tar.gz" ]] || missing+=("vendor.tar.gz")
      ;;
  esac
  if [[ ${#missing[@]} -gt 0 ]]; then
    die "[$sm] container bundling produced no '${missing[*]}' in $staging. Check container runtime/image."
  fi
}

publish_rpm_package() {
  local sm="$1" pkg="$2" proj="$3" version="$4"
  local sm_path="$REPO_ROOT/$sm"
  local staging="$REPO_ROOT/.obs-test/work/$pkg"
  local src_dir="${pkg}-${version}"

  log "[$pkg] Preparing staging dir"
  rm -rf "$staging"
  mkdir -p "$staging"

  local spec_src=""
  for candidate in \
    "$sm_path/packaging/suse/rpm/${pkg}.spec" \
    "$sm_path/packaging/suse/${pkg}.spec"
  do
    [[ -f "$candidate" ]] && { spec_src="$candidate"; break; }
  done
  [[ -n "$spec_src" ]] || die "spec file for $pkg not found under $sm_path/packaging/suse/"

  cp "$spec_src" "$staging/${pkg}.spec"
  sed -i "s|^Version:.*|Version:        ${version}|" "$staging/${pkg}.spec"
  if [[ "$pkg" == "trento-web" ]]; then
    sed -i "s|%%GTM_ID%%|GTM-N3JHF5M6|g" "$staging/${pkg}.spec"
    cp "$sm_path/assets/package-lock.json" "$staging/package-lock.json"
    # Ship factory-equivalent _service (all manual, never runs on OBS). The
    # presence of <service name="node_modules"> declares the cpio so OBS
    # handles extraction the same way it does for the SCM-synced factory pkg.
    cat > "$staging/_service" <<'XML'
<services>
  <service name="node_modules" mode="manual">
    <param name="cpio">node_modules.obscpio</param>
    <param name="output">node_modules.spec.inc</param>
    <param name="source-offset">10000</param>
  </service>
</services>
XML
  fi

  log "[$pkg] Building source tarball (version=$version)"
  build_source_tarball "$sm_path" "$pkg" "$version" "$staging/${src_dir}.tar.gz"

  bundle_deps_in_container "$sm" "$pkg" "$staging" "$version"

  osc_commit_package "$proj" "$pkg" "$staging"
  record_publish "$pkg" "$version" "committed"
}

publish_image_package() {
  local sm="$1" pkg="$2" rpm_pkg="$3" proj="$4"
  local sm_path="$REPO_ROOT/$sm"
  local staging="$REPO_ROOT/.obs-test/work/$pkg"

  log "[$pkg] Preparing image package"
  rm -rf "$staging"
  mkdir -p "$staging"
  cp "$sm_path/packaging/suse/container/Dockerfile" "$staging/"
  if [[ -f "$sm_path/packaging/suse/container/README.md" ]]; then
    cp "$sm_path/packaging/suse/container/README.md" "$staging/"
  fi

  # web-image alone tags with %%VERSION_SHORT%% in addition to %%VERSION%%.
  local short_block=""
  if [[ "$pkg" == "trento-web-image" ]]; then
    short_block=$(cat <<XML
  <service name="replace_using_package_version" mode="buildtime">
    <param name="file">Dockerfile</param>
    <param name="regex">%%VERSION_SHORT%%</param>
    <param name="parse-version">patch</param>
    <param name="package">${rpm_pkg}</param>
  </service>
XML
)
  fi
  cat > "$staging/_service" <<XML
<services>
  <service name="docker_label_helper" mode="buildtime"/>
  <service name="kiwi_metainfo_helper" mode="buildtime"/>
  <service name="replace_using_package_version" mode="buildtime">
    <param name="file">Dockerfile</param>
    <param name="regex">%%VERSION%%</param>
    <param name="package">${rpm_pkg}</param>
  </service>
${short_block}
  <service name="replace_using_package_version" mode="buildtime">
    <param name="file">Dockerfile</param>
    <param name="regex">\+git\.</param>
    <param name="replacement">-git.</param>
  </service>
</services>
XML

  osc_commit_package "$proj" "$pkg" "$staging"
  record_publish "$pkg" "(image of ${rpm_pkg})" "committed"
}

# Helm chart package: ship Chart.yaml + values.yaml + contents.tar.gz +
# a small buildtime _service. Mirrors what factory's `trento-server-helm`
# package contains (minus the SCM-sync wiring).
publish_helm_package() {
  local sm="$1" pkg="$2" proj="$3"
  local chart_src="$REPO_ROOT/$sm/charts/trento-server"
  local staging="$REPO_ROOT/.obs-test/work/$pkg"

  [[ -f "$chart_src/Chart.yaml" ]] \
    || die "[$pkg] Chart.yaml not found at $chart_src/Chart.yaml"

  # Helm version: take the existing Chart.yaml version (SemVer) and tack on
  # +local.<sha>[.dirty] as SemVer build metadata so OBS rebuilds on each push.
  local base_version dirty="" version
  base_version="$(awk '/^version:[[:space:]]+/{print $2; exit}' "$chart_src/Chart.yaml")"
  [[ -n "$base_version" ]] || die "[$pkg] could not read 'version:' from Chart.yaml"
  if [[ -n "$(git -C "$REPO_ROOT/$sm" status --porcelain)" ]]; then
    dirty=".dirty"
  fi
  version="${base_version}+local.$(super_sha)${dirty}"
  log "Submodule '$sm' helm → version $version"

  log "[$pkg] Preparing helm chart package"
  rm -rf "$staging"
  mkdir -p "$staging"

  # Chart.yaml — copy and patch the `version:` line.
  sed -E "s|^(version:[[:space:]]+).*|\1${version}|" "$chart_src/Chart.yaml" > "$staging/Chart.yaml"

  # values.yaml — patch ghcr.io references to OBS substitution placeholders
  # (the buildtime _service below resolves these to the real registry).
  sed 's|ghcr.io/trento-project|%%REGISTRY%%/%%IMG_REPOSITORY_PREFIX%%|g' \
    "$chart_src/values.yaml" > "$staging/values.yaml"

  # contents.tar.gz: templates + vendored sub-charts (already on disk under
  # helm-charts/charts/trento-server/charts/).
  tar -C "$chart_src" -czf "$staging/contents.tar.gz" templates charts

  # _helmignore if upstream ships one.
  [[ -f "$chart_src/.helmignore" ]] && cp "$chart_src/.helmignore" "$staging/_helmignore"

  cat > "$staging/_service" <<'XML'
<services>
  <service name="replace_using_env" mode="buildtime">
    <param name="file">values.yaml</param>
    <param name="eval">REGISTRY=$(rpm --macros=/root/.rpmmacros -E %registry_url)</param>
    <param name="var">REGISTRY</param>
    <param name="eval">IMG_REPOSITORY_PREFIX=$(rpm --macros=/root/.rpmmacros -E %img_repository_prefix)</param>
    <param name="var">IMG_REPOSITORY_PREFIX</param>
  </service>
  <service mode="buildtime" name="kiwi_metainfo_helper"/>
</services>
XML

  osc_commit_package "$proj" "$pkg" "$staging"
  record_publish "$pkg" "$version" "committed"
}

osc_commit_package() {
  local proj="$1" pkg="$2" staging="$3"
  local checkout_dir="$REPO_ROOT/.obs-test/co/$pkg"

  if ! osc_package_exists "$proj" "$pkg"; then
    log "[$pkg] Creating package in $proj"
    # `osc mkpac` is local-only and requires being inside a project checkout.
    # The cleanest server-side create is to push a minimal package meta.
    printf '<package name="%s" project="%s"><title/><description/></package>\n' "$pkg" "$proj" \
      | osc_proxy meta pkg "$proj" "$pkg" -F -
  fi

  rm -rf "$checkout_dir"
  mkdir -p "$(dirname "$checkout_dir")"
  log "[$pkg] Checking out $proj/$pkg"
  osc_proxy co --output-dir "$checkout_dir" "$proj" "$pkg" >/dev/null
  find "$checkout_dir" -maxdepth 1 -type f \
    \( -name '*.tar.gz' -o -name '*.obscpio' -o -name '_service' \) -delete
  find "$staging" -maxdepth 1 -type f -exec cp -t "$checkout_dir" {} +

  log "[$pkg] Committing"
  ( cd "$checkout_dir" && osc_proxy addremove && osc_proxy commit -m "local test $(super_sha)" )
}

publish_submodule() {
  local sm="$1" proj="$2"
  local rpm_pkg="${SUBMODULE_RPM[$sm]:-}"
  local image_pkg="${SUBMODULE_IMAGE[$sm]:-}"
  local helm_pkg="${SUBMODULE_HELM[$sm]:-}"
  if [[ -n "$rpm_pkg" ]]; then
    local version
    version="$(synth_version "$sm")"
    log "Submodule '$sm' RPM → version $version"
    publish_rpm_package "$sm" "$rpm_pkg" "$proj" "$version"
  fi
  if [[ -n "$image_pkg" ]]; then
    publish_image_package "$sm" "$image_pkg" "$rpm_pkg" "$proj"
  fi
  if [[ -n "$helm_pkg" ]]; then
    publish_helm_package "$sm" "$helm_pkg" "$proj"
  fi
}

# ── Commands ─────────────────────────────────────────────────────────────────

cmd_publish() {
  local sm_list=("$@")
  local proj
  proj="$(resolve_subproject)"
  log "Publish target: $proj"
  container_preflight
  ensure_subproject "$proj"
  if [[ ${#sm_list[@]} -eq 0 ]]; then
    sm_list=("${SUBMODULES[@]}")
  fi
  # Track per-package outcomes; print summary even on partial failure (EXIT trap).
  PUBLISH_REPORT=()
  PUBLISH_PROJ="$proj"
  PUBLISH_FILTER=""
  if [[ ${#sm_list[@]} -lt ${#SUBMODULES[@]} ]]; then
    local IFS=','; PUBLISH_FILTER="${sm_list[*]}"
  fi
  trap publish_report EXIT
  for sm in "${sm_list[@]}"; do
    is_known_submodule "$sm" || die "unknown submodule '$sm' (valid: ${SUBMODULES[*]})"
    publish_submodule "$sm" "$proj"
  done
}

# Append a per-package row to the report. Called from osc_commit_package on
# success and (via a trap inside publish_*_package) on failure.
record_publish() {
  local pkg="$1" version="$2" status="$3"
  PUBLISH_REPORT+=("${pkg}|${version}|${status}")
}

publish_report() {
  trap - EXIT
  local rc=$?
  echo >&2
  echo "==============================================================" >&2
  echo "PUBLISH SUMMARY — ${PUBLISH_PROJ}" >&2
  if [[ ${#PUBLISH_REPORT[@]} -eq 0 ]]; then
    echo "  (no packages committed)" >&2
  else
    printf "  %-30s %-40s %s\n" PACKAGE VERSION STATUS >&2
    local row pkg version status
    for row in "${PUBLISH_REPORT[@]}"; do
      IFS='|' read -r pkg version status <<<"$row"
      printf "  %-30s %-40s %s\n" "$pkg" "$version" "$status" >&2
    done
  fi
  echo "==============================================================" >&2
  echo "URL:    https://build.opensuse.org/project/show/${PUBLISH_PROJ}" >&2
  echo "Status: $(basename "$0") status${PUBLISH_FILTER:+ -f $PUBLISH_FILTER}" >&2
  exit "$rc"
}

cmd_status() {
  local filter=("$@")
  local proj
  proj="$(resolve_subproject)"
  osc_project_exists "$proj" || die "Subproject $proj does not exist. Run 'publish' first."
  local allow_csv=""
  if [[ ${#filter[@]} -gt 0 ]]; then
    local pkgs=()
    for sm in "${filter[@]}"; do
      is_known_submodule "$sm" || die "unknown submodule '$sm'"
      [[ -n "${SUBMODULE_RPM[$sm]:-}" ]]   && pkgs+=("${SUBMODULE_RPM[$sm]}")
      [[ -n "${SUBMODULE_IMAGE[$sm]:-}" ]] && pkgs+=("${SUBMODULE_IMAGE[$sm]}")
      [[ -n "${SUBMODULE_HELM[$sm]:-}" ]]  && pkgs+=("${SUBMODULE_HELM[$sm]}")
    done
    local IFS=','
    allow_csv="${pkgs[*]}"
  fi
  # `osc prjresults --xml PROJECT` instead of `osc results --xml PROJECT`:
  # some osc versions misparse a single dotted project name (e.g.
  # `home:balanza:trento.main`) when passed to `results` without a package,
  # returning 404 for a phantom package. `prjresults` is the dedicated
  # project-wide query and returns the same XML schema.
  #
  # Python script via `-c` (not `python3 - <<HEREDOC`) so the pipe's XML
  # actually reaches sys.stdin — the heredoc form would override stdin.
  local py_script
  py_script="$(cat <<'PY'
import sys, xml.etree.ElementTree as ET
allow = set(filter(None, sys.argv[1].split(",")))
root = ET.fromstring(sys.stdin.read())
agg = {}
for res in root.findall("result"):
    for st in res.findall("status"):
        pkg = st.get("package", "")
        if allow and pkg not in allow:
            continue
        code = st.get("code", "")
        if code == "succeeded": bucket = "OK"
        elif code in ("failed", "unresolvable", "broken"): bucket = "FAIL"
        elif code in ("scheduled", "building", "blocked", "finished",
                      "signing", "dispatching"): bucket = "PEND"
        else: bucket = "OTHER"
        d = agg.setdefault(pkg, {"OK": 0, "FAIL": 0, "PEND": 0, "OTHER": 0})
        d[bucket] += 1
print(f"{'PACKAGE':<30}{'OK':>6}{'FAIL':>6}{'PEND':>6}{'OTHER':>6}{'TOTAL':>7}")
total_fail = total_pend = total_other = 0
for pkg in sorted(agg):
    d = agg[pkg]
    total = sum(d.values())
    total_fail  += d["FAIL"]
    total_pend  += d["PEND"]
    total_other += d["OTHER"]
    print(f"{pkg:<30}{d['OK']:>6}{d['FAIL']:>6}{d['PEND']:>6}{d['OTHER']:>6}{total:>7}")
if total_fail: sys.exit(1)
if total_pend or total_other: sys.exit(2)
sys.exit(0)
PY
)"
  osc_proxy prjresults --xml "$proj" | python3 -c "$py_script" "$allow_csv"
}

# Reverse lookup: which submodule owns a given package name.
submodule_for_package() {
  local pkg="$1" sm
  for sm in "${SUBMODULES[@]}"; do
    if [[ "${SUBMODULE_RPM[$sm]:-}" == "$pkg" ]] \
    || [[ "${SUBMODULE_IMAGE[$sm]:-}" == "$pkg" ]] \
    || [[ "${SUBMODULE_HELM[$sm]:-}" == "$pkg" ]]; then
      echo "$sm"; return 0
    fi
  done
  return 1
}

# Block until OBS has settled (no pending/building/scheduled/dispatching/etc.)
# or until $timeout_s elapses. Returns 0 either way.
wait_for_builds() {
  local proj="$1" timeout_s="${2:-1800}" poll_s="${3:-30}"
  # Give OBS a moment to register the just-committed change before polling,
  # otherwise we'd see the previous run's "succeeded" and exit immediately.
  sleep 10
  local waited=10 pending
  local py_pending
  py_pending="$(cat <<'PY'
import sys, xml.etree.ElementTree as ET
PEND = ("scheduled", "building", "blocked", "finished", "signing", "dispatching")
root = ET.fromstring(sys.stdin.read())
n = sum(1 for r in root.findall("result")
          for s in r.findall("status")
          if s.get("code", "") in PEND)
print(n)
PY
)"
  while true; do
    pending=$(osc_proxy prjresults --xml "$proj" | python3 -c "$py_pending")
    if [[ "$pending" == "0" ]]; then
      log "Builds settled."
      return 0
    fi
    if [[ "$waited" -ge "$timeout_s" ]]; then
      log "Timed out waiting for builds ($pending still pending)."
      return 0
    fi
    log "  $pending build(s) still in-flight; waiting ${poll_s}s (waited ${waited}s)…"
    sleep "$poll_s"
    waited=$((waited + poll_s))
  done
}

cmd_fix() {
  local filter=("$@")
  local proj
  proj="$(resolve_subproject)"
  osc_project_exists "$proj" || die "Subproject $proj does not exist. Run 'publish' first."

  # Build package allow-set (same logic as cmd_status).
  local allow_csv=""
  if [[ ${#filter[@]} -gt 0 ]]; then
    local pkgs=()
    for sm in "${filter[@]}"; do
      is_known_submodule "$sm" || die "unknown submodule '$sm'"
      [[ -n "${SUBMODULE_RPM[$sm]:-}" ]]   && pkgs+=("${SUBMODULE_RPM[$sm]}")
      [[ -n "${SUBMODULE_IMAGE[$sm]:-}" ]] && pkgs+=("${SUBMODULE_IMAGE[$sm]}")
      [[ -n "${SUBMODULE_HELM[$sm]:-}" ]]  && pkgs+=("${SUBMODULE_HELM[$sm]}")
    done
    local IFS=','
    allow_csv="${pkgs[*]}"
  fi

  # Extract every (package, repo, arch, code) tuple with a failure-class status.
  local py_failed
  py_failed="$(cat <<'PY'
import sys, xml.etree.ElementTree as ET
allow = set(filter(None, sys.argv[1].split(",")))
FAIL = ("failed", "unresolvable", "broken")
root = ET.fromstring(sys.stdin.read())
for res in root.findall("result"):
    repo = res.get("repository", "")
    arch = res.get("arch", "")
    for st in res.findall("status"):
        pkg = st.get("package", "")
        code = st.get("code", "")
        if allow and pkg not in allow:
            continue
        if code in FAIL:
            print(f"{pkg}|{repo}|{arch}|{code}")
PY
)"

  local logs_dir="$REPO_ROOT/.obs-test/logs"
  local max_iter="${OBS_TEST_MAX_FIX_ITER:-5}"
  local wait_timeout="${OBS_TEST_FIX_WAIT_TIMEOUT:-1800}"
  local iter=0

  while true; do
    iter=$((iter + 1))
    if [[ "$iter" -gt "$max_iter" ]]; then
      die "Reached max fix iterations ($max_iter). Inspect manually."
    fi
    log "=== fix iteration $iter/$max_iter ==="

    local failed_lines
    failed_lines="$(osc_proxy prjresults --xml "$proj" | python3 -c "$py_failed" "$allow_csv")"

    if [[ -z "$failed_lines" ]]; then
      log "No failed builds. Done."
      return 0
    fi

    rm -rf "$logs_dir"; mkdir -p "$logs_dir"

    # Per-iteration: track which submodules the agents actually edited so we
    # only republish those.
    declare -A touched=()
    local pkg repo arch code log_file sm before after
    while IFS='|' read -r pkg repo arch code; do
      if ! sm="$(submodule_for_package "$pkg")"; then
        log "  [$pkg] unknown package — skipping"
        continue
      fi
      log_file="$logs_dir/${pkg}__${repo}__${arch}.log"
      log "  [$pkg / $repo / $arch ($code)] fetching log…"
      # `osc buildlog` re-splits the project arg on ':' under some name shapes
      # (yielding "no architecture 'pkg'" 404s for `home:balanza:trento.main`).
      # `osc remotebuildlog` has explicit positional parsing and is the right
      # call for server-side log fetch.
      osc_proxy remotebuildlog "$proj" "$pkg" "$repo" "$arch" >"$log_file" 2>&1 || true
      log "    log: $log_file ($(wc -l <"$log_file") lines)"

      before="$(git -C "$REPO_ROOT/$sm" status --porcelain)"
      log "    invoking fix-obs-build agent on '$sm'…"
      ( cd "$REPO_ROOT/$sm" && \
        PATH="$REPO_ROOT/hack:$PATH" amake run fix-obs-build \
          -f "$REPO_ROOT/Amakefile" \
          --var submodule="$sm" \
          --var package="$pkg" \
          --var repo="$repo" \
          --var arch="$arch" \
          --var code="$code" \
          --var log_file="$log_file" \
      ) || log "    agent exited non-zero (continuing)"
      after="$(git -C "$REPO_ROOT/$sm" status --porcelain)"
      if [[ "$before" != "$after" ]]; then
        log "    agent modified files in '$sm'"
        touched[$sm]=1
      else
        log "    agent made no changes to '$sm'"
      fi
    done <<<"$failed_lines"

    if [[ ${#touched[@]} -eq 0 ]]; then
      log "No submodules were modified this iteration — agents made no progress."
      die "Bailing out (would loop forever)."
    fi

    log "Re-publishing modified submodules: ${!touched[*]}"
    for sm in "${!touched[@]}"; do
      publish_submodule "$sm" "$proj"
    done

    log "Waiting for OBS to rebuild (timeout ${wait_timeout}s)…"
    wait_for_builds "$proj" "$wait_timeout"
  done
}

cmd_cleanup() {
  local yes="$1"
  local proj
  proj="$(resolve_subproject)"
  osc_project_exists "$proj" || die "Subproject $proj does not exist."
  if [[ "$yes" != "yes" ]]; then
    read -r -p "Delete OBS project $proj and local .obs-test/? [y/N] " ans
    [[ "$ans" =~ ^[yY]$ ]] || { log "Aborted"; exit 0; }
  fi
  log "Removing $proj"
  # -f: the subproject's `containers`/`charts` repos have path dependencies
  # back to the project itself (we wrote those at creation time so image
  # packages pull RPMs from the same subproject), which OBS treats as a
  # dependency that blocks deletion. --force lets us delete anyway.
  osc_proxy rdelete -r -f -m "cleanup local test" "$proj"
  rm -rf "$REPO_ROOT/.obs-test"
  log "Cleanup complete"
}

# ── Main ─────────────────────────────────────────────────────────────────────

usage() {
  cat >&2 <<USAGE
Usage:
  $(basename "$0") publish [-f|--filter <sm1,sm2,...>]
  $(basename "$0") status  [-f|--filter <sm1,sm2,...>]
  $(basename "$0") fix     [-f|--filter <sm1,sm2,...>]
  $(basename "$0") cleanup [-y|--yes]

Submodules in scope: ${SUBMODULES[*]}
Subproject:          $(resolve_subproject 2>/dev/null || echo "<unknown — not in a git repo>")
USAGE
  exit 1
}

main() {
  [[ $# -ge 1 ]] || usage
  local cmd="$1"; shift
  local filter=()
  local yes="no"
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -f|--filter) IFS=',' read -ra filter <<< "$2"; shift 2;;
      -y|--yes)    yes="yes"; shift;;
      -h|--help)   usage;;
      *)           die "Unknown argument: $1";;
    esac
  done
  # Auth probe: /person/<user> requires authentication and returns XML on success.
  # Using this instead of `osc whoami` because whoami's output varies by version
  # (stdout vs stderr, with/without email).
  osc_proxy api "/person/$OBS_USER" >/dev/null 2>&1 \
    || die "Cannot authenticate to OBS as '$OBS_USER'. Check ~/.config/osc/oscrc credentials inside distrobox '$OSC_DISTROBOX'."
  case "$cmd" in
    publish) cmd_publish "${filter[@]}";;
    status)  cmd_status  "${filter[@]}";;
    fix)     cmd_fix     "${filter[@]}";;
    cleanup) cmd_cleanup "$yes";;
    *)       usage;;
  esac
}

main "$@"

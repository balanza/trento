# Trento monorepo — agent guide

## What is Trento

Trento is an observability tool for SAP installations. It has three runtime
components, each developed in its own upstream repo and pulled in here as a git
submodule:

| Path     | What it is                                                        |
|----------|-------------------------------------------------------------------|
| `web/`   | Central Phoenix/Elixir web app (UI + REST API + RabbitMQ events). |
| `wanda/` | Check execution engine. Runs the catalog from `checks/`.          |
| `agent/` | Go agent installed on every monitored host. Talks to web + RMQ.   |

Hosts run a `trento-agent` that connects to `trento-web` over HTTP (with an
API key) and to RabbitMQ for facts/check results. The agent ships discovery
data (HA cluster, SAP, hosts metadata) and acts as a worker for `wanda`.

Other submodules:

- `checks/`     — YAML check catalog consumed by `wanda`.
- `contracts/`  — shared protobuf/cloudevents schemas across components.
- `helm-charts/` — official charts. `charts/trento-server` is the umbrella
  chart that installs `trento-web`, `trento-wanda`, `trento-mcp-server`,
  Postgres, Prometheus, and RabbitMQ.

## Purpose of this repo

A single repository that wires every moving part of Trento together so the
whole system can be built and run end-to-end on a local k8s cluster. The
runtime components stay in their upstream repos — they appear here only as
submodule pins.

## How to run

`./hack/start.sh` is the one entry point. It assumes:

- The current `kubectl` context points at a **k3d** cluster (context name
  starts with `k3d-`). The script refuses any other context.
- `kubectl`, `helm`, `docker`, and `k3d` are installed.

Flow:

1. Optionally `docker build` + `k3d image import` for `web`, `wanda`, `checks`
   (only when `--build web,wanda,checks` is passed; default = reuse existing
   `local/trento-{web,wanda,checks}:dev`).
2. Wait for the Traefik `Middleware` CRD (k3s installs Traefik async, so a
   freshly created cluster races the first `helm install`).
3. `helm upgrade --install trento ./helm-charts/charts/trento-server`
   pinned to the local `local/trento-*:dev` images with `pullPolicy=Never`.
4. Extract the API key from the running web pod via
   `kubectl exec … -- /app/bin/trento eval …`. The key is bound to web's
   `ACCESS_TOKEN_ENC_SECRET` (regenerated on every fresh install), so it must
   be read from the live instance, never hard-coded.
5. Build the host-fixture images (see next section) and `helm upgrade --install
   host-fixtures ./container_fixtures/hosts/helm` with `trento.apiKey=<value>`.
6. Print the URL to reach `trento-web` (k3d loadbalancer port mapping or, on
   Linux, the Traefik LB IP).

Useful flags: `--build web,wanda,checks`, `--release-name`, `--namespace`.

## Mocked hosts (`container_fixtures/hosts/`)

Hosts are simulated, not real VMs. The fixture for the active scenario is
shipped as **two images** per host:

- `local/agent:dev` — built once from `Dockerfile.agent`. SUSE BCI base +
  `trento-agent` (built from `./agent`) + `alloy`, `soappatrol`, `fakemall`,
  `catatonit`, plus `init.sh`. Same image for every host.
- `local/files:<scenario>-<host>` — built per-host from
  `Dockerfile.filesystem` (SUSE BCI base). Just the per-host `/etc` and
  `/usr` overlay under `/fixture/` (corosync config, SAP profiles, HANA
  shims, etc.).

At pod startup the helm chart runs the per-host files image as an
**initContainer** that copies its `/fixture/` tree into a shared `emptyDir`.
The agent container mounts that `emptyDir` read-only at `/fixture`, and
`init.sh` overlays `/fixture/etc` and `/fixture/usr` onto the agent's own
root before launching `trento-agent` and `alloy`.

Default scenario is `hana-scale-out` with three hosts:

| Name            | hasSap | Role                |
|-----------------|--------|---------------------|
| `hana-s1-db1`   | true   | HANA primary site   |
| `hana-s2-db1`   | true   | HANA secondary site |
| `hana-s-mm`     | false  | Majority maker      |

The host list, agent IDs, image names, and `hasSap` flag live in
`container_fixtures/hosts/helm/values.yaml` and **must stay in sync** with the
`HOSTS` array in `hack/start.sh`.

## Work on tooling OR on the product, never both

This repo's job is to glue the pieces together — not to ship product code.

- **Tooling work** (allowed here): `hack/`, `container_fixtures/`,
  `.claude/`, the top-level `.gitmodules`, and the shared `docs/`. None of
  this is shipped to users.
- **Product work** (NOT allowed here): anything inside `web/`, `wanda/`,
  `agent/`, `checks/`, `contracts/`, or `helm-charts/`. These are submodules.
  Edits there belong in their own upstream repos and PRs. From this repo,
  only bump the submodule pin once the upstream change has merged.

If a task seems to require touching both, stop and confirm with the user
which side it really belongs on.

## "It works if…" — verification

The system is healthy when:

1. `helm list` shows `trento` and `host-fixtures` both `deployed`.
2. All `trento-*` and host pods are `Running` / `Ready`.
3. Each host agent pod logs a successful registration with `trento-web` and
   an established AMQP connection to `trento-rabbitmq`.
4. The web UI lists all three hosts, the HANA cluster is discovered, and
   discovery / check events flow without errors.

Quick checks:

```bash
kubectl get pods
kubectl logs -l app.kubernetes.io/name=web      | grep -iE 'register|error'
kubectl logs <host-fixture-pod>                 | grep -iE 'discover|amqp|error'
```

UI URL is printed at the end of `hack/start.sh`. Default credentials seeded
by the script: `admin` / `adminpassword`.

## Gotchas

- **API key freshness.** `web`'s `ACCESS_TOKEN_ENC_SECRET` is regenerated on
  every fresh helm install, so any cached `apiKey` is invalidated.
  `host-fixtures` reads it via `--set trento.apiKey=…`; the values file
  intentionally leaves it empty so a bare `helm install` produces a visibly
  broken ConfigMap rather than silently using a stale token.

- **Traefik CRD race.** On a freshly created k3d cluster, k3s installs
  Traefik (and its `Middleware` CRD) asynchronously. Helm releases that ship
  Middleware CRs will fail with `no matches for kind "Middleware"` if
  installed too early. `hack/start.sh` waits up to 120s for the CRD before
  handing off to helm.

- **Submodules unchecked-out.** A fresh clone needs
  `git submodule update --init --recursive` before anything will build.

## Pointers for common tasks

| Task                                       | Look at                                                  |
|--------------------------------------------|----------------------------------------------------------|
| Change how the cluster gets installed      | `hack/start.sh`                                          |
| Add/remove a simulated host                | `container_fixtures/hosts/helm/values.yaml` + `HOSTS=()` in `hack/start.sh` |
| Add/modify per-host filesystem overlay     | `container_fixtures/hosts/<scenario>/<host>/{etc,usr}/`  |
| Change the agent image (system deps)       | `container_fixtures/hosts/Dockerfile.agent`              |
| Change pod startup behavior                | `container_fixtures/hosts/init.sh`                       |
| Add a new scenario                         | New dir under `container_fixtures/hosts/<scenario>/` + new `HOSTS_SCENARIO` block |

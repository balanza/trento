# Trento

Glue repo that wires the Trento components together so the whole system runs
end-to-end on a local Kubernetes cluster.

The runtime components live in their own upstream repos and are pulled in here
as submodules:

- `web/`   — Phoenix/Elixir UI + REST API + RabbitMQ events
- `wanda/` — check execution engine
- `agent/` — Go agent that runs on every monitored host
- `checks/`, `contracts/`, `helm-charts/` — shared assets

Hosts are simulated by container fixtures under `container_fixtures/hosts/`.

## Prerequisites

- A running [k3d](https://k3d.io) cluster, with `kubectl` pointed at it
  (the context name must start with `k3d-`)
- `kubectl`, `helm`, `docker`, `k3d` on `PATH`

## First-time setup

```sh
git clone <this-repo>
cd trento
git submodule update --init --recursive
```

## Run

```sh
./hack/start.sh
```

This installs the `trento-server` Helm chart, extracts the freshly generated
API key from the running web pod, then installs the simulated hosts wired to
that key. When it finishes it prints the URL for the web UI.

Default login: `admin` / `adminpassword`.

### Useful flags

| Flag                              | Meaning                                                              |
|-----------------------------------|----------------------------------------------------------------------|
| `--build web,wanda,checks`        | Rebuild the listed component images and import them into k3d         |
| `--release-name <name>`           | Helm release name (default: `trento`)                                |
| `--namespace <ns>`                | Target namespace                                                     |

Without `--build`, the script reuses the existing `local/trento-{web,wanda,checks}:dev`
images.

## Verify it's working

```sh
helm list                                    # trento + host-fixtures both Deployed
kubectl get pods                             # everything Running / Ready
kubectl logs -l app.kubernetes.io/name=web | grep -iE 'register|error'
```

In the UI you should see all three simulated hosts, the discovered HANA
cluster, and discovery / check events flowing in.

## Default scenario

`hana-scale-out` — three hosts:

| Host          | Role                |
|---------------|---------------------|
| `hana-s1-db1` | HANA primary site   |
| `hana-s2-db1` | HANA secondary site |
| `hana-s-mm`   | Majority maker      |

Add or change hosts by editing `container_fixtures/hosts/helm/values.yaml`
**and** the `HOSTS` array in `hack/start.sh` — they must stay in sync.

## Scope

This repo is for tooling that wires the system together (`hack/`,
`container_fixtures/`, top-level config). Product changes belong in the
upstream submodule repos; from here you only bump the submodule pin once
those changes have merged.

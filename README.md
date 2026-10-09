# pack — a package format for horchestra, and the tool that renders it

`pack` turns a **component schema** (a JSON Schema generated from Go) plus a **values document** into what horchestra
runs: an `ApplicationSet` and the
`Secret`s that carry its configuration files. It needs no cluster and no network — every input is a directory on disk,
every output is YAML.

```
Go structs ──go generate──▶ schema.json ──┐
                                          ├── pack template ──▶ applicationset.yaml + secrets/*.yaml + report.yaml
values.yaml ──────────────────────────────┘
```

The tool is **phase 1** of the pack design: the format, the authoring workflow and the renderer. Phase 2 is horchestra
learning the handful of fields the output already carries (`merge`/`substitute` on a secret mount,
`readiness`, `${horchestra.index}` in a mount item). See
[docs/output.md](docs/output.md#what-horchestra-does-not-know-yet).

## Concepts

| term                   | what it is                                                                                                                                                                                        | where it lives                                                                           |
|------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------|
| **component**          | one application: its image, the schema of its values, and — inside the schema — how its config files, argv and environment are made, what it needs from neighbours and libraries, how it upgrades | a directory: `schema.json`, `templates/`, `image.json`, `pack.yaml`                      |
| **library**            | a component without a workload of its own — layers (a JDK, plugins) mounted into a consumer, and settings the consumer's values carry under its key                                               | same shape, no `platform`/`ports`/`scale`                                                |
| **composition** (pack) | the keys of a system, each a `$ref` to a component by name, and what no component can know: who is whose neighbour when the type is ambiguous, ordering, spread, savepoints of the upgrade saga   | a directory with the composition's `schema.json`; components are the sibling directories |
| **values document**    | what the operator writes: one tree shaped by the composition's effective schema                                                                                                                   | a YAML file                                                                              |
| **output**             | `ApplicationSet` + config `Secret`s + report                                                                                                                                                      | `-o <dir>` or stdout                                                                     |

Both the component and the composition are **JSON Schema documents**. The vocabulary the tool adds — `x-pack-file`,
`x-pack-arg`, `x-pack-env`,
`x-pack-compute`, `x-pack-mount`, `x-pack` — rides on standard keywords a validator ignores; the tool is the one thing
that reads them. Schemas are not written by hand: a Go default value is the source, and `go generate`
produces `schema.json` ([docs/authoring.md](docs/authoring.md)).

## Quick start

Author a component in Go (`examples/postgres/postgres.go`):

```go
type Postgres struct {
sdk.Workload                  // scale (lifted into x-pack.scale) + platform — the target's schema
Ports     Ports               `json:"ports"`
Claims    Claims              `json:"claims"`
Database  string              `json:"database" jsonschema:"required"`
Superuser Superuser           `json:"superuser"`
Conf      Conf                `json:"conf" pack:"file=/etc/postgresql/postgresql.conf,format=properties"`
}

type Superuser struct {
Name     string            `json:"name" pack:"default,env=POSTGRES_USER"`
Password packschema.Secret `json:"password" jsonschema:"required" pack:"env=POSTGRES_PASSWORD"`
}

func Defaults() Postgres { return Postgres{Workload: sdk.Single("500m", "512Mi"), Ports: Ports{PG: 5432}, /* … */} }
func (Postgres) PackRoot() packschema.Root { return packschema.Root{Readiness: &packschema.Readiness{TCP: "pg"}} }
func (Postgres) PackCompute() map[string]string {
return map[string]string{"/conf/shared_buffers": "string(limits.memory / 1048576 / 4) + 'MB'"}
}
```

Generate, check, render:

```sh
go generate ./examples/...                       # writes examples/*/schema.json
go build -o bin/ ./cmd/...                       # pack and the providers it runs, side by side
bin/pack check examples/postgres                 # author checks — the same ones the loader repeats
cat > values.yaml <<'Y'
values:
  postgres:
    database: app
    superuser: {password: {secretRef: {name: pg, key: password}}}
Y
bin/pack template --pack examples/postgres --values values.yaml --name dev --namespace apps -o out/
```

`out/applicationset.yaml` holds one application named `postgres` with the image from `pack.yaml`, `POSTGRES_USER`/
`POSTGRES_PASSWORD` in `env` (the password as a `secretRef`), port 5432, a `pv` for `/var/lib/postgresql/data`
and a secret mount for `/etc/postgresql`; `out/secrets/dev-postgres-….yaml`
holds `postgresql.conf` with `shared_buffers=128MB` computed from the memory limit.

A composition of several components — ZooKeeper, Kafka on it, two Postgres instances, a JDK library shared by the JVMs —
is `examples/adh`; its golden output is `examples/adh/testdata/expected`.

`pack` knows no target. The objects come from a **provider**, a program of its own: `--target horchestra` (the
default) runs `pack-provider-horchestra`, found in `$PACK_PROVIDER_PATH`, beside `pack`, or on `PATH`, and speaks
`proto/provider.proto` with it — what a key's platform means on the target (names, addresses, instances, limits),
whether it meets the composition's minimum, what a rendered release becomes, and on a target that performs steps
each step of the saga. `--provider` names another: a binary's path, or a **container image**,
`--provider oci://<host>/<repo>:<tag>`, for a provider in Python or Java on a machine without their runtime — pack
runs it with podman or docker (`$PACK_CONTAINER_RUNTIME`), the provider listening on TCP inside, published on
127.0.0.1 only. A provider in any language reads `PACK_PROVIDER_ADDRESS` (`unix:///…` or `tcp://…`) and
`PACK_PROVIDER_TOKEN` (demand it as the `pack-provider-token` metadata of every call), and exits on EOF on stdin;
in Go, `provider.Serve` does all of it.

**Kubernetes.** `--target k8s` runs `pack-provider-k8s`: a key becomes a StatefulSet and a headless Service
`<installation>-<key>` (a Job when it runs to completion), its config files a Secret per directory mounted file by file
over the image (a per-instance file by `subPathExpr: $(POD_NAME)`), its claims `volumeClaimTemplates`, its libraries
image volumes (a runtime that has them: CRI-O, containerd ≥ 2.1, the ImageVolume gate). Its half of the platform is
`replicas`, `nodeSelector`, `priorityClassName`, `storageClass`; it performs the saga's steps with `kubectl` — the
kubeconfig, context and namespace from the environment — resolves `${secret:…}` from the cluster's Secrets when it
applies, runs hooks as Jobs and keeps the installation's record in the ConfigMap `pack-<installation>`
(`examples/adh/envs/k8s.yaml`, golden output in `target/k8s/testdata/adh`).

**Environments.** A pack is described once — its values — and installed into as many environments as there are
files: `--config envs/prod.yaml` says where (`target`, `provider`) and what the target needs (a namespace, an
inventory…), and names the Vault KV of that environment (`vault.addr`, `vault.paths`; the token from `$VAULT_TOKEN`
only). Vault's keys go over the file's, `--set` over both; the result, flattened by the
[app-config](https://github.com/ks-tool/app-config) rule (`{vault: {addr: …}}` is `vault_addr`), is the set the
provider gets, whole, in `Configure` — never in its environment or its arguments, where a container would show the
secrets. The same command with the same file does the same on a console and in CI:

```sh
VAULT_TOKEN=… pack apply --config envs/staging.yaml --pack oci://registry.example/packs/pg:16.3 --values pg.yaml --name pg
VAULT_TOKEN=… pack apply --config envs/prod.yaml    --pack oci://registry.example/packs/pg:16.3 --values pg.yaml --name pg
```

From a registry instead of a checkout: give `--pack` the reference —
`pack template --pack oci://10.92.16.25:5000/ee/bundles/adh:4.3.0 …`. The tool reads the index, the schemas, the
templates and the image configs itself and never an image layer — see
[docs/artifacts.md](docs/artifacts.md#rendering-from-a-registry).

## CLI

| command                                                                                                                                                                                | what it does                                                                                                                                                                                                                                                                                                                                   |
|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `pack check [--plain-http] <dir\|ref>`                                                                                                                                                 | Author checks on a component directory (`schema.json`, `templates/`, `image.json`), or on a composition — a directory or a pack in a registry (`oci://host/repo:tag`) — then every member is checked and the composition's own rules (role binding, ordering, membership) on top. Exit 1 with one line per problem.                             |
| `pack template --pack <dir\|ref> --values <file> --name <installation> [--config <env.yaml>] [--target horchestra] [--provider <path\|oci://image>] [--set <k>=<v> …] [--namespace <ns>] [-o <dir>] [--plain-http] [--secretProvider generate\|env\|file:<dir> --secret <pointer> …]` | Render for a target. `--pack` is a composition or a lone component, as a directory or a registry reference. `--target` picks the system — its provider `pack-provider-<target>` is run (today `horchestra`, the default) — `--set` passes its settings — horchestra takes `namespace`, `--namespace` is the shorthand; a setting the target does not know is refused. Without `-o`, the target's objects as a YAML stream on stdout. `--secret` names password leaves the tool provisions itself (an explicit allowlist); the provider says where their values come from — see [docs/values.md](docs/values.md#credentials-the-tool-creates). |
| `pack schema vocabulary\|platform [--target <t>]` | Print the `x-pack-*` vocabulary the tool ships (`horchestra://pack/v1`), or the schema a target gives the platform half (the slot `urn:pack:platform`). |
| `pack plan [--from <dir\|ref>] --to <dir\|ref> --values <file> [--savepoints auto\|none\|k1,k2] [--no-rollback k1,k2\|all] [--strict] [-o plan.yaml] [--plain-http]`                   | The saga: a first installation (no `--from`) or the upgrade between two versions — what changes, which hooks run, the rollback units and how far a failure rolls back ([docs/saga.md](docs/saga.md)). Prints the plan; `-o` also writes it as YAML.                                                                                            |
| `pack apply --pack <dir\|ref> --values <file> --name <installation> [--config <env.yaml>] [--target <t>] [--set <k>=<v> …] [--savepoints …] [--no-rollback …] [--strict] [--dry-run] [--from-start] [--secretProvider … --secret …]` | Install or upgrade on a target that performs steps (`execute` capability): the target says what is installed, pack plans the saga, drives it step by step through the provider and rolls a failed unit back; an unfinished saga is continued from its last savepoint by the same command, `--from-start` runs it from its first step; the record of the installation stays with the target ([docs/saga.md](docs/saga.md#execution)). |
| `pack rollback --name <installation> [--config <env.yaml>] [--target <t>] [--set <k>=<v> …] [--dry-run]` | Take an installation back: an unfinished saga is undone whole — its units behind savepoints too; after a finished one, a saga back to the version it replaced ([docs/saga.md](docs/saga.md#execution)). |

## Repository layout

```
pack/
  cmd/pack/            the CLI
  cmd/pack-provider-horchestra/  the horchestra provider: target/horchestra behind the provider protocol
  cmd/pack-provider-k8s/         the Kubernetes provider: target/k8s — StatefulSets, Services, Secrets; steps through kubectl
  proto/               provider.proto — the protocol between pack and a provider; gen.sh regenerates provider/providerpb
  provider/            Launch (pack's side: a provider binary as a target.Target) and Serve (a provider binary's main)
  environment/         an environment: its file and Vault KV, the set its provider gets (app-config)
  sdk/                 the author's SDK — what a component is written against, and what the tool is built on:
    sdk.go               Meta, Component, Workload, Generate, Verify, Main
    packschema/          the vocabulary (Go types) and the generator Reflect()
    schema/              the tool's own view of a schema: nodes, annotations, defaults, validating view
    check/               author checks
    compute/             the CEL environment of x-pack-compute
    semver/              versions and constraints
    platform/            the vocabulary the tool ships (horchestra://pack/v1) and the platform slot urn:pack:platform, filled by a target
    python/, java/       the same SDK in Python and Java — sdk/README.md
  examples/            jdk, zookeeper, kafka, postgres, adh — go generate writes their schema.json
  emit/                file emitters
  bundle/              loading a composition or component from directories or a registry
  registry/            the tool's own OCI distribution reader (manifests, schemas, templates, image configs — no layers)
  render/              the core: configure → mutate → validate → a target-neutral Release (units, rendered files)
  target/              what the core asks of a system a pack is installed on: Target, Installation (names, Convert)
  target/horchestra/   the horchestra target: a Release → ApplicationSet + config Secrets; its names for formulas
  plan/                the saga: a first installation, or the upgrade between two versions
  saga/                drives a plan on a target: every step through the provider, the record after each, the rollback of a failed unit
  docs/                this documentation
```

## Documentation

- [docs/authoring.md](docs/authoring.md) — writing a component in Go: facts, settings, files, argv, env, secrets,
  libraries, neighbours, formulas, readiness, hooks; every check and how to satisfy it
- [docs/composition.md](docs/composition.md) — composing components into a system
- [docs/values.md](docs/values.md) — the operator's document and how the effective document is built
- [docs/output.md](docs/output.md) — the `ApplicationSet` and `Secret` contract, field by field
- [docs/schema-reference.md](docs/schema-reference.md) — the `x-pack-*` vocabulary and the `pack:"…"` tag grammar
- [docs/emitters.md](docs/emitters.md) — file formats
- [docs/artifacts.md](docs/artifacts.md) — building and publishing with `oci-packer`
- [docs/saga.md](docs/saga.md) — upgrades: hooks, savepoints, rollback

## Building and testing

Go 1.27 (`go.mod` says `go 1.27.1`); one module, no `go.work`:

```sh
go test ./...                # -short skips building the provider binary
go build -o bin/ ./cmd/...   # pack + pack-provider-horchestra, side by side
go generate ./examples/...   # after editing an example
PATH="$HOME/go/bin:$PATH" proto/gen.sh            # after editing proto/provider.proto (protoc, protoc-gen-go, protoc-gen-go-grpc)
UPDATE_GOLDEN=1 go test ./target/... ./plan/   # after a deliberate output change
```

Tests fail when a committed `schema.json` drifts from its Go source, and when the rendered output of `examples/adh`
drifts from the golden files — both are meant to be reviewed diffs, not surprises.

# Authoring a component

A component is a JSON Schema. You do not write the schema; you write a Go
value and the schema is generated from it. This page walks through everything
a component can say, in the order you will meet it, and ends with the checks
`pack check` runs and what each one means.

## 1. The shape of a component

```go
package kafka

type Kafka struct {
    Ports    Ports               `json:"ports"`
    sdk.Workload                                     // scale (x-pack.scale, a fact) and platform (the target's schema)
    JDK      jdk.JDK             `json:"jdk" pack_ref:"jdk" pack:"mount=/opt/java"`
    Claims   Claims              `json:"claims"`
    Conf     Conf                `json:"conf" pack:"file=/opt/kafka/config/server.properties,format=properties"`
    Jaas     Jaas                `json:"jaas" pack:"file=/opt/kafka/config/kafka_jaas.conf,format=template,template=jaas.tmpl,parseAs=properties,mode=0400"`
    HeapOpts string              `json:"heapOpts,omitempty" pack:"env=KAFKA_HEAP_OPTS"`
}

func Defaults() Kafka { /* the defaults document */ }
func (Kafka) PackRoot() packschema.Root { /* readiness, peers, requires, upgrade, hooks */ }
func (Kafka) PackCompute() map[string]string { /* JSON Pointer → CEL */ }
```

Three things make a component:

1. **The struct** — every field is a property of the schema; nested structs
   are nested objects. `json` tags name the properties (use the application's
   own key names, `min.insync.replicas` included); `jsonschema` tags carry
   the standard keywords (`minimum`, `enum`, `required`, `title`,
   `description`); `pack` tags carry the vocabulary.
2. **`Defaults()`** — the value you pass to the generator. It is the
   **defaults document**: what a field holds here is what the schema will
   default (or fix) it to.
3. **`PackRoot()` and `PackCompute()`** — the root annotation and the
   formulas. Formulas are CEL strings; they do not live in struct tags
   (quotes and commas), so they are keyed by JSON Pointer instead.

Generate with a small program or a `go:generate` line:

```go
s, err := packschema.Reflect(kafka.Defaults())   // *jsonschema.Schema
raw, _ := json.MarshalIndent(s, "", "  ")
os.WriteFile("schema.json", raw, 0o644)
```

`examples/gen` does this for every example; `examples/examples_test.go`
fails when a committed `schema.json` no longer matches its Go source, so
regeneration is part of every change.

## 2. Facts, settings, and the operator's fields

Every leaf of the schema is one of three kinds, and the `pack` tag decides:

| tag | schema | meaning | validator | at render |
|---|---|---|---|---|
| `pack:"const"` | `const: <value>` | a **fact** about the software: a port, a mount path | a different value is refused | filled in when absent |
| `pack:"default"` | `default: <value>` | a **setting** with a sensible default | checked by type | filled in when absent |
| (no tag) | — | the **operator's** field; add `jsonschema:"required"` to make them name it | checked by type; required must be present | — |

```go
type Ports struct {
    Broker   int `json:"broker" pack:"const"`     // 9092 in Defaults(): a fact
    Internal int `json:"internal" pack:"const"`
}
type Conf struct {
    LogDirs           string `json:"log.dirs" pack:"const"`                                   // the image's data dir
    MinInsyncReplicas int    `json:"min.insync.replicas" jsonschema:"minimum=1" pack:"default"` // 1 in Defaults()
    ZookeeperConnect  string `json:"zookeeper.connect,omitempty"`                              // computed, see §8
}
```

Prefer `const` for anything the image fixes (ports the process listens on,
the data directory baked into it). An operator who needs a
different port is asking for a different image.

### `ports` and `scale`

`ports` is a property with meaning to the tool; `scale` is a fact of the root:

- `ports` — an object of named ports, every one `const`. Names are what
  neighbours and readiness refer to (`peers.zk.ports.client`,
  `readiness: {tcp: "broker"}`); numbers go to `spec.ports`.
- `x-pack.scale` — `single` or `replicas`, a fact about the application,
  not a value anyone writes: `sdk.Workload` lifts it there (`sdk.Single`,
  `sdk.Replicated`), Python and Java say `Root(scale=…)`. `replicas` means the
  target decides how many (horchestra: `platform.replicas`), each instance
  has its own name (horchestra: `<installation>-<key>-<i>`), and per-instance
  files are allowed. A component with `platform` must declare it; the old
  `scale` value leaf is refused.

## 3. The platform half

```go
sdk.Workload    // embeds: Scale string `pack:"scale"` (x-pack.scale), Platform sdk.Platform `pack:"ref=urn:pack:platform,default"`
```

`platform` is two halves of one schema — the **platform slot**
(`urn:pack:platform`):

- **the pack's half** — `resources` (requests and limits: cgroups, the same
  whether Kubernetes, horchestra or a VM applies them), additive `env`,
  `metadata` (labels, annotations). Its schema is the pack's
  (`sdk/platform/platform_v1.json`); the author gives defaults, the pack
  reads it.
- **the target's half** — how many instances and where: `replicas`,
  `placement`, whatever the target's scheduler has words for. The
  environment is deployed before pack comes; pack schedules nothing. The
  provider gives this half's schema (`pack schema platform --target
  horchestra` prints the two joined) and its values for each key, from its
  environment.

`sdk.Workload` carries the pack's half together with the `scale` fact; the
`default` tag turns your default into the key's default — resources, never
replicas or placement:

```go
Workload: sdk.Replicated("1", "2Gi"),      // or sdk.Single("500m", "512Mi").Limits("1Gi")
```

Formulas do not read `platform` itself. What a formula may know of it is
**`limits`** — the pack's `resources.limits`, `limits.memory` in bytes,
`limits.cpu` in millicores, absent where there is no limit
(`has(limits.memory) ? limits.memory / 1048576 * 3 / 4 : 1024`). Nothing a
formula computes can change the sizing.

A **library** has no `platform` (and no `ports`, no `x-pack.scale`): it never runs
on its own.

## 4. Claims — the volumes

```go
type Claims struct {
    Data packschema.Claim `json:"data"`
}
// packschema.Claim:
//   MountPath    string `json:"mountPath" pack:"const"`
//   Size         string `json:"size" pack:"default"`
```

Each claim becomes a `pv` volume at `mountPath`. The path is the image's
fact; the size defaults from `Defaults()` and the operator overrides size
in the values (`claims: {data: {size: 4Ti}}`).

## 5. Files

A subtree becomes a file with the `file=` tag on the field that holds it:

```go
Conf Conf `json:"conf" pack:"file=/opt/kafka/config/server.properties,format=properties"`
```

Everything under `conf` is serialised by the `properties` emitter into that
path. The subtree is the file — there is no separate pointer to keep in
sync. Options:

| option | meaning |
|---|---|
| `format=` | `json`, `yaml`, `properties`, `env`, `ini`, `flagfile`, `hadoop-xml`, `text`, `template` — see [emitters.md](emitters.md) |
| `template=` | for `format=template`: the file name under `templates/` |
| `parseAs=` | for `format=template`: the format the result must parse as; mandatory — a template is the one emitter that can produce garbage |
| `mode=` | octal file mode, default `0400` (owned by the workload's uid); use `0500` for scripts |
| `perInstance` | one file per replica — see §9 |

Rules the checks enforce: the path is absolute and clean; it is not in a
directory the node owns (`/etc/resolv.conf`, `/etc/passwd`, `/etc/group`,
`/etc/hosts`, `/etc/ssl`, `/etc/pki`, `/proc`, `/sys`, `/dev`,
`/run/secrets`); no file inside another file's subtree; one path, one file.

A subtree that ends up with **no values at all** (no defaults, nothing from
the operator) produces no file — a section nobody filled is a section that
does not exist.

Files in one directory are grouped into one Secret and mounted together
over the image's directory (`merge: true`); see [output.md](output.md#config-secrets).

### Templates

```go
Jaas Jaas `json:"jaas" pack:"file=/opt/kafka/config/kafka_jaas.conf,format=template,template=jaas.tmpl,parseAs=properties,mode=0400"`
```

`templates/jaas.tmpl` is a Go `text/template`. Its data is the subtree's
fields at the top level, plus `values` (the whole component subtree) and
`pack` (the context, see [values.md](values.md#the-pack-context)):

```
KafkaServer {
  org.apache.kafka.common.security.scram.ScramLoginModule required
  username="{{ .username }}"
  password="{{ .password }}";
};
```

For a file whose node is a scalar, the data has it under `value`. Missing
keys are errors (`missingkey=error`); there are no functions beyond the
built-in ones; the rendered text is parsed back as `parseAs` and refused if
it does not parse.

## 6. argv

The command line is the image's `Entrypoint` + `Cmd` (from `image.json`),
followed by every leaf tagged `arg=`, in the order the fields are declared:

```go
MinInsyncReplicas int `json:"min.insync.replicas" pack:"default,arg=--override,prefix=min.insync.replicas="`
// → --override min.insync.replicas=2
```

| tag | argv |
|---|---|
| `arg=--zk` | `--zk`, `<value>` (two elements) |
| `arg=--zk=` | `--zk=<value>` (a flag ending in `=` or `:` joins) |
| `arg=--override,prefix=k=` | `--override`, `k=<value>` |
| `arg=--dir,each` | the flag repeated per element of a list |
| `…,omitEmpty` | no value (or a formula that returned null) — no argument; this is the only "if" there is |

Do not put a **password** on an `arg=` leaf: `/proc/<pid>/cmdline` is
world-readable, and the check refuses it. Use a file (`flagfile` for gflags
applications) or `env=`.

To replace the image's entrypoint entirely, set `Command` in `PackRoot()`.

## 7. Environment

```go
HeapOpts string `json:"heapOpts,omitempty" pack:"env=KAFKA_HEAP_OPTS"`
Name     string `json:"name" pack:"default,env=POSTGRES_USER"`
Password packschema.Secret `json:"password" jsonschema:"required" pack:"env=POSTGRES_PASSWORD"`
```

A leaf tagged `env=` lands in `spec.env`. A string becomes `value`; a
`packschema.Secret` (a `{secretRef: {name, key}}`) becomes `secretRef` —
the author writes the same tag either way, the type decides. The operator
may add more variables under `platform.env`, but not one the component
declares.

## 8. Secrets

`packschema.Secret` is the type of a credential leaf. It reflects as
`{"type": "string", "format": "password"}`, and the tool gives that format
its meaning:

- in the values document only a reference is accepted:
  `password: {secretRef: {name: prod-kafka-sasl, key: password}}` — a
  plaintext string is refused by the validator;
- under an `env=` leaf it becomes `env[].secretRef`;
- inside a file subtree it is rendered as the placeholder
  `${secret:<name>/<key>}`, and the secret's name is listed in the mount's
  `substitute`; the node resolves it at materialisation — the tool never
  sees the value;
- a formula may not read it, and an `arg=` may not carry it.

Every password leaf must be reachable: under a file subtree or on an `env=`
leaf. One with nowhere to go is a check failure.

Prefer, when the application allows it, keeping the secret in its **own**
file and pointing the config at the path (`PGPASSFILE`,
`--ldap_bind_password_cmd`): then no placeholder is needed at all. Kafka's
JAAS cannot, so it gets the placeholder.

## 9. Per-instance files and the identity of a replica

Replicas of a component carry a byte-identical spec. The only thing that
differs per replica is what the target gives each instance — on horchestra
the child's name, `HORCHESTRA_INDEX`, `HORCHESTRA_ORDINAL`,
`HORCHESTRA_INSTANCE` in the environment, and `${horchestra.index}` in a
secret mount's item key.

For a file that must differ per replica (ZooKeeper's `myid`), mark it
`perInstance` and compute it from the context:

```go
MyID string `json:"myid,omitempty" pack:"file=/var/lib/zookeeper/myid,format=text,perInstance"`

func (ZooKeeper) PackCompute() map[string]string {
    return map[string]string{"/myid": "string(pack.ordinal)"}
}
```

The tool renders the file once per replica into keys `myid.0`, `myid.1`, …
of one Secret and mounts `myid.${horchestra.index}` as `myid` (horchestra's
way; another target delivers each replica its own bytes its own way). A
formula that reads `pack.instance`, `.index` or `.ordinal` is allowed **only**
under a `perInstance` file; anywhere else (argv, env, a shared file) the
check refuses it — that value would not survive the byte-identical rule.

`ordinal` is 1-based, `index` 0-based: ZooKeeper's `server.N` and `myid`
start at 1, a child's name suffix at 0, and an off-by-one here is not
diagnosed by anything — the ensemble forms, logs plausibly, and never
elects a leader.

## 10. Libraries

A library is a component without a workload: a JDK, a set of plugins. It
has layers (its `pack.yaml` mounts an image whose layers they are) and a
schema of settings. It never appears as a key of a composition; it lives
**inside its consumer**:

```go
JDK jdk.JDK `json:"jdk" pack_ref:"jdk" pack:"mount=/opt/java"`

func (Kafka) PackRoot() packschema.Root {
    return packschema.Root{Requires: map[string]string{"jdk": ">=17"}}
}
```

- `pack_ref:"jdk"` references the library **by name** (the property is
  `{"$ref": "pack://jdk#"}`); the composition pins which version (one
  version per pack).
- `mount=` says where its layers go: an image volume with `merge: true`.
- `Requires` declares the version constraint; the check insists the two
  agree (a reference without `requires`, or vice versa, is refused).
- the library's values are a subtree of the consumer's
  (`kafka.jdk.heapMB`), and the library's **formulas run in the consumer's
  environment**: `limits` are Kafka's, `values` is the `jdk` subtree. So
  Kafka and ZooKeeper sharing one JDK get two `heapMB`s, each from its own
  memory limit.
- a library's files render into the consumer's Secrets by their own paths.

A library's own schema must not declare `platform`, `ports` or `x-pack.scale`, and
its formulas may not read `peers`.

## 11. Neighbours

```go
func (Kafka) PackRoot() packschema.Root {
    return packschema.Root{
        Peers: map[string]packschema.Peer{"zk": {Schema: "zookeeper", Required: true}},
    }
}
```

`Peers` names the **roles** a component needs and the **type** of component
each role wants (the `io.horchestra.name` of the neighbour). Who actually
fills the role is the composition's decision; when the composition holds
exactly one component of that type, the role binds itself. Formulas then
see the neighbour by names:

```
peers.zk.instances.map(h, h + ':' + string(peers.zk.ports.client)).join(',')
```

`Required: false` means the component works without the neighbour; a
formula should then guard with `has(peers.zk)`.

A component with no required roles can be installed alone
(`pack template --pack examples/zookeeper`); one with a required role only
through a composition that binds it.

## 12. Formulas

```go
func (Kafka) PackCompute() map[string]string {
    return map[string]string{
        "/conf/zookeeper.connect": "peers.zk.instances.map(h, h + ':' + string(peers.zk.ports.client)).join(',') + '/' + pack.installation",
        "/conf/listeners":         "'PLAINTEXT://:' + string(values.ports.broker) + ',INTERNAL://:' + string(values.ports.internal)",
        "/heapOpts":               "values.jdk.heapOpts",
    }
}
```

A formula fills a leaf **only when the operator did not write it**; an
explicit value always wins, and removing it brings the formula back. The
environment ([values.md](values.md#the-cel-environment) has the full table):

| variable | what it is |
|---|---|
| `values` | this component's subtree after defaults and the formulas already settled; for a library's formula, the library's subtree |
| `limits` | the target's facts about the key's limits: `memory` (bytes), `cpu` (millicores) |
| `common` | the composition's `common` |
| `me` | `service`, `instances[]`, `replicas`, `ports{}` — names, never addresses |
| `peers.<role>` | the same about a bound neighbour |
| `peers.<role>.values` | the neighbour's own values: defaults, the operator's, its settled formulas; no password leaves, no per-replica leaves — see [values.md](values.md#a-neighbours-values) |
| `pack` | `namespace`, `installation`, `component`, `service`, `replicas`, `keys` (every key as `me`, plus `scaled`; binds nothing), `values` (every key's values, [binds nothing](values.md#every-keys-values)); per instance (§9) also `instance`, `index`, `ordinal` |

Functions: the CEL standard library, the `strings`, `lists` and `math`
extensions, and two of the tool's own:

- `quantity("32Gi")` → thousandths of a unit: `32·2³⁰·1000`;
  `quantity("500m")` → `500`. So MiB from a memory limit is
  `quantity(x) / 1000 / 1048576`.
- `indexMap(list, from)` → `{"from": list[0], "from+1": list[1], …}` — the
  shape of quorum sections like `server.N=…`.

Rules:

- returning `null` means "I don't know": the leaf keeps its default or stays
  absent — `cond ? value : null` is the idiom. `pack check` types the
  formula by its other branch: `string(…)` into an `integer` node, or
  `1 + 'a'`, is refused there; `values.x` alone is dynamic and checked at
  render.
- a formula may read another computed leaf (`values.jdk.heapOpts` above);
  the tool orders them by their reads and refuses a cycle.
- no clocks, no randomness, no I/O: rendering the same inputs twice must
  yield the same bytes, or a no-op `apply` would restart workloads.
- a formula must not read a password leaf (the derived value would be
  stored), and may read the instance identity only under a `perInstance`
  file.

## 13. Readiness, upgrade, hooks

```go
func (Postgres) PackRoot() packschema.Root {
    return packschema.Root{
        Readiness: &packschema.Readiness{TCP: "pg"},
        Upgrade:   &packschema.Upgrade{From: ">=15"},
        Hooks: []packschema.Hook{{
            Name: "init-extensions", Phase: "postInstall",         // once, after the first start
            Command: []string{"/opt/pack/init-extensions.sh"}, Idempotent: true,
        }, {
            Name: "pg-upgrade", Phase: "preUpgrade",
            When:      &packschema.When{From: "<16", To: ">=16"},
            Image:     "new",
            Command:   []string{"/usr/lib/postgresql/16/bin/pg_upgrade", "…"},
            Exclusive: true, Timeout: "30m",
            Compensate: &packschema.Step{Image: "old", Command: []string{"/opt/pack/restore-15.sh"}},
        }},
    }
}
```

- `Readiness` — `{TCP: "<port name>"}`, `{HTTP: "<port name>/<path>"}` or
  `{Exec: [...]}`; rendered into `spec.readiness` by port number. Without
  it the image's `HEALTHCHECK` applies, and without that "the process is
  running" is all the set knows.
- `Upgrade.From` — the versions this one can be upgraded from.
- `Hooks` — steps the component contributes to its own installation
  (`preInstall` / `postInstall`, once around the first apply: a format, a
  schema bootstrap) and upgrade (`preUpgrade` … `postDowngrade`, around a
  version transition); see [saga.md](saga.md).

## 13a. The SDK

The pieces every component has — the scale fact, the platform half, claims,
secrets — and the artifact's identity are ready in `pack/sdk`: embed
`sdk.Workload`, give `Meta()`, and `sdk.Generate` writes
`schema.json`, `pack.yaml` and `templates/`; `sdk.Verify` runs these checks
in a test; `sdk.Run` is a generator CLI. The same description in Python or
Java (`sdk/python`, `sdk/java`) yields the same schema — see
[../sdk/README.md](../sdk/README.md).

## 14. The component directory

```
kafka/
  kafka.go          the Go source (anywhere, really — it only has to produce schema.json)
  schema.json       generated
  templates/        one file per template, referenced by file name
  image.json        the image's OCI config: {"config": {"Entrypoint": …, "Cmd": …, "Env": …}, …}
  pack.yaml         the oci-packer pack file — the tool reads name, version and the image reference from it
```

`image.json` is optional: without it argv is just `x-pack.command` plus the
`arg=` leaves, and the `-flagfile=` check sees only those. Get it with
`docker image inspect <ref> | jq '.[0]'` (the `Config` key is what matters)
or from the image's config blob.

`pack.yaml` doubles as the source of `spec.image` when rendering from a
directory: its `oci://<repo>:<tag>` item — no host, the image is in the
pack's registry — is the reference the output carries (`image.ref` beside it
is the fallback). Deployed from the bundle's reference, the image is the
member the bundle carries, `<host>/<repo>@<digest>`.
[artifacts.md](artifacts.md) describes the file.

## 15. `pack check`, item by item

`pack check <dir>` runs the author checks; the loader repeats them at
acceptance, so an author who skipped them is refused later with the same
messages. Each problem is `path: sentence`.

| # | check | what to do when it fires |
|---|---|---|
| 1 | the defaults document passes the schema; `const` and `default` on one node | fix `Defaults()`; a field is a fact or a setting, not both |
| 2 | every formula parses and type-checks; where it fixes a type (`string(…)`, `.join()`, `+ 1`) that type fits the node's `type`; its reads are static (no `values[x]`); no cycle among formulas; a library reads no `peers`; instance identity only under `perInstance` | rewrite the formula; move an instance-dependent value into a `perInstance` file |
| 3 | no `arg=` on a password; no formula reads a password | use a flagfile or `env=`; never derive from a secret |
| 4 | every password is under a file or on an `env=` | give the credential a place |
| 5 | `-flagfile=<p>` in argv matches an `x-pack-file` of format `flagfile` | make the paths agree; the check needs `image.json` or `Command` to see argv |
| 6 | file paths are absolute, clean, unique, outside node-owned directories; `format` is an emitter (or `secret`, on a password leaf, without template/parseAs); `template` exists and has `parseAs`; `mode` is octal; `perInstance` only with `scale: replicas` | fix the tag |
| 7 | readiness names a declared port; `ports.*` are `const`; `x-pack.scale` ∈ single, replicas on every workload (the old `scale` leaf refused); `restartPolicy` ∈ Always, OnFailure, Never and `Never` not with replicas; a library declares none of them | fix `PackRoot()` / the struct |
| 8 | every `pack://<lib>#` is in `Requires` and vice versa; a library key has `mount=` | keep the two in step |
| 9 | hooks: phase, `when` constraints, `image` ∈ new, old, a command, `exclusive` only with an image, `compensate` with a command; an install hook has no `exclusive`, `when.from` or `image: old`; `upgrade.from` parses | fix the hook |
| 10 | every `x-pack-*` annotation decodes strictly into its type | a misspelled key (`fromat`) is refused here rather than ignored forever |

What it looks like — a schema with a file in `/etc`, a missing template and a
password on an argument:

```
$ pack check ./kafka
/conf: x-pack-file.path "/etc/passwd" is owned by the node: the file would silently lose
/jaas: template "jaas.tmpl" is not among the component's templates ()
/jaas/password: x-pack-arg on a password: /proc/<pid>/cmdline is world-readable — use a flagfile or x-pack-env
pack: 3 problem(s)
```

What the check does **not** do: know the type of a bare `values.x` read
(every variable is a dynamic map, so only what the formula itself fixes is
checked), or verify that the config the emitters produce is one the
application accepts — for that, feed the rendered file to the binary in a
container (`postgres -C`, `nginx -t`).

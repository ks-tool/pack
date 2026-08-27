# Vocabulary reference

The tool's keywords all start with `x-pack`. A JSON Schema validator ignores
them; the tool decodes each one **strictly** — an unknown key inside an
annotation is refused rather than ignored.

## `x-pack` on a component root

```json
"x-pack": {
  "readiness": {"tcp": "broker"},
  "peers":     {"zk": {"schema": "zookeeper", "required": true}},
  "requires":  {"jdk": ">=17"},
  "upgrade":   {"from": ">=3.0"},
  "hooks":     [ … ],
  "command":   ["/opt/app/bin/start"],
  "restartPolicy": "Never"
}
```

| field | type | meaning |
|---|---|---|
| `readiness` | `{tcp: <port name>}` \| `{http: "<port name>/<path>"}` \| `{exec: [string]}` | how a child is known to be ready |
| `peers` | `{<role>: {schema: <component name>, required?: bool}}` | roles the component needs and the type each wants |
| `requires` | `{<library name>: <constraint>}` | libraries and versions; must match the `pack://` keys |
| `upgrade` | `{from: <constraint>}` | versions this one can be upgraded from |
| `hooks` | `[Hook]` | steps of the upgrade saga — see below |
| `command` | `[string]` | replaces the image's entrypoint + cmd wholesale |
| `restartPolicy` | `Always` (default), `OnFailure`, `Never` | `Never` makes the component a run-to-completion step of the set (a bootstrap, a schema migration): it runs once, in order, and the set waits for it to finish instead of to become ready; not with `scale: replicas` |

Version constraints: a comma- or space-separated conjunction of
`>=`, `>`, `<=`, `<`, `=`, `!=` followed by a version (`>=15`, `<16`,
`>=21, <22`); empty or `*` matches anything.

### Hook

```json
{"name": "pg-upgrade", "phase": "preUpgrade",
 "when": {"from": "<16", "to": ">=16"},
 "image": "new", "command": ["…"],
 "exclusive": true, "timeout": "30m", "idempotent": false,
 "compensate": {"image": "old", "command": ["…"]}}
```

| field | meaning |
|---|---|
| `phase` | `preInstall`, `postInstall` (once, around the first apply of the key), `preUpgrade`, `postUpgrade`, `preDowngrade`, `postDowngrade` (around a version transition) |
| `when` | `{from?, to?}` constraints on the component version being left / arrived at; absent — on any image change; an install hook may constrain `to` only |
| `image` | component hook: `new` (default) or `old` — whose image runs it; composition hook: the name of the member whose image runs it |
| `command` | argv of the hook |
| `exclusive` | the component is stopped; the hook gets its volumes, values and secrets (not on an install hook: nothing runs yet) |
| `timeout` | how long before the step counts as failed |
| `idempotent` | `false` (default): a marker guards against a second run |
| `compensate` | `{image?, command}` — how to undo; without it the step is irreversible |
| `for` | composition hooks only: the keys the hook is about |

## `x-pack` on a composition root

```json
"x-pack": {"savepoints": ["zookeeper", "kafka"], "hooks": [ … ], "rollout": {"maxUnavailable": 1}}
```

## `x-pack` on a composition key

```json
"kafka": {"$ref": "pack://kafka#", "x-pack": {"spread": true, "peers": {"zk": "zookeeper"}, "after": ["init"], "minimum": {"memory": "4Gi"}}}
```

| field | meaning |
|---|---|
| `peers` | `{<role>: <key>}` — only needed when the role's type is ambiguous |
| `after` | keys this one must follow beyond what the peers graph implies |
| `spread` | replicas on different hosts |
| `minimum` | `{<resource>: <quantity>}` lower bound on `platform.resources.requests` |

## `x-pack-file` on a subtree

```json
{"path": "/opt/kafka/config/server.properties", "format": "properties",
 "template": "…", "parseAs": "…", "mode": "0400", "perInstance": false}
```

The subtree, whole, becomes the file. `format` ∈ `json`, `yaml`,
`properties`, `env`, `shell`, `ini`, `flagfile`, `lines`, `hadoop-xml`,
`text`, `template` — or `secret`, which renders nothing: the leaf is
`format: password` and the referenced Secret's key is mounted at the path as
it is. `template`/`parseAs` only with `template`; `mode` octal;
`perInstance` needs `scale: replicas`.

## `x-pack-arg` on a leaf

```json
{"flag": "--override", "prefix": "min.insync.replicas=", "each": false, "omitEmpty": false}
```

## `x-pack-env` on a leaf

```json
"x-pack-env": "KAFKA_HEAP_OPTS"
```

## `x-pack-compute` on a leaf

```json
"x-pack-compute": "peers.zk.instances.map(h, h + ':2181').join(',')"
```

A CEL expression; see [values.md](values.md#the-cel-environment).

## `x-pack-mount` on a library key

```json
"jdk": {"$ref": "pack://jdk#", "x-pack-mount": "/opt/java"}
```

## `format: password`

A standard keyword the tool gives meaning to: the leaf holds a credential,
only `{secretRef: {name, key}}` is accepted, and the leaf is rendered as a
placeholder or an env `secretRef`.

## The vocabulary as a schema

`pack schema vocabulary` prints `horchestra://pack/v1`: every annotation
above as a `$defs` entry (`root`, `key`, `file`, `arg`, `hook`, `readiness`,
`peer`, `upgrade`, `when`, `step`, `rollout`, `claim`) reflected from the Go
types the tool decodes them into, plus `secretRef`. It is what an author on
another language validates annotations against; `pack schema platform`
prints the platform half's schema the same way.

## Foreign references

| `$ref` | resolves to |
|---|---|
| `#/$defs/<name>` | a definition in the same document |
| `horchestra://platform/v1#/$defs/component` | the platform half's schema, shipped with the tool |
| `horchestra://pack/v1#/$defs/secretRef` | the secret reference object (what a password leaf becomes in the validating view) |
| `pack://<name>#` | a component or library by name — a sibling directory at render time, a member of the pack in the artifact |

Other schemes (`http://`, `file://`) and `$dynamicRef` are refused: a
component is self-contained.

## Reserved keys

| key | owner |
|---|---|
| `platform` | the platform — the only key under a component whose schema is not the author's |
| `common` | the composition |
| `horchestra` | the tool's context; never in a schema, never in values |

## The `pack:"…"` tag

Comma-separated; `key=value` splits on the first `=`.

| item | on | effect |
|---|---|---|
| `const` | any field | the field's value in `Defaults()` becomes `const` |
| `default` | any field | … becomes `default`; on a `ref=` field, emitted without zero values |
| `pack_ref:"<name>"` (its own tag) | a struct field | a member of the pack by name: the property is `{"$ref": "pack://<name>#"}` — a library (with `pack:"mount=…"`) or a composition key (with `pack:"default,spread,…"`) |
| `ref=<uri>` | a struct field | a foreign schema by URI — the platform half inside `sdk.Workload`; the Go type only types the defaults |
| `mount=<path>` | with `pack_ref` | `x-pack-mount` |
| `file=<path>,format=<f>[,template=<t>][,parseAs=<f>][,mode=<octal>][,perInstance]` | a field | `x-pack-file` |
| `arg=<flag>[,prefix=<p>][,each][,omitEmpty]` | a leaf | `x-pack-arg` |
| `env=<NAME>` | a leaf | `x-pack-env` |
| `spread`, `peers=<role>:<key>;…`, `after=<key>;…`, `minimum=<res>:<qty>;…` | a composition key | `x-pack` on the key |

Formulas: `PackCompute() map[string]string`, JSON Pointer → CEL. Root
annotation: `PackRoot() packschema.Root`.

Standard keywords come from `jsonschema:"…"` tags (`invopop/jsonschema`):
`minimum=`, `maximum=`, `enum=a,enum=b`, `required`, `title=`,
`description=`, `pattern=`, `format=`.

## Go types with schema meaning

| type | schema | use |
|---|---|---|
| `sdk.Workload` | `scale` (const) and `platform` (`$ref` to the platform half with a default) | embed it; `sdk.Single(cpu, mem)`, `sdk.Replicated(n, cpu, mem)` |
| `sdk.Platform` | the platform half's value type | the `default` under `platform` |
| `packschema.Claim` | `{mountPath: const, size: default, storageClass}` | one volume |
| `packschema.Secret` | `{type: string, format: password}` | a credential leaf |

# The values document and the effective document

## What the operator writes

One YAML file, one tree, shaped by the composition's effective schema:

```yaml
values:
  common: {realm: CORP.EXAMPLE}
  zookeeper: {}
  kafka:
    platform:
      resources: {requests: {cpu: "8", memory: "32Gi"}, limits: {memory: "32Gi"}}
    claims: {data: {size: 4Ti}}
    conf: {min.insync.replicas: 2}
    jaas: {password: {secretRef: {name: prod-kafka-sasl, key: password}}}
  ranger-db:
    superuser: {password: {secretRef: {name: prod-ranger-db, key: password}}}
rollout: {maxUnavailable: 1}
```

- Under each key, `platform` is the **platform half**: the pack's —
  `resources` (cgroups), additive `env`, `metadata` — and the target's —
  `replicas`, `placement` and the like — which the environment gives through
  its provider; the horchestra environment of adh
  (`examples/adh/envs/horchestra.yaml`) says `platform: {kafka: {replicas: 3,
  placement: {queue: data-prod}}}`. Values may set the target's half too; the
  environment's goes over it. Everything else is the **application half**,
  shaped by the component.
- A key named as `{}` is selected and writes nothing: its defaults and
  formulas stay.
- A library's settings sit under its key inside the consumer (`kafka.jdk.gc`).
- A credential is only ever a reference, `{secretRef: {name, key}}`; a
  plain string is refused.
- `rollout` overrides the composition's default.
- The installation name and the namespace are **not** in the document: they
  are `--name` and `--namespace`, so one document installs in `dev` and in
  `prod` alike.

The minimal document is empty: every component gets its defaults, and only
the fields marked `required` (typically credentials and names) must be
supplied — the validator lists them.

## How the effective document is built

```
1. defaults(effective schema)    the composition's, merged through every $ref
2. computed                      formulas, for leaves the operator did not write
3. values                        the operator's document — always on top
4. horchestra                    the context, read-only
```

Objects merge deeply; **arrays are replaced whole** (merging would need an
identity key nobody declared); `null` is a value, not a deletion — to get a
default back, remove the key.

The validator then checks the **effective** document: a `required` leaf
closed by a default or a formula passes; a `const` the operator overrode
does not; a password written as a string does not.

### Defaults

The schema's `default`s and `const`s are gathered into a document (a node's
default is taken whole; an object gathers its properties'; `allOf` branches
merge and conflict on a shared path; arrays contribute only an explicit
default; `anyOf`/`oneOf`/`if` contribute nothing). A `default` on a `$ref`
key — a composition key's — merges over the target's defaults.

### Formulas

Every `x-pack-compute` in the composition is collected with the subtree
`values` means for it (the key, or a library under it). Formulas whose leaf
the operator wrote are skipped. The rest run in an order where every
`values.*` a formula reads is already settled — a mutual wait is refused as
a cycle. A result of `null` leaves the leaf as it was. Per-instance formulas
(those reading `pack.instance`/`index`/`ordinal`) are not run here;
they run once per replica when the `perInstance` file is rendered.

## The CEL environment

| variable | contents |
|---|---|
| `values` | the formula's subtree: the key's, after defaults and settled formulas (for a library's formula, the library's subtree) |
| `limits` | what the target tells of the key's limits: `memory` in bytes, `cpu` in millicores; absent where there is none |
| `common` | the composition's `common` |
| `me` | `{service, instances, replicas, ports}` of the key |
| `peers.<role>` | the same for the key the role is bound to; absent when an optional role is unbound |
| `pack` | the context below |

`me.service` is `<installation>-<key>`; `me.instances` are
`<service>-<i>.<service>.<namespace>` for `x-pack.scale: replicas`, or
`<service>.<namespace>` for `single`; `me.ports` are the component's
`const` ports by name. Names, never addresses: they are stable under any
network mode, and they are known before anything is scheduled.

Functions: CEL standard, `strings`/`lists`/`math` extensions,
`quantity(s) → int` (thousandths of a unit: `quantity("32Gi") = 32·2³⁰·1000`,
`quantity("500m") = 500`), `indexMap(list, from) → map` (`{"1": l[0], "2": l[1], …}`).

`cond ? value : null` is the idiom for "I don't know". `pack check`
type-checks the formula with that branch dropped, and compares the type it
fixes — `string(…)` is a string, `quantity(…) / 2` an integer, `.join(',')`
a string — with the node's `type`. Every variable is a dynamic map, so a
bare `values.x` fixes nothing; its type error surfaces at render, with the
pointer and the formula's text.

## The `pack` context

Where and as what the key is installed — the same for every target; names
and the namespace are the target's (horchestra's below). Before 2026-10 it
was called `horchestra`.

```yaml
pack:
  namespace: data
  installation: prod
  component: kafka
  service: prod-kafka
  replicas: 3
  # every key of the installation, as `me` describes it, plus scaled —
  # whether its instances are replicas (a perInstance Secret key carries the
  # index); reading it binds no peer and orders nothing
  keys:
    zookeeper: {service: prod-zookeeper, instances: [prod-zookeeper-0, …], replicas: 3, ports: {client: 2181}, scaled: true}
    kafka: {…}
  # only while rendering a perInstance file, once per replica:
  instance: prod-kafka-0
  index: 0        # 0-based: the child's name suffix
  ordinal: 1      # 1-based: ZooKeeper's server.N and myid
```

`keys` is for the one key that has to name every host before any of them
runs — a CA issuing their certificates, a realm's plan of their principals.
A peer could not: a key comes after the keys its roles are bound to, and
those hosts need what it issues first. Anything that talks to a neighbour
binds it as a peer.

## Credentials the tool creates

A credential is normally a reference to a Secret somebody made beforehand —
an operator writing a Vault pointer, an earlier `POST secrets`. When there
is no such Secret and the tool is asked to make one, it needs the value from
somewhere, and it takes it only for the leaves it is told to:

```sh
pack template --pack examples/adh --values values.yaml --name prod \
  --secretProvider generate \
  --secret /kafka/jaas/password --secret /ranger-db/superuser/password
```

- `--secret <pointer>` is an **explicit allowlist**: a password leaf of the
  effective schema, by its values pointer. Any other leaf is untouched; a
  pointer that is not a password leaf, or one the values already reference,
  is refused.
- `--secretProvider` says where the value comes from:

  | provider | value |
  |---|---|
  | `generate` | 24 random bytes, base64url — new on every run, so keep the Secret the run wrote |
  | `env` | `$PACK_SECRET_<POINTER>` with `/` and `-` as `_`, upper-cased: `PACK_SECRET_KAFKA_JAAS_PASSWORD` |
  | `file:<dir>` | `<dir>/<pointer with dots>`: `<dir>/kafka.jaas.password`, trailing newline dropped |

For each pointer the tool writes a `Secret` named
`<installation>-<pointer with dashes>` (`prod-kafka-jaas-password`, type
`Opaque`, one key named after the leaf) next to the config Secrets, and
puts the reference into the values before rendering — the render then sees
a reference like any other. A Vault is never read: its pointers are the
operator's to create ahead of time.

## What the operator may not do

- override a `const` (a port, a mount path) — refused;
- write a credential as a string — refused;
- add a key no component provides — refused (`additionalProperties: false`);
- add an env variable the component itself declares — refused;
- change what a formula computes by any route other than writing the leaf
  — writing it is the way.

## Errors

Validation reports every violation at once, `path: message`, sorted:

```
$ pack template --pack examples/adh --values bad.yaml --name x
pack: values do not validate:
  /hive-db/superuser: missing property 'password'
  /kafka/jaas/password: got string, want object
  /kafka/platform/resources/requests/memory: 2Gi is below the composition's minimum 4Gi
  /ranger-db/superuser: missing property 'password'
```

A formula that fails reports the pointer, the formula text and CEL's
message; a cycle names every formula still waiting.

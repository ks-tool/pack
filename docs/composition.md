# Composing components into a system

A composition (a **pack**) is also a JSON Schema, also generated from Go.
Its properties are the keys of the system; each is a `$ref` to a component
by name; the `x-pack` annotations carry what no component can know about the
system it lands in.

## The Go source

```go
package adh

type ADH struct {
    Common    Common              `json:"common"`
    Zookeeper zookeeper.ZooKeeper `json:"zookeeper" pack_ref:"zookeeper" pack:"spread"`
    Kafka     kafka.Kafka         `json:"kafka" pack_ref:"kafka" pack:"spread,minimum=memory:4Gi"`
    RangerDB  postgres.Postgres   `json:"ranger-db" pack_ref:"postgres" pack:"default"`
    HiveDB    postgres.Postgres   `json:"hive-db" pack_ref:"postgres" pack:"default"`
}

type Common struct {
    Realm    string `json:"realm"`
    Timezone string `json:"timezone" pack:"default"`
}

func Defaults() ADH {
    return ADH{
        Common:   Common{Timezone: "UTC"},
        RangerDB: postgres.Postgres{Database: "ranger"},
        HiveDB:   postgres.Postgres{Database: "metastore"},
    }
}

func (ADH) PackRoot() packschema.Root {
    return packschema.Root{
        Savepoints: []string{"zookeeper", "kafka"},
        Rollout:    &packschema.Rollout{MaxUnavailable: 1},
    }
}
```

The field types are the components' Go types so that `Defaults()` is typed,
but the schema only carries the `$ref` — the component's schema is read from
its own directory at render time.

## Keys

- **A key is a property name.** The same component may appear under two
  keys (`ranger-db`, `hive-db` are both `pack://postgres#`); it is one
  version of Postgres, instantiated twice, each with its own values and its
  own children (`prod-ranger-db`, `prod-hive-db`).
- **`common`** is an ordinary object of the composition: what must agree
  across components (realm, timezone). Components read it as `common.*` in
  formulas and never write it.
- **`default` on a key** (the `default` tag) layers over the component's
  own defaults rather than replacing them — `Database: "ranger"` sets one
  field, Postgres keeps its ports, resources and everything else. Zero
  values in the Go struct are not emitted, so only what you set counts.

## Compositions of compositions

A key may name another composition instead of a component — a pack made of
packs (ADH of Ozone, Impala, …; a stand of ADH and ADPS):

```go
type Stand struct {
    ADH  adh.ADH   `json:"adh" pack_ref:"adh" pack:"after=kdc"`
    ADPS adps.ADPS `json:"adps" pack_ref:"adps"`
    KDC  kdc.KDC   `json:"kdc" pack_ref:"kdc"`
}
```

The system has one namespace of keys, so the nested composition is not a key:
**its keys take its place**, in its order, and the values stay flat
(`ozone-scm:`, never `adh: {ozone-scm: …}`). The loader reads a schema as a
composition when every property but `common` is a `pack://` reference and
nothing makes it a component.

- A key two compositions both hold (`zookeeper` under ADH and under ADPS) is
  **one key** when both name the same component; their annotations join —
  `after` together, `spread` if either, `peers` with no role bound two ways.
  Two components under one key are refused.
- What the nested composition says of the system comes along: its `common`
  (properties side by side, a property defined two ways is refused), its
  savepoints and hooks, its rollout where the outer has none.
- The key that names the composition may say only `after`, and it applies to
  each of the nested keys; `spread`, `peers` and `minimum` have no meaning for
  several keys at once and are refused.
- A composition containing itself is refused with the chain.

`pack-sdk` writes the pack file of such a composition with **every** member it
holds, nested compositions' members too, each once — the loader finds any of
them by name in the one index.

## Neighbours: binding roles

A component declares roles and the type each wants (`Peers` in its
`PackRoot`). The composition binds them:

- **automatically**, when exactly one key of the composition has the type
  the role wants. Kafka's `zk` role wants a `zookeeper`; the composition has
  one key referencing `pack://zookeeper#`; bound. Nothing to write.
- **explicitly**, when the type is ambiguous — two Postgres keys, and
  `ranger` needs one of them:

  ```go
  Ranger ranger.Ranger `json:"ranger" pack_ref:"ranger" pack:"peers=db:ranger-db"`
  ```

  Leaving an ambiguous role unbound is a check failure that lists the
  candidates; binding a role to a key of the wrong type, or a role the
  component does not declare, is refused too.
- a required role with **no** candidate is refused; an optional one stays
  empty (formulas guard with `has(peers.<role>)`).

## Order

The order of the set's applications — which is the order of rollout and of
upgrade — is derived: a key comes after every key its roles are bound to.
Kafka after ZooKeeper, without anyone writing it. `after=` adds edges the
peers graph does not have (an init step a service must wait for):

```go
Ranger ranger.Ranger `json:"ranger" pack_ref:"ranger" pack:"peers=db:ranger-db,after=ranger-schema"`
```

A cycle is refused with the chain that forms it. Among independent keys the
composition's own order is kept.

## Placement and sizing

- `spread` — the component's replicas must land on different hosts:
  rendered as a required anti-affinity on the set's reserved labels
  (`horchestra.io/application-set`, `horchestra.io/component`).
- `minimum=memory:4Gi;cpu:2` — a lower bound on `platform.resources.requests`
  for this key; the composition may demand more than the component asks for
  itself. The validator refuses values below it, with the numbers.

## Savepoints, hooks, rollout

`PackRoot()` of a composition carries:

- `Savepoints` — keys after which an upgrade's rollback does not go; see
  [saga.md](saga.md). Without them every component boundary is a savepoint.
- `Hooks` — system-level steps of the saga, with `For` naming the keys they
  are about (`maintenance-on` for `ranger`); a composition hook without
  `For` is refused.
- `Rollout` — the set's default `maxUnavailable`; the operator may override
  it in the values document.

## Directory layout

```
examples/
  adh/         schema.json (the composition), pack.yaml, testdata/
  zookeeper/   schema.json, templates/, pack.yaml
  kafka/       …
  postgres/    …
  jdk/         …
```

The composition's components are its **sibling directories**, found by the
name in `pack://<name>#`; libraries a component requires are found the same
way. `pack template --pack examples/adh` loads all of them.

## `pack check` on a composition

Runs every member's own checks, then the composition's:

| check | fires when |
|---|---|
| membership | a `pack://<name>#` names no sibling directory |
| roles | a required role has no candidate, an ambiguous one is not bound, a binding names the wrong type or an undeclared role |
| order | peers ⊕ `after` form a cycle |
| libraries | a `requires` is unmet, or the member's version (from its `pack.yaml`) is outside the constraint |
| savepoints, hooks | they name keys that do not exist; a hook has no `for` |
| minimum | the key's `minimum` is above what the component defaults to |

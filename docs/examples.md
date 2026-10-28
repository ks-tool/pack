# Examples: describing services

Recipes for the services you will meet most often. Each one is a Go value
([authoring.md](authoring.md) explains every tag); the full, compiling
sources are in `examples/`. Pick the recipe nearest your service and change
the facts (ports, paths, image) to yours.

| recipe | shape | source |
|---|---|---|
| [1. A database](#1-a-database-single-instance-with-a-volume) | one instance, a volume, a password, hooks | `examples/postgres` |
| [2. A quorum](#2-a-quorum-replicas-and-an-identity-per-instance) | replicas, a file that differs per instance | `examples/zookeeper` |
| [3. A service with a neighbour](#3-a-service-with-a-neighbour) | a required peer, a formula over its addresses | `examples/kafka` |
| [4. A stateless service](#4-a-stateless-service) | replicas, no volume, HTTP readiness, config as YAML | below |
| [5. A run-to-completion step](#5-a-run-to-completion-step) | a migration that runs once, in order | below |
| [6. A library](#6-a-library) | layers and settings, no workload | `examples/jdk` |
| [7. A system of them](#7-a-system-composing-the-services) | keys, binding, order | below, `examples/adh` |

Recipes 4, 5 and 7 are not in `examples/`; they were generated and checked
(`pack check`, `pack template`) while this page was written.

## 1. A database: single instance with a volume

Postgres: one instance, data on a volume, a superuser whose password is a
reference to a Secret, a config file, a hook after the first start.

```go
type Postgres struct {
    sdk.Workload                                              // scale and the pack's half of platform

    Ports     Ports     `json:"ports"`
    Claims    Claims    `json:"claims"`
    Database  string    `json:"database" jsonschema:"required"`
    Superuser Superuser `json:"superuser"`
    Conf      Conf      `json:"conf" pack:"file=/etc/postgresql/postgresql.conf,format=properties"`
}

type Ports struct {
    PG int `json:"pg" pack:"const"`                          // a fact about the image
}

type Claims struct {
    Data packschema.Claim `json:"data"`                      // a pv volume at mountPath
}

type Superuser struct {
    Name     string            `json:"name" pack:"default,env=POSTGRES_USER"`
    Password packschema.Secret `json:"password" jsonschema:"required" pack:"env=POSTGRES_PASSWORD"`
}

type Conf struct {
    MaxConnections int    `json:"max_connections" jsonschema:"minimum=1" pack:"default"`
    SharedBuffers  string `json:"shared_buffers,omitempty"`   // the operator's — or computed below
    ListenAddress  string `json:"listen_addresses" pack:"const"`
}

func Defaults() Postgres {
    return Postgres{
        Workload:  sdk.Single("500m", "512Mi"),
        Ports:     Ports{PG: 5432},
        Claims:    Claims{Data: packschema.Claim{MountPath: "/var/lib/postgresql/data", Size: "10Gi"}},
        Superuser: Superuser{Name: "postgres"},
        Conf:      Conf{MaxConnections: 100, ListenAddress: "*"},
    }
}

func (Postgres) PackRoot() packschema.Root {
    return packschema.Root{
        Readiness: &packschema.Readiness{TCP: "pg"},
        Hooks: []packschema.Hook{{
            Name: "init-extensions", Phase: "postInstall", Image: "new",
            Command: []string{"/opt/pack/init-extensions.sh"}, Idempotent: true, Timeout: "5m",
        }},
    }
}

func (Postgres) PackCompute() map[string]string {
    return map[string]string{
        "/conf/shared_buffers": "has(limits.memory) ? string(limits.memory / 1048576 / 4) + 'MB' : '128MB'",
    }
}
```

What each choice does:

- `const` for the port and `listen_addresses`: the image fixes them, so a
  different value is refused. `default` for `max_connections`: a setting.
- `Password` has no default and is `required`: the operator must name a
  Secret. It lands in `env` as a `secretRef`; the value never reaches pack.
- the `shared_buffers` formula fills the leaf only when the operator left it
  empty, and falls back to `128MB` where the target gives no memory limit.
- `Workload: sdk.Single(…)` — one instance, addressed as the key.

The operator's side:

```yaml
values:
  postgres:
    database: app
    superuser: {password: {secretRef: {name: pg, key: password}}}
    claims: {data: {size: 100Gi}}
```

## 2. A quorum: replicas and an identity per instance

ZooKeeper: replicas, a server list built from the instances' own names, and
a `myid` file whose content differs per replica.

```go
type ZooKeeper struct {
    sdk.Workload

    Ports    Ports   `json:"ports"`
    JDK      jdk.JDK `json:"jdk" pack_ref:"jdk" pack:"mount=/opt/java"`   // a library, §6
    Claims   Claims  `json:"claims"`
    Conf     Conf    `json:"conf" pack:"file=/conf/zoo.cfg,format=properties"`
    MyID     string  `json:"myid,omitempty" pack:"file=/var/lib/zookeeper/myid,format=text,perInstance"`
    HeapOpts string  `json:"heapOpts,omitempty" pack:"env=JVMFLAGS"`
}

func Defaults() ZooKeeper {
    return ZooKeeper{
        Workload: sdk.Replicated("500m", "1Gi"),
        Ports:    Ports{Client: 2181, Peer: 2888, Election: 3888},
        /* … */
    }
}

func (ZooKeeper) PackRoot() packschema.Root {
    return packschema.Root{
        Readiness: &packschema.Readiness{TCP: "client"},
        Requires:  map[string]string{"jdk": ">=11"},
    }
}

func (ZooKeeper) PackCompute() map[string]string {
    return map[string]string{
        // server.1=<instance-1>:2888:3888, server.2=…
        "/conf/server": "indexMap(me.instances.map(h, h + ':' + string(values.ports.peer) + ':' + string(values.ports.election)), 1)",
        "/myid":        "string(pack.ordinal)",   // allowed only because the file is perInstance
        "/heapOpts":    "values.jdk.heapOpts",
    }
}
```

- `sdk.Replicated` declares `x-pack.scale: replicas`; how many is the
  target's (`platform.replicas` on horchestra), not the author's.
- `perInstance` renders `myid` once per replica (`myid.0`, `myid.1`, …) into
  one Secret. `pack.ordinal` is 1-based: ZooKeeper's `server.N` and `myid`
  start at 1, and nothing diagnoses an off-by-one.
- `me.instances` are names, never addresses: the target resolves them.

## 3. A service with a neighbour

Kafka needs ZooKeeper. It names a **role** (`zk`) and the **type** it wants;
the composition decides who fills it.

```go
func (Kafka) PackRoot() packschema.Root {
    return packschema.Root{
        Readiness: &packschema.Readiness{TCP: "broker"},
        Peers:     map[string]packschema.Peer{"zk": {Schema: "zookeeper", Required: true}},
        Requires:  map[string]string{"jdk": ">=17"},
        Upgrade:   &packschema.Upgrade{From: ">=3.0"},
    }
}

func (Kafka) PackCompute() map[string]string {
    return map[string]string{
        "/conf/zookeeper.connect": "peers.zk.instances.map(h, h + ':' + string(peers.zk.ports.client)).join(',') + '/' + pack.installation",
        "/conf/listeners":         "'PLAINTEXT://:' + string(values.ports.broker) + ',INTERNAL://:' + string(values.ports.internal)",
        "/heapOpts":               "values.jdk.heapOpts",
    }
}
```

Two more things Kafka shows:

```go
// a password that has nowhere but a file: the placeholder ${secret:…} is rendered,
// the node substitutes it; the tool never sees the value
Jaas Jaas `json:"jaas" pack:"file=/opt/kafka/config/kafka_jaas.conf,format=template,template=jaas.tmpl,parseAs=properties,mode=0400"`

// a secret that is a whole FILE (a keystore): mounted as is, nothing rendered
type TLS struct {
    Keystore packschema.Secret `json:"keystore,omitzero" pack:"file=/opt/kafka/config/keystore.jks,format=secret,mode=0400"`
}

// a flag on the command line — fine for a setting, never for a password
MinInsyncReplicas int `json:"min.insync.replicas" pack:"default,arg=--override,prefix=min.insync.replicas="`
// → --override min.insync.replicas=2
```

## 4. A stateless service

An HTTP API: replicas, no volume, a YAML config, one secret in the
environment, a database it finds through a required neighbour.

```go
type API struct {
    sdk.Workload

    Ports Ports  `json:"ports"`
    Conf  Conf   `json:"conf" pack:"file=/etc/api/config.yaml,format=yaml"`
    Auth  Auth   `json:"auth"`
    Level string `json:"logLevel,omitempty" jsonschema:"enum=debug,enum=info,enum=warn" pack:"env=LOG_LEVEL"`
}

type Ports struct {
    HTTP int `json:"http" pack:"const"`
}

type Conf struct {
    Listen      string `json:"listen" pack:"const"`
    DatabaseURL string `json:"databaseUrl,omitempty"`           // computed from the neighbour
    Workers     int    `json:"workers" jsonschema:"minimum=1" pack:"default"`
}

type Auth struct {
    JWTKey packschema.Secret `json:"jwtKey" jsonschema:"required" pack:"env=JWT_KEY"`
}

func Defaults() API {
    return API{
        Workload: sdk.Replicated("250m", "256Mi"),
        Ports:    Ports{HTTP: 8080},
        Conf:     Conf{Listen: ":8080", Workers: 4},
    }
}

func (API) PackRoot() packschema.Root {
    return packschema.Root{
        Readiness: &packschema.Readiness{HTTP: "http/healthz"},     // <port name>/<path>
        Peers:     map[string]packschema.Peer{"db": {Schema: "postgres", Required: true}},
    }
}

func (API) PackCompute() map[string]string {
    return map[string]string{
        "/conf/databaseUrl": "'postgres://' + peers.db.instances[0] + ':' + string(peers.db.ports.pg) + '/app'",
    }
}
```

No `Claims`, so nothing is persistent and replicas are interchangeable; no
`perInstance`, so every replica's spec is byte-identical. The rendered
`config.yaml` has `databaseUrl: postgres://dev-db.apps:5432/app`.

## 5. A run-to-completion step

A schema migration: it runs once, after the database and before the
service, and the set waits for it to **finish**, not to become ready.

```go
type Migrate struct {
    sdk.Workload

    DBPassword packschema.Secret `json:"dbPassword" jsonschema:"required" pack:"env=PGPASSWORD"`
    Target     string            `json:"target,omitempty" pack:"env=SCHEMA_VERSION"`
}

func Defaults() Migrate { return Migrate{Workload: sdk.Single("100m", "128Mi")} }

func (Migrate) PackRoot() packschema.Root {
    return packschema.Root{
        RestartPolicy: "Never",                                   // not with replicas
        Command:       []string{"/opt/migrate/run.sh", "up"},      // replaces the image's entrypoint
        Peers:         map[string]packschema.Peer{"db": {Schema: "postgres", Required: true}},
    }
}
```

A step has no `ports` and no `readiness` — there is nothing to wait on but
its exit. `Never` together with `sdk.Replicated` is refused by `pack check`.

## 6. A library

A JDK: no workload of its own, layers that are mounted into the consumer and
two settings every JVM service needs. It never appears as a key of a
composition.

```go
type JDK struct {
    GC       string `json:"gc" jsonschema:"enum=G1,enum=ZGC" pack:"default"`
    HeapMB   int    `json:"heapMB,omitempty" jsonschema:"minimum=128"`
    HeapOpts string `json:"heapOpts,omitempty"`
}

func Defaults() JDK { return JDK{GC: "G1"} }

func (JDK) PackCompute() map[string]string {
    return map[string]string{
        "/heapMB":   "has(limits.memory) ? limits.memory / 1048576 * 3 / 4 : 1024",
        "/heapOpts": "'-Xmx' + string(values.heapMB) + 'm -Xms' + string(values.heapMB) + 'm -XX:+Use' + values.gc + 'GC'",
    }
}
```

The consumer says `JDK jdk.JDK `json:"jdk" pack_ref:"jdk" pack:"mount=/opt/java"``
and `Requires: {"jdk": ">=17"}` (§2, §3). The formulas run in the consumer's
environment, so Kafka and ZooKeeper sharing one JDK each get a `heapMB` from
their own memory limit.

## 7. A system: composing the services

Recipes 1, 4 and 5 as one system: a database, a migration after it, an API
after the migration.

```go
type Shop struct {
    DB      postgres.Postgres `json:"db" pack_ref:"postgres" pack:"default"`
    Migrate migrate.Migrate   `json:"migrate" pack_ref:"migrate" pack:"peers=db:db"`
    API     api.API           `json:"api" pack_ref:"api" pack:"peers=db:db,after=migrate"`
}

func Defaults() Shop { return Shop{DB: postgres.Postgres{Database: "app"}} }

func (Shop) PackRoot() packschema.Root { return packschema.Root{} }
```

- with exactly one `postgres` key the `db` role binds itself; `peers=db:db`
  is written here only to show the syntax (it is required when two keys
  have the same type, as `ranger-db` and `hive-db` in `examples/adh`).
- the order is derived: `migrate` after `db` (its peer), and `api` after
  both — `after=migrate` adds the edge the peers graph does not have.
- `default` on `db` layers `database: app` over Postgres's own defaults.

The directory:

```
shop/
  schema.json  pack.yaml          the composition
postgres/  api/  migrate/         siblings, found by pack://<name>#
```

`pack.yaml` of the composition lists the members' artifacts (`type:
application/vnd.horchestra.pack.v1+json`, one `oci://components/<name>:<tag>`
item per member); [artifacts.md](artifacts.md) has the file.

The operator's document — one tree shaped by the composition:

```yaml
values:
  db:
    superuser: {password: {secretRef: {name: shop-db, key: password}}}
  migrate:
    dbPassword: {secretRef: {name: shop-db, key: password}}
  api:
    auth: {jwtKey: {secretRef: {name: shop-jwt, key: key}}}
```

How many replicas `api` gets is not written here: that is the target's half
of `platform`, given by the environment (`examples/adh/envs/horchestra.yaml`).

```sh
pack check    ./shop
pack template --pack ./shop --values values.yaml --name dev --namespace apps -o out/
```

`out/applicationset.yaml` carries three applications in the order `db`,
`migrate` (`restartPolicy: Never`, the command above), `api`; the config
Secrets are `dev-db-etc-postgresql` and `dev-api-etc-api`. A larger system —
ZooKeeper, Kafka, two Postgres, a shared JDK — is `examples/adh`, with its
values in `examples/adh/testdata/values.yaml`.

## Mistakes the checks catch

What you will most likely write wrong the first time, and what `pack check`
says (see [authoring.md §15](authoring.md#15-pack-check-item-by-item)):

| mistake | the check |
|---|---|
| a password on `arg=` | `x-pack-arg on a password: /proc/<pid>/cmdline is world-readable` |
| a file in `/etc/passwd`, `/etc/hosts`, … | the path is owned by the node |
| `format=template` without `parseAs` | a template must say what its result parses as |
| `pack_ref` without `Requires` (or the reverse) | the two must agree |
| `perInstance` on a `single` component | `perInstance` only with `scale: replicas` |
| `pack.ordinal` in an env var or a shared file | instance identity only under a `perInstance` file |
| `restartPolicy: Never` with `Replicated` | not with replicas |
| readiness naming a port that is not declared | readiness names a declared port |

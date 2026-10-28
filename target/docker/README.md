# docker — Docker engines as a target

`--target docker` runs `pack-provider-docker`. A pack goes to one Docker engine, or to several hosts, each with its
own engine. The provider talks to every engine with the `docker` CLI. It needs no swarm, no SSH of its own and no
agent on the hosts: whatever endpoint `docker --host` / `--context` can reach will do (`unix://`, `tcp://` with or
without TLS, `ssh://`).

## Where settings come from

A setting is a key of the environment's set. The provider gets the whole set, built in this order, each source over
the one before:

1. the environment file (`--config env.yaml`, YAML or JSON), flattened: `{hosts: {a: {address: …}}}` is
   `hosts_a_address`, a list is `a,b,c`;
2. the Vault KV the file names (`vault.addr`, `vault.paths`, the token from `$VAULT_TOKEN`);
3. the command line, `--set <key>=<value>` (repeatable).

Keys are case-insensitive: they are lowered as they are read. `target`, `provider` and `vault.*` are pack's own; the
rest are the provider's.

## Settings

| Setting                  | Default                                   | Meaning                                                                                                                                                                                                                            |
|--------------------------|-------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `hosts.<name>.address`   | —                                         | How a neighbour reaches the host: an IP or a DNS name. Required for every host. Naming any host switches the target to [several hosts](#several-hosts).                                                                            |
| `hosts.<name>.host`      | —                                         | The host's engine, as `docker --host`: `ssh://user@host`, `tcp://host:2376`, `unix:///…`.                                                                                                                                         |
| `hosts.<name>.context`   | —                                         | The host's engine, as a `docker --context`, instead of `host`.                                                                                                                                                                   |
| `host`                   | docker's own (`$DOCKER_HOST`, the context) | One engine: its `docker --host`. Ignored when `hosts` are named.                                                                                                                                                                |
| `context`                | docker's own                              | One engine: its `docker --context`. Ignored when `hosts` are named.                                                                                                                                                              |
| `docker`                 | `docker` from `PATH`                       | The docker CLI binary. A provider run from an image has to carry one.                                                                                                                                                             |
| `network_subnet`, `network_gateway` | the engine's choice              | The network's IPv4 subnet and gateway, for keys with a fixed `ip`.                                                                                                                                                                  |
| `network`                | the installation's name                   | The user-defined bridge network created on each engine, labelled with the installation. On one engine, containers find each other on it by name.                                                                                 |
| `state`                  | the engine keeps it                       | A file that keeps the installation's record and the credentials pack provisioned — see [State](#state). `~/` is the home directory; a relative path is the provider's working directory's (the container's, for a provider run from an image). |
| `secrets.<name>.<key>`   | —                                         | The value `${secret:<name>/<key>}` and a `secretRef {name, key}` stand for, when it is neither a credential of the step nor one kept in the state. Best kept in Vault.                                                             |
| `compose`                | `false`                                   | `true`: `pack template` also writes a compose file per engine — see [Output](#output-of-pack-template).                                                                                                                           |
| `platform.<key>.…`       | —                                         | The target's half of a key's platform, over what the values say — see [Platform](#platform).                                                                                                                                      |

Any other key is refused before anything is rendered: a typo, or a setting of another target, never passes
silently. The whole set is checked — the file's keys, the Vault KV's (so a KV path read for this environment holds this
target's settings only) and `--set`'s — except pack's own `target`, `provider`, `vault.addr` and `vault.paths`. The
target's keys are `host`, `context`, `docker`, `network`, `network_subnet`, `network_gateway`, `state`, `compose`, `hosts.…`, `secrets.…`, `platform.…`. Within `platform.<key>.…`, a property the target has no word for is refused too.

## Platform

A key's platform has two halves. The pack's half is the same on every target: `resources`, `env`, `labels`,
`annotations`. The target's half below says how many instances run and where. It can come from the values
(`values: {<key>: {platform: {…}}}`) or from the environment (`platform: {<key>: {…}}`); the environment wins.

| Property   | Environment key                         | Meaning                                                                                                                                                                                                                               |
|------------|-----------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `replicas` | `platform.<key>.replicas`               | Instances of a `scale: replicas` key. On one engine it defaults to 1. Across hosts it defaults to the number of hosts, and any other number is refused. A `scale: single` key always runs one instance.                              |
| `hosts`    | `platform.<key>.hosts` (`[a, b, c]`)    | Across hosts: which hosts the key runs on, instance `i` on the `i`-th. Required for every key once the environment names hosts, and refused when it names none. A host may appear once per key, and a `scale: single` key names exactly one. |
| `name`, `hostname`, `aliases`, `ip`, `dns`, `user`, `groupAdd`, `sysctls`, `securityOpt` | `platform.<key>.<property>` | What a compose service says of its container: its name instead of `<installation>-<key>` (the key's address `<name>.<network>`), a hostname (dotted: split as compose does, and the key's address), more names on the network, a fixed address, name servers, `docker run`'s `--user`, `--group-add`, `--sysctl`, `--security-opt`. |
| `mounts`   | `platform.<key>.mounts` (`[a, b]`)      | `<source>:<target>[:<options>]`: a path of the host (starting with `/`, options as `docker -v` takes them), or a volume of the installation the keys that name it share, `<volume>[/<subpath>]` (option `ro`), created with its subpaths on first use and kept. |
| `restart`  | `platform.<key>.restart`                | The engine's restart policy instead of the pack's; `on-failure` on a key that runs to completion retries it, and the wait is for the run that exits 0. |
| `publish`  | `platform.<key>.publish.<port name>`    | On one engine: host ports by the component's port name (`{pg: 15432}`); refused for more than one replica. Refused across hosts, where every port is published as itself.                                                         |

`resources` become `--cpus` (`limits.cpu`), `--cpu-shares` (`requests.cpu` × 1024), `--memory` (`limits.memory`) and
`--memory-reservation` (`requests.memory`). Labels and annotations both become container labels.

## One engine

With no `hosts`, everything runs on one engine. A key becomes the container `<installation>-<key>`, or
`<installation>-<key>-<i>` for each replica of a `scale: replicas` key. Each replica also answers to
`<installation>-<key>` on the network. A neighbour reaches an instance at `<container>.<network>` and the component's
port, e.g. `prod-zookeeper-0.prod:2181`: the name the engine's resolver answers a reverse query with, and the one the
container calls itself (`--domainname`), so Kerberos makes one principal of it on both ends.

## Several hosts

When the environment names hosts, the operator decides where each key runs, and pack only follows: it schedules
nothing. Instance `i` of a key runs on the `i`-th of its `hosts`. A neighbour reaches the instance at
`<address>:<port>`: the host's address, an IP or a DNS name, and the component's own port, which the host publishes
under the same number. For example: `zookeeper.connect=10.0.0.11:2181,10.0.0.12:2181,node-c.dc1.example:2181/prod`.
Which network carries the traffic does not matter, as long as the hosts can reach each other's addresses.

Two containers that publish the same port on one host collide. `pack template` refuses this outright. `pack apply`
finds out when the engine does, and rolls the unit back. Two Postgres instances on 5432 therefore go to different
hosts.

A hook runs on the host of replica 0. The state, unless `state` is set, lives on the first host by name.

```yaml
target: docker
state: ~/.local/state/pack/adh-docker.json
hosts:
  a: {address: 10.0.0.11, host: ssh://ops@10.0.0.11}
  b: {address: 10.0.0.12, host: ssh://ops@10.0.0.12}
  c: {address: node-c.dc1.example, host: tcp://node-c.dc1.example:2376}
platform:
  zookeeper: {hosts: [a, b, c]}
  kafka:     {hosts: [a, b, c]}
  hive-db:   {hosts: [a]}
  ranger-db: {hosts: [b]}
```

([`examples/adh/envs/docker-hosts.yaml`](../../examples/adh/envs/docker-hosts.yaml); one engine:
[`docker.yaml`](../../examples/adh/envs/docker.yaml).)

The record keeps the platform each key was installed with. Moving a key to other hosts, or changing its replica count,
is therefore a change that `pack apply` performs: the instance starts on its new host and leaves the old one. Its
claims stay on the old host, so it starts there with empty volumes. `pack rollback` moves it back, even when the
environment no longer names the old hosts.

## What runs on an engine

- **Containers**: each is created with `--restart unless-stopped`, or `no` for a key that runs to completion. It is
  recreated only when what it runs changed — its arguments, env, files or secrets, all hashed into a label.
- **Config files**: they are kept in a volume of the container's own, `<container>-files-<hash>`, on its host. The
  container mounts them file by file over its image (`volume-subpath`, Docker ≥ 26), byte for byte, binary included.
  `${secret:…}` is resolved before the file is written. The volume is filled through a container of the same image
  that is never started, so it works through any endpoint. It is replaced when the files change, and removed with the
  key, since it holds secrets.
- **Claims**: named volumes `<claim>-<container>`, labelled with the installation. They outlive the key.
- **Publish volume**: a container that runs to completion, a key's or a hook's, mounts
  `<installation>-<key>-publish` at `/var/run/pack` — see [Publishing](#publishing). It stays, like a claim.
- **Libraries**: image mounts (`--mount type=image`, Docker ≥ 28).
- **Readiness**: a healthcheck, at the container's own name (a service bound to the address its name resolves to answers
  there, so does one bound to every address). A tcp probe runs `nc -z` or bash's `/dev/tcp`, and an http probe runs `curl` or `wget`,
  so the image has to carry one of them. An exec probe runs as given.
- **Secret env**: a variable whose value is a secret reaches `docker create` through the CLI's environment, never its
  argv.

## Steps

| Step   | What the provider does                                                                                                                                  |
|--------|---------------------------------------------------------------------------------------------------------------------------------------------------------|
| apply  | Creates or recreates the key's containers on their hosts, and removes those it no longer has: a replica beyond the count, an instance moved elsewhere. |
| wait   | Waits until every container runs and is healthy; a key that runs to completion, until it exits 0, and then keeps what it [published](#publishing). Logs are told when it fails or completes. |
| stop   | `docker stop --time 30` on the key's containers, on every host.                                                                                          |
| hook   | A container of the hook's image and command, with replica 0's files, env and libraries; an exclusive hook, and a `preInstall` one (before the first apply, the volumes still empty), also gets replica 0's claims. What it [published](#publishing) is kept when it exits 0. |
| remove | Removes the key's containers and files volumes on every host; the claims stay.                                                                           |

## Secrets

`${secret:<name>/<key>}` in a file, a `secretRef` in env, and a secret file are resolved when applying, in this order:

1. a credential pack provisioned for the step (`--secretProvider … --secret …`);
2. one kept in the state from an earlier step — provisioned, or [published](#publishing) by a key before;
3. the environment's `secrets.<name>.<key>`.

## Publishing

A key that runs to completion, or a hook, can make credentials for the keys after it — a CA and the hosts'
certificates, a realm's keytabs, a root token — the way a Kubernetes job publishes a Secret. It writes them as files:

    /var/run/pack/publish/<name>/<key>

and when the container exits 0 each becomes `${secret:<name>/<key>}`, kept in the state, which every later key
resolves at its apply. A key the target makes per instance is `<key>.<index>` (`dn-keystore.jks.1`). A name or key
starting with a dot, and anything deeper, is not published: the job's working directory may live there.

`/var/run/pack` is a volume of the key's, `<installation>-<key>-publish`, on the container's host; `publish/` in it
is writable by any user (1777), so an image that runs unprivileged publishes as well. The volume stays, removed
neither with the container nor with the key: a rerun finds what it published before and can keep it — the CA it
signed with, the token it was given once. Its contents are credentials in plain text, as the state's are.

## State

The installation's record (what is installed, the saga, the platforms) and the credentials pack provisioned are kept:

- **on the engine**, by default (the first host's, across hosts): in labels of volumes that hold nothing. The record is
  `pack-<installation>.<generation>`, gzipped; a write makes the next generation, then removes the old ones. Each
  credential is a volume `pack-secret-…` of its own, its value in a label — in 64 KiB chunks when it is longer (one
  argument of docker's argv is at most 128 KiB). Whoever can reach the engine can read them.
- **in a file**, with `state: <path>`: JSON, holding every installation of the environment by name; a credential that
  is no UTF-8 text (a keytab, a keystore) is kept base64 under `binary`. It is created 0600
  in a directory made 0700, and a write replaces it by rename under an exclusive lock (`<path>.lock`). Credentials are
  in plain text there.

## Output of `pack template`

| File                         | When                | What                                                                                                                                                     |
|------------------------------|---------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------|
| `containers.yaml`            | always (stdout without `-o`) | Every container: host or network, image, entrypoint and command, env (secrets as `${secret:…}`), ports, mounts, resources, healthcheck, labels.  |
| `files/<container>/<path>`   | always              | Each config file, byte for byte, `${secret:…}` as rendered.                                                                                              |
| `compose.yaml` / `compose.<host>.yaml` | `compose: true` | A compose file per engine. Its services bind the files from `files/`, a secret env or secret file is the variable `PACK_SECRET_<NAME>_<KEY>`, and `$` is escaped. The output directory is the bundle: `docker compose -f compose.<host>.yaml --env-file secrets.env up`. |
| `secrets.env`                | credentials provisioned | `PACK_SECRET_<NAME>_<KEY>=<value>`, 0600.                                                                                                              |
| `report.yaml`                | always              | What came from where.                                                                                                                                     |

A compose bundle runs pack's containers, but not pack's saga: no hooks, no order, no waits. `${secret:…}` in a file
stays as rendered, so a file that carries one needs its value put in by hand before the bundle runs.

## Requirements and limits

- Docker ≥ 26 on every engine (`volume-subpath`), and ≥ 28 for libraries (image mounts). The `docker` CLI must be
  where the provider runs.
- Every image the pack uses must be on each engine, or pullable by it. The provider does not distribute images.
- Across hosts there is no failover: an instance is where the operator put it.

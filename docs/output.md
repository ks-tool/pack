# The output: ApplicationSet and config Secrets

`pack template` writes three things:

```
out/
  applicationset.yaml     the set — one application per key, in dependency order
  secrets/<name>.yaml     one Secret per (key, directory) holding the rendered config files
  report.yaml             what came from where
```

Nothing in the output knows the word "pack": the set is an ordinary
`ApplicationSet`, the secrets ordinary `Secret`s. A set left alone by the
tool keeps running.

## The ApplicationSet

```yaml
apiVersion: horchestra.io/v1
kind: ApplicationSet
metadata:
  name: prod                                   # --name
  namespace: data                              # --namespace
  labels: {horchestra.io/pack: adh, horchestra.io/pack-version: 2.1.0}
spec:
  rollout: {maxUnavailable: 1}
  applications:                                # dependency order: zookeeper before kafka
  - name: kafka
    metadata: {labels: {horchestra.io/component: kafka}}
    scale: {replicas: 3}                       # scale: replicas → platform.replicas; single → no scale
    spec:
      image: localhost:5001/images/kafka:3.7.1 # pack.yaml's cr:// item
      command: […]                             # image.json Entrypoint (or x-pack.command)
      args: [--override, min.insync.replicas=2]   # image.json Cmd ⊕ arg= leaves in author order
      env:
      - {name: KAFKA_HEAP_OPTS, value: -Xmx24576m -Xms24576m -XX:+UseG1GC}
      - {name: POSTGRES_PASSWORD, secretRef: {name: pg, key: password}}   # a Secret leaf on env=
      ports: [{name: broker, port: 9092}, {name: internal, port: 9093}]
      resources: {requests: {cpu: "8", memory: 32Gi}, limits: {memory: 32Gi}}
      placement:
        queue: data-prod
        affinity:                              # spread (composition) or placement.spread: host
          workloadAntiAffinity:
            requiredDuringScheduling:
            - topologyKey: horchestra.io/hostname
              labelSelector: {horchestra.io/application-set: prod, horchestra.io/component: kafka}
      readiness: {tcp: {port: 9092}}          # x-pack.readiness by port number
      volumes:
      - mountPath: /var/lib/kafka/data         # claims.data
        volume: {pv: {size: 4Ti, storageClass: nvme}}
      - mountPath: /opt/java                   # the jdk library: x-pack-mount
        merge: true
        volume: {image: {reference: localhost:5001/images/jdk:21.0.4}}
      - mountPath: /opt/kafka/config           # every x-pack-file in that directory
        merge: true
        volume:
          secret:
            name: prod-kafka-5b06f451
            items:
            - {key: kafka_jaas.conf, path: kafka_jaas.conf, mode: 256}     # 0400, decimal as in Kubernetes
            - {key: server.properties, path: server.properties}
            substitute: [prod-kafka-sasl]      # secrets whose ${secret:…} the node substitutes
```

| field | source |
|---|---|
| `name`, `metadata.labels[horchestra.io/component]` | the composition key |
| `scale.replicas` | `platform.replicas` when `scale` is `replicas` |
| `spec.image` | the component's `pack.yaml` (`cr://` item) or `image.ref` |
| `spec.command`, `spec.args` | `image.json` Entrypoint/Cmd, or `x-pack.command`; then every `arg=` leaf in declaration order |
| `spec.env` | `env=` leaves (string → `value`, Secret → `secretRef`), then `platform.env` |
| `spec.ports` | the `ports` object (`const`), sorted by name |
| `spec.resources` | `platform.resources` |
| `spec.placement` | `platform.placement` (`nodeSelector`, `queue`, `priority`) plus the anti-affinity for `spread` |
| `spec.readiness` | `x-pack.readiness`, port names resolved to numbers |
| `spec.volumes` | claims → `pv`; libraries → `image` with `merge`; config directories → `secret` with `merge`; `format: secret` files → the referenced Secret mounted with `merge`, one mount per (directory, Secret) |
| `spec.lifecycle.restartPolicy` | `x-pack.restartPolicy` |

## Config Secrets

Every directory a component (or one of its libraries) writes files into is
**one Secret**, mounted over that directory with `merge: true` — the
image's own files in it stay visible, the pack's win a name collision.

```yaml
apiVersion: horchestra.io/v1
kind: Secret
metadata:
  name: prod-kafka-5b06f451                    # <installation>-<key>-<8 hex of sha256 of the contents>
  namespace: data
  labels: {horchestra.io/installation: prod, horchestra.io/component: kafka}
type: horchestra.io/config                     # a config, not a credential — audit and UI tell them apart
immutable: true
stringData:
  server.properties: |
    broker.id.generation.enable=true
    listeners=PLAINTEXT://:9092,INTERNAL://:9093
    zookeeper.connect=prod-zookeeper-0.prod-zookeeper.data:2181,…/prod
  kafka_jaas.conf: |
    KafkaServer {
      org.apache.kafka.common.security.scram.ScramLoginModule required
      username="kafka"
      password="${secret:prod-kafka-sasl/password}";
    };
```

- **Named by content.** A changed file is a new Secret with a new name,
  referenced from the child's spec — so a config change is a spec change,
  and rolls out like an image change, with the set's `maxUnavailable` and
  in dependency order. Rendering the same inputs again produces the same
  name and changes nothing.
- **Credentials never appear.** A password leaf under a file becomes
  `${secret:<name>/<key>}`; the mount lists the name under `substitute`; the
  node substitutes at materialisation. The tool has no access to secret
  values and needs none.
- **Per-instance files** are keys `<name>.<index>` in the same Secret, and
  the mount item is `{key: "myid.${horchestra.index}", path: myid}` — the
  set controller fills in the index for each child.
- **Secret files** (`format: secret`) are not in a config Secret at all: the
  Secret the leaf references is mounted directly —
  `{secret: {name: prod-kafka-tls, items: [{key: keystore.jks, path: keystore.jks, mode: 256}]}}`
  over the file's directory, `merge: true`, beside the directory's config
  Secret. A per-instance secret file (a keytab per host) has the item key
  `<key>.${horchestra.index}`.
- Binary content (not valid UTF-8) goes to `data` base64-encoded; text to
  `stringData`.
- `mode` is the item's file mode in decimal (`0400` → `256`), the way
  Kubernetes writes it.

## The report

```yaml
installation: prod
namespace: data
pack: adh
order: [zookeeper, kafka, ranger-db, hive-db]
peers:
  kafka: {zk: zookeeper}
computed:
  /kafka/conf/zookeeper.connect: prod-zookeeper-0.prod-zookeeper.data:2181,…
  /kafka/jdk/heapMB: 24576
  /zookeeper/jdk/heapMB: 768
secrets:
  prod-kafka-5b06f451: [kafka_jaas.conf, server.properties]
```

`computed` is every leaf a formula wrote — the answer to "why is this value
what it is" without opening the artifact.

## What horchestra does not know yet

The output is the contract of phase 2. Fields horchestra's API does not have
today, and what they mean:

| field | meaning |
|---|---|
| `volumes[].merge` on a **secret** mount | union with the image's directory; the secret wins a name collision (image volumes already have it) |
| `volumes[].volume.secret.substitute` | secrets whose `${secret:name/key}` placeholders the agent resolves while materialising the config; a placeholder outside the list is an error, not silence |
| `volumes[].volume.secret.items[].key` with `${horchestra.index}` | the set controller substitutes the child's index (also `ordinal`, `instance`) |
| `spec.readiness` | `{tcp: {port}}`, `{http: {port, path}}`, `{exec: […]}` — what the set counts a child ready by |
| `volume.pv.storageClass` | the class the claim asks for |
| `type: horchestra.io/config` on a Secret | a config file, not a credential |

Until then the output is reviewable, testable, diffable — and applied by
the tool's phase-2 `apply` once the fields land.

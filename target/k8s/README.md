# k8s — Kubernetes as a target

`--target k8s` runs `pack-provider-k8s`. A pack goes to one namespace of a cluster: StatefulSets, Jobs, Services and
Secrets. The provider performs each step of a saga with `kubectl`, which carries whatever authentication the kubeconfig
names.

## Where settings come from

A setting is a key of the environment's set. The provider gets the whole set, built in this order, each source over
the one before:

1. the environment file (`--config env.yaml`, YAML or JSON), flattened: `{platform: {kafka: {replicas: 3}}}` is
   `platform_kafka_replicas`;
2. the Vault KV the file names (`vault.addr`, `vault.paths`, the token from `$VAULT_TOKEN`);
3. the command line: `--set <key>=<value>` (repeatable), and `--namespace` as the shorthand for
   `--set namespace=…`.

Keys are case-insensitive: they are lowered as they are read. `target`, `provider` and `vault.*` are pack's own; the
rest are the provider's.

## Settings

| Setting            | Default                                  | Meaning                                                                                                      |
|--------------------|------------------------------------------|--------------------------------------------------------------------------------------------------------------|
| `namespace`        | `default`                                | The namespace everything goes to, the record included. It must exist.                                         |
| `kubeconfig`       | kubectl's own (`$KUBECONFIG`, `~/.kube/config`) | `kubectl --kubeconfig`.                                                                                |
| `context`          | the kubeconfig's current context         | `kubectl --context`.                                                                                          |
| `kubectl`          | `kubectl` from `PATH`                     | The kubectl binary. A provider run from an image has to carry one.                                            |
| `platform.<key>.…` | —                                        | The target's half of a key's platform, over what the values say — see [Platform](#platform).                  |

The provider reads nothing else; other keys are ignored. A `platform.<key>.…` key the target has no word for is
refused.

## Platform

A key's platform has two halves. The pack's half is the same on every target: `resources`, `env`, `labels`,
`annotations`. The target's half below says how many instances run and where. It can come from the values
(`values: {<key>: {platform: {…}}}`) or from the environment (`platform: {<key>: {…}}`); the environment wins.

| Property            | Environment key                                  | Meaning                                                                                               |
|---------------------|--------------------------------------------------|-------------------------------------------------------------------------------------------------------|
| `replicas`          | `platform.<key>.replicas`                        | Pods of a `scale: replicas` key; default 1. A `scale: single` key is one pod.                         |
| `nodeSelector`      | `platform.<key>.nodeSelector.<label>: <value>`   | The pod's `nodeSelector`. Labels from the environment are merged over the values'. A label key that arrives through the environment is lowered with it. |
| `priorityClassName` | `platform.<key>.priorityClassName`               | The pod's `priorityClassName`.                                                                        |
| `storageClass`      | `platform.<key>.storageClass`                    | `storageClassName` of the key's claims; the cluster's default class when unset.                       |

`resources` become the container's `requests` and `limits` as they are. Labels go on the pods, and annotations too, as
annotations. `spread` becomes a required `podAntiAffinity` on `kubernetes.io/hostname`.

```yaml
target: k8s
namespace: data
kubeconfig: ~/.kube/prod
context: prod-admin
platform:
  zookeeper: {replicas: 3}
  kafka:
    replicas: 3
    nodeSelector: {node-role.kubernetes.io/data: "true"}
    storageClass: fast-ssd
```

([`examples/adh/envs/k8s.yaml`](../../examples/adh/envs/k8s.yaml).)

The record keeps the platform each key was installed with. A change of replicas or placement in the environment is
therefore a change that `pack apply` performs, and `pack rollback` returns to the platform recorded before it.

## What goes to the cluster

- **Workload**: a key becomes a StatefulSet (`podManagementPolicy: Parallel`) with a headless Service
  `<installation>-<key>`. Pods are `<service>-<i>`, reached at `<service>-<i>.<service>.<namespace>.svc`, and the key
  as a whole at `<service>.<namespace>.svc`. A key that runs to completion becomes a Job with `backoffLimit: 0`.
- **Config files**: a Secret per directory, `<installation>-<key>-<directory>`, with each file mounted over the image
  by `subPath`. A per-instance file is the pod's own key, mounted by `subPathExpr: $(POD_NAME)`. A checksum annotation
  on the pod template rolls the pods when a file changes.
- **Claims**: `volumeClaimTemplates` (a Job's claims are PVCs of their own). They outlive the key.
- **Libraries**: image volumes. The runtime must support them: CRI-O, or containerd ≥ 2.1 with the `ImageVolume`
  feature gate.
- **Readiness**: a `readinessProbe` (tcp, http or exec).
- **Credentials pack provisioned**: a Secret per credential.

## Steps

| Step   | What the provider does                                                                                                                       |
|--------|----------------------------------------------------------------------------------------------------------------------------------------------|
| apply  | Server-side apply (`--field-manager=pack --force-conflicts`) of the key's objects, with `${secret:…}` in config Secrets resolved first.      |
| wait   | `kubectl rollout status`; for a Job, polling until it succeeds or fails. Logs are told either way.                                           |
| stop   | Scales the StatefulSet to 0 and waits for the pods to go.                                                                                    |
| hook   | A Job of the hook's image and command, with the key's files and env; an exclusive hook also gets replica 0's claims.                         |
| remove | Deletes the workload, the Service and the config Secrets; the claims stay.                                                                   |

## Secrets

`${secret:<name>/<key>}` in a config file is resolved from the Secret `<name>`, key `<key>`, in the namespace when
applying; the config Secret holds the value. A `secretRef` in env and a secret file stay references to that Secret
(`secretKeyRef`, a Secret volume). The Secrets an operator keeps must exist in the namespace before `pack apply`.

## State

The installation's record (what is installed, the saga, the platforms) is the ConfigMap `pack-<installation>` in the
namespace.

## Output of `pack template`

`<key>.yaml` for each key, a YAML stream of its credentials, config Secrets, Service and workload, in apply order
(stdout without `-o`), and `report.yaml`. `${secret:…}` stays in the config Secrets as rendered.

## Requirements and limits

- `kubectl` where the provider runs, and rights in the namespace for StatefulSets, Jobs, Services, Secrets, ConfigMaps,
  PVCs and pods (logs).
- The immutable fields of a StatefulSet (`volumeClaimTemplates`, the selector) are not recreated on a change: apply
  refuses.

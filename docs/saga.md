# Installs and upgrades: hooks, savepoints, rollback

> `pack plan` derives and prints the plan; `pack apply` drives it on a
> target that performs steps (see [Execution](#execution)). This page is the
> contract the hooks in a component and the savepoints in a composition are
> written against.

An installation or an upgrade is a **saga**: a sequence of steps, each an
object in horchestra with a status, with an automatic rollback to a point
the pack's author chose. Nobody writes the sequence. The component author
declares the steps the component contributes; the composition author
declares where the rollback stops; the tool derives the plan from the
target version — and, for an upgrade, its difference from the installed
one — and the dependency graph, shows it before running, and executes it
step by step.

## What a component author writes

```go
func (Postgres) PackRoot() packschema.Root {
    return packschema.Root{
        Upgrade: &packschema.Upgrade{From: ">=15"},
        Hooks: []packschema.Hook{{
            Name: "init-extensions", Phase: "postInstall", Image: "new",   // once, after the first start
            Command: []string{"/opt/pack/init-extensions.sh"}, Idempotent: true, Timeout: "5m",
        }, {
            Name: "pg-upgrade", Phase: "preUpgrade",
            When:       &packschema.When{From: "<16", To: ">=16"},
            Image:      "new",
            Command:    []string{"/usr/lib/postgresql/16/bin/pg_upgrade", "…"},
            Exclusive:  true,        // the component is stopped; the hook gets its volumes
            Timeout:    "30m",
            Idempotent: false,       // a marker refuses a second run
            Compensate: &packschema.Step{Image: "old", Command: []string{"/opt/pack/restore-15.sh"}},
        }, {
            Name: "analyze", Phase: "postUpgrade", When: &packschema.When{To: ">=16"},
            Command: []string{"vacuumdb", "--all", "--analyze-in-stages"}, Idempotent: true,
        }},
    }
}
```

A hook is a one-shot run of the component's image (new or old) with the
component's volumes, values and secrets — the same shape as an init step.
`When` selects it by the version transition; `Compensate` is how to undo
it; without one the step is irreversible and the plan says so.

Six phases. `preInstall` / `postInstall` run **once, around the first
apply** of the key — before it with the component's (still empty) volumes
(`hdfs namenode -format`, `ozone scm --init`), after it against the running
component (a schema bootstrap, extensions, the first admin user). They
select whenever a key is installed: a first installation of the pack, or a
key the previous version did not have, inside an upgrade. An install hook
has no `exclusive` (nothing is running yet), no `when.from` and no `image:
old` — `pack check` refuses them. `preUpgrade` / `postUpgrade` and
`preDowngrade` / `postDowngrade` run around a version transition of an
existing key.

## What a composition author writes

```go
func (ADH) PackRoot() packschema.Root {
    return packschema.Root{
        Savepoints: []string{"zookeeper", "ranger"},
        Hooks: []packschema.Hook{{Name: "maintenance-on", Phase: "preUpgrade", For: []string{"ranger"},
                                  Image: "ranger-tools", Command: []string{"/opt/ranger-tools/maintenance", "on"}}},
    }
}
```

- `Savepoints` — keys after which the rollback does not go. Everything
  between two savepoints is one **rollback unit**. Without savepoints every
  component boundary is one (`auto`).
- system-level `Hooks` with `For`: what no component knows about the system.

## What the tool derives

```sh
pack plan --to ./adh-2.1.0 --values values.yaml --name prod --namespace data                      # first installation
pack plan --from ./adh-2.0.0 --to ./adh-2.1.0 --values values.yaml --name prod --namespace data   # upgrade
```

Both arguments are composition (or lone component) directories — the
installed version and the target. Without `--from` the plan is that of a
**first installation**: every key is added, in dependency order, `apply` →
`wait ready` with the install hooks around it, and undoing an `apply` is
removing the key (its PVs stay):

```
adh: install 2.1.0
  1. zookeeper        apply 3.9.2
  2. zookeeper        wait  ready (tcp:client)
     ── savepoint: zookeeper ──   unit 1–2
  3. kafka            apply 3.7.1
  4. kafka            wait  ready (tcp:broker)
     ── savepoint: kafka ──   unit 3–4
  5. ranger-db        apply 16.3
  6. ranger-db        wait  ready (tcp:pg)
  7. ranger-db        hook  init-extensions (16.3)                   idempotent   ~5m
  8. hive-db          apply 16.3
  9. hive-db          wait  ready (tcp:pg)
 10. hive-db          hook  init-extensions (16.3)                   idempotent   ~5m
     ── end ──   unit 5–10; rollback order: hive-db → ranger-db
rollback policy: auto, to last savepoint; irreversible: none
```

(the golden `examples/adh/testdata/install-plan.txt`.) With `--from` the
values document is rendered against both versions; when the old schema does
not accept the new values, the old side is skipped and config-only changes
are not detected (the plan says so).

1. the diff per component — image, schema, config only, nothing; unchanged
   components are not in the saga;
2. `upgrade.from` of every changed component — refused before planning
   when it fails, naming the intermediate version to go through;
3. the order — the dependency graph (peers ⊕ `after`), reversed for a
   downgrade;
4. per component, a fixed layout nobody writes: `[stop]` (if an exclusive
   hook) → `preUpgrade` hooks → `apply` (a rolling update of the set) →
   `wait ready` → `postUpgrade` hooks; an added key gets `preInstall` →
   `apply` → `wait ready` → `postInstall` instead;
5. savepoints from the composition; the units between them;
6. the plan, printed before anything runs:

```
adh: 2.0.0 → 2.1.0 (upgrade)
unchanged: 1 key(s)
  1. kafka            apply 3.7.1
  2. kafka            wait  ready (tcp:broker)
     ── savepoint: kafka ──   unit 1–2
  3. ranger-db        stop  exclusive hook follows
  4. ranger-db        hook  pg-upgrade (15.6 → 16.3)                 compensate: /opt/pack/restore-15.sh   ~30m
  5. ranger-db        apply 16.3
  6. ranger-db        wait  ready (tcp:pg)
  7. ranger-db        hook  analyze (15.6 → 16.3)                    idempotent
  8. hive-db          stop  exclusive hook follows
  9. hive-db          hook  pg-upgrade (15.6 → 16.3)                 compensate: /opt/pack/restore-15.sh   ~30m
 10. hive-db          apply 16.3
 11. hive-db          wait  ready (tcp:pg)
 12. hive-db          hook  analyze (15.6 → 16.3)                    idempotent
     ── end ──   unit 3–12; rollback order: hive-db → ranger-db
rollback policy: auto, to last savepoint; irreversible: none
```

This is `examples/adh` upgraded from a copy with Postgres 15.6 and Kafka
3.6.0 (the golden `examples/adh/testdata/plan.txt`): ZooKeeper did not
change and has no steps; Kafka's image changed and its component declares
no hooks; both Postgres keys select `pg-upgrade` because the transition
matches its `when`, and `exclusive` puts a `stop` before it. The
composition's savepoints are `zookeeper` and `kafka`; ZooKeeper has no unit,
Kafka's steps are one, and the two Postgres keys share the last unit — a
failure on `hive-db` rolls `hive-db` back first, then `ranger-db`.

How a key is classified: `image` when the image reference or the version
differs (hooks apply); `schema` when only the schema changed; `config` when
the rendered config Secrets differ; `added` / `removed` for keys that
appear or disappear (an added key is installed — install hooks, removal as
the compensation); `none` otherwise. Hooks with a `when` need both
versions (from `pack.yaml`) — without them the hook is skipped with a
warning. A transition outside the target's `upgrade.from` is refused before
any plan is printed, naming the constraint.

## Rollback

Automatic, to the last savepoint, and no further. A failure inside a unit
rolls back **every step of the unit in reverse plan order** — dependents
before dependencies, the database last: `ranger → ranger-schema →
ranger-db`. Rolling one component back undoes what was done to it in
reverse: compensate its `postUpgrade` hooks; if `apply` went through,
downgrade (`preDowngrade`, apply the previous revision, `postDowngrade`);
compensate its `preUpgrade` hooks. A config Secret is named by content, so
the previous revision's name is still the previous one — the `apply` step
is always reversible.

Policy is set by flags **before** the start and recorded with the saga:

| flag | meaning |
|---|---|
| (default) | roll the failed unit back to its savepoint |
| `--savepoints auto\|none\|k1,k2` | override the composition: `auto` — every component; `none` — the whole saga is one unit |
| `--no-rollback k1,k2` | **where the rollback stops**: the unit rolls back in reverse order until it reaches one of these keys; that key and everything before it stay as they are — "roll the applications back, but not the database", leaving the evidence for a diagnosis |
| `--no-rollback all` | roll nothing back; stop on the failed step |
| `--strict` | refuse a plan with an irreversible step |

After a rollback the record reports the failed step, the error, what was
undone and where it stopped; running `pack apply` again continues the saga
from that unit's savepoint ([Execution](#execution)); `pack rollback`
undoes the committed units too. A failed
compensation is the worst state and is reported as such.

## Execution

`pack apply` performs and controls the saga; the target only carries out
each step in its own system, through its provider (`proto/provider.proto`).
Postgres on Kubernetes: the provider makes a StatefulSet and a Secret of the
step and applies them. Postgres on a VM: it writes the config files to the
host and installs packages, or starts the image with podman, docker or
sandbox. A provider that can do this declares the `execute` capability; a
target without it renders (`pack template`) and refuses `apply`.

1. **What is installed** comes from the target: `ReadInstalled` returns the
   record pack keeps there — where the installed version came from, its
   version and values, the target's half of each key's platform as the
   target resolved it (`platform`: replicas, hosts, placement — what the
   environment said then), and a saga under way or the last one that failed.
   pack has no state of its own.
2. pack renders the installed version with its recorded values and
   platforms — `Resolve` with `installed`, the environment not consulted, so
   the installed version is what was installed — and the new one with the
   operator's values and the environment as it is now, and derives the plan,
   the same as `pack plan`. A key whose unit differs in anything — files,
   platform, env, resources — is a change: moving it to other hosts, or
   another replica count, is an apply.
3. **Every step** goes to the provider as `Execute`: the step (stop, hook,
   apply, wait, remove), the key's unit in the new version and in the
   installed one, for a hook the image and command, the credentials
   provisioned for the key. The provider streams what it does; the last
   event says whether the step succeeded. A hook's `timeout` bounds it.
4. After every step pack writes the record (`WriteInstalled`), so a saga
   cut short is visible to whoever looks next.
5. **A failed step** rolls its unit back, in reverse plan order, until the
   rollback policy stops it: a hook that ran is compensated (one without
   `compensate` is left as it is); an `apply` is undone by applying the
   previous unit — for a key that changed version, between the installed
   version's hooks for the reverse transition (`preDowngrade`, apply,
   `postDowngrade` for an upgrade; a stop first when a pre hook is
   exclusive; `new` is the version restored, `old` the one rolled back;
   `pack plan` prints them as "rollback of <key>") — or by removing a key
   the saga added; a `remove` by applying the previous unit. The failed step itself counts as performed
   if it was an `apply` or a `remove` — it may have half-happened — and
   never if it was a hook. Units behind a savepoint stay.
6. The record says `installed` with the new version, or `failed` with the
   step, the error and what was undone.
7. **`pack apply` always starts from a savepoint.** A saga the record shows
   unfinished — failed, or cut short with no rollback — is continued by the
   same command: what of the unit it stopped in is still in effect is
   undone (whatever the rollback policy left there too — a unit is all or
   nothing), and the saga runs again from that unit's first step; the units
   behind the savepoint are not performed again. The same command means
   the same `--pack`, values as given and rollback policy — the record
   keeps them; the credentials the tool provisions are provisioned again,
   since the record never holds a value. A different command over an
   unfinished saga is refused; `--from-start` runs the saga from its first
   step, or starts the new one over the unfinished.

8. **`pack rollback`** takes an installation back. A saga that did not
   finish is undone whole — every step of it still in effect, unit by unit
   from the last, the units behind savepoints too, whatever the rollback
   policy said — and the record says the version before it is installed
   (an undone first installation leaves nothing installed). A finished
   saga recorded what it replaced (`previous`): rollback is a saga back to
   that version, planned and performed like any other.

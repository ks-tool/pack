# Artifacts: building and publishing with oci-packer

The tool renders from directories or straight from a registry. Packaging
directories into OCI artifacts and moving them between registries is the job
of [`oci-packer`](https://github.com/arenadata/oci-packer), used as a
command; the one thing of it the tool links is the pack-file type
(`packer.Pack`), so that the `pack.yaml` the SDK writes and `bundle` reads
is oci-packer's own.

## A component's artifact

`kafka/pack.yaml`:

```yaml
type: application/vnd.horchestra.component.v1+json
annotations: {io.horchestra.name: kafka, io.horchestra.version: "3.7.1"}
items:
  - from: oci://registry.example/images/kafka:3.7.1     # the image: mounted from the same registry, not copied
    platform: linux/arm64
  - from: file://schema.json
    type: application/vnd.horchestra.component.schema.v1+json
  - from: dir://templates/                              # zero or more; the file name is the template name
    type: application/vnd.horchestra.template.v1
```

```sh
oci-packer -f pack.yaml oci://registry.example/components/kafka:3.7.1
```

What oci-packer builds: any `oci://` or `platform` item makes the artifact
an **index**, one member per item. The image item becomes the image's own
manifest as a member (with its `platform`); the schema and the templates
become one manifest each, tagged by their `type`. The image must already be
in the same registry — the item is a cross-repository mount, and its
manifest and blobs are copied into the artifact's repository.

**No external registries.** Every image a pack uses is loaded into the
registry the pack is published to beforehand, and its `oci://` item names it
there: the item's host is the artifact's own. oci-packer refuses an item on
another registry's host, or in a local OCI layout, before it builds anything
(the mount is cross-repository within one registry), so an image from
outside is copied in first:

```sh
oci-packer copy oci://docker.io/library/postgres:18-alpine oci://registry.example/images/postgres:18-alpine
```

The tool reads the same file for two things: the component's **name and
version** (the annotations) and the **image reference** for `spec.image`
(the `oci://` item, scheme stripped). Without a `pack.yaml`, `image.ref`
beside the schema supplies the reference and the directory name supplies
the name.

A library is built the same way; its `oci://` item is the image whose layers
consumers mount.

## A composition's artifact

`adh/pack.yaml`:

```yaml
type: application/vnd.horchestra.pack.v1+json
annotations: {io.horchestra.name: adh, io.horchestra.version: "2.1.0"}
items:
  - from: file://schema.json
    type: application/vnd.horchestra.pack.schema.v1+json
  - from: oci://registry.example/components/zookeeper:3.9.2   # every component and library, once per name
  - from: oci://registry.example/components/kafka:3.7.1
  - from: oci://registry.example/components/postgres:16.3
  - from: oci://registry.example/components/jdk:21.0.4
```

The pack pins the **digest** of every member; the composition's schema
refers to them by **name** (`pack://kafka#`), so bumping a component whose
schema did not change leaves the composition schema's digest — the version
of the system's configuration — unchanged. One version per name: two
members with the same `io.horchestra.name` and different digests are
refused.

## Rendering from a registry

The tool reads a pack **straight from the registry** — its own small client
(`pack/registry`), no `oci-packer`, no layout:

```sh
pack check    --plain-http oci://registry.example/bundles/adh:4.3.0
pack template --plain-http --pack oci://registry.example/bundles/adh:4.3.0 --values values.yaml --name adh --namespace demo -o ./out
pack plan     --plain-http --to   oci://registry.example/bundles/adh:4.3.0 --values values.yaml
```

What it fetches: the pack's index; the composition schema (the first layer
of the `…pack.schema` member); for every mounted component — its index (the
name and version are the nested index's annotations, a mounted member's
descriptor carries none), the schema, the template layers at their titles,
and the mounted image's **config** (entrypoint, cmd, env — a few KB).
**Never an image layer**: `spec.image` in the output is
`<host>/<pack repo>@<digest>`, and pulling it is horchestra's job.

`oci://` may be omitted (`host/repo:tag`); `@sha256:…` pins a digest;
`--plain-http` for a registry without TLS; `PACK_REGISTRY_USER` /
`PACK_REGISTRY_PASSWORD` for one that wants credentials (basic, or a bearer
token from the realm it names). A lone component is renderable by reference
when the libraries it needs are mounted into its own artifact; in a pack
they are the pack's members.

## Signatures

The artifact's index digest is what a signature covers; everything it
references is pinned by digest, so signing the pack signs the whole. The
tool has a `SignatureHook` seam and no implementation yet — verification is
the last item of the plan. Image signatures remain horchestra's concern.

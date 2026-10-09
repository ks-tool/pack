"""The vocabulary the schema carries beyond JSON Schema: x-pack on the root
(Root), the pieces every component has (Platform, Scale, Claim, Secret), and
the artifact's identity (Meta). Field names mirror packschema's JSON."""

from __future__ import annotations

import dataclasses
from dataclasses import dataclass, field
from typing import Any

from ._fields import PACK, const, default, operator


def _clean(obj: Any) -> Any:
    """Serialise like Go with omitempty: drop None, False, empty lists/dicts."""
    if dataclasses.is_dataclass(obj) and not isinstance(obj, type):
        out = {}
        for f in dataclasses.fields(obj):
            v = _clean(getattr(obj, f.name))
            if v is None or v is False or v == [] or v == {} or v == "":
                continue
            out[f.metadata.get("json", f.name)] = v
        return out
    if isinstance(obj, list):
        return [_clean(x) for x in obj]
    if isinstance(obj, dict):
        return {k: _clean(v) for k, v in obj.items() if _clean(v) not in (None, {}, [])}
    return obj


def _j(name: str) -> Any:
    return field(default=None, metadata={"json": name})


@dataclass
class Readiness:
    tcp: str | None = None
    http: str | None = None
    exec: list[str] | None = None


@dataclass
class Peer:
    schema: str
    required: bool = False


@dataclass
class Upgrade:
    from_: str = field(default="", metadata={"json": "from"})


@dataclass
class When:
    from_: str | None = field(default=None, metadata={"json": "from"})
    to: str | None = None


@dataclass
class Step:
    command: list[str]
    image: str | None = None


@dataclass
class Hook:
    name: str
    phase: str
    command: list[str]
    when: When | None = None
    image: str | None = None
    exclusive: bool = False
    timeout: str | None = None
    idempotent: bool = False
    compensate: Step | None = None
    for_: list[str] | None = field(default=None, metadata={"json": "for"})


@dataclass
class Rollout:
    max_unavailable: int = field(default=0, metadata={"json": "maxUnavailable"})


@dataclass
class Root:
    """x-pack on the root: what the component asks of the system, how it
    changes versions; a composition's savepoints, hooks, rollout. scale —
    Scale.SINGLE or Scale.REPLICAS — is a fact about the application: what
    sdk.Workload lifts into x-pack.scale in Go."""
    scale: str | None = None
    readiness: Readiness | None = None
    peers: dict[str, Peer] | None = None
    requires: dict[str, str] | None = None
    upgrade: Upgrade | None = None
    hooks: list[Hook] | None = None
    command: list[str] | None = None
    restart_policy: str | None = field(default=None, metadata={"json": "restartPolicy"})
    savepoints: list[str] | None = None
    rollout: Rollout | None = None

    def to_json(self) -> dict[str, Any]:
        return _clean(self)


@dataclass
class Meta:
    name: str
    version: str
    image: str = ""          # oci://host/repo:tag; "" for a library or a composition
    artifact: str = ""       # oci://host/repo (no tag): where the artifact is published
    templates: str = ""      # a directory of template files to ship
    platform: str = "linux/amd64"


class Component:
    """Base of a component dataclass: meta, and optional root()/compute()."""
    meta: Meta

    def root(self) -> Root | None:
        return None

    def compute(self) -> dict[str, str]:
        return {}


class Composition(Component):
    """A pack: fields are keys (key(...)), members() the components it mounts."""

    def members(self) -> list[Component]:
        return []


class Scale:
    SINGLE = "single"
    REPLICAS = "replicas"


@dataclass
class Resources:
    requests: dict[str, str] = field(default_factory=dict)
    limits: dict[str, str] = field(default_factory=dict)


@dataclass
class Platform:
    """The pack's half of the platform — resources (cgroups), the same on
    every target. Use as `platform: Platform = Platform.sized("500m", "512Mi")`;
    it reflects to {"$ref": "urn:pack:platform", "default": …}. Replicas and
    placement are the target's half: pack schedules nothing."""
    resources: Resources = field(default_factory=Resources)

    REF = "urn:pack:platform"

    @classmethod
    def sized(cls, cpu: str, memory: str) -> "Platform":
        return cls(resources=Resources(requests={"cpu": cpu, "memory": memory}, limits={"memory": memory}))

    def default_json(self) -> dict[str, Any]:
        return _clean(self)


@dataclass
class Claim:
    """A persistent volume: mountPath a fact, size and class settings."""
    mount_path: str = const("", json="mountPath", pattern="^/", required=True)
    size: str = default("")

    @classmethod
    def at(cls, mount_path: str, size: str) -> "Claim":
        return cls(mount_path=mount_path, size=size)


class Secret(str):
    """Marker type of a credential leaf; declare the field with secret()."""

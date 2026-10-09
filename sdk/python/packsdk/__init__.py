"""packsdk — describe a pack component as dataclasses; generate the artifact.

The component is a dataclass whose DEFAULT VALUES are the defaults document;
every field says what it is with one of const / default / operator / file /
ref, and where it goes with env= / arg=. The pieces every component has are
ready: Platform (the platform half — its schema is the target's), Scale, Claim, Secret.

    @dataclass
    class Conf:
        max_connections: int = default(100, minimum=1)
        listen_addresses: str = const("*")

    @dataclass
    class Postgres(Component):
        ports: Ports = default_factory(Ports)
        scale: str = const(Scale.SINGLE)
        platform: Platform = Platform.sized("500m", "512Mi")
        conf: Conf = file(Conf(), path="/etc/postgresql/postgresql.conf", format="properties")

        meta = Meta(name="postgres", version="16.3", image="cr://…/postgres:16.3", artifact="cr://…/components/postgres")

        def root(self): return Root(readiness=Readiness(tcp="pg"))
        def compute(self): return {"/conf/shared_buffers": "…"}

schema(Postgres()) is the schema.json the Go SDK would write for the same
description — the pack tool reads either. generate(dir, Postgres()) writes
schema.json, pack.yaml and templates/; main(...) is a generator CLI.
"""

from ._fields import (
    arg,
    const,
    default,
    default_factory,
    env,
    file,
    key,
    operator,
    pack_ref,
    ref,
    secret,
)
from ._model import (
    Claim,
    Component,
    Composition,
    Hook,
    Meta,
    Peer,
    Platform,
    Readiness,
    Rollout,
    Root,
    Scale,
    Secret,
    Step,
    Upgrade,
    When,
)
from ._pack import generate, main, pack_file
from ._schema import schema, schema_dict

__all__ = [
    "Claim", "Component", "Composition", "Hook", "Meta", "Peer", "Platform", "Readiness", "Rollout", "Root",
    "Scale", "Secret", "Step", "Upgrade", "When",
    "arg", "const", "default", "default_factory", "env", "file", "key", "operator", "pack_ref", "ref", "secret",
    "schema", "schema_dict", "pack_file", "generate", "main",
]

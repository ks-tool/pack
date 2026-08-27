"""The Python description of examples/postgres is the Go one: same JSON
Schema (as a value, and in property order), and a pack file with the image."""

import json
import os
import sys

HERE = os.path.dirname(__file__)
sys.path.insert(0, os.path.join(HERE, ".."))
sys.path.insert(0, os.path.join(HERE, "..", "example"))

from dataclasses import dataclass  # noqa: E402

from packsdk import Component, Meta, pack_file, pack_ref, schema_dict  # noqa: E402
from postgres import Postgres  # noqa: E402

GO_SCHEMA = os.path.join(HERE, "..", "..", "..", "examples", "postgres", "schema.json")


def _orders(node, path=""):
    """Every properties object's key order, for comparing author order."""
    out = {}
    if isinstance(node, dict):
        if isinstance(node.get("properties"), dict):
            out[path] = list(node["properties"].keys())
            for k, v in node["properties"].items():
                out.update(_orders(v, f"{path}/{k}"))
    return out


def test_schema_equals_go():
    with open(GO_SCHEMA, encoding="utf-8") as f:
        want = json.load(f)
    got = schema_dict(Postgres())
    assert got == want, json.dumps(got, indent=2, ensure_ascii=False)
    assert _orders(got) == _orders(want)


def test_pack_ref():
    """pack_ref(name) is the Go pack_ref:"name" tag: a library with its mount, a composition key with its x-pack."""
    @dataclass
    class Kafka(Component):
        jdk: object = pack_ref("jdk", mount="/opt/java")
        meta = Meta(name="kafka", version="1")

    @dataclass
    class Adh(Component):
        kafka: object = pack_ref("kafka", spread=True, minimum={"memory": "4Gi"})
        meta = Meta(name="adh", version="1")

    props = schema_dict(Kafka())["properties"]
    assert props["jdk"] == {"$ref": "pack://jdk#", "x-pack-mount": "/opt/java"}
    props = schema_dict(Adh())["properties"]
    assert props["kafka"] == {"$ref": "pack://kafka#", "x-pack": {"spread": True, "minimum": {"memory": "4Gi"}}}


def test_pack_file():
    pf = pack_file(Postgres())
    assert "cr://localhost:5001/images/postgres:16.3" in pf
    assert "io.horchestra.name: postgres" in pf


if __name__ == "__main__":
    test_schema_equals_go()
    test_pack_ref()
    test_pack_file()
    print("ok")

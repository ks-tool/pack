"""The generator: a component dataclass → the JSON Schema the pack tool
reads, with the same shape packschema.Reflect gives a Go struct."""

from __future__ import annotations

import dataclasses
import json
import types
import typing
from typing import Any

from ._fields import PACK
from ._model import Claim, Component, Platform, Secret

SCHEMA_VERSION = "https://json-schema.org/draft/2020-12/schema"


def schema_dict(component: Any) -> dict[str, Any]:
    """The schema as a dict (property order = field order)."""
    s: dict[str, Any] = {"$schema": SCHEMA_VERSION}
    s.update(_object(component, ""))
    if isinstance(component, Component):
        root = component.root()
        if root is not None:
            s["x-pack"] = root.to_json()
        for ptr, expr in component.compute().items():
            node = _lookup(s, ptr)
            node["x-pack-compute"] = expr
    return s


def schema(component: Any) -> str:
    """schema.json text: two-space indent, trailing newline."""
    return json.dumps(schema_dict(component), indent=2, ensure_ascii=False) + "\n"


def _lookup(s: dict[str, Any], ptr: str) -> dict[str, Any]:
    node = s
    for seg in ptr.strip("/").split("/"):
        props = node.get("properties") or {}
        if seg not in props:
            raise KeyError(f"x-pack-compute {ptr}: no property {seg!r}")
        node = props[seg]
    return node


def _object(value: Any, path: str) -> dict[str, Any]:
    cls = value if isinstance(value, type) else type(value)
    hints = typing.get_type_hints(cls)
    props: dict[str, Any] = {}
    required: list[str] = []
    for f in dataclasses.fields(cls):
        meta = f.metadata.get(PACK, {})
        name = meta.get("json") or f.name
        fv = getattr(value, f.name, None) if not isinstance(value, type) else None
        prop = _property(hints.get(f.name, Any), fv, meta, f"{path}/{name}")
        props[name] = prop
        if meta.get("required"):
            required.append(name)
    out: dict[str, Any] = {"properties": props, "additionalProperties": False, "type": "object"}
    if required:
        out["required"] = required
    return out


def _property(hint: Any, value: Any, meta: dict[str, Any], path: str) -> dict[str, Any]:
    kind = meta.get("kind", "object")
    prop: dict[str, Any]
    if kind == "ref":
        prop = {"$ref": meta["ref"]}
        if value is not None and not isinstance(value, type(None)):
            d = _prune(_json_value(value))
            if d not in (None, {}):
                prop["default"] = d
        if meta.get("mount"):
            prop["x-pack-mount"] = meta["mount"]
        if meta.get("key"):
            prop["x-pack"] = meta["key"]
        return prop
    if kind == "secret" or hint is Secret:
        prop = {"type": "string", "format": "password"}
    elif isinstance(value, Platform) or hint is Platform:
        prop = {"$ref": Platform.REF}
        if value is not None:
            d = _prune(value.default_json())
            if d:
                prop["default"] = d
        _annotate(prop, meta)
        return prop
    elif dataclasses.is_dataclass(hint) or (value is not None and dataclasses.is_dataclass(value)):
        prop = _object(value if value is not None else hint, path)
        _annotate(prop, meta)
        return prop
    else:
        prop = _type_of(hint)
        if kind == "const":
            prop["const"] = _json_value(value)
        elif kind == "default":
            prop["default"] = _json_value(value)
    prop.update(meta.get("schema", {}))
    _annotate(prop, meta)
    return prop


def _annotate(prop: dict[str, Any], meta: dict[str, Any]) -> None:
    if meta.get("file"):
        prop["x-pack-file"] = meta["file"]
    if meta.get("arg"):
        prop["x-pack-arg"] = meta["arg"]
    if meta.get("env"):
        prop["x-pack-env"] = meta["env"]
    for k, v in meta.get("schema", {}).items():
        prop.setdefault(k, v)


def _type_of(hint: Any) -> dict[str, Any]:
    origin = typing.get_origin(hint)
    if origin is types.UnionType or origin is typing.Union:
        args = [a for a in typing.get_args(hint) if a is not type(None)]
        return _type_of(args[0]) if args else {}
    if hint is bool:
        return {"type": "boolean"}
    if hint is int:
        return {"type": "integer"}
    if hint is float:
        return {"type": "number"}
    if hint is str or hint is Secret:
        return {"type": "string"}
    if origin in (list, tuple) or hint is list:
        args = typing.get_args(hint)
        return {"items": _type_of(args[0]) if args else {}, "type": "array"}
    if origin is dict or hint is dict:
        args = typing.get_args(hint)
        return {"additionalProperties": _type_of(args[1]) if len(args) == 2 else {}, "type": "object"}
    if hint is Any:
        return {}
    if isinstance(hint, type) and dataclasses.is_dataclass(hint):
        return _object(hint, "")
    return {}


def _json_value(v: Any) -> Any:
    if dataclasses.is_dataclass(v) and not isinstance(v, type):
        out = {}
        for f in dataclasses.fields(v):
            meta = f.metadata.get(PACK, {})
            name = meta.get("json") or f.metadata.get("json") or f.name
            out[name] = _json_value(getattr(v, f.name))
        return out
    if isinstance(v, dict):
        return {k: _json_value(x) for k, x in v.items()}
    if isinstance(v, (list, tuple)):
        return [_json_value(x) for x in v]
    return v


def _prune(v: Any) -> Any:
    """Drop zero values the way Go's prune does for a ref's default."""
    if isinstance(v, dict):
        out = {k: _prune(x) for k, x in v.items()}
        return {k: x for k, x in out.items() if x not in (None, "", 0, False, {}, [])}
    if isinstance(v, list):
        return [_prune(x) for x in v]
    return v

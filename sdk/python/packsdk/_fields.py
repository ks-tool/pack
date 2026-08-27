"""Field declarations: what a field IS (const / default / operator / file /
ref) and where it GOES (env / arg). Each returns a dataclasses.field whose
metadata the schema generator reads; the field's default is the value the
defaults document carries."""

from __future__ import annotations

import copy
import dataclasses
from typing import Any

PACK = "pack"  # metadata key


def _field(value: Any, meta: dict[str, Any]) -> Any:
    """A dataclass field with a (deep-copied) default and pack metadata.
    _NO_DEFAULT leaves the field without one: an operator's field."""
    if value is _NO_DEFAULT:
        return dataclasses.field(default=None, metadata={PACK: meta})
    if isinstance(value, (list, dict, set)) or dataclasses.is_dataclass(value):
        return dataclasses.field(default_factory=lambda v=value: copy.deepcopy(v), metadata={PACK: meta})
    return dataclasses.field(default=value, metadata={PACK: meta})


class _NoDefault:
    def __repr__(self) -> str:
        return "<no default>"


_NO_DEFAULT = _NoDefault()

# keywords every declaration may carry; they land on the property as is
_SCHEMA_KEYS = ("title", "description", "minimum", "maximum", "enum", "pattern", "min_length", "max_length", "min_items")
_RENAME = {"min_length": "minLength", "max_length": "maxLength", "min_items": "minItems"}


def _schema_kw(kw: dict[str, Any]) -> dict[str, Any]:
    out = {}
    for k in _SCHEMA_KEYS:
        if k in kw and kw[k] is not None:
            out[_RENAME.get(k, k)] = kw[k]
    return out


def _where(kw: dict[str, Any]) -> dict[str, Any]:
    """env= and arg= (with prefix/each/omit_empty) — where a leaf goes."""
    out = {}
    if kw.get("env"):
        out["env"] = kw["env"]
    if kw.get("arg"):
        a = {"flag": kw["arg"]}
        if kw.get("prefix"):
            a["prefix"] = kw["prefix"]
        if kw.get("each"):
            a["each"] = True
        if kw.get("omit_empty"):
            a["omitEmpty"] = True
        out["arg"] = a
    if kw.get("json"):
        out["json"] = kw["json"]
    if kw.get("required"):
        out["required"] = True
    return out


def const(value: Any, *, json: str | None = None, **kw: Any) -> Any:
    """A fact about the software: the operator cannot change it."""
    return _field(value, {"kind": "const", "schema": _schema_kw(kw), **_where({**kw, "json": json})})


def default(value: Any, *, json: str | None = None, **kw: Any) -> Any:
    """A setting with a default the operator may override."""
    return _field(value, {"kind": "default", "schema": _schema_kw(kw), **_where({**kw, "json": json})})


def operator(*, json: str | None = None, required: bool = False, **kw: Any) -> Any:
    """The operator's field: no default, no const (a formula may fill it)."""
    return _field(_NO_DEFAULT, {"kind": "operator", "schema": _schema_kw(kw), **_where({**kw, "json": json, "required": required})})


def default_factory(cls: Any, *, json: str | None = None, **kw: Any) -> Any:
    """A nested object with its own defaults (a dataclass instance)."""
    return _field(cls() if isinstance(cls, type) else cls, {"kind": "object", "schema": _schema_kw(kw), **_where({**kw, "json": json})})


def secret(*, json: str | None = None, required: bool = True, **kw: Any) -> Any:
    """A credential leaf: format password; the operator gives a Secret reference."""
    return _field(_NO_DEFAULT, {"kind": "secret", "schema": _schema_kw(kw), **_where({**kw, "json": json, "required": required})})


def file(value: Any, *, path: str, format: str, template: str | None = None, parse_as: str | None = None,
         mode: str | None = None, per_instance: bool = False, json: str | None = None, **kw: Any) -> Any:
    """The subtree (or leaf) becomes a file at path, rendered in the given format."""
    f: dict[str, Any] = {"path": path, "format": format}
    if template:
        f["template"] = template
    if parse_as:
        f["parseAs"] = parse_as
    if mode:
        f["mode"] = mode
    if per_instance:
        f["perInstance"] = True
    kind = "object"
    if kw.get("const"):
        kind = "const"
    elif kw.get("default") or not (isinstance(value, (list, dict)) or dataclasses.is_dataclass(value)):
        kind = "default" if value is not _NO_DEFAULT else "operator"
    return _field(value, {"kind": kind, "file": f, "schema": _schema_kw(kw), **_where({**kw, "json": json})})


def ref(uri: str, value: Any = None, *, json: str | None = None) -> Any:
    """A foreign schema by URI (the platform half); a member of the pack is
    pack_ref(name)."""
    return _ref(uri, value, json=json)


def pack_ref(name: str, value: Any = None, *, mount: str | None = None, spread: bool = False, peers: dict[str, str] | None = None,
             after: list[str] | None = None, minimum: dict[str, str] | None = None, json: str | None = None) -> Any:
    """A member of the pack by name — the property is {"$ref": "pack://<name>#"}:
    a library (with mount=, where its layers go) or a composition key (with
    the key's x-pack: spread, peers, after, minimum) — with an optional
    default layered over the member's own. The Go tag is pack_ref:"<name>"."""
    return _ref(f"pack://{name}#", value, mount=mount, spread=spread, peers=peers, after=after, minimum=minimum, json=json)


def _ref(uri: str, value: Any = None, *, mount: str | None = None, spread: bool = False, peers: dict[str, str] | None = None,
         after: list[str] | None = None, minimum: dict[str, str] | None = None, json: str | None = None) -> Any:
    meta: dict[str, Any] = {"kind": "ref", "ref": uri, "schema": {}}
    if mount:
        meta["mount"] = mount
    key_ann = {}
    if spread:
        key_ann["spread"] = True
    if peers:
        key_ann["peers"] = peers
    if after:
        key_ann["after"] = after
    if minimum:
        key_ann["minimum"] = minimum
    if key_ann:
        meta["key"] = key_ann
    if json:
        meta["json"] = json
    return _field(value if value is not None else _NO_DEFAULT, meta)


def key(component: Any, *, default: Any = None, spread: bool = False, peers: dict[str, str] | None = None,
        after: list[str] | None = None, minimum: dict[str, str] | None = None, json: str | None = None) -> Any:
    """A composition key: pack_ref of the component's own name, with the key's x-pack."""
    name = component.meta.name if hasattr(component, "meta") else str(component)
    return pack_ref(name, default, spread=spread, peers=peers, after=after, minimum=minimum, json=json)


def env(name: str) -> dict[str, str]:
    """Sugar for readability: env="X" as a keyword works the same."""
    return {"env": name}


def arg(flag: str, *, prefix: str | None = None, each: bool = False, omit_empty: bool = False) -> dict[str, Any]:
    return {"arg": flag, "prefix": prefix, "each": each, "omit_empty": omit_empty}

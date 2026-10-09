"""pack.yaml, the directory tree oci-packer packs, and a generator CLI."""

from __future__ import annotations

import os
import shutil
import sys
from typing import Any

from ._model import Component, Composition, Meta
from ._schema import schema


def pack_file(c: Component) -> str:
    m: Meta = c.meta
    if isinstance(c, Composition):
        lines = ["type: application/vnd.horchestra.pack.v1+json",
                 f'annotations: {{io.horchestra.name: {m.name}, io.horchestra.version: "{m.version}"}}',
                 "items:", "  - from: file://schema.json", "    type: application/vnd.horchestra.pack.schema.v1+json"]
        for member in c.members():
            mm = member.meta
            if not mm.artifact:
                raise ValueError(f"{m.name}: member {mm.name} has no artifact (oci://repo)")
            lines.append(f"  - from: {mm.artifact}:{mm.version}")
        return "\n".join(lines) + "\n"
    lines = ["type: application/vnd.horchestra.component.v1+json",
             f'annotations: {{io.horchestra.name: {m.name}, io.horchestra.version: "{m.version}"}}',
             "items:"]
    if m.image:
        lines += [f"  - from: {m.image}", f"    platform: {m.platform}"]
    lines += ["  - from: file://schema.json", "    type: application/vnd.horchestra.component.schema.v1+json"]
    if m.templates and os.path.isdir(m.templates):
        lines += ["  - from: dir://templates/", "    type: application/vnd.horchestra.template.v1"]
    return "\n".join(lines) + "\n"


def _all(items: list[Component]) -> list[Component]:
    seen: dict[str, Component] = {}

    def add(c: Component) -> None:
        if c.meta.name in seen:
            return
        seen[c.meta.name] = c
        if isinstance(c, Composition):
            for m in c.members():
                add(m)

    for c in items:
        add(c)
    return [seen[n] for n in sorted(seen)]


def generate(directory: str, *items: Component) -> list[str]:
    """Write <dir>/<name>/{schema.json, pack.yaml, templates/} for every item
    and every member of a composition; returns the directories written."""
    out = []
    for c in _all(list(items)):
        d = os.path.join(directory, c.meta.name)
        os.makedirs(d, exist_ok=True)
        with open(os.path.join(d, "schema.json"), "w", encoding="utf-8") as f:
            f.write(schema(c))
        with open(os.path.join(d, "pack.yaml"), "w", encoding="utf-8") as f:
            f.write(pack_file(c))
        if c.meta.templates and os.path.isdir(c.meta.templates):
            dst = os.path.join(d, "templates")
            if os.path.abspath(dst) != os.path.abspath(c.meta.templates):
                shutil.rmtree(dst, ignore_errors=True)
                shutil.copytree(c.meta.templates, dst)
        out.append(d)
    return out


def main(*items: Component, argv: list[str] | None = None) -> None:
    """generate [dir] | schema <name>"""
    args = sys.argv[1:] if argv is None else argv
    if not args:
        print("usage: generate [dir] | schema <name>", file=sys.stderr)
        sys.exit(2)
    if args[0] == "generate":
        for d in generate(args[1] if len(args) > 1 else ".", *items):
            print("wrote", d)
    elif args[0] == "schema" and len(args) > 1:
        for c in _all(list(items)):
            if c.meta.name == args[1]:
                sys.stdout.write(schema(c))
                return
        print(f"no component {args[1]!r}", file=sys.stderr)
        sys.exit(1)
    else:
        print(f"unknown command {args[0]!r}", file=sys.stderr)
        sys.exit(2)

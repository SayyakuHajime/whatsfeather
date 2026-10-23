#!/usr/bin/env python3
"""Build a deterministic, offline dependency inventory from checked-in manifests.

This intentionally does not resolve package registries, infer licenses, or query
vulnerability databases. It reports manifest facts and checksum coverage only.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path
from typing import Any


def parse_go_mod(path: Path) -> tuple[str | None, list[dict[str, Any]]]:
    go_version: str | None = None
    requirements: list[dict[str, Any]] = []
    in_require = False
    for raw_line in path.read_text(encoding="utf-8").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("//"):
            continue
        go_match = re.fullmatch(r"go\s+(\S+)", line)
        if go_match:
            go_version = go_match.group(1)
            continue
        if line == "require (":
            in_require = True
            continue
        if in_require and line == ")":
            in_require = False
            continue
        if line.startswith("require "):
            line = line[len("require "):].strip()
            is_indirect = "// indirect" in line
        elif in_require:
            is_indirect = "// indirect" in line
        else:
            continue
        line = line.split("//", 1)[0].strip()
        fields = line.split()
        if len(fields) < 2:
            continue
        requirements.append({
            "ecosystem": "Go",
            "name": fields[0],
            "version": fields[1],
            "dependency_type": "indirect" if is_indirect else "direct",
            "license_status": "not_assessed",
            "source": "backend/go.mod",
        })
    requirements.sort(key=lambda item: (item["name"].casefold(), item["version"]))
    return go_version, requirements


def parse_go_sums(path: Path) -> set[tuple[str, str]]:
    checksums: set[tuple[str, str]] = set()
    for line in path.read_text(encoding="utf-8").splitlines():
        fields = line.split()
        if len(fields) == 3 and fields[2].startswith("h1:"):
            checksums.add((fields[0], fields[1]))
    return checksums


def parse_cmake(path: Path) -> tuple[str | None, str | None, list[str]]:
    text = path.read_text(encoding="utf-8")
    cmake_match = re.search(r"cmake_minimum_required\s*\(\s*VERSION\s+([0-9.]+)", text, re.I)
    qt_match = re.search(r"find_package\s*\(\s*Qt6\s+([0-9.]+)(.*?)\)", text, re.I | re.S)
    qt_version = qt_match.group(1) if qt_match else None
    components: list[str] = []
    if qt_match:
        body = qt_match.group(2)
        component_match = re.search(
            r"\bCOMPONENTS\s+(.*?)(?=\b(?:OPTIONAL_COMPONENTS|REQUIRED|QUIET|CONFIG|NO_MODULE)\b|$)",
            body,
            re.I | re.S,
        )
        if component_match:
            components = component_match.group(1).split()
    return cmake_match.group(1) if cmake_match else None, qt_version, sorted(set(components))


def build_inventory(root: Path) -> dict[str, Any]:
    root = root.resolve()
    go_mod = root / "backend" / "go.mod"
    go_sum = root / "backend" / "go.sum"
    cmake = root / "CMakeLists.txt"
    for required in (go_mod, go_sum, cmake):
        if not required.is_file():
            raise FileNotFoundError(f"required manifest missing: {required.relative_to(root)}")

    go_version, go_dependencies = parse_go_mod(go_mod)
    checksums = parse_go_sums(go_sum)
    missing_checksums = [
        f"{item['name']} {item['version']}"
        for item in go_dependencies
        if (item["name"], item["version"]) not in checksums
    ]
    cmake_minimum, qt_minimum, qt_components = parse_cmake(cmake)
    if qt_minimum is None or not qt_components:
        raise ValueError("CMakeLists.txt does not declare a parseable Qt6 minimum and component list")

    qt_dependencies = [
        {
            "ecosystem": "Qt",
            "name": f"Qt6::{component}",
            "version_constraint": f">={qt_minimum}",
            "dependency_type": "direct",
            "license_status": "not_assessed",
            "source": "CMakeLists.txt",
        }
        for component in qt_components
    ]
    license_path = root / "LICENSE"
    license_status = (
        "present_not_interpreted" if license_path.is_file() and license_path.stat().st_size else "missing"
    )
    return {
        "schema": "whatsfeather-dependency-inventory/v1",
        "network_access": False,
        "scope_note": (
            "Manifest inventory only. No registry, vulnerability database, or license text was queried; "
            "license_status=not_assessed is not a license approval."
        ),
        "project_license_status": license_status,
        "build_requirements": {
            "cmake_minimum": cmake_minimum,
            "go_language_version": go_version,
            "qt_minimum": qt_minimum,
            "python": "required by CMakeLists.txt; exact version not pinned",
            "cxx_standard": "20",
        },
        "dependencies": sorted(
            go_dependencies + qt_dependencies,
            key=lambda item: (item["ecosystem"], item["name"].casefold(), item.get("version", item.get("version_constraint", ""))),
        ),
        "integrity": {
            "go_requirements": len(go_dependencies),
            "go_requirements_with_h1_sum": len(go_dependencies) - len(missing_checksums),
            "go_requirements_missing_h1_sum": missing_checksums,
        },
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1], help="repository root")
    parser.add_argument("--format", choices=("text", "json"), default="text")
    parser.add_argument("--check", action="store_true", help="fail if a Go requirement lacks an h1 go.sum entry")
    args = parser.parse_args(argv)
    try:
        report = build_inventory(args.root)
    except (OSError, ValueError) as exc:
        print(f"dependency inventory error: {exc}", file=sys.stderr)
        return 2

    missing = report["integrity"]["go_requirements_missing_h1_sum"]
    if args.format == "json":
        print(json.dumps(report, indent=2, sort_keys=True))
    else:
        print("Offline dependency inventory (manifest-derived; licenses/vulnerabilities not assessed)")
        build = report["build_requirements"]
        print(f"Go requirements: {report['integrity']['go_requirements']} (missing h1 checksums: {len(missing)})")
        print(f"Qt baseline: >= {build['qt_minimum']} ({len([d for d in report['dependencies'] if d['ecosystem'] == 'Qt'])} components)")
        print(f"Project LICENSE file: {report['project_license_status']}")
        for dep in report["dependencies"]:
            version = dep.get("version", dep.get("version_constraint", ""))
            relation = f" [{dep['dependency_type']}]" if dep["ecosystem"] == "Go" else ""
            print(f"- {dep['ecosystem']}: {dep['name']} {version}{relation} — license not assessed")
        for missing_item in missing:
            print(f"FAIL missing go.sum h1 entry: {missing_item}")

    if args.check and missing:
        print(f"dependency check failed: {len(missing)} Go requirement(s) lack h1 checksums", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

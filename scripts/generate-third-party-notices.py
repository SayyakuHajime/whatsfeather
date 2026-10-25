#!/usr/bin/env python3
"""Render a deterministic inventory of vendored Go license and notice files."""

from __future__ import annotations

import argparse
import gzip
import hashlib
import re
import sys
import tarfile
import zlib
from dataclasses import dataclass
from pathlib import Path, PurePosixPath

MAX_LICENSE_FILE_BYTES = 2 * 1024 * 1024
MAX_TOTAL_LICENSE_BYTES = 16 * 1024 * 1024
MAX_MODULE_MANIFEST_BYTES = 4 * 1024 * 1024
MAX_ARCHIVE_COMPRESSED_BYTES = 64 * 1024 * 1024
MAX_ARCHIVE_EXPANDED_BYTES = 256 * 1024 * 1024
MAX_ARCHIVE_MEMBERS = 10_000
MAX_ARCHIVE_MEMBER_BYTES = 64 * 1024 * 1024
MAX_ARCHIVE_TOTAL_MEMBER_BYTES = 256 * 1024 * 1024
LICENSE_NAME = re.compile(r"(?:LICENSE|LICENCE|COPYING|COPYRIGHT|NOTICE|PATENTS)(?:[-_.][A-Za-z0-9][A-Za-z0-9._-]*)?\Z", re.IGNORECASE)

SYSTEM_RUNTIME_PACKAGES = (
    (
        "ffmpeg",
        "PKGBUILD runtime dependency",
        "GPL-3.0-only",
        "https://archlinux.org/packages/extra/x86_64/ffmpeg/",
    ),
    (
        "qt6-base",
        "PKGBUILD runtime dependency",
        "GPL-3.0-only; LGPL-3.0-only; LicenseRef-Qt-Commercial; Qt-GPL-exception-1.0",
        "https://archlinux.org/packages/extra/x86_64/qt6-base/",
    ),
    (
        "qt6-declarative",
        "PKGBUILD runtime dependency",
        "GPL-3.0-only; LGPL-3.0-only; LicenseRef-Qt-Commercial; Qt-GPL-exception-1.0",
        "https://archlinux.org/packages/extra/x86_64/qt6-declarative/",
    ),
    (
        "qt6-multimedia",
        "PKGBUILD runtime dependency",
        "GPL-3.0-only; LGPL-3.0-only; LicenseRef-Qt-Commercial; Qt-GPL-exception-1.0",
        "https://archlinux.org/packages/extra/x86_64/qt6-multimedia/",
    ),
    (
        "qt6-multimedia-ffmpeg",
        "Host provider of qt6-multimedia-backend; not a direct PKGBUILD dependency",
        "GPL-3.0-only; LGPL-3.0-only; LicenseRef-Qt-Commercial; Qt-GPL-exception-1.0",
        "https://archlinux.org/packages/extra/x86_64/qt6-multimedia-ffmpeg/",
    ),
    (
        "qt6-svg",
        "PKGBUILD runtime dependency",
        "GPL-3.0-only; LGPL-3.0-only; LicenseRef-Qt-Commercial; Qt-GPL-exception-1.0",
        "https://archlinux.org/packages/extra/x86_64/qt6-svg/",
    ),
)


@dataclass(frozen=True)
class LicenseDocument:
    module: str
    version: str
    path: str
    content: str
    sha256: str


def _module_entries(manifest: str, source: str) -> list[tuple[str, str]]:
    entries: dict[str, str] = {}
    for line_number, line in enumerate(manifest.splitlines(), start=1):
        if not line.startswith("# ") or line.startswith("## "):
            continue
        fields = line[2:].split()
        if len(fields) < 2:
            continue
        module, version = fields[:2]
        module_path = PurePosixPath(module)
        if (
            module_path.is_absolute()
            or module_path.as_posix() != module
            or not module_path.parts
            or any(part in {".", ".."} for part in module_path.parts)
            or "\\" in module
            or not version.startswith("v")
        ):
            raise ValueError(f"invalid module entry at {source}:{line_number}")
        previous = entries.get(module)
        if previous is not None and previous != version:
            raise ValueError(f"conflicting vendored versions for module {module}")
        entries[module] = version
    if not entries:
        raise ValueError("vendored modules manifest contains no module versions")
    return sorted(entries.items())


def _check_archive_expanded_size(archive_path: Path) -> None:
    total_bytes = 0
    try:
        with gzip.open(archive_path, "rb") as archive_stream:
            while True:
                remaining = MAX_ARCHIVE_EXPANDED_BYTES - total_bytes
                chunk = archive_stream.read(min(64 * 1024, remaining + 1))
                if not chunk:
                    return
                total_bytes += len(chunk)
                if total_bytes > MAX_ARCHIVE_EXPANDED_BYTES:
                    raise ValueError("vendored source archive exceeds the expanded byte limit")
    except (EOFError, OSError, zlib.error) as exc:
        raise ValueError(f"cannot decompress vendored source archive: {exc}") from exc


def _bounded_archive_members(archive: tarfile.TarFile) -> list[tarfile.TarInfo]:
    members: list[tarfile.TarInfo] = []
    total_bytes = 0
    for member in archive:
        if len(members) >= MAX_ARCHIVE_MEMBERS:
            raise ValueError("vendored source archive exceeds the member-count limit")
        if member.size < 0 or member.size > MAX_ARCHIVE_MEMBER_BYTES:
            raise ValueError(f"vendored source archive exceeds the member-size limit: {member.name}")
        total_bytes += member.size
        if total_bytes > MAX_ARCHIVE_TOTAL_MEMBER_BYTES:
            raise ValueError("vendored source archive exceeds the total declared member-size limit")
        members.append(member)
    return members


def _archive_module_entries(
    archive: tarfile.TarFile, archive_members: list[tarfile.TarInfo]
) -> list[tuple[str, str]]:
    members = [member for member in archive_members if member.name == "vendor/modules.txt"]
    if len(members) != 1:
        raise ValueError("vendored source archive must contain exactly one vendor/modules.txt")
    member = members[0]
    if not member.isfile():
        raise ValueError("vendored modules manifest is not a regular file")
    if member.size > MAX_MODULE_MANIFEST_BYTES:
        raise ValueError("vendored modules manifest exceeds the size limit")
    extracted = archive.extractfile(member)
    if extracted is None:
        raise ValueError("cannot read vendored modules manifest")
    raw = extracted.read(MAX_MODULE_MANIFEST_BYTES + 1)
    if len(raw) != member.size or len(raw) > MAX_MODULE_MANIFEST_BYTES:
        raise ValueError("vendored modules manifest size mismatch")
    try:
        manifest = raw.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise ValueError("vendored modules manifest is not UTF-8 text") from exc
    return _module_entries(manifest, f"{archive.name}:{member.name}")


def _license_documents(root: Path) -> list[LicenseDocument]:
    backend = root / "backend"
    archive_path = backend / "vendor.tar.gz"
    documents: list[LicenseDocument] = []
    total_bytes = 0
    seen: set[str] = set()

    if archive_path.stat().st_size > MAX_ARCHIVE_COMPRESSED_BYTES:
        raise ValueError("vendored source archive exceeds the compressed size limit")
    _check_archive_expanded_size(archive_path)

    with tarfile.open(archive_path, mode="r:gz") as archive:
        archive_members = _bounded_archive_members(archive)
        modules = _archive_module_entries(archive, archive_members)
        expected = {module: version for module, version in modules}
        for member in archive_members:
            parts = PurePosixPath(member.name).parts
            if len(parts) < 3 or parts[0] != "vendor":
                continue
            module = "/".join(parts[1:-1])
            filename = parts[-1]
            if module not in expected or not LICENSE_NAME.fullmatch(filename):
                continue
            if not member.isfile():
                raise ValueError(f"vendored license path is not a regular file: {member.name}")
            if member.size > MAX_LICENSE_FILE_BYTES:
                raise ValueError(f"vendored license file exceeds size limit: {member.name}")
            if member.name in seen:
                raise ValueError(f"duplicate vendored license path: {member.name}")
            seen.add(member.name)
            extracted = archive.extractfile(member)
            if extracted is None:
                raise ValueError(f"cannot read vendored license file: {member.name}")
            raw = extracted.read(MAX_LICENSE_FILE_BYTES + 1)
            if len(raw) != member.size or len(raw) > MAX_LICENSE_FILE_BYTES:
                raise ValueError(f"vendored license size mismatch: {member.name}")
            total_bytes += len(raw)
            if total_bytes > MAX_TOTAL_LICENSE_BYTES:
                raise ValueError("vendored license texts exceed the total size limit")
            try:
                content = raw.decode("utf-8")
            except UnicodeDecodeError as exc:
                raise ValueError(f"vendored license is not UTF-8 text: {member.name}") from exc
            documents.append(
                LicenseDocument(
                    module=module,
                    version=expected[module],
                    path=member.name,
                    content=content,
                    sha256=hashlib.sha256(raw).hexdigest(),
                )
            )

    modules_with_notices = {document.module for document in documents}
    missing = sorted(set(expected) - modules_with_notices)
    if missing:
        raise ValueError("vendored module has no root license or notice file: " + ", ".join(missing))
    return sorted(documents, key=lambda document: (document.module, document.path.casefold()))


def _fence(content: str) -> str:
    longest = max((len(run) for run in re.findall(r"`+", content)), default=0)
    return "`" * max(3, longest + 1)


def render_notices(root: Path) -> str:
    documents = _license_documents(root)
    modules = sorted({(document.module, document.version) for document in documents})
    lines = [
        "# Third-party software notices",
        "",
        "This file reproduces root-level license, copyright, patent, and notice files",
        "for the Go modules listed in the `vendor/modules.txt` manifest inside the",
        "pinned `backend/vendor.tar.gz` source archive. Versions and file contents are",
        "read from that archive. Run",
        "`python3 scripts/generate-third-party-notices.py --check` to verify this file.",
        "",
        "This is a mechanical attribution inventory, not a legal opinion, a license-",
        "compatibility decision, source-provenance proof, or approval to redistribute.",
        "Original WhatsFeather code and the separately listed assets are licensed as",
        "described by the root `LICENSE` and `LICENSES/ASSET-LICENSES.md`.",
        "",
        f"Vendored Go modules: {len(modules)}; reproduced root notice files: {len(documents)}.",
        "",
        "## Vendored Go modules",
        "",
    ]
    for document in documents:
        lines.extend(
            [
                f"### `{document.module}` `{document.version}` — `{document.path.rsplit('/', 1)[-1]}`",
                "",
                f"Source path: `{document.path}`",
                "",
                f"SHA-256: `{document.sha256}`",
                "",
            ]
        )
        fence = _fence(document.content)
        body = document.content
        if not body.endswith("\n"):
            body += "\n"
        lines.extend([f"{fence}text", body.rstrip("\n"), fence, ""])

    lines.extend(
        [
            "## System-provided runtime packages",
            "",
            "The Arch/CachyOS PKGBUILD directly declares `ffmpeg` and the listed Qt6",
            "base, declarative, multimedia, and svg runtime dependencies. The",
            "`qt6-multimedia-ffmpeg` row records a package observed on the build host;",
            "it is not a direct PKGBUILD dependency. None is bundled in the application.",
            "These license expressions",
            "below are package metadata, not a complete inventory of transitive system",
            "libraries. The target distribution supplies the corresponding license",
            "files with each package; verify downstream metadata for other distributions.",
            "",
            "| Package | Role | Arch package license metadata | Package record |",
            "|---|---|---|---|",
        ]
    )
    for package, role, licenses, url in SYSTEM_RUNTIME_PACKAGES:
        lines.append(f"| `{package}` | {role} | {licenses} | [Arch Linux]({url}) |")
    lines.extend(
        [
            "",
            "Qt package metadata includes GPL-3.0-only, LGPL-3.0-only,",
            "`LicenseRef-Qt-Commercial`, and `Qt-GPL-exception-1.0`; FFmpeg package",
            "metadata reports GPL-3.0-only. These external packages remain governed by",
            "their own terms and are not relicensed by the WhatsFeather project license.",
            "",
        ]
    )
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="fail if the checked-in file is stale")
    args = parser.parse_args(argv)
    root = Path(__file__).resolve().parents[1]
    target = root / "LICENSES" / "THIRD-PARTY-NOTICES.md"
    try:
        rendered = render_notices(root)
    except (OSError, tarfile.TarError, ValueError) as exc:
        print(f"third-party notice generation failed: {exc}", file=sys.stderr)
        return 2
    if args.check:
        try:
            current = target.read_text(encoding="utf-8")
        except OSError as exc:
            print(f"third-party notice file is unavailable: {exc}", file=sys.stderr)
            return 1
        if current != rendered:
            print("third-party notices are stale; regenerate with scripts/generate-third-party-notices.py")
            return 1
        print(f"PASS: {target.relative_to(root)} matches {len(_license_documents(root))} vendored notice files")
        return 0
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(rendered, encoding="utf-8", newline="\n")
    print(f"Wrote {target.relative_to(root)} from {len(_license_documents(root))} vendored notice files")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

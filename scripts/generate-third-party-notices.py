#!/usr/bin/env python3
"""Render a deterministic inventory of vendored Go license and notice files."""

from __future__ import annotations

import argparse
import gzip
import hashlib
import re
import stat
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
        "PKGBUILD runtime dependency; minimum reviewed package version 2:9.0.2-2.3",
        "GPL-3.0-or-later (reviewed candidate .PKGINFO metadata; not legal approval)",
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
        "qt6-imageformats",
        "PKGBUILD runtime dependency",
        "GPL-3.0-only; LGPL-3.0-only; LicenseRef-Qt-Commercial; Qt-GPL-exception-1.0",
        "https://archlinux.org/packages/extra/x86_64/qt6-imageformats/",
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
    (
        "gocryptfs",
        "PKGBUILD exact runtime dependency 2.6.1.r20261022.ga8b36f0-1; provides `/usr/bin/gocryptfs`",
        "Not independently assessed in this inventory",
        "https://archlinux.org/packages/extra/x86_64/gocryptfs/",
    ),
    (
        "libsecret",
        "PKGBUILD runtime dependency; Provides the `secret-tool` executable at `/usr/bin/secret-tool`",
        "Not independently assessed in this inventory",
        "https://archlinux.org/packages/extra/x86_64/libsecret/",
    ),
    (
        "fuse3",
        "PKGBUILD runtime dependency; Provides the `fusermount3` helper at `/usr/bin/fusermount3`",
        "Not independently assessed in this inventory",
        "https://archlinux.org/packages/extra/x86_64/fuse3/",
    ),
)

MAX_REVIEWED_GENERATED_SOURCE_BYTES = 512 * 1024
MAX_SUPPLEMENTAL_LICENSE_BYTES = 1024 * 1024
MODERNC_MODULE = "modernc.org/libc"
MODERNC_VERSION = "v1.74.4"
MODERNC_GO_SUM_H1 = "h1:fX1Omw4o2/1C2iRkkIsrQTasJQldLhRmuPreXLoWs9k="
MODERNC_VENDOR_ARCHIVE_SHA256 = "5987becd4d02cc9b462767606577053d8c8af9c3d8f9f399431670c44bd0c111"
AGE_MODULE = "filippo.io/age"
AGE_VERSION = "v1.3.2"
AGE_BECH32_SOURCE_PATH = "vendor/filippo.io/age/internal/bech32/bech32.go"
AGE_BECH32_SOURCE_SHA256 = "b541ad4caca899fb3e11902aa3f478321fe9d623edbf43a76dd306433a7b0436"
AGE_BECH32_NOTICE_START = "// Copyright (c) 2017 Takatoshi Nakagawa"
AGE_BECH32_NOTICE_END = "// THE SOFTWARE."
GO_RUNTIME_SOURCE_PIN = "go1.27.3"
GO_RUNTIME_SOURCE_URL = "https://go.googlesource.com/go/+/refs/tags/go1.27.3/LICENSE"
GO_RUNTIME_TOOLCHAIN_ID = "go version go1.27.3-X:nodwarf5 linux/amd64"
GO_RUNTIME_TOOLCHAIN_BINARY_SHA256 = "21401c8d2dae2e6db0d756f7b9ae5a885ac6c14882cbbe521c6f49b184da8402"
GO_RUNTIME_VERSION_FILE_SHA256 = "49b1651ee92b70282766993b556905e9464050a811572e047a74f95534ee9ce6"
MODERNC_GENERATED_SOURCES = (
    (
        "vendor/modernc.org/libc/sys/types/types_linux_amd64.go",
        "1ada8b2599b13281b8fe191cfb24c02896ec3781b95a3b5cf76e1d74424eea2e",
        ((
            "Observed LGPL-2.1-or-later header attribution",
            "// Copyright (C) 1991-2020 Free Software Foundation, Inc.",
            "//    <https://www.gnu.org/licenses/>.",
        ),),
    ),
    (
        "vendor/modernc.org/libc/uuid/uuid/uuid_linux_amd64.go",
        "c677ffac9af69102fabc82443b8799b4071c7ead9f02237bb2f01af7782af428",
        (
            (
                "Observed BSD-style UUID attribution and permission notice",
                "// Public include file for the UUID library",
                "// %End-Header%",
            ),
            (
                "Observed LGPL-2.1-or-later header attribution",
                "// Copyright (C) 1991-2020 Free Software Foundation, Inc.",
                "//    <https://www.gnu.org/licenses/>.",
            ),
            (
                "Observed GPL-3.0-or-later WITH GCC-exception-3.1 header attribution",
                "// This file is part of GCC.",
                "// <http://www.gnu.org/licenses/>.",
            ),
        ),
    ),
)
SUPPLEMENTAL_LICENSES = (
    (
        "GNU Lesser General Public License version 2.1",
        "LICENSES/supplemental/GNU-LGPL-2.1.txt",
        "20e50fe7aae3e56378ebf0417d9de904f55a0e61e4df315333e632a4d3555d95",
        "https://www.gnu.org/licenses/old-licenses/lgpl-2.1.txt",
    ),
    (
        "GCC Runtime Library Exception version 3.1",
        "LICENSES/supplemental/GCC-Runtime-Library-Exception-3.1.txt",
        "9d6b43ce4d8de0c878bf16b54d8e7a10d9bd42b75178153e3af6a815bdc90f74",
        "https://gnu.googlesource.com/gcc/+/f71fe877da75e9be746b6bbc27cea8007c83c983/COPYING.RUNTIME",
    ),
)
GO_RUNTIME_LICENSE = (
    "Go toolchain runtime license",
    "LICENSES/supplemental/go-toolchain-LICENSE.txt",
    "911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad",
    GO_RUNTIME_SOURCE_URL,
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


def _validate_archive_member_name(name: str) -> PurePosixPath:
    try:
        name.encode("utf-8")
    except UnicodeEncodeError as exc:
        raise ValueError("non-canonical vendored archive member path") from exc
    path = PurePosixPath(name)
    if (
        not name
        or "\\" in name
        or any(ord(character) < 32 or ord(character) == 127 for character in name)
        or path.is_absolute()
        or path.as_posix() != name
        or not path.parts
        or any(part in {".", ".."} for part in path.parts)
        or re.match(r"^[A-Za-z]:", path.parts[0])
    ):
        raise ValueError(f"non-canonical vendored archive member path: {name!r}")
    return path


def _bounded_archive_members(archive: tarfile.TarFile) -> list[tarfile.TarInfo]:
    members: list[tarfile.TarInfo] = []
    total_bytes = 0
    seen_names: set[str] = set()
    for member in archive:
        if len(members) >= MAX_ARCHIVE_MEMBERS:
            raise ValueError("vendored source archive exceeds the member-count limit")
        _validate_archive_member_name(member.name)
        if member.name in seen_names:
            raise ValueError(f"duplicate vendored archive member path: {member.name}")
        seen_names.add(member.name)
        if not member.isfile() and not member.isdir():
            raise ValueError(f"unsupported vendored archive member type: {member.name}")
        if member.isdir() and member.size != 0:
            raise ValueError(f"vendored archive directory member has nonzero size: {member.name}")
        if member.size < 0 or member.size > MAX_ARCHIVE_MEMBER_BYTES:
            raise ValueError(f"vendored source archive exceeds the member-size limit: {member.name}")
        total_bytes += member.size
        if total_bytes > MAX_ARCHIVE_TOTAL_MEMBER_BYTES:
            raise ValueError("vendored source archive exceeds the total declared member-size limit")
        members.append(member)
    return members


def _read_archive_member(archive: tarfile.TarFile, member: tarfile.TarInfo, limit: int, label: str) -> bytes:
    if not member.isfile():
        raise ValueError(f"{label} is not a regular file: {member.name}")
    if member.size > limit:
        raise ValueError(f"{label} exceeds the size limit: {member.name}")
    extracted = archive.extractfile(member)
    if extracted is None:
        raise ValueError(f"cannot read {label}: {member.name}")
    raw = extracted.read(limit + 1)
    if len(raw) != member.size or len(raw) > limit:
        raise ValueError(f"{label} size mismatch: {member.name}")
    return raw


def _archive_module_entries(
    archive: tarfile.TarFile, archive_members: list[tarfile.TarInfo]
) -> list[tuple[str, str]]:
    members = [member for member in archive_members if member.name == "vendor/modules.txt"]
    if len(members) != 1:
        raise ValueError("vendored source archive must contain exactly one vendor/modules.txt")
    raw = _read_archive_member(archive, members[0], MAX_MODULE_MANIFEST_BYTES, "vendored modules manifest")
    try:
        manifest = raw.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise ValueError("vendored modules manifest is not UTF-8 text") from exc
    return _module_entries(manifest, f"{archive.name}:vendor/modules.txt")


def _module_for_notice_path(path: PurePosixPath, modules: dict[str, str]) -> str | None:
    if len(path.parts) < 3 or path.parts[0] != "vendor":
        return None
    module_directory = path.parts[1:-1]
    matches = [
        module
        for module in modules
        if len(PurePosixPath(module).parts) <= len(module_directory)
        and module_directory[:len(PurePosixPath(module).parts)] == PurePosixPath(module).parts
    ]
    if not matches:
        return None
    return max(matches, key=lambda module: len(PurePosixPath(module).parts))


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _vendored_material(root: Path) -> tuple[list[LicenseDocument], dict[str, str]]:
    archive_path = root / "backend" / "vendor.tar.gz"
    archive_stat = archive_path.lstat()
    if not stat.S_ISREG(archive_stat.st_mode):
        raise ValueError("vendored source archive is not a regular file")
    if archive_stat.st_size > MAX_ARCHIVE_COMPRESSED_BYTES:
        raise ValueError("vendored source archive exceeds the compressed size limit")
    _check_archive_expanded_size(archive_path)

    documents: list[LicenseDocument] = []
    generated_sources: dict[str, str] = {}
    total_bytes = 0
    seen: set[str] = set()

    with tarfile.open(archive_path, mode="r:gz") as archive:
        archive_members = _bounded_archive_members(archive)
        modules = dict(_archive_module_entries(archive, archive_members))
        has_reviewed_module = MODERNC_MODULE in modules
        if has_reviewed_module:
            if modules[MODERNC_MODULE] != MODERNC_VERSION:
                raise ValueError(
                    f"reviewed generated-source coverage requires {MODERNC_MODULE}@{MODERNC_VERSION}; "
                    "review the new vendored version before regeneration"
                )
            if _sha256_file(archive_path) != MODERNC_VENDOR_ARCHIVE_SHA256:
                raise ValueError("vendored archive changed from the reviewed modernc source identity")
            sum_path = root / "backend" / "go.sum"
            expected_sum = f"{MODERNC_MODULE} {MODERNC_VERSION} {MODERNC_GO_SUM_H1}"
            try:
                go_sum_lines = sum_path.read_text(encoding="utf-8").splitlines()
            except OSError as exc:
                raise ValueError("go.sum is unavailable for reviewed modernc source identity") from exc
            if expected_sum not in go_sum_lines:
                raise ValueError("go.sum does not contain the reviewed modernc module checksum")

        expected_sources = {path: digest for path, digest, _ in MODERNC_GENERATED_SOURCES} if has_reviewed_module else {}
        age_version = modules.get(AGE_MODULE)
        if age_version is not None:
            if age_version != AGE_VERSION:
                raise ValueError(
                    f"reviewed inline-source coverage requires {AGE_MODULE}@{AGE_VERSION}; "
                    "review the new vendored version before regeneration"
                )
            expected_sources[AGE_BECH32_SOURCE_PATH] = AGE_BECH32_SOURCE_SHA256
        for member in archive_members:
            archive_path_value = _validate_archive_member_name(member.name)
            if member.name in expected_sources:
                raw = _read_archive_member(
                    archive, member, MAX_REVIEWED_GENERATED_SOURCE_BYTES, "reviewed generated source"
                )
                digest = hashlib.sha256(raw).hexdigest()
                if digest != expected_sources[member.name]:
                    raise ValueError(f"reviewed generated source changed; re-review required: {member.name}")
                try:
                    generated_sources[member.name] = raw.decode("utf-8")
                except UnicodeDecodeError as exc:
                    raise ValueError(f"reviewed generated source is not UTF-8 text: {member.name}") from exc

            module = _module_for_notice_path(archive_path_value, modules)
            filename = archive_path_value.name
            if module is None or not LICENSE_NAME.fullmatch(filename):
                continue
            if member.name in seen:
                raise ValueError(f"duplicate vendored license path: {member.name}")
            seen.add(member.name)
            raw = _read_archive_member(archive, member, MAX_LICENSE_FILE_BYTES, "vendored license file")
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
                    version=modules[module],
                    path=member.name,
                    content=content,
                    sha256=hashlib.sha256(raw).hexdigest(),
                )
            )

    modules_with_notices = {document.module for document in documents}
    missing = sorted(set(modules) - modules_with_notices)
    if missing:
        raise ValueError("vendored module has no root license or notice file: " + ", ".join(missing))
    missing_sources = sorted(set(expected_sources) - generated_sources.keys())
    if missing_sources:
        raise ValueError("reviewed generated source is missing; re-review required: " + ", ".join(missing_sources))
    return sorted(documents, key=lambda document: (document.module, document.path.casefold())), generated_sources


def _license_documents(root: Path) -> list[LicenseDocument]:
    documents, _ = _vendored_material(root)
    return documents


def _source_excerpt(source: str, start: str, end: str, path: str) -> str:
    lines = source.splitlines()
    try:
        start_index = lines.index(start)
        end_index = next(index for index in range(start_index, len(lines)) if lines[index] == end)
    except (ValueError, StopIteration) as exc:
        raise ValueError(f"reviewed source header excerpt changed; re-review required: {path}") from exc
    return "\n".join(lines[start_index:end_index + 1]) + "\n"


def _supplemental_license(root: Path, label: str, relative_path: str, expected_sha256: str, origin: str) -> tuple[str, str, str]:
    path = root / relative_path
    file_stat = path.lstat()
    if not stat.S_ISREG(file_stat.st_mode):
        raise ValueError(f"supplemental license text is not a regular file: {relative_path}")
    if file_stat.st_size > MAX_SUPPLEMENTAL_LICENSE_BYTES:
        raise ValueError(f"supplemental license text exceeds the size limit: {relative_path}")
    raw = path.read_bytes()
    if hashlib.sha256(raw).hexdigest() != expected_sha256:
        raise ValueError(f"supplemental license text differs from canonical source: {relative_path}")
    try:
        content = raw.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise ValueError(f"supplemental license text is not UTF-8: {relative_path}") from exc
    return label, content, f"{relative_path} — SHA-256 `{expected_sha256}`; source: <{origin}>"


def _fence(content: str) -> str:
    longest = max((len(run) for run in re.findall(r"`+", content)), default=0)
    return "`" * max(3, longest + 1)


def _render_modernc_generated_source_coverage(root: Path, generated_sources: dict[str, str]) -> list[str]:
    if not generated_sources:
        return []
    backend = root / "backend"
    archive_sha = _sha256_file(backend / "vendor.tar.gz")
    lines = [
        "## Reviewed modernc generated-source header coverage",
        "",
        f"This bounded review is tied to `modernc.org/libc@{MODERNC_VERSION}` in `backend/vendor.tar.gz` ",
        f"(SHA-256 `{archive_sha}`) and `backend/go.sum` checksum `{MODERNC_GO_SUM_H1}`. ",
        "It retains the reviewed Linux/amd64 generated-file header notices only; it is not a scan or ",
        "license classification of all generated sources.",
        "",
        "The following are **observed source-header terms and attributions**, not a legal determination ",
        "that any term applies to all generated declarations or the resulting executable:",
        "",
    ]
    for path, expected_sha256, excerpts in MODERNC_GENERATED_SOURCES:
        source = generated_sources[path]
        lines.extend([f"### `{path}`", "", f"SHA-256: `{expected_sha256}`", ""])
        for label, start, end in excerpts:
            excerpt = _source_excerpt(source, start, end, path)
            lines.extend([f"**{label}.**", ""])
            fence = _fence(excerpt)
            lines.extend([f"{fence}go", excerpt.rstrip("\n"), fence, ""])

    lines.extend(
        [
            "Observed terms are LGPL-2.1-or-later in the GNU C Library-derived header; GPL-3.0-or-later ",
            "WITH GCC-exception-3.1 in the GCC header; and the BSD-style Theodore Ts'o UUID notice. ",
            "The complete canonical LGPL-2.1 and GCC Runtime Library Exception 3.1 texts follow below. ",
            "The root `LICENSE` already contains the project GPL-3 text; these notices do not alter it.",
            "",
            "Header presence alone does not settle whether terms apply to generated declarations, retained ",
            "executable material, or distribution. Applicability, static-link/relink route, source delivery, ",
            "and the corresponding-source/reproducible-build package require legal and maintainer review. ",
            "The source availability and reproducible build instructions remain unresolved; this section ",
            "does not provide them.",
            "",
        ]
    )
    for label, relative_path, expected_sha256, origin in SUPPLEMENTAL_LICENSES:
        title, content, provenance = _supplemental_license(
            root, label, relative_path, expected_sha256, origin
        )
        lines.extend([f"### {title}", "", provenance, ""])
        fence = _fence(content)
        lines.extend([f"{fence}text", content.rstrip("\n"), fence, ""])
    return lines


def _render_age_bech32_source_notice(archive_sha256: str, generated_sources: dict[str, str]) -> list[str]:
    source = generated_sources.get(AGE_BECH32_SOURCE_PATH)
    if source is None:
        return []
    excerpt = _source_excerpt(
        source, AGE_BECH32_NOTICE_START, AGE_BECH32_NOTICE_END, AGE_BECH32_SOURCE_PATH
    )
    lines = [
        "## Reviewed age/bech32 inline source notice",
        "",
        f"This complete 20-line inline permission notice is reproduced from `{AGE_BECH32_SOURCE_PATH}`",
        f"in `filippo.io/age@{AGE_VERSION}` inside `backend/vendor.tar.gz` (SHA-256 `{archive_sha256}`).",
        f"Source SHA-256: `{AGE_BECH32_SOURCE_SHA256}`. The source is size-bounded and its hash is",
        "checked before this excerpt is rendered. This coverage is for the vendored source notice; it",
        "does not assert that the source's symbols are retained in a particular executable.",
        "",
        "The excerpt is copied verbatim, including source comment markers:",
        "",
    ]
    fence = _fence(excerpt)
    lines.extend([f"{fence}go", excerpt.rstrip("\n"), fence, ""])
    return lines


def _render_go_runtime_toolchain_coverage(root: Path) -> list[str]:
    title, content, provenance = _supplemental_license(root, *GO_RUNTIME_LICENSE)
    lines = [
        "## Go runtime toolchain license and source identity",
        "",
        "This explicit hash-checked supplement is separate from the Go module archive and filename collector.",
        f"Upstream Go source pin: `{GO_RUNTIME_SOURCE_PIN}` (`go tool dist version`; upstream source tag).",
        f"Toolchain identity: `{GO_RUNTIME_TOOLCHAIN_ID}`.",
        f"Toolchain executable SHA-256: `{GO_RUNTIME_TOOLCHAIN_BINARY_SHA256}`.",
        f"GOROOT/VERSION SHA-256: `{GO_RUNTIME_VERSION_FILE_SHA256}`.",
        "The toolchain identity above includes the local `X:nodwarf5` build suffix; the upstream source pin",
        "does not claim that this distribution-specific executable was rebuilt byte-for-byte from the tag.",
        "",
        f"Canonical {title} source observed: `/usr/share/licenses/go/LICENSE`; {provenance}.",
        "The complete canonical distribution license follows:",
        "",
    ]
    fence = _fence(content)
    lines.extend([f"{fence}text", content.rstrip("\n"), fence, ""])
    lines.extend(
        [
            "This records the canonical toolchain license, not every Go standard-library inline header or",
            "a symbol-retention analysis of a final product executable. Recheck the exact shipped binary's",
            "source/runtime notice closure before release. This is not a legal approval.",
            "",
        ]
    )
    return lines


def render_notices(root: Path) -> str:
    documents, generated_sources = _vendored_material(root)
    modules = sorted({(document.module, document.version) for document in documents})
    lines = [
        "# Third-party software notices",
        "",
        "This file mechanically reproduces license, copyright, patent, and notice files located within the",
        "vendored Go module trees listed in `vendor/modules.txt` in the pinned `backend/vendor.tar.gz` archive.",
        "Root module texts are selected by filename; inline source headers and the Go toolchain license are",
        "not discovered by that filename collector and are included only in the explicit hash-anchored",
        "sections below. Run `python3 scripts/generate-third-party-notices.py --check` to verify this file.",
        "",
        "This is an attribution inventory, not a legal opinion, license-compatibility decision, source-",
        "provenance proof, or approval to redistribute. Original WhatsFeather code and separately listed",
        "assets are licensed as described by the root `LICENSE` and `LICENSES/ASSET-LICENSES.md`.",
        "",
        f"Vendored Go modules: {len(modules)}; reproduced vendored notice files: {len(documents)}.",
        "Those counts exclude the separate hash-anchored source-header and Go toolchain sections.",
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

    lines.extend(_render_modernc_generated_source_coverage(root, generated_sources))
    archive_sha256 = _sha256_file(root / "backend" / "vendor.tar.gz")
    lines.extend(_render_age_bech32_source_notice(archive_sha256, generated_sources))
    lines.extend(_render_go_runtime_toolchain_coverage(root))
    lines.extend(
        [
            "## System-provided runtime packages",
            "",
            "Qt and FFmpeg are system-provided, not bundled in the app artifact. The current PKGBUILD",
            "directly declares `ffmpeg>=2:9.0.2-2.3`, `gocryptfs=2.6.1.r20261022.ga8b36f0-1`, `libsecret`,",
            "`fuse3`, and Qt6 `base`, `declarative`, `imageformats`, `multimedia`, and `svg`. The minimum FFmpeg package version is the reviewed candidate baseline; future FFmpeg versions are not pre-approved and still require security and ABI review.",
            "The `qt6-multimedia-ffmpeg` row records a host-side plugin provider, not a direct PKGBUILD dependency.",
            "",
            "The encrypted-profile implementation invokes `/usr/bin/gocryptfs`, `/usr/bin/secret-tool`,",
            "and `/usr/bin/fusermount3`. The declared exact `gocryptfs` package provides the engine;",
            "`libsecret` provides the `secret-tool` executable; and `fuse3` provides the `fusermount3`",
            "helper. These are explicitly declared runtime dependencies. License metadata for these three",
            "packages is not independently assessed here.",
            "",
            "Package license expressions below are distro metadata, not a complete inventory of transitive system libraries. Qt license choices are alternatives in package metadata; this inventory does not select one or assert all listed options are required. The target distribution supplies corresponding license files with each package; verify downstream metadata for other distributions.",
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
            "Qt package metadata lists GPL-3.0-only, LGPL-3.0-only, `LicenseRef-Qt-Commercial`, and",
            "`Qt-GPL-exception-1.0`; these are metadata choices, not a claim that every option is required.",
            "The reviewed candidate FFmpeg package's `.PKGINFO` reports `GPL-3.0-or-later`. This is package metadata, not legal approval, and does not change the project's authorized `GPL-3.0-only` selection. External packages remain governed by their own terms and are not relicensed by the WhatsFeather project license.",
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

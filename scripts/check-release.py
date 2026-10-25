#!/usr/bin/env python3
"""Offline source secret and dependency-integrity gate for verification/releases.

The scanner reads only non-ignored Git candidate text files. It explicitly skips
.env files, local profiles, credential directories, ignored files and symlinks.
It never prints a matched value. This is not a vulnerability-database scan,
license approval, signing operation, or release attestation.
"""

from __future__ import annotations

import argparse
import importlib.util
import math
import re
import stat
import subprocess
import sys
from collections import Counter
from pathlib import Path
from typing import Any

TEXT_SUFFIXES = {
    ".c", ".cc", ".cpp", ".css", ".desktop", ".diff", ".go", ".h", ".hpp", ".html", ".ini",
    ".java", ".js", ".jsx", ".json", ".lock", ".m", ".md", ".mod", ".mm", ".patch",
    ".properties", ".py", ".qml", ".rs", ".sh", ".sum", ".svg", ".toml", ".ts", ".tsx",
    ".txt", ".xml", ".yaml", ".yml", ".cmake", ".cfg", ".csv", ".rst", ".sql",
}
TEXT_NAMES = {
    "CMakeLists.txt", "Dockerfile", "Makefile", "LICENSE", "COPYING", "NOTICE",
    "AUTHORS", "CONTRIBUTING", ".editorconfig", ".gitignore", ".gitattributes",
}
PRIVATE_PARTS = {".git", ".hermes", "profiles", "credentials"}
SENSITIVE_SUFFIXES = {".db", ".sqlite", ".sqlite3", ".pem", ".key", ".log"}
MAX_TEXT_FILE_BYTES = 2 * 1024 * 1024

KNOWN_SECRET_RULES: tuple[tuple[str, re.Pattern[str]], ...] = (
    (
        "private-key-marker",
        re.compile(r"-----BEGIN (?:(?:RSA|EC|DSA|OPENSSH|ENCRYPTED) )?PRIVATE KEY-----"),
    ),
    ("aws-access-key", re.compile(r"\b(?:AKIA|ASIA)[A-Z0-9]{16}\b")),
    ("github-token", re.compile(r"\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b")),
    ("slack-token", re.compile(r"\bxox[baprs]-[A-Za-z0-9-]{20,}\b")),
    ("google-api-key", re.compile(r"\bAIza[0-9A-Za-z_-]{35}\b")),
    ("telegram-bot-token", re.compile(r"\b[0-9]{8,10}:[A-Za-z0-9_-]{30,}\b")),
)
ASSIGNMENT_SECRET = re.compile(
    r"\b(api[_-]?key|access[_-]?token|client[_-]?secret|password|passwd|"
    r"auth[_-]?token|refresh[_-]?token|private[_-]?key)\b\s*[\"']?\s*[:=]\s*[\"']([^\"'\r\n]{16,})[\"']",
    re.IGNORECASE,
)
PLACEHOLDER = re.compile(
    r"(?:^|[^a-z0-9])(?:example|changeme|change-me|placeholder|redacted|dummy|fake|sample|"
    r"your[_ -]?(?:key|token|secret|password)|todo|notasecret|not-a-secret)(?:$|[^a-z0-9])",
    re.IGNORECASE,
)


def candidate_paths(root: Path) -> list[Path]:
    """Return safe Git candidates; Git excludes ignored local artifacts."""
    result = subprocess.run(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"],
        cwd=root,
        check=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    listed: list[Path] = []
    for raw in result.stdout.split(b"\0"):
        if raw:
            listed.append(Path(raw.decode("utf-8", errors="surrogateescape")))

    # Force-added tracked files can still match ignore rules. Exclude them too,
    # so ignored private artifacts are never scan inputs.
    ignored_result = subprocess.run(
        ["git", "check-ignore", "--no-index", "-z", "--stdin"],
        cwd=root,
        input=b"\0".join(path.as_posix().encode("utf-8", errors="surrogateescape") for path in listed) + b"\0",
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if ignored_result.returncode not in (0, 1):
        raise subprocess.CalledProcessError(ignored_result.returncode, ignored_result.args)
    ignored = {
        Path(raw.decode("utf-8", errors="surrogateescape"))
        for raw in ignored_result.stdout.split(b"\0") if raw
    }

    paths: list[Path] = []
    for relative in listed:
        if relative in ignored:
            continue
        parts = relative.parts
        name = relative.name
        if not parts or any(part.casefold() in PRIVATE_PARTS for part in parts):
            continue
        if name.casefold().startswith(".env"):
            continue
        if relative.suffix.casefold() in SENSITIVE_SUFFIXES:
            continue
        if relative.suffix.casefold() not in TEXT_SUFFIXES and name not in TEXT_NAMES:
            continue
        path = root / relative
        try:
            file_stat = path.lstat()
        except OSError as exc:
            raise OSError(f"cannot inspect candidate path: {relative}") from exc
        if stat.S_ISLNK(file_stat.st_mode) or not stat.S_ISREG(file_stat.st_mode):
            continue
        try:
            resolved = path.resolve(strict=True)
            resolved.relative_to(root.resolve())
        except (OSError, ValueError) as exc:
            raise OSError(f"candidate path is not contained in the repository: {relative}") from exc
        if file_stat.st_size > MAX_TEXT_FILE_BYTES:
            raise OSError(f"candidate text exceeds scanner size limit: {relative}")
        paths.append(relative)
    return sorted(paths, key=lambda path: path.as_posix().casefold())


def _entropy(value: str) -> float:
    counts = Counter(value)
    length = len(value)
    return -sum((count / length) * math.log2(count / length) for count in counts.values())


def scan_text(text: str) -> list[tuple[int, str]]:
    """Return only line/rule metadata, never matched secret text."""
    findings: list[tuple[int, str]] = []
    for number, line in enumerate(text.splitlines(), start=1):
        seen: set[str] = set()
        for rule, pattern in KNOWN_SECRET_RULES:
            if pattern.search(line):
                seen.add(rule)
        assignment = ASSIGNMENT_SECRET.search(line)
        if assignment:
            value = assignment.group(2).strip()
            alpha_classes = sum((any(ch.islower() for ch in value), any(ch.isupper() for ch in value),
                                 any(ch.isdigit() for ch in value), any(not ch.isalnum() for ch in value)))
            if len(value) >= 24 and alpha_classes >= 3 and _entropy(value) >= 3.45 and not PLACEHOLDER.search(value):
                seen.add("high-entropy-secret-assignment")
        findings.extend((number, rule) for rule in sorted(seen))
    return findings


def scan_repository(root: Path) -> tuple[int, list[tuple[str, int, str]]]:
    findings: list[tuple[str, int, str]] = []
    candidates = candidate_paths(root)
    for relative in candidates:
        path = root / relative
        try:
            data = path.read_bytes()
        except OSError as exc:
            raise OSError(f"cannot read candidate for secret scan: {relative}") from exc
        if b"\0" in data:
            raise OSError(f"candidate text file contains a NUL byte: {relative}")
        text = data.decode("utf-8", errors="replace")
        for line, rule in scan_text(text):
            findings.append((relative.as_posix(), line, rule))
    return len(candidates), findings


def _load_inventory_module() -> Any:
    module_path = Path(__file__).with_name("dependency-inventory.py")
    spec = importlib.util.spec_from_file_location("whatsfeather_dependency_inventory", module_path)
    if spec is None or spec.loader is None:
        raise RuntimeError("cannot load dependency-inventory.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _load_notice_generator() -> Any:
    module_path = Path(__file__).with_name("generate-third-party-notices.py")
    spec = importlib.util.spec_from_file_location("whatsfeather_third_party_notice_generator", module_path)
    if spec is None or spec.loader is None:
        raise RuntimeError("cannot load generate-third-party-notices.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def forbidden_tracked_paths(root: Path) -> list[str]:
    """Inspect index names only, never open forbidden content, even if force-added."""
    tracked = subprocess.run(["git", "ls-files", "--cached", "-z"], cwd=root,
                             check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    forbidden = []
    for raw in tracked.stdout.split(b"\0"):
        if not raw:
            continue
        path = Path(raw.decode("utf-8", errors="surrogateescape"))
        if (any(part.casefold() in PRIVATE_PARTS for part in path.parts) or
                path.name.casefold().startswith(".env") and path.name != ".env.example" or
                path.suffix.casefold() in SENSITIVE_SUFFIXES or
                any(part in {"build", "dist", "__pycache__"} for part in path.parts)):
            forbidden.append(path.as_posix())
    return sorted(set(forbidden))


def evaluate(root: Path, mode: str) -> tuple[list[str], int, int]:
    errors: list[str] = [f"forbidden publication path: {path}" for path in forbidden_tracked_paths(root)]
    count, findings = scan_repository(root)
    errors.extend(f"secret pattern {rule} at {path}:{line}" for path, line, rule in findings)

    inventory_module = _load_inventory_module()
    try:
        inventory = inventory_module.build_inventory(root)
    except (OSError, ValueError) as exc:
        errors.append(f"dependency inventory invalid: {exc}")
        go_count = 0
    else:
        missing = inventory["integrity"]["go_requirements_missing_h1_sum"]
        go_count = inventory["integrity"]["go_requirements"]
        errors.extend(f"Go dependency lacks go.sum h1 entry: {item}" for item in missing)

    if mode == "release":
        license_path = root / "LICENSE"
        if not license_path.is_file() or license_path.stat().st_size == 0:
            errors.append("release mode requires a non-empty root LICENSE; no license has been selected")
        asset_license_path = root / "LICENSES" / "ASSET-LICENSES.md"
        if not asset_license_path.is_file() or asset_license_path.stat().st_size == 0:
            errors.append("release mode requires a non-empty LICENSES/ASSET-LICENSES.md")
        third_party_notice_path = root / "LICENSES" / "THIRD-PARTY-NOTICES.md"
        if not third_party_notice_path.is_file() or third_party_notice_path.stat().st_size == 0:
            errors.append("release mode requires a non-empty LICENSES/THIRD-PARTY-NOTICES.md")
        else:
            try:
                expected_notices = _load_notice_generator().render_notices(root)
                current_notices = third_party_notice_path.read_text(encoding="utf-8")
            except Exception as exc:
                errors.append(
                    "release mode cannot verify third-party notices from the pinned vendor archive "
                    f"({type(exc).__name__})"
                )
            else:
                if current_notices != expected_notices:
                    errors.append("release mode requires up-to-date LICENSES/THIRD-PARTY-NOTICES.md")
    return errors, count, go_count


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1], help="repository root")
    parser.add_argument("--mode", choices=("verify", "release"), default="verify")
    args = parser.parse_args(argv)
    root = args.root.resolve()
    try:
        errors, scanned, go_count = evaluate(root, args.mode)
    except (OSError, subprocess.CalledProcessError) as exc:
        print(f"release gate could not inspect repository: {exc}", file=sys.stderr)
        return 2

    label = "prealpha verification" if args.mode == "verify" else "release-mode checks"
    print(f"Offline release security/dependency gate — {label}")
    print(f"Source candidates scanned: {scanned}; Go manifest dependencies inventoried: {go_count}")
    print("No vulnerability database, license interpretation, signing, or provenance check is performed.")
    if errors:
        for error in errors:
            print(f"FAIL: {error}")
        print(f"NO-GO: {len(errors)} gate failure(s)")
        return 1
    print("PASS: no high-confidence secret patterns; Go checksum coverage complete")
    if args.mode == "verify":
        print("INFO: verify mode permits the prealpha tree without a selected project license")
    else:
        print("PASS: project LICENSE and asset/third-party notices are present and generated notices are current (terms and provenance are not interpreted)")
        print("NOTE: this result is not release approval, a signed build, or a provenance attestation")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

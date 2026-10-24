#!/usr/bin/env python3
"""生成 CPA 插件商店要求的 ZIP 和 SHA-256 条目。"""
import argparse
import hashlib
import re
from pathlib import Path
from zipfile import ZIP_DEFLATED, ZipFile

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("binary", type=Path)
parser.add_argument("version")
parser.add_argument("os", choices=["linux", "darwin"])
parser.add_argument("arch", choices=["amd64", "arm64"])
args = parser.parse_args()
if not re.fullmatch(r"\d+\.\d+\.\d+", args.version):
    parser.error("版本必须是 X.Y.Z，不含 v 前缀")
suffix = ".so" if args.os == "linux" else ".dylib"
if not args.binary.is_file() or args.binary.suffix != suffix:
    parser.error("请提供对应平台的共享库文件")
name = "codex-5h-reset" + suffix
archive = args.binary.parent / f"codex-5h-reset_{args.version}_{args.os}_{args.arch}.zip"
with ZipFile(archive, "w", ZIP_DEFLATED) as z:
    z.write(args.binary, name)
with ZipFile(archive) as z:
    assert z.namelist() == [name], "共享库必须位于 ZIP 根目录"
    assert z.read(name) == args.binary.read_bytes(), "压缩包内容校验失败"
checksum = hashlib.sha256(archive.read_bytes()).hexdigest()
archive.with_suffix(".sha256").write_text(f"{checksum}  {archive.name}\n", encoding="utf-8")
print(archive)

#!/usr/bin/env python3
"""临时把 OpenCode 的 axonhub provider 指向本地 Linkgate，并支持还原。

用法:
  python3 tools/opencode-proxy-test.py point    # 指到 127.0.0.1:8765
  python3 tools/opencode-proxy-test.py restore  # 还原
"""
import hashlib
import re
import shutil
import sys
from pathlib import Path

CFG = Path.home() / ".config/opencode/opencode.jsonc"
BAK = Path("/tmp/opencode.jsonc.linkgate-bak")
PROXY = "http://127.0.0.1:8765/v1"


def sha(p: Path) -> str:
    return hashlib.sha256(p.read_bytes()).hexdigest()


def main() -> int:
    action = sys.argv[1] if len(sys.argv) > 1 else ""
    if action == "point":
        if not BAK.exists():
            shutil.copy2(CFG, BAK)
        text = CFG.read_text()
        # 只改 axonhub 这个 provider 块里的第一个 baseURL
        m = re.search(r'("axonhub"\s*:\s*\{.*?)("baseURL"\s*:\s*")([^"]+)(")', text, re.S)
        if not m:
            print("没找到 axonhub provider 的 baseURL")
            return 1
        old = m.group(3)
        text = text[: m.start(3)] + PROXY + text[m.end(3):]
        CFG.write_text(text)
        print(f"已指向代理（原地址已备份到 {BAK.name}，长度 {len(old)}）")
        return 0
    if action == "restore":
        if not BAK.exists():
            print("没有备份，跳过")
            return 1
        shutil.copy2(BAK, CFG)
        BAK.unlink()
        print("已还原，sha256 =", sha(CFG)[:16])
        return 0
    print(__doc__)
    return 2


if __name__ == "__main__":
    raise SystemExit(main())

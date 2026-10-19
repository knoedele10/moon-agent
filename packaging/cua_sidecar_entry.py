"""bin/cua-sidecar.exe 的入口（集成契约 §5）。

为什么单独写一个入口，而不是直接打 cua/main.py：
1) cua 包只有 "python -m cua.main" 这种模块调用方式，PyInstaller 需要一个脚本入口。
2) 打包成单文件 exe 后，--selfcheck 内部会用
       [sys.executable, "-m", "cua.main", "--dry-run", ...]
   起子进程验证事件流 UTF-8 往返（task-21）。单文件 exe 不认 "-m cua.main"：
   argparse 会把 -m 当成未知参数，子进程直接 exit 2，自检必然失败。
   这里在入口把这种前缀吃掉，让打包产物与源码态跑同一份自检、同一套判据。
"""

from __future__ import annotations

import sys


def _strip_module_invocation() -> None:
    argv = sys.argv
    if len(argv) >= 3 and argv[1] == "-m" and argv[2] == "cua.main":
        del argv[1:3]


def main() -> int:
    _strip_module_invocation()
    from cua.main import main as cua_main

    return cua_main()


if __name__ == "__main__":
    sys.exit(main())

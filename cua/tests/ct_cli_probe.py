# -*- coding: utf-8 -*-
"""在安全环境里运行 cua.main 的探测脚本（verifier 独立编写）。

先安装记录式 pyautogui 假件，再用 runpy 以 __main__ 身份运行 cua.main，
因此即使实现忽略了 --dry-run，也绝不可能动到真实键鼠。

用法：python cua/tests/ct_cli_probe.py --help
退出码：0 正常 / 3 cua.main 不存在或导入失败 / 4 其它异常
"""
import runpy
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO_ROOT = HERE.parents[1]
for _p in (str(HERE), str(REPO_ROOT)):
    if _p not in sys.path:
        sys.path.insert(0, _p)

from ct_fake_pyautogui import install  # noqa: E402

install()


def run():
    argv = sys.argv[1:]
    sys.argv = ["cua.main"] + list(argv)
    try:
        runpy.run_module("cua.main", run_name="__main__")
    except SystemExit as exc:
        code = exc.code
        if code is None:
            return 0
        if isinstance(code, int):
            return code
        return 0
    except (ImportError, ModuleNotFoundError) as exc:
        sys.stderr.write("CLI_PROBE_IMPORT_ERROR: %s\n" % exc)
        return 3
    except Exception as exc:
        sys.stderr.write("CLI_PROBE_ERROR: %s: %s\n" % (type(exc).__name__, exc))
        return 4
    return 0


if __name__ == "__main__":
    raise SystemExit(run())

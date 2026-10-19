# -*- coding: utf-8 -*-
"""契约测试套件入口（标准库 unittest；本仓库未安装 pytest）。

用法（在仓库根目录执行）：
    python cua/tests/run_tests.py            # 跑全部
    python cua/tests/run_tests.py -v         # 详细（会打印每个 skip 的原因）
    python cua/tests/run_tests.py -k coords  # 只跑文件名含 coords 的模块

退出码：0 = 全通过；1 = 有失败/错误；2 = 无失败但存在 skip（说明有未验证项，需人看）。
"""
import argparse
import sys
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO_ROOT = HERE.parents[1]
for _p in (str(HERE), str(REPO_ROOT)):
    if _p not in sys.path:
        sys.path.insert(0, _p)


def build_suite(filter_text=""):
    files = sorted(p for p in HERE.glob("ct_test_*.py")
                   if not filter_text or filter_text in p.stem)
    loader = unittest.TestLoader()
    suite = unittest.TestSuite()
    for f in files:
        mod = __import__(f.stem)
        suite.addTests(loader.loadTestsFromModule(mod))
    return files, suite


def main(argv=None):
    ap = argparse.ArgumentParser(description="CUA 契约测试套件（verifier 独立编写）")
    ap.add_argument("-v", "--verbose", action="store_true", help="详细输出")
    ap.add_argument("-k", "--filter", default="", help="只跑文件名包含该子串的测试模块")
    args = ap.parse_args(argv)

    files, suite = build_suite(args.filter)
    if not files:
        print("没有匹配的测试模块")
        return 1
    runner = unittest.TextTestRunner(verbosity=2 if args.verbose else 1)
    result = runner.run(suite)

    print("")
    print("---- 汇总 ----")
    print("模块: %s" % ", ".join(f.stem for f in files))
    print("用例: run=%d failures=%d errors=%d skipped=%d"
          % (result.testsRun, len(result.failures), len(result.errors), len(result.skipped)))
    if result.skipped:
        print("跳过原因（前 20 条）：")
        for case, reason in result.skipped[:20]:
            print("  - %s :: %s" % (case.id().split(".")[-1], reason))
    if result.failures or result.errors:
        return 1
    if result.skipped:
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

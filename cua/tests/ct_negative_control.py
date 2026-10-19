# -*- coding: utf-8 -*-
"""测试套件「有效性证明」（verifier 自检）：注入契约违规，套件必须变红。

一个全绿的契约套件本身不构成证据 —— 它可能什么都没验。本工具在**内存里**给实现
打上已知的契约违规补丁（不改任何磁盘文件），再跑对应用例，要求它们**必须失败**。
没被抓住的变异，说明那条判据是空的。

用法：
    python cua/tests/ct_negative_control.py            # 逐个变异开子进程跑，打印汇总表
    python cua/tests/ct_negative_control.py --list     # 只列出变异
    python cua/tests/ct_negative_control.py --mutation NAME --module ct_test_04_bounds
        # 单次运行：打补丁 + 跑该模块（子进程内部使用）

退出码：0 = 所有变异都被抓住；1 = 有变异没被抓住（套件存在盲区）。
"""
from __future__ import annotations

import subprocess
import sys
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO_ROOT = HERE.parent.parent


# --------------------------------------------------------------------------
# 违规补丁（每个都对应一条契约判据）
# --------------------------------------------------------------------------
def _patch_extract_json_always_ok():
    """违反 §2.1：坏 JSON 也返回「成功」。"""
    import cua.action_parser as ap

    def fake(text):
        return ap.ExtractResult(
            value={"protocol": "cua/1", "thought": "伪造", "action": {"type": "wait", "ms": 100},
                   "done": False},
            method="direct", raw=str(text))

    ap.extract_json = fake


def _patch_schema_accepts_all():
    """违反 §2 严出：任何 Step 都当合法。"""
    import cua.action_parser as ap

    def fake(step, cfg=None, schema=None):
        return step

    ap.validate_step_dict = fake


def _patch_bounds_never_rejects():
    """违反 §1.1/§8.2：越界不拒绝（等于放任 clamp）。"""
    import cua.screenshot as sc
    sc.check_image_bounds = lambda x, y, frame, what="坐标": None


def _patch_key_whitelist_widened():
    """违反 §3.3：把白名单外的键放进白名单。"""
    import cua.executor as ex
    wl = ex.KEY_WHITELIST
    try:
        wl.add("f13")           # 可变集合
    except AttributeError:
        ex.KEY_WHITELIST = set(wl) | {"f13"}


def _patch_destructive_words_disabled():
    """违反 §7：命中断词表也不拦。"""
    import cua.executor as ex
    ex.destructive_hits = lambda text: []


def _patch_click_guard_always_passes():
    """违反 §7 硬约束：破坏性按钮护栏永远放行。"""
    import cua.executor as ex
    ex.destructive_guard = lambda step: []


def _patch_dry_run_sends_input():
    """违反 §6/§8：dry-run 记录完动作后**仍然**真的发送键鼠输入。

    保留 DryRunDevice 的完整接口，只在记录之后补一次真实调用 —— 这样变异体在
    「别的方面都正常、唯独漏了安全前提」这一点上忠实于实现者的可能失误。
    """
    import cua.executor as ex

    class LeakyDryRunDevice(ex.DryRunDevice):
        def __init__(self, *args, **kwargs):
            super().__init__(*args, **kwargs)
            import pyautogui
            self._leak = pyautogui

        def click(self, x, y, button="left", clicks=1):
            super().click(x, y, button=button, clicks=clicks)
            self._leak.click(int(x), int(y), clicks=int(clicks), button=button)

        def key_down(self, key):
            super().key_down(key)
            self._leak.keyDown(key)

        def key_up(self, key):
            super().key_up(key)
            self._leak.keyUp(key)

        def press(self, key):
            super().press(key)
            self._leak.press(key)

        def write(self, text, interval=0.0):
            super().write(text, interval=interval)
            self._leak.write(text, interval=float(interval))

        def hotkey(self, *keys):
            super().hotkey(*keys)
            self._leak.hotkey(*keys)

    ex.DryRunDevice = LeakyDryRunDevice


def _patch_never_releases_modifiers():
    """违反 §8.7：失败终止后不补 keyUp。"""
    import cua.executor as ex
    ex.PyAutoGUIDevice.key_up = lambda self, key: None


def _patch_all_errors_retryable():
    """违反 §5：不可重试的错误也判成可重试（含必须重规划的 E_STALL）。

    实现同时暴露「数据表 + policy() 对象 + 判定函数」三个面，变异必须把三个面一起改，
    否则只改一个面并不构成实现缺陷（另外两个面仍给出正确结论）。
    """
    import cua.errors as er

    er.is_retryable = lambda code, http_status=None: True
    policy = er.policy

    def leaky_policy(code):
        base = policy(code)
        return type(base)(code=base.code, retryable=True,
                          max_retries=max(1, base.max_retries),
                          category=base.category, disposition=base.disposition)

    er.policy = leaky_policy
    for code, spec in list(er.ERROR_TABLE.items()):
        er.ERROR_TABLE[code] = type(spec)(code=spec.code, retryable=True,
                                          max_retries=max(1, spec.max_retries),
                                          category=spec.category,
                                          disposition=spec.disposition)


def _patch_everything_replans():
    """违反 §5：任何错误都要求重规划（把 E_SCHEMA 也说成 must_replan）。"""
    import cua.errors as er
    er.is_replan = lambda code: True


def _patch_failsafe_disabled():
    """违反 §8.1：关掉 FAILSAFE。"""
    import cua.executor as ex
    ex.PyAutoGUIDevice.__init__ = lambda self: setattr(self, "_pg", sys.modules["pyautogui"])
    sys.modules["pyautogui"].FAILSAFE = False


MUTATIONS = [
    # name, 补丁, 目标模块, 期望被抓住的判据说明
    ("extract_json_always_ok", _patch_extract_json_always_ok, "ct_test_01_json",
     "坏 JSON 必须报 E_BAD_JSON（§2.1）"),
    ("schema_accepts_all", _patch_schema_accepts_all, "ct_test_02_schema",
     "非法 Step 必须被拒并给正确错误码（§2/§3/§5）"),
    ("bounds_never_rejects", _patch_bounds_never_rejects, "ct_test_04_bounds",
     "越界必须 E_OUT_OF_BOUNDS、绝不 clamp（§1.1/§8.2）"),
    ("key_whitelist_widened", _patch_key_whitelist_widened, "ct_test_05_keys",
     "白名单外的键必须 E_KEY_NOT_ALLOWED（§3.3）"),
    ("destructive_words_disabled", _patch_destructive_words_disabled, "ct_test_07_destructive",
     "破坏性词表必须命中（§7）"),
    ("click_guard_always_passes", _patch_click_guard_always_passes, "ct_test_07_destructive",
     "命中破坏性词后不得点击、必须转人工（§7 硬约束）"),
    ("dry_run_sends_input", _patch_dry_run_sends_input, "ct_test_09_dryrun_mock",
     "dry-run/mock 下零键鼠调用（§6/§8）"),
    ("never_releases_modifiers", _patch_never_releases_modifiers, "ct_test_08_modifier_release",
     "失败终止必须释放修饰键（§8.7）"),
    ("all_errors_retryable", _patch_all_errors_retryable, "ct_test_06_errors",
     "不可重试错误必须不可重试（§5）"),
    ("everything_replans", _patch_everything_replans, "ct_test_06_errors",
     "只有 E_STALL/E_LOOP 才是必须重规划（§5）"),
    ("failsafe_disabled", _patch_failsafe_disabled, "ct_test_08_modifier_release",
     "FAILSAFE 必须永久开启（§8.1）"),
]


def _load(name):
    sys.path.insert(0, str(HERE))
    sys.path.insert(0, str(REPO_ROOT))
    import importlib
    return importlib.import_module(name)


def run_one(mutation_name):
    """在子进程里：装假件 -> 打补丁 -> 跑目标模块 -> 返回 unittest 结果。"""
    entry = [m for m in MUTATIONS if m[0] == mutation_name]
    if not entry:
        print("未知变异：%s" % mutation_name)
        return 2
    _, patch, module_name, _ = entry[0]
    import ct_fake_pyautogui as fake  # noqa: F401  （安装到 sys.modules）
    import ct_adapter  # noqa: F401      （在任何 cua.* 之前装好假件）
    patch()
    mod = _load(module_name)
    suite = unittest.defaultTestLoader.loadTestsFromModule(mod)
    result = unittest.TextTestRunner(verbosity=0).run(suite)
    bad = len(result.failures) + len(result.errors)
    print("变异 %s 在 %s 上触发 %d 个失败/错误" % (mutation_name, module_name, bad))
    return 0 if bad > 0 else 1   # 0 = 被抓住（期望），1 = 没抓住（盲区）


def main(argv):
    if "--list" in argv:
        for name, _, module, desc in MUTATIONS:
            print("%-30s -> %-28s %s" % (name, module, desc))
        return 0
    if "--mutation" in argv:
        return run_one(argv[argv.index("--mutation") + 1])
    print("==== 负向对照（注入契约违规，套件必须变红）====")
    print("%-30s %-30s %s" % ("变异", "目标模块", "结果"))
    holes = []
    for name, _, module, desc in MUTATIONS:
        proc = subprocess.run([sys.executable, str(Path(__file__).resolve()),
                               "--mutation", name],
                              cwd=str(REPO_ROOT), stdout=subprocess.PIPE,
                              stderr=subprocess.STDOUT, timeout=600)
        caught = proc.returncode == 0
        if not caught:
            holes.append(name)
        print("%-30s %-30s %s" % (name, module, "已抓住" if caught else "未抓住 <<< 盲区"))
        if not caught:
            print("    %s" % desc)
    print("")
    if holes:
        print("结果：%d/%d 变异被抓住；盲区：%r" % (len(MUTATIONS) - len(holes), len(MUTATIONS), holes))
        return 1
    print("结果：%d/%d 变异全部被抓住（套件对已列判据有区分能力）" % (len(MUTATIONS), len(MUTATIONS)))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

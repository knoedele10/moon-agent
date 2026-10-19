# -*- coding: utf-8 -*-
"""契约测试 09：--dry-run / --mock 下不产生真实键鼠调用（§6/§8 安全前提）。

三层取证：
  1) CLI 面：cua.main --help 必须暴露 --dry-run 与 --mock（子进程里已先装假件，安全）；
  2) 静态面：若实现用了假件拦不住的输入后端（pynput / win32api.SendInput 等），
     「零真实输入」判据不成立，必须如实记录为待人工复核，而不是假装通过；
  3) 执行面：显式声明了 dry_run/mock 参数的执行入口，在开关打开时不得产生任何键鼠调用。
"""
import unittest

import ct_adapter as A
import ct_fake_pyautogui as F

# 只盯「真正会注入键鼠」的入口；user32 之类既能取 DPI 也能发输入，因此必须精确到 API 名，
# 否则会把 GetDpiForSystem 这种只读查询误判成绕过假件（本仓实测就有这一处）。
ALT_BACKEND_RE = (
    r"(import\s+pynput|from\s+pynput"
    r"|SendInput|mouse_event|keybd_event|SetCursorPos|BlockInput"
    r"|import\s+keyboard|pydirectinput"
    r"|win32api\.(mouse_event|keybd_event|SetCursorPos)"
    r"|Xlib\.display|libXtst)"
)


class CliDryRunFlags(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        rc, out = A.cli_help()
        if rc == 3:
            raise unittest.SkipTest("cua.main 尚不存在或导入失败（probe 退出码 3）：%s"
                                    % out.strip().splitlines()[-1:])
        cls.rc, cls.out = rc, out

    def test_help_runs(self):
        self.assertEqual(self.rc, 0, "cua.main --help 退出码 %s；输出：%s" % (self.rc, self.out[:800]))

    def test_dry_run_flag_exposed(self):
        self.assertIn("--dry-run", self.out, "--help 未暴露 --dry-run")

    def test_mock_flag_exposed(self):
        self.assertIn("--mock", self.out, "--help 未暴露 --mock")


class NoUninterceptableInputBackend(unittest.TestCase):
    def test_backend_scan(self):
        hits = A.scan_sources(ALT_BACKEND_RE)
        if hits:
            self.fail("实现使用了记录式假件拦不住的输入后端 %r："
                      "「零真实输入」判据不成立，必须人工复核 dry-run 行为" % (hits,))
        self.assertEqual(hits, [])


class DryRunMakesNoInputCalls(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.cap = A.dry_run_executor()

    def _assert_no_input(self, action):
        F.get().reset()
        res = self.cap(action)
        calls = F.get().input_calls()
        self.assertEqual(calls, [],
                         "dry-run/mock 打开时仍发生键鼠调用 %r（入口 %s，结果 %s）"
                         % (calls, self.cap.where, res.describe()))
        # 防止「动作根本没执行」造成的假通过：dry-run 应当照常走完执行路径（§6 只打印不真按）
        self.assertFalse(A.rejected(res),
                         "dry-run 下动作未被执行（%s）；此时「零输入」不构成证据（入口 %s）"
                         % (res.describe(), self.cap.where))

    def test_click_under_dry_run(self):
        self._assert_no_input({"type": "click", "x": 100, "y": 100})

    def test_type_under_dry_run(self):
        self._assert_no_input({"type": "type", "text": "hello"})

    def test_hotkey_under_dry_run(self):
        self._assert_no_input({"type": "hotkey", "keys": ["ctrl", "s"]})


class DryRunEvidenceScope(unittest.TestCase):
    """把「哪些层已取证、哪些层待验证」写进测试输出，避免用弱证据冒充强结论。"""

    def test_scope_note(self):
        print("")
        print("==== DRY-RUN 取证范围 ====")
        print("已取证: CLI --help 暴露 --dry-run/--mock（子进程，假件已装）")
        try:
            cap = A.dry_run_executor()
            print("已取证: 执行入口 %s 在开关打开时不产生键鼠调用" % cap.where)
        except unittest.SkipTest as exc:
            print("待验证: %s" % exc)
        print("待验证: 主循环级（真实模型 + 截图的端到端 dry-run）需要实现提供截图/模型注入点")
        print("==== END ====")
        self.assertTrue(True)

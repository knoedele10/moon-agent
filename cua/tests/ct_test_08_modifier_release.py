# -*- coding: utf-8 -*-
"""契约测试 08：§8 安全不变量 —— 失败终止时必须释放修饰键；FAILSAFE 永久开启。

取证环境是记录式 pyautogui 假件：
  - FAKE.pressed 跟踪「当前仍按下的键」，失败终止后必须为空集；
  - FAKE.fail_next() 在指定的键鼠调用上注入 FailSafeException，模拟执行中途失败。
"""
import unittest

import ct_adapter as A
import ct_fake_pyautogui as F
from ct_fake_pyautogui import MOUSE_MUTATING

CLICK_WITH_CTRL = {"type": "click", "x": 100, "y": 100, "modifiers": ["ctrl"]}
HOTKEY_STEP = {"type": "hotkey", "keys": ["ctrl", "s"], "repeat": 1}


class FailsafeInvariant(unittest.TestCase):
    def test_failsafe_is_true_after_import(self):
        self.assertTrue(F.get().FAILSAFE, "pyautogui.FAILSAFE 被关闭，违反 §8.1（永久开启）")

    def test_no_source_disables_failsafe(self):
        hits = A.scan_sources(r"FAILSAFE\s*=\s*False")
        self.assertEqual(hits, [], "实现源码里出现 FAILSAFE = False，违反 §8.1：%r" % (hits,))


class ModifierReleaseOnFailure(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.exec_action = A.action_executor()

    def _observe_first_sequence(self, action):
        F.get().reset()
        outcome = self.exec_action(action)
        names = F.get().call_names()
        return outcome, names

    def test_modifier_key_released_when_execution_fails(self):
        outcome, names = self._observe_first_sequence(CLICK_WITH_CTRL)
        if not F.get().input_calls():
            self.skipTest("执行路径未产生任何真实输入调用（%s），残留键判据不成立" % outcome.describe())
        if "keyDown" not in names:
            self.skipTest("该执行路径不使用 keyDown/keyUp（调用序列 %r），无法用残留键状态取证" % (names,))
        mouse_name = next((n for n in names if n in MOUSE_MUTATING), None)
        if mouse_name is None:
            self.skipTest("调用序列里没有鼠标动作（%r），无法在动作中途注入失败" % (names,))
        F.get().reset()
        F.get().fail_next(mouse_name, 1)
        res = self.exec_action(CLICK_WITH_CTRL)
        self.assertEqual(F.get().pressed, set(),
                         "失败终止后仍残留按下的键 %r；§8.7 要求对所有修饰键补 keyUp（结果 %s）"
                         % (F.get().pressed, res.describe()))
        unpaired = F.get().unpaired_keydowns()
        self.assertEqual(unpaired, [],
                         "存在没有配对 keyUp 的 keyDown：%r（完整调用 %r）" % (unpaired, F.get().call_names()))

    def test_failsafe_exception_maps_to_e_executor_failsafe(self):
        """FAILSAFE 触发必须是 E_EXECUTOR_FAILSAFE（§5 不可重试），且不残留按下的键。

        hotkey 走 pyautogui.hotkey（不在假件里展开成 keyDown/keyUp），所以这里验的是
        错误码面 + 残留键状态，而不是 keyDown/keyUp 配对。
        """
        F.get().reset()
        F.get().fail_next("hotkey", 1)
        res = self.exec_action(HOTKEY_STEP)
        self.assertTrue(A.rejected(res), "FailSafeException 未转化为拒绝：%s" % res.describe())
        self.assertEqual(A.rejection_code(res), "E_EXECUTOR_FAILSAFE",
                         "FAILSAFE 应报 E_EXECUTOR_FAILSAFE，实际 %s（%s）"
                         % (A.rejection_code(res), str(res.value)[:200]))
        self.assertEqual(F.get().pressed, set(),
                         "FAILSAFE 终止后仍残留按下的键 %r" % (F.get().pressed,))
        self.assertEqual(F.get().unpaired_keydowns(), [],
                         "存在没有配对 keyUp 的 keyDown：%r" % (F.get().call_names(),))

    def test_failsafe_on_mouse_maps_to_e_executor_failsafe(self):
        """点选（含 ctrl 修饰）中途触发 FAILSAFE：错误码正确且修饰键已补 keyUp。"""
        outcome, names = self._observe_first_sequence(CLICK_WITH_CTRL)
        mouse_name = next((n for n in names if n in MOUSE_MUTATING), None)
        if not F.get().input_calls() or mouse_name is None:
            self.skipTest("该执行路径未产生鼠标调用（%r），无法注入 FAILSAFE" % (names,))
        F.get().reset()
        F.get().fail_next(mouse_name, 1)
        res = self.exec_action(CLICK_WITH_CTRL)
        self.assertTrue(A.rejected(res), "FailSafeException 未转化为拒绝：%s" % res.describe())
        self.assertEqual(A.rejection_code(res), "E_EXECUTOR_FAILSAFE",
                         "FAILSAFE 应报 E_EXECUTOR_FAILSAFE，实际 %s（%s）"
                         % (A.rejection_code(res), str(res.value)[:200]))
        self.assertEqual(F.get().pressed, set(),
                         "失败终止后仍残留按下的键 %r；§8.7 要求补 keyUp（%s）"
                         % (F.get().pressed, res.describe()))

    def test_no_residual_keys_at_rest(self):
        F.get().reset()
        self.exec_action(CLICK_WITH_CTRL)
        if not F.get().input_calls():
            self.skipTest("执行路径未产生输入调用，无法取证")
        self.assertEqual(F.get().pressed, set(), "正常执行结束后仍残留按下的键：%r" % (F.get().pressed,))

# -*- coding: utf-8 -*-
"""契约测试 07：§7 破坏性词表与弹窗硬约束。

硬约束：可疑弹窗按钮文字命中破坏性词表 -> 不点击，直接转人工（E_NEED_HUMAN）。
取证方式：call guard -> 断言「拒绝 + 转人工」，并断言记录式假件上没有任何键鼠调用。
"""
import unittest

import ct_adapter as A
import ct_oracle as O

HIT_TEXTS = [
    "删除", "删除文件", "卸载程序", "格式化磁盘", "清空回收站", "重置设置", "覆盖原文件",
    "立即支付", "确认付款", "确认购买", "允许访问", "授权登录", "安装驱动", "我同意",
    "Delete account", "delete", "DELETE", "Uninstall app", "Format disk", "Erase all",
    "Reset", "Overwrite", "Pay now", "PAY", "Purchase", "Allow", "Grant access",
    "Install", "Agree", "aLlOw",
]

SAFE_TEXTS = [
    "取消", "关闭", "稍后", "Cancel", "Close", "Later", "确定", "查看详情",
]


class OracleDestructiveWords(unittest.TestCase):
    def test_word_table_matches_doc(self):
        self.assertEqual(len(O.DESTRUCTIVE_WORDS_ZH), 13)
        self.assertEqual(len(O.DESTRUCTIVE_WORDS_EN), 12)

    def test_hits(self):
        for t in HIT_TEXTS:
            with self.subTest(text=t):
                self.assertTrue(O.is_destructive(t), "%r 应命中破坏性词表" % t)

    def test_safe_texts_are_not_hits(self):
        for t in SAFE_TEXTS:
            with self.subTest(text=t):
                self.assertFalse(O.is_destructive(t), "%r 不该命中破坏性词表" % t)

    def test_destructive_hits_detail(self):
        self.assertIn("删除", O.destructive_hits("删除文件"))
        self.assertTrue(O.destructive_hits("Confirm Purchase Now"))


def _hit(cap, text):
    res = cap(text)
    if not res.ok:
        return (A.rejection_code(res) or "ERROR") in ("E_NEED_HUMAN", "E_POPUP_BLOCKING")
    v = res.value
    if isinstance(v, (list, tuple)):
        return bool(v)
    return bool(v)


class ImplDestructiveWords(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.cap = A.destructive_predicate()

    def test_hits(self):
        for t in HIT_TEXTS:
            with self.subTest(text=t):
                self.assertTrue(_hit(self.cap, t), "%r 应命中破坏性词表（入口 %s）" % (t, self.cap.where))

    def test_safe_texts_are_not_hits(self):
        for t in SAFE_TEXTS:
            with self.subTest(text=t):
                self.assertFalse(_hit(self.cap, t), "%r 不该命中破坏性词表" % t)


class ImplDestructivePopupGuard(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.guard = A.click_guard()

    def test_destructive_button_is_refused_and_routed_to_human(self):
        import ct_fake_pyautogui as F
        for text in ("删除", "Delete", "确认购买", "Uninstall"):
            with self.subTest(text=text):
                F.get().reset()
                res = self.guard(text, {"type": "click", "x": 100, "y": 200})
                refused = A.rejected(res)
                self.assertTrue(
                    refused,
                    "破坏性弹窗按钮 %r 未被拒绝（返回 %r）；§7 硬约束要求不点击、直接 E_NEED_HUMAN"
                    % (text, res.value))
                code = A.rejection_code(res)
                if code is not None:
                    self.assertIn(code, ("E_NEED_HUMAN", "E_POPUP_BLOCKING"),
                                  "%r 应转人工（E_NEED_HUMAN），实际 %s" % (text, code))
                self.assertEqual(F.get().input_calls(), [],
                                 "命中破坏性词表后仍发生了键鼠调用：%r" % (F.get().input_calls(),))

    def test_safe_button_is_not_forced_to_human(self):
        import ct_fake_pyautogui as F
        for text in ("取消", "Cancel", "关闭"):
            with self.subTest(text=text):
                F.get().reset()
                res = self.guard(text, {"type": "click", "x": 10, "y": 10})
                code = A.rejection_code(res)
                self.assertNotIn(code, ("E_NEED_HUMAN",),
                                 "非破坏性按钮 %r 不该被直接转人工" % text)


class DestructiveGuardWithoutDedicatedEntry(unittest.TestCase):
    """若实现只提供词表判定、没有护栏入口，则记录为「待验证」，而不是假装通过。"""

    def test_guard_entry_presence_is_reported(self):
        try:
            A.click_guard()
            present = True
        except unittest.SkipTest:
            present = False
        if not present:
            try:
                A.destructive_predicate()
            except unittest.SkipTest:
                self.skipTest("破坏性词表与护栏入口都未解析到，整项待验证")
            self.skipTest("仅解析到破坏性词表判定，未解析到弹窗护栏入口："
                          "「命中即不点击」这一硬约束待实现落地后复核")
        self.assertTrue(present)

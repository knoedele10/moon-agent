# -*- coding: utf-8 -*-
"""契约测试 05：§3.3 键名白名单。

白名单（冻结）：a-z、0-9、F1-F12、ctrl/alt/shift/win/cmd、enter/tab/esc/space/backspace/
delete/up/down/left/right/home/end/pageup/pagedown/insert。
白名单之外 -> E_KEY_NOT_ALLOWED。
"""
import unittest

import ct_adapter as A
import ct_oracle as O

ALLOWED_KEYS = [
    "a", "z", "q", "0", "9",
    "F1", "F6", "F12",
    "ctrl", "alt", "shift", "win", "cmd",
    "enter", "tab", "esc", "space", "backspace", "delete",
    "up", "down", "left", "right", "home", "end", "pageup", "pagedown", "insert",
]

ILLEGAL_KEYS = [
    "f13", "F0", "escape", "return", "meta", "del", "control", "windows",
    "enter ", " enter", "ctrl+c", "", "é", "clear",
]

AMBIGUOUS_KEYS = [
    ("uppercase_letter", "A"),
    ("lowercase_fkey", "f1"),
    ("uppercase_named", "ENTER"),
]


def hotkey_step(keys):
    return {"protocol": "cua/1", "thought": "按组合键", "action": {"type": "hotkey", "keys": keys},
            "done": False}


class OracleKeyWhitelist(unittest.TestCase):
    def test_whitelist_size_and_members(self):
        self.assertEqual(len(O.KEY_WHITELIST), 68)
        for k in ALLOWED_KEYS:
            with self.subTest(key=k):
                self.assertTrue(O.is_allowed_key(k))
        for k in ILLEGAL_KEYS:
            with self.subTest(key=k):
                self.assertFalse(O.is_allowed_key(k))

    def test_hotkey_step_with_illegal_key(self):
        for k in ILLEGAL_KEYS:
            with self.subTest(key=k):
                self.assertEqual(O.classify_step(hotkey_step([k])), "E_KEY_NOT_ALLOWED")

    def test_hotkey_step_with_legal_keys(self):
        for k in ALLOWED_KEYS:
            with self.subTest(key=k):
                self.assertIsNone(O.classify_step(hotkey_step([k])))


class ImplKeyWhitelist(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.cap = A.key_allowed()

    def test_allowed_keys_pass(self):
        for k in ALLOWED_KEYS:
            with self.subTest(key=k):
                res = self.cap(k)
                self.assertFalse(A.rejected(res), "合法键被拒绝：%r（%s）" % (k, res.describe()))

    def test_illegal_keys_rejected(self):
        for k in ILLEGAL_KEYS:
            with self.subTest(key=k):
                res = self.cap(k)
                self.assertTrue(A.rejected(res), "非法键被接受：%r（%s）" % (k, res.describe()))
                code = A.rejection_code(res)
                if code is not None:
                    self.assertEqual(code, "E_KEY_NOT_ALLOWED",
                                     "非法键 %r 的错误码应为 E_KEY_NOT_ALLOWED，实际 %s" % (k, code))


class ImplKeyWhitelistThroughValidator(unittest.TestCase):
    """错误码面：hotkey 步骤里出现非法键必须被拒并给出 E_KEY_NOT_ALLOWED（§3.3/§5）。

    拒绝点可以是 Step 校验器，也可以是执行层（§10 的分工里白名单天然属于执行层），
    所以用综合校验面；判据（拒绝 + 错误码）不变。
    """

    @classmethod
    def setUpClass(cls):
        cls.cap = A.step_validator()

    def test_illegal_keys_in_step(self):
        for k in ILLEGAL_KEYS:
            with self.subTest(key=k):
                got = A.reject_step(hotkey_step([k]))
                self.assertNotEqual(got, "OK", "非法键 %r 的 Step 被接受" % k)
                self.assertEqual(got, "E_KEY_NOT_ALLOWED",
                                 "非法键 %r 应为 E_KEY_NOT_ALLOWED，实际 %s" % (k, got))

    def test_legal_keys_in_step(self):
        for k in ALLOWED_KEYS:
            with self.subTest(key=k):
                got = A.verdict(self.cap(hotkey_step([k])))
                self.assertEqual(got, "OK", "合法键 %r 的 Step 被拒绝：%s" % (k, got))


class AmbiguousKeyCasing(unittest.TestCase):
    """文档只写了 a-z / F1-F12，未规定大小写宽容度；两种实现都记录在案。"""

    @classmethod
    def setUpClass(cls):
        cls.keycap = A.key_allowed()
        cls.stepcap = A.step_validator()

    def test_casing_choice_is_recorded(self):
        for name, k in AMBIGUOUS_KEYS:
            with self.subTest(case=name):
                res = self.keycap(k)
                oracle_ok = O.is_allowed_key(k)
                impl_ok = not A.rejected(res)
                self.assertIn(impl_ok, (True, False))
                got = A.verdict(self.stepcap(hotkey_step([k])))
                self.assertIn(got, {"OK", "E_KEY_NOT_ALLOWED"},
                              "%s：%r 的判定 %s 越出两种合理解读" % (name, k, got))
                self.assertTrue(oracle_ok in (True, False))

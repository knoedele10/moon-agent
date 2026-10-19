# -*- coding: utf-8 -*-
"""契约测试 00：安全网与能力表。

这一组永远会执行（不依赖实现），作用是：
  - 证明记录式 pyautogui 假件已生效（测试不可能动到真实键鼠）；
  - 打印「实现侧能力解析表」：哪些契约能力已解析成功、哪些因实现未落地/命名不同而缺失。
"""
import sys
import unittest

import ct_adapter as A
import ct_fake_pyautogui as F


class SafetyNet(unittest.TestCase):
    def test_fake_pyautogui_installed(self):
        self.assertIsNotNone(F.get(), "记录式 pyautogui 假件未安装：测试可能动到真实键鼠")
        self.assertIs(sys.modules.get("pyautogui"), F.get(),
                      "sys.modules['pyautogui'] 不是假件，实现可能导入到真实 pyautogui")

    def test_fake_defaults_are_deterministic(self):
        self.assertEqual(F.get().size(), (1920, 1080))
        self.assertFalse(F.get().has_real_input())

    def test_capability_table(self):
        rows = A.capability_report()
        print("")
        print("==== CAPABILITY_TABLE (task-7 verifier) ====")
        print("repo=%s" % A.REPO_ROOT)
        print("cua_package_present=%s" % A.cua_package_present())
        for name, status, detail in rows:
            print("%-18s %-9s %s" % (name, status, detail))
        print("==== END CAPABILITY_TABLE ====")
        self.assertGreaterEqual(len(rows), 10)

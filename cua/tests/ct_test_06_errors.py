# -*- coding: utf-8 -*-
"""契约测试 06：§5 错误码分类 —— 可重试 vs 必须重规划（E_STALL / E_LOOP）。

关键区分：可重试错误是「同一动作再来一次」；E_STALL / E_LOOP 是语义上认定这条路走不通，
必须换策略重规划（retryable=False，且重规划标记为真）。
"""
import unittest

import ct_adapter as A
import ct_oracle as O


class OracleErrorPolicy(unittest.TestCase):
    def test_table_is_frozen_size(self):
        self.assertEqual(len(O.ALL_CODES), 20)
        self.assertEqual(sorted(O.ALL_CODES), sorted(set(O.ALL_CODES)))

    def test_retryable_sets(self):
        self.assertIn("E_BAD_JSON", O.RETRYABLE_CODES)
        self.assertIn("E_SCHEMA", O.RETRYABLE_CODES)
        self.assertIn("E_ARG_RANGE", O.RETRYABLE_CODES)
        self.assertIn("E_OUT_OF_BOUNDS", O.RETRYABLE_CODES)
        self.assertIn("E_KEY_NOT_ALLOWED", O.RETRYABLE_CODES)
        self.assertIn("E_NO_FOCUS", O.RETRYABLE_CODES)
        self.assertIn("E_MODEL_TIMEOUT", O.RETRYABLE_CODES)
        self.assertIn("E_EXPECT_TIMEOUT", O.RETRYABLE_CODES)
        self.assertIn("E_POPUP_BLOCKING", O.RETRYABLE_CODES)
        self.assertIn("E_UNKNOWN_ACTION", O.NON_RETRYABLE_CODES)
        self.assertIn("E_MODEL_REFUSAL", O.NON_RETRYABLE_CODES)
        self.assertIn("E_EXECUTOR_FAILSAFE", O.NON_RETRYABLE_CODES)
        self.assertIn("E_STALL", O.NON_RETRYABLE_CODES)
        self.assertIn("E_LOOP", O.NON_RETRYABLE_CODES)
        self.assertIn("E_NEED_HUMAN", O.NON_RETRYABLE_CODES)

    def test_must_replan_is_exactly_stall_and_loop(self):
        self.assertEqual(O.MUST_REPLAN_CODES, frozenset({"E_STALL", "E_LOOP"}))
        for code in O.MUST_REPLAN_CODES:
            self.assertFalse(O.ERROR_POLICY[code]["retryable"],
                             "%s 必须重规划，不能当作「同一动作重试」" % code)

    def test_max_retries(self):
        self.assertEqual(O.MAX_RETRIES["E_BAD_JSON"], 2)
        self.assertEqual(O.MAX_RETRIES["E_SCHEMA"], 2)
        self.assertEqual(O.MAX_RETRIES["E_ARG_RANGE"], 1)
        self.assertEqual(O.MAX_RETRIES["E_OUT_OF_BOUNDS"], 1)
        self.assertEqual(O.MAX_RETRIES["E_KEY_NOT_ALLOWED"], 1)
        self.assertEqual(O.MAX_RETRIES["E_MODEL_TIMEOUT"], 2)
        self.assertEqual(O.MAX_RETRIES["E_EXPECT_TIMEOUT"], 1)
        self.assertEqual(O.MAX_RETRIES["E_POPUP_BLOCKING"], 2)

    def test_model_http_is_context_dependent(self):
        self.assertTrue(O.ERROR_POLICY["E_MODEL_HTTP"]["retryable_5xx"])
        self.assertFalse(O.ERROR_POLICY["E_MODEL_HTTP"]["retryable_4xx"])


class ImplErrorRetryability(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.policy = A.error_policy()

    def test_retryable_flags_for_all_documented_codes(self):
        missing = []
        wrong = []
        for code in O.ALL_CODES:
            if code == "E_MODEL_HTTP":
                continue  # 由 test_model_http_context 单独判
            expected = O.ERROR_POLICY[code]["retryable"]
            got = self.policy.retryable(code)
            if got is None:
                missing.append(code)
                continue
            if bool(got) is not bool(expected):
                wrong.append((code, got, expected))
        self.assertEqual(wrong, [], "可重试性判定错误：%r" % (wrong,))
        self.assertEqual(missing, [], "§5 冻结错误码未被分类（实现未暴露 retryable 信息）：%r" % (missing,))

    def test_stall_and_loop_are_not_retryable(self):
        for code in ("E_STALL", "E_LOOP"):
            got = self.policy.retryable(code)
            if got is None:
                self.skipTest("实现未暴露 %s 的可重试性，无法判定" % code)
            self.assertFalse(bool(got), "%s 必须是不可重试（语义上要重规划，不是重试）" % code)

    def test_retryable_counterexample_is_retryable(self):
        got = self.policy.retryable("E_EXPECT_TIMEOUT")
        if got is None:
            self.skipTest("实现未暴露 E_EXPECT_TIMEOUT 的可重试性")
        self.assertTrue(bool(got), "E_EXPECT_TIMEOUT 属可重试（最多 1 次）")


class ImplErrorReplan(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.policy = A.error_policy()

    def test_must_replan_flags(self):
        values = dict((c, self.policy.must_replan(c)) for c in O.ALL_CODES)
        known = dict((c, v) for c, v in values.items() if v is not None)
        if not known:
            self.skipTest("实现未暴露「必须重规划」标记（可能只实现了可重试性），记为待验证")
        for code, got in known.items():
            expected = code in O.MUST_REPLAN_CODES
            self.assertEqual(bool(got), expected,
                             "%s 的 must_replan 应为 %s，实际 %s" % (code, expected, got))


class ImplErrorPolicyConsistency(unittest.TestCase):
    """两个策略面必须一致：§5 数据表 vs 代码真正调用的判定函数。

    只验一个面会漏掉「表对、函数错」这类缺陷 —— 运行期执行的是函数。
    """

    @classmethod
    def setUpClass(cls):
        cls.policy = A.error_policy()

    def test_table_and_functions_agree(self):
        fn_view = dict((c, self.policy.spec(c, source="fns")) for c in O.ALL_CODES)
        if not any(fn_view.values()):
            self.skipTest("实现只暴露了数据表，没有可调用的判定函数，无法做一致性比对")
        disagreements = []
        for code in O.ALL_CODES:
            if code == "E_MODEL_HTTP":
                continue
            table = self.policy.spec(code, source="table")
            fns = self.policy.spec(code, source="fns")
            for key in ("retryable", "max_retries", "must_replan"):
                if key in table and key in fns and bool(table[key]) is not bool(fns[key]):
                    disagreements.append((code, key, table[key], fns[key]))
        self.assertEqual(disagreements, [],
                         "策略数据表与判定函数结论不一致：%r" % (disagreements,))


class ImplErrorMaxRetries(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.policy = A.error_policy()

    def test_max_retries_when_exposed(self):
        values = dict((c, self.policy.max_retries(c)) for c in O.MAX_RETRIES)
        known = dict((c, v) for c, v in values.items() if v is not None)
        if not known:
            self.skipTest("实现未暴露重试上限计数，记为待验证（§5 的「最多 N 次」未落地为数据）")
        for code, got in known.items():
            self.assertEqual(int(got), O.MAX_RETRIES[code],
                             "%s 的最大重试次数应为 %s，实际 %s" % (code, O.MAX_RETRIES[code], got))


class ImplModelHttpContext(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.policy = A.error_policy()

    def test_4xx_not_retryable_5xx_retryable(self):
        five = self.policy.retryable("E_MODEL_HTTP", 503)
        four = self.policy.retryable("E_MODEL_HTTP", 404)
        if five is None and four is None:
            self.skipTest("实现未暴露 E_MODEL_HTTP 的上下文相关判定，记为待验证")
        if five is not None:
            self.assertTrue(bool(five), "5xx 应可重试")
        if four is not None:
            self.assertFalse(bool(four), "4xx 不应重试（key/额度/模型名问题，重试无意义）")

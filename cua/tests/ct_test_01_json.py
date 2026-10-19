# -*- coding: utf-8 -*-
"""契约测试 01：§2.1 JSON 提取（纯 JSON / 围栏 / 自然语言包裹 / 字符串内花括号 / 转义引号）。

分组约定：
  Oracle*    : 跑 verifier 参考实现（证明判据本身可执行、有内容）；
  Impl*      : 跑真实实现（入口解析不到 -> skip，绝不 ImportError）；
  Ambiguous* : 文档字面存在多种合理解读，断言落在允许集合内，并在报告里列为契约歧义。
"""
import json
import unittest

import ct_adapter as A
import ct_oracle as O


def step(action=None, **over):
    s = {
        "protocol": "cua/1",
        "thought": "看到地址栏已在焦点，直接输入网址",
        "action": action if action is not None else {"type": "wait", "ms": 500, "reason": "等加载"},
        "done": False,
    }
    s.update(over)
    return s


def fenced(payload, lang="json"):
    fence = chr(96) * 3
    return fence + lang + chr(10) + payload + chr(10) + fence


BS = chr(92)  # 反斜杠，避免本文件自身出现转义歧义

THOUGHT_BRACES = "看到 {弹窗} 里的 {确定} 按钮，先关掉它"
THOUGHT_QUOTES = '提示写着 "确定要删除吗" 需要我确认'
THOUGHT_NESTED = '参数是 {"a": 1, "b": [2, 3]} 这样的结构'
THOUGHT_BACKSLASH = "路径是 C:" + BS + "tmp" + BS + "a.txt"

PURE = json.dumps(step(), ensure_ascii=False)

OK_CASES = [
    ("pure_json", PURE, step()),
    ("fenced_json", fenced(PURE, "json"), step()),
    ("fenced_plain", fenced(PURE, ""), step()),
    ("prose_wrapped", "好的，这是本步输出：" + chr(10) + PURE + chr(10) + "希望有帮助。", step()),
    ("fenced_with_prose_around", "分析如下" + chr(10) + fenced(PURE, "json") + chr(10) + "以上。", step()),
    ("braces_inside_string", json.dumps(step(thought=THOUGHT_BRACES), ensure_ascii=False), step(thought=THOUGHT_BRACES)),
    ("escaped_quotes", json.dumps(step(thought=THOUGHT_QUOTES), ensure_ascii=False), step(thought=THOUGHT_QUOTES)),
    ("nested_json_inside_string", json.dumps(step(thought=THOUGHT_NESTED), ensure_ascii=False), step(thought=THOUGHT_NESTED)),
    ("escaped_backslash", json.dumps(step(thought=THOUGHT_BACKSLASH), ensure_ascii=False), step(thought=THOUGHT_BACKSLASH)),
]

BAD_CASES = [
    ("no_json_at_all", "抱歉，我无法完成这个操作。", "E_BAD_JSON"),
    ("unbalanced_object", '{"protocol": "cua/1", "thought": "x"', "E_BAD_JSON"),
    ("trailing_comma", '{"protocol": "cua/1", "done": false,}', "E_BAD_JSON"),
    ("empty_text", "", "E_BAD_JSON"),
    ("fence_with_broken_json", fenced('{"broken": ', "json"), "E_BAD_JSON"),
]

AMBIGUOUS_CASES = [
    ("prose_brace_before_json", "注意 {这里只是说明} 然后 " + PURE),
    ("toplevel_array", "[1, 2, 3]"),
]


class OracleJsonExtraction(unittest.TestCase):
    def test_pure_and_wrapped(self):
        for name, text, expected in OK_CASES:
            with self.subTest(case=name):
                got = O.extract_json(text)
                self.assertEqual(got, expected)

    def test_bad_input_reports_bad_json(self):
        for name, text, code in BAD_CASES:
            with self.subTest(case=name):
                with self.assertRaises(O.ContractError) as ctx:
                    O.extract_json(text)
                self.assertEqual(ctx.exception.code, code)

    def test_balance_scanner_ignores_braces_in_strings(self):
        got = O.extract_json(json.dumps(step(thought=THOUGHT_BRACES), ensure_ascii=False))
        self.assertEqual(got["thought"], THOUGHT_BRACES)

    def test_balance_scanner_handles_escapes(self):
        got = O.extract_json('{"thought": "他说 ' + BS + '"确定' + BS + '" 然后继续"}')
        self.assertEqual(got["thought"], '他说 "确定" 然后继续')

    def test_fence_strip(self):
        self.assertEqual(O.strip_code_fence(fenced(PURE, "json")).strip(), PURE)
        self.assertEqual(O.strip_code_fence(fenced(PURE, "")).strip(), PURE)
        self.assertIsNone(O.strip_code_fence(PURE))


class ImplJsonExtraction(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.cap = A.json_extractor()

    def test_valid_inputs_extract(self):
        for name, text, expected in OK_CASES:
            with self.subTest(case=name):
                res = self.cap(text)
                self.assertTrue(res.ok, "应能提取 JSON，实际失败：%s" % res.describe())
                got = A.as_object(res.value)
                self.assertIsNotNone(got, "返回值不是对象/JSON 对象字符串：%r" % (res.value,))
                self.assertEqual(got, expected)

    def test_invalid_inputs_report_bad_json(self):
        for name, text, code in BAD_CASES:
            with self.subTest(case=name):
                res = self.cap(text)
                if res.ok:
                    self.fail("应报 %s，实际返回了 %r" % (code, res.value))
                self.assertEqual(res.code, code,
                                 "错误码应为 %s，实际 %s（%r）" % (code, res.code, res.exc))

    def test_string_content_not_truncated_by_inner_brace(self):
        res = self.cap(json.dumps(step(thought=THOUGHT_BRACES), ensure_ascii=False))
        self.assertTrue(res.ok, res.describe())
        got = A.as_object(res.value)
        self.assertEqual(got["thought"], THOUGHT_BRACES)


class AmbiguousJsonExtraction(unittest.TestCase):
    """契约字面未唯一确定的两种输入：接受「解析出对象」或「E_BAD_JSON」两种实现。"""

    def test_prose_brace_before_json(self):
        name, text = AMBIGUOUS_CASES[0]
        with self.subTest(runner="oracle"):
            try:
                O.extract_json(text)
                ok = True
            except O.ContractError:
                ok = False
            self.assertIn(ok, (True, False))
        cap = A.json_extractor()
        res = cap(text)
        if res.ok:
            self.assertIsNotNone(A.as_object(res.value))
        else:
            self.assertEqual(res.code, "E_BAD_JSON")

    def test_toplevel_array(self):
        cap = A.json_extractor()
        res = cap(AMBIGUOUS_CASES[1][1])
        if res.ok:
            self.assertIsInstance(A.unwrap(res.value), (list, tuple))
        else:
            self.assertEqual(res.code, "E_BAD_JSON")

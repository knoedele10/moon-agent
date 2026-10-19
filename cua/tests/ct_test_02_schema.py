# -*- coding: utf-8 -*-
"""契约测试 02：§2/§3 Step 与动作的 Schema 校验与错误分类。

判据来源：docs/cua/action.schema.json（机器可读、可执行）+ md §2 表格与 §5 错误码表。
所有非法 fixture 都刻意只含「一处故障」，避免文档未规定的错误优先级影响结论。

分组：
  Oracle*    : verifier 参考实现 + jsonschema(2020-12) 交叉校验；
  Impl*      : 真实实现（入口解析不到 -> skip）；
  Ambiguous* : schema 与 md 解读存在多种合理归类（E_SCHEMA 还是 E_ARG_RANGE），断言落在允许集合内；
  Conflict*  : md 与 schema 明确互相矛盾之处（见 docs/verify/task-7-cua-contract-tests.md）。
"""
import unittest

import ct_adapter as A
import ct_oracle as O


def mk(action, done=False, **over):
    s = {"protocol": "cua/1", "thought": "先看再动，只做一步", "action": action, "done": done}
    s.update(over)
    return s


def bad(name, step, code, schema_invalid=True):
    return {"name": name, "step": step, "code": code, "schema_invalid": schema_invalid}


OK_STEPS = [
    ("click", mk({"type": "click", "x": 640, "y": 210})),
    ("click_double_right", mk({"type": "click", "x": 10, "y": 20, "button": "right", "clicks": 2,
                               "modifiers": ["ctrl", "shift"]})),
    ("type", mk({"type": "type", "text": "Hello", "clear_first": True, "submit": True, "interval_ms": 0})),
    ("hotkey_single", mk({"type": "hotkey", "keys": ["enter"]})),
    ("hotkey_combo", mk({"type": "hotkey", "keys": ["ctrl", "shift", "s"], "repeat": 1})),
    ("drag", mk({"type": "drag", "from": {"x": 100, "y": 200}, "to": {"x": 800, "y": 640},
                 "duration_ms": 600, "button": "left"})),
    ("scroll", mk({"type": "scroll", "x": 960, "y": 540, "dy": -3, "dx": 0, "unit": "line"})),
    ("move", mk({"type": "move", "x": 300, "y": 400, "duration_ms": 200})),
    ("wait", mk({"type": "wait", "ms": 1500, "reason": "等待页面加载"})),
    ("screenshot_full", mk({"type": "screenshot", "region": None})),
    ("screenshot_region", mk({"type": "screenshot", "region": {"x": 100, "y": 100, "width": 320, "height": 200}})),
    ("done_action", mk({"type": "done", "status": "success", "summary": "已完成", "artifacts": []})),
    ("done_flag", mk({"type": "click", "x": 1, "y": 2}, done=True,
                     final={"status": "success", "summary": "已把 README 标题改为 Hello"})),
    ("done_action_consistent_with_final",
     mk({"type": "done", "status": "success", "summary": "已完成"},
        done=True, final={"status": "success", "summary": "已完成"})),
    ("unknown_extra_fields_ignored", mk({"type": "wait", "ms": 500}, done=False,
                                        whatever="向前兼容：未知字段一律忽略", extra={"a": 1})),
    ("expect_block", mk({"type": "wait", "ms": 500}, done=False,
                        expect={"what": "页面开始加载", "timeout_ms": 8000, "must_change": True},
                        risk="low")),
    ("expect_null", mk({"type": "wait", "ms": 500}, done=False, expect=None)),
    ("final_null_when_not_done", mk({"type": "wait", "ms": 500}, done=False, final=None)),
]

LENIENT_STEPS = [
    ("float_coords", mk({"type": "click", "x": 100.4, "y": 200.6})),
]

BAD_STEPS = [
    bad("missing_protocol", {"thought": "x", "action": {"type": "wait", "ms": 500}, "done": False}, "E_SCHEMA"),
    bad("wrong_protocol", mk({"type": "wait", "ms": 500}, protocol="cua/2"), "E_SCHEMA"),
    bad("missing_thought", {"protocol": "cua/1", "action": {"type": "wait", "ms": 500}, "done": False}, "E_SCHEMA"),
    bad("empty_thought", mk({"type": "wait", "ms": 500}, thought=""), "E_SCHEMA"),
    bad("thought_not_string", mk({"type": "wait", "ms": 500}, thought=123), "E_SCHEMA"),
    bad("thought_too_long", mk({"type": "wait", "ms": 500}, thought="x" * 1201), "E_SCHEMA"),
    bad("missing_done", {"protocol": "cua/1", "thought": "x", "action": {"type": "wait", "ms": 500}}, "E_SCHEMA"),
    bad("done_not_bool", mk({"type": "wait", "ms": 500}, done="true"), "E_SCHEMA"),
    bad("action_null_when_not_done", mk(None), "E_SCHEMA"),
    bad("action_missing_type", mk({"x": 1, "y": 2}), "E_SCHEMA"),
    bad("click_missing_x", mk({"type": "click", "y": 210}), "E_SCHEMA"),
    bad("click_x_string", mk({"type": "click", "x": "640", "y": 210}), "E_SCHEMA"),
    bad("type_missing_text", mk({"type": "type", "clear_first": True}), "E_SCHEMA"),
    bad("risk_invalid", mk({"type": "wait", "ms": 500}, risk="whatever"), "E_SCHEMA"),
    bad("done_true_missing_final", mk({"type": "click", "x": 1, "y": 2}, done=True), "E_SCHEMA"),
    bad("action_null_when_done_true",
        mk(None, done=True, final={"status": "success", "summary": "done"}), "E_SCHEMA"),
    bad("done_action_status_conflicts_with_final",
        mk({"type": "done", "status": "success", "summary": "同样"},
           done=True, final={"status": "failed", "summary": "同样"}), "E_SCHEMA", schema_invalid=False),
    bad("done_action_summary_conflicts_with_final",
        mk({"type": "done", "status": "success", "summary": "总结 A"},
           done=True, final={"status": "success", "summary": "总结 B"}), "E_SCHEMA", schema_invalid=False),
    bad("done_true_final_empty", mk({"type": "click", "x": 1, "y": 2}, done=True, final={}), "E_SCHEMA"),
    bad("done_true_final_bad_status", mk({"type": "click", "x": 1, "y": 2}, done=True,
                                         final={"status": "ok", "summary": "x"}), "E_SCHEMA"),
    bad("expect_timeout_wrong_type", mk({"type": "wait", "ms": 500}, expect={"timeout_ms": "8000"}), "E_SCHEMA"),
    bad("expect_not_object", mk({"type": "wait", "ms": 500}, expect="soon"), "E_SCHEMA"),
    bad("screenshot_region_missing_height",
        mk({"type": "screenshot", "region": {"x": 0, "y": 0, "width": 100}}), "E_SCHEMA"),
    bad("done_action_bad_status", mk({"type": "done", "status": "ok", "summary": "x"}), "E_SCHEMA"),

    bad("unknown_type_exec", mk({"type": "exec", "cmd": "whoami"}), "E_UNKNOWN_ACTION"),
    bad("unknown_type_shell", mk({"type": "shell", "command": "dir"}), "E_UNKNOWN_ACTION"),
    bad("unknown_type_press", mk({"type": "press", "key": "enter"}), "E_UNKNOWN_ACTION"),

    bad("scroll_dy_99", mk({"type": "scroll", "x": 960, "y": 540, "dy": 99}), "E_ARG_RANGE"),
    bad("scroll_dy_minus99", mk({"type": "scroll", "x": 960, "y": 540, "dy": -99}), "E_ARG_RANGE"),
    bad("wait_ms_50", mk({"type": "wait", "ms": 50}), "E_ARG_RANGE"),
    bad("wait_ms_20000", mk({"type": "wait", "ms": 20000}), "E_ARG_RANGE"),
    bad("drag_duration_50", mk({"type": "drag", "from": {"x": 0, "y": 0}, "to": {"x": 10, "y": 10},
                                "duration_ms": 50}), "E_ARG_RANGE"),
    bad("drag_duration_6000", mk({"type": "drag", "from": {"x": 0, "y": 0}, "to": {"x": 10, "y": 10},
                                  "duration_ms": 6000}), "E_ARG_RANGE"),
    bad("type_interval_600", mk({"type": "type", "text": "hi", "interval_ms": 600}), "E_ARG_RANGE"),
    bad("hotkey_repeat_21", mk({"type": "hotkey", "keys": ["enter"], "repeat": 21}), "E_ARG_RANGE"),
    bad("hotkey_repeat_0", mk({"type": "hotkey", "keys": ["enter"], "repeat": 0}), "E_ARG_RANGE"),
    bad("move_duration_5000", mk({"type": "move", "x": 1, "y": 2, "duration_ms": 5000}), "E_ARG_RANGE"),

    bad("key_f13", mk({"type": "hotkey", "keys": ["f13"]}), "E_KEY_NOT_ALLOWED", schema_invalid=False),
    bad("key_escape", mk({"type": "hotkey", "keys": ["escape"]}), "E_KEY_NOT_ALLOWED", schema_invalid=False),
    bad("key_return", mk({"type": "hotkey", "keys": ["return"]}), "E_KEY_NOT_ALLOWED", schema_invalid=False),
    bad("key_meta", mk({"type": "hotkey", "keys": ["meta"]}), "E_KEY_NOT_ALLOWED", schema_invalid=False),
    bad("key_trailing_space", mk({"type": "hotkey", "keys": ["enter "]}), "E_KEY_NOT_ALLOWED", schema_invalid=False),
    bad("key_unknown_word", mk({"type": "hotkey", "keys": ["ctrl", "whatever"]}), "E_KEY_NOT_ALLOWED", schema_invalid=False),
]

AMBIGUOUS_STEPS = [
    ("keys_empty", mk({"type": "hotkey", "keys": []}), {"E_SCHEMA", "E_ARG_RANGE"}),
    ("keys_four", mk({"type": "hotkey", "keys": ["ctrl", "alt", "shift", "s"]}), {"E_SCHEMA", "E_ARG_RANGE"}),
    ("modifiers_four", mk({"type": "click", "x": 1, "y": 2, "modifiers": ["ctrl", "alt", "shift", "win"]}),
     {"E_SCHEMA", "E_ARG_RANGE"}),
    ("modifiers_cmd", mk({"type": "click", "x": 1, "y": 2, "modifiers": ["cmd"]}), {"E_SCHEMA", "E_ARG_RANGE"}),
    ("clicks_3", mk({"type": "click", "x": 1, "y": 2, "clicks": 3}), {"E_SCHEMA", "E_ARG_RANGE"}),
    ("screenshot_width_8", mk({"type": "screenshot", "region": {"x": 0, "y": 0, "width": 8, "height": 100}}),
     {"E_SCHEMA", "E_ARG_RANGE"}),
    ("expect_timeout_40000", mk({"type": "wait", "ms": 500}, expect={"timeout_ms": 40000}),
     {"E_SCHEMA", "E_ARG_RANGE"}),
    ("dy_float", mk({"type": "scroll", "x": 960, "y": 540, "dy": 3.5}), {"E_SCHEMA", "E_ARG_RANGE"}),
]


class OracleSchema(unittest.TestCase):
    def test_schema_module_available(self):
        self.assertTrue(O.schema_available(), "jsonschema 不可用，无法交叉校验")

    def test_valid_steps_pass(self):
        for name, s in OK_STEPS:
            with self.subTest(case=name):
                self.assertIsNone(O.classify_step(s), "参考实现判为非法：%s" % name)
                self.assertTrue(O.schema_valid(s), "schema 判为非法：%s" % name)

    def test_invalid_steps_rejected_with_expected_code(self):
        for case in BAD_STEPS:
            with self.subTest(case=case["name"]):
                self.assertEqual(O.classify_step(case["step"]), case["code"])

    def test_schema_crosscheck(self):
        """每个非法 fixture 的「是否违反 frozen schema」与预期一致（键名白名单属解析器规则，故例外）。"""
        for case in BAD_STEPS:
            with self.subTest(case=case["name"]):
                schema_bad = not O.schema_valid(case["step"])
                self.assertEqual(schema_bad, case["schema_invalid"],
                                 "schema 有效性与预期不一致：%s" % case["name"])

    def test_lenient_float_coords_accepted_but_schema_invalid(self):
        s = dict(LENIENT_STEPS)["float_coords"]
        self.assertIsNone(O.classify_step(s), "§2.1 明确要求解析器对数值坐标宽容（接受小数）")
        self.assertFalse(O.schema_valid(s), "schema 要求 integer；此处正是 md/schema 的差异点")

    def test_error_table_covers_all_documented_codes(self):
        self.assertEqual(len(O.ALL_CODES), 20)
        self.assertEqual(O.MUST_REPLAN_CODES, frozenset({"E_STALL", "E_LOOP"}))


class ImplSchema(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.cap = A.step_validator()

    def test_valid_steps_pass(self):
        for name, s in OK_STEPS:
            with self.subTest(case=name):
                got = A.verdict(self.cap(s))
                self.assertEqual(got, "OK", "合法 Step 被拒绝：%s（%s）" % (name, got))

    def test_lenient_float_coords_accepted(self):
        for name, s in LENIENT_STEPS:
            with self.subTest(case=name):
                got = A.verdict(self.cap(s))
                self.assertEqual(got, "OK", "§2.1 要求接受小数坐标并四舍五入，实际 %s" % got)

    def test_invalid_steps_rejected_with_expected_code(self):
        # 综合校验面：契约只要求「被拒 + 正确错误码」，未规定必须由解析器还是执行层拒绝。
        for case in BAD_STEPS:
            with self.subTest(case=case["name"]):
                got = A.reject_step(case["step"])
                self.assertNotEqual(got, "OK", "非法 Step 被接受：%s" % case["name"])
                self.assertEqual(got, case["code"],
                                 "错误码应为 %s，实际 %s" % (case["code"], got))


class AmbiguousSchema(unittest.TestCase):
    """schema 与 md 对「越界参数」的归类未唯一确定：E_SCHEMA 与 E_ARG_RANGE 都算合理。"""

    @classmethod
    def setUpClass(cls):
        cls.cap = A.step_validator()

    def test_ambiguous_cases_land_in_allowed_set(self):
        for name, s, allowed in AMBIGUOUS_STEPS:
            with self.subTest(case=name):
                oracle_code = O.classify_step(s)
                self.assertIn(oracle_code, allowed, "参考实现的归类越出允许集合")
                got = A.verdict(self.cap(s))
                self.assertNotEqual(got, "OK", "非法 Step 被接受：%s" % name)
                self.assertIn(got, allowed, "实现错误码 %s 不在允许集合 %s" % (got, sorted(allowed)))


class ContractRulingActionNeverNull(unittest.TestCase):
    """原 md/schema 冲突点（done=true 时 action 可否为 null）。

    Lead 于 2026-10-11 裁定：以 Schema 为准，action 永远是必填的动作对象，不可为 null。
    本类把裁定固化成硬判据。
    """

    def setUp(self):
        self.step = mk(None, done=True, final={"status": "success", "summary": "已完成"})

    def test_oracle_matches_ruling(self):
        self.assertEqual(O.classify_step(self.step), "E_SCHEMA")
        self.assertFalse(O.schema_valid(self.step), "schema 本身就不接受 action=null")

    def test_impl_rejects_null_action(self):
        cap = A.step_validator()
        got = A.verdict(cap(self.step))
        self.assertNotEqual(got, "OK", "裁定要求 action 不可为 null，实际实现接受了")
        self.assertEqual(got, "E_SCHEMA", "应为 E_SCHEMA，实际 %s" % got)


class DoneActionMustAgreeWithFinal(unittest.TestCase):
    """Lead 裁定新增约束：action.type=done 的 status/summary 必须与顶层 final 一致。"""

    def _cases(self):
        return [
            ("status_conflict", mk({"type": "done", "status": "success", "summary": "同样"},
                                   done=True, final={"status": "failed", "summary": "同样"})),
            ("summary_conflict", mk({"type": "done", "status": "success", "summary": "总结 A"},
                                    done=True, final={"status": "success", "summary": "总结 B"})),
            ("consistent", mk({"type": "done", "status": "success", "summary": "一致"},
                              done=True, final={"status": "success", "summary": "一致"})),
        ]

    def test_oracle_applies_ruling(self):
        for name, step in self._cases():
            with self.subTest(case=name):
                expected = "E_SCHEMA" if name != "consistent" else None
                self.assertEqual(O.classify_step(step), expected)

    def test_impl_applies_ruling(self):
        cap = A.step_validator()
        for name, step in self._cases():
            with self.subTest(case=name):
                got = A.verdict(cap(step))
                if name == "consistent":
                    self.assertEqual(got, "OK", "一致的 done 动作被误拒：%s" % got)
                else:
                    self.assertNotEqual(got, "OK", "%s：不一致的 done 动作被接受" % name)
                    self.assertEqual(got, "E_SCHEMA", "%s：应为 E_SCHEMA，实际 %s" % (name, got))

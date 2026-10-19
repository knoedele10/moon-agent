# -*- coding: utf-8 -*-
"""契约测试 04：越界拒绝（§1.1 / §8.2）——必须报 E_OUT_OF_BOUNDS，绝不 clamp。

合法区间按 0 基像素 [0, width) x [0, height)；因此 x == image.width 也算越界
（任务书写明「坐标达到或超出 image 宽高必须报 E_OUT_OF_BOUNDS」）。
"""
import unittest

import ct_adapter as A
import ct_oracle as O

W, H = 1472, 828
FULL = {"x": 0, "y": 0}

OOB_POINTS = [
    ("x_equals_width", W, 10),
    ("x_over_width", 1600, 10),
    ("y_equals_height", 10, H),
    ("y_over_height", 10, 999),
    ("negative_x", -1, 10),
    ("negative_y", 10, -1),
    ("far_out", 99999, 99999),
]
IN_RANGE_POINTS = [
    ("origin", 0, 0),
    ("typical", 736, 414),
    ("bottom_right", W - 1, H - 1),
]


def click_step(x, y):
    return {"protocol": "cua/1", "thought": "点这里", "action": {"type": "click", "x": x, "y": y},
            "done": False}


class OracleBounds(unittest.TestCase):
    def test_out_of_bounds_rejected(self):
        for name, x, y in OOB_POINTS:
            with self.subTest(case=name):
                with self.assertRaises(O.ContractError) as ctx:
                    O.check_bounds(x, y, W, H)
                self.assertEqual(ctx.exception.code, "E_OUT_OF_BOUNDS")

    def test_in_range_accepted(self):
        for name, x, y in IN_RANGE_POINTS:
            with self.subTest(case=name):
                self.assertIsNone(O.check_bounds(x, y, W, H))

    def test_no_clamping(self):
        """拒绝就是拒绝：不得返回 clamp 后的 (W-1, H-1)。"""
        with self.assertRaises(O.ContractError):
            O.check_bounds(99999, 99999, W, H)


class ImplBoundsRejection(unittest.TestCase):
    """越界必须在「换算入口」被拒绝：检验的是 §1.1 的调用链（先检后换）。

    单看 image_to_screen 不带边界检查并不违反契约 —— 文档把检查放在
    check_image_bounds/to_screen_point（执行器唯一入口），所以这里用一步到位的
    受检入口；适配层会在无专用入口时退回「check_image_bounds + image_to_screen」。
    """

    @classmethod
    def setUpClass(cls):
        cls.to_screen = A.bounds_enforced_convert()
        cls.raw = A.image_to_screen()

    def test_out_of_bounds_points_are_rejected(self):
        for name, x, y in OOB_POINTS:
            with self.subTest(case=name):
                res = self.to_screen(x, y, FULL, 1.0, W, H)
                if not A.rejected(res):
                    self.fail("image(%s,%s) 未被拒绝，返回 %r；契约 §1.1/§8.2 要求报 "
                              "E_OUT_OF_BOUNDS 且绝不 clamp（入口 %s）"
                              % (x, y, res.value, self.to_screen.where))

    def test_no_clamping_on_oob(self):
        """拒绝绝不等于 clamp：若越界点被换算返回，返回值不得是夹回边界的结果。"""
        for name, x, y in OOB_POINTS:
            with self.subTest(case=name):
                res = self.to_screen(x, y, FULL, 1.0, W, H)
                if A.rejected(res):
                    continue
                got = tuple(A.as_point(res.value))
                clamped = (min(max(x, 0), W - 1), min(max(y, 0), H - 1))
                self.assertNotEqual(got, clamped,
                                    "越界 (%s,%s) 被 clamp 成 %r（入口 %s）"
                                    % (x, y, got, self.to_screen.where))

    def test_in_range_points_are_not_rejected(self):
        for name, x, y in IN_RANGE_POINTS:
            with self.subTest(case=name):
                res = self.to_screen(x, y, FULL, 1.0, W, H)
                self.assertFalse(A.rejected(res),
                                 "合法坐标被拒绝：(%s,%s) -> %s" % (x, y, res.describe()))
                got = tuple(A.as_point(res.value))
                self.assertEqual(got, O.image_to_screen(x, y, FULL, 1.0),
                                 "合法坐标换算结果与 §1.1 公式不符（疑似 clamp）")

    def test_oob_reports_e_out_of_bounds_code(self):
        """至少有一个契约入口把越界上报为 E_OUT_OF_BOUNDS（§4/§5 的错误码面）。"""
        seen = []
        res = self.to_screen(W, 10, FULL, 1.0, W, H)
        if A.rejected(res):
            seen.append((self.to_screen.where, A.rejection_code(res)))
        raw = self.raw(W, 10, FULL, 1.0)
        if A.rejected(raw):
            seen.append(("image_to_screen", A.rejection_code(raw)))
        try:
            bc = A.bounds_checker()
            r2 = bc(W, 10, W, H)
            if A.rejected(r2):
                seen.append(("bounds_check", A.rejection_code(r2)))
        except unittest.SkipTest:
            pass
        try:
            v = A.step_validator_with_image()
            r3 = v(click_step(W, 10), {"width": W, "height": H})
            seen.append(("validate_with_image", A.verdict(r3)))
        except unittest.SkipTest:
            pass
        codes = [c for _, c in seen if c and c.startswith("E_")]
        self.assertIn("E_OUT_OF_BOUNDS", codes,
                      "越界拒绝未以 E_OUT_OF_BOUNDS 上报；观察到的入口/错误码：%r" % (seen,))


class ImplDedicatedBoundsChecker(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.check = A.bounds_checker()

    def test_rejects_out_of_bounds(self):
        for name, x, y in OOB_POINTS:
            with self.subTest(case=name):
                res = self.check(x, y, W, H)
                self.assertTrue(A.rejected(res),
                                "专用越界判定未拒绝 (%s,%s)：%s" % (x, y, res.describe()))

    def test_accepts_in_range(self):
        for name, x, y in IN_RANGE_POINTS:
            with self.subTest(case=name):
                res = self.check(x, y, W, H)
                self.assertFalse(A.rejected(res),
                                 "专用越界判定误拒合法坐标 (%s,%s)：%s" % (x, y, res.describe()))


class ImplBoundsThroughExecutor(unittest.TestCase):
    """端到端：执行器是 §1.1 越界判定的真正入口（click / scroll / drag / region 全覆盖）。

    解析层不认识图像尺寸（§10 把解析器定位为 Step 解析/Schema 校验），所以越界只能由
    执行层把关；本类用执行入口（带 image 尺寸的帧）复验，避免把「解析器没有 image 参数」
    误判成契约缺失。
    """

    @classmethod
    def setUpClass(cls):
        cls.cap = A.action_executor()

    def _expect_oob(self, name, action):
        res = self.cap(action, width=W, height=H)
        got = A.verdict(res)
        self.assertNotEqual(got, "OK", "%s：越界动作被接受（%s）" % (name, res.describe()))
        self.assertEqual(got, "E_OUT_OF_BOUNDS",
                         "%s：应为 E_OUT_OF_BOUNDS，实际 %s（入口 %s）"
                         % (name, got, self.cap.where))

    def test_click_at_width(self):
        self._expect_oob("click_x_eq_width", {"type": "click", "x": W, "y": 10})

    def test_scroll_anchor_out_of_bounds(self):
        self._expect_oob("scroll_anchor_oob", {"type": "scroll", "x": 10, "y": H, "dy": -3})

    def test_drag_endpoint_out_of_bounds(self):
        self._expect_oob("drag_to_oob", {"type": "drag", "from": {"x": 1, "y": 1},
                                         "to": {"x": W + 5, "y": 10}, "duration_ms": 300})

    def test_screenshot_region_out_of_bounds(self):
        self._expect_oob("region_oob", {"type": "screenshot",
                                        "region": {"x": W - 10, "y": 10, "width": 100, "height": 100}})

    def test_in_range_still_accepted(self):
        res = self.cap({"type": "click", "x": W - 1, "y": 10}, width=W, height=H)
        self.assertEqual(A.verdict(res), "OK", "合法坐标被误拒：%s" % res.describe())

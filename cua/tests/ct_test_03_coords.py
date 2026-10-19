# -*- coding: utf-8 -*-
"""契约测试 03：§1.1 坐标换算（全屏 / 局部 region 的 origin/scale，正算与逆算，小数取整）。

硬判据全部手算自 §1.1 公式 screen = origin + round(image / scale)；
其中 full_screen_doc_example 直接取自 md §4 的 ActionResult 例子（640,210 -> 835,274），
属于文档自带的金标准。
"""
import unittest

import ct_adapter as A
import ct_oracle as O

FULL = {"x": 0, "y": 0}
REGION = {"x": 300, "y": 200}
LEFT_MONITOR = {"x": -1920, "y": 0}
REGION_075 = {"x": 100, "y": 50}

# (名称, 图内坐标 x, y, origin, scale, 期望屏幕坐标)
COORD_CASES = [
    ("full_screen_doc_example", 640, 210, FULL, 0.7667, (835, 274)),
    ("identity", 640, 210, FULL, 1.0, (640, 210)),
    ("downscale_2x", 640, 210, FULL, 0.5, (1280, 420)),
    ("region_upscale_2x", 640, 210, REGION, 2.0, (620, 305)),
    ("region_scale_075", 150, 75, REGION_075, 0.75, (300, 150)),
    ("negative_origin_left_monitor", 960, 540, LEFT_MONITOR, 1.0, (-960, 540)),
    ("float_round_down", 100.4, 200.4, FULL, 1.0, (100, 200)),
    ("float_round_up", 100.6, 200.6, FULL, 1.0, (101, 201)),
    ("bottom_right_pixel", 1471, 827, FULL, 0.7667, (1919, 1079)),
]

# 正算 -> 逆算 往返（文档未显式冻结逆公式，此处由 §1.1 推导；结论是往返必须回到原值）
ROUND_TRIP_CASES = [
    ("full_screen_doc_example", 640, 210, FULL, 0.7667),
    ("region_upscale_2x", 640, 210, REGION, 2.0),
    ("identity", 100, 100, REGION_075, 1.0),
    ("bottom_right_pixel", 1471, 827, FULL, 0.7667),
]

# 0.5 边界：md 写「四舍五入」（half up=进位），Python 内置 round 是银行家舍入（.5 向偶）。
HALF_CASES = [
    ("half_0_5", 1, 3, FULL, 2.0, (1, 2)),      # 0.5 -> 1(四舍五入) / 0(round); 1.5 -> 2 相同
    ("half_2_5", 5, 101, FULL, 2.0, (3, 51)),   # 2.5 -> 3 / 2 ; 50.5 -> 51 / 50
]


class OracleCoords(unittest.TestCase):
    def test_image_to_screen(self):
        for name, x, y, origin, scale, expected in COORD_CASES:
            with self.subTest(case=name):
                self.assertEqual(O.image_to_screen(x, y, origin, scale), expected)

    def test_round_trip(self):
        for name, x, y, origin, scale in ROUND_TRIP_CASES:
            with self.subTest(case=name):
                sx, sy = O.image_to_screen(x, y, origin, scale)
                self.assertEqual(O.screen_to_image(sx, sy, origin, scale), (x, y))

    def test_half_up_is_documented_reading(self):
        self.assertEqual(O.round_half_up(0.5), 1)
        self.assertEqual(O.round_half_up(50.5), 51)
        self.assertNotEqual(O.round_half_up(0.5), round(0.5), "Python round 是银行家舍入")


class ImplCoords(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.to_screen = A.image_to_screen()

    def test_image_to_screen(self):
        for name, x, y, origin, scale, expected in COORD_CASES:
            with self.subTest(case=name):
                res = self.to_screen(x, y, origin, scale)
                self.assertTrue(res.ok, "换算失败：%s" % res.describe())
                got = tuple(A.as_point(res.value))
                self.assertEqual(got, expected, "image(%s,%s) origin=%s scale=%s" % (x, y, origin, scale))

    def test_round_trip(self):
        try:
            back = A.screen_to_image()
        except unittest.SkipTest as exc:
            self.skipTest(str(exc))
        for name, x, y, origin, scale in ROUND_TRIP_CASES:
            with self.subTest(case=name):
                fwd = self.to_screen(x, y, origin, scale)
                self.assertTrue(fwd.ok, fwd.describe())
                sx, sy = A.as_point(fwd.value)
                rev = back(sx, sy, origin, scale)
                self.assertTrue(rev.ok, rev.describe())
                self.assertEqual(tuple(A.as_point(rev.value)), (x, y),
                                 "往返不一致：image(%s,%s) -> screen(%s,%s) -> %s" % (x, y, sx, sy, rev.value))

    def test_monotonic_geometry_sanity(self):
        """同一 origin/scale 下，图内坐标越大，屏幕坐标必须不减（防 x/y 颠倒）。"""
        pts = [(0, 0), (10, 5), (100, 50), (500, 250)]
        for scale in (0.5, 0.7667, 1.0, 2.0):
            prev = None
            for x, y in pts:
                res = self.to_screen(x, y, FULL, scale)
                self.assertTrue(res.ok, res.describe())
                cur = tuple(A.as_point(res.value))
                if prev is not None:
                    self.assertGreaterEqual(cur[0], prev[0], "scale=%s 时 x 单调性被破坏" % scale)
                    self.assertGreaterEqual(cur[1], prev[1], "scale=%s 时 y 单调性被破坏" % scale)
                prev = cur


class AmbiguousHalfRounding(unittest.TestCase):
    """0.5 边界：md 字面「四舍五入」= half up；若实现用 Python round() 则为银行家舍入。"""

    @classmethod
    def setUpClass(cls):
        cls.to_screen = A.image_to_screen()

    def test_half_boundary_lands_in_allowed_set(self):
        import math
        for name, x, y, origin, scale, _ in HALF_CASES:
            with self.subTest(case=name):
                half_up = (origin["x"] + math.floor(x / scale + 0.5),
                           origin["y"] + math.floor(y / scale + 0.5))
                banker = (origin["x"] + round(x / scale), origin["y"] + round(y / scale))
                allowed = {half_up, banker}
                res = self.to_screen(x, y, origin, scale)
                self.assertTrue(res.ok, res.describe())
                got = tuple(A.as_point(res.value))
                self.assertIn(got, allowed,
                              "%s: 实际 %s 既不是四舍五入 %s 也不是银行家舍入 %s" % (name, got, half_up, banker))

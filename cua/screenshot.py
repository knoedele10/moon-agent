"""截图、坐标换算、帧差分与弹窗启发式。

对应 docs/cua/03-action-protocol.md：
  - 第 1 节 Observation 里的 screen / image 字段；
  - 第 1.1 节 坐标空间（唯一换算依据是 image.scale 与 image.origin，禁止 clamp）；
  - 第 7 节 弹窗检测口径（画面差分 bbox + 填充率）。

本模块不做任何鼠标/键盘操作；只有 PyAutoGUIScreenshot.capture() 会调用真实截图 API，
MockScreenshot 完全离线合成图像，供无显示器自测使用。
"""

from __future__ import annotations

import base64
import hashlib
import io
import math
import time
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Optional, Sequence, Tuple

from .config import Config
from .errors import (
    E_INTERNAL,
    E_OUT_OF_BOUNDS,
    CuaError,
)

# ---------------------------------------------------------------------------
# 基础工具
# ---------------------------------------------------------------------------


def round_half_up(value: float) -> int:
    """四舍五入（.5 向上），避免 Python round() 的银行家舍入带来不可预测的坐标。

    协议第 1.1 节写的是 round(x / scale)，本函数就是它的确定性实现。
    """
    return int(math.floor(float(value) + 0.5))


def require_pyautogui():
    """惰性导入 pyautogui；不可用时抛 E_INTERNAL（便于 dry-run 在无 GUI 环境跑）。"""
    try:
        import pyautogui  # noqa: WPS433 - 故意惰性导入
        return pyautogui
    except Exception as exc:  # pragma: no cover - 依赖环境
        raise CuaError(
            E_INTERNAL,
            "pyautogui 不可用：%s" % exc,
            hint="请安装 pyautogui，或使用 --mock/--dry-run 离线模式",
            cause=exc,
        )


def system_dpi_scale() -> float:
    """Windows 系统缩放（125% -> 1.25）。仅用于日志，不参与坐标换算（协议第 1 节）。"""
    try:  # pragma: no cover - 平台相关
        import ctypes

        dpi = int(ctypes.windll.user32.GetDpiForSystem())
        if dpi > 0:
            return round(dpi / 96.0, 4)
    except Exception:
        pass
    return 1.0


# ---------------------------------------------------------------------------
# 帧
# ---------------------------------------------------------------------------


@dataclass
class CaptureFrame:
    """一帧截图 + 它到屏幕坐标的映射。

    scale  = image.width / 捕获区域宽（协议第 1.1 节）
    origin = 捕获区域左上角在屏幕坐标中的位置
    """

    image: Any                                   # PIL.Image.Image
    origin: Tuple[int, int] = (0, 0)
    scale: float = 1.0
    screen_size: Tuple[int, int] = (0, 0)        # 虚拟屏幕/主屏几何（宽, 高）
    media_type: str = "image/png"
    region_screen: Optional[Tuple[int, int, int, int]] = None  # 捕获区域（屏幕坐标）
    label: str = "full"
    captured_at: float = field(default_factory=time.time)
    _png: Optional[bytes] = None

    @property
    def width(self) -> int:
        return int(self.image.width)

    @property
    def height(self) -> int:
        return int(self.image.height)

    def png_bytes(self) -> bytes:
        if self._png is None:
            buf = io.BytesIO()
            self.image.save(buf, format="PNG")
            self._png = buf.getvalue()
        return self._png

    def sha256(self) -> str:
        return hashlib.sha256(self.png_bytes()).hexdigest()

    def data_url(self) -> str:
        return "data:%s;base64,%s" % (
            self.media_type,
            base64.b64encode(self.png_bytes()).decode("ascii"),
        )

    def image_block(self) -> Dict[str, Any]:
        """协议第 1 节 Observation.image 字段。"""
        return {
            "width": self.width,
            "height": self.height,
            "scale": round(float(self.scale), 6),
            "origin": {"x": int(self.origin[0]), "y": int(self.origin[1])},
            "media_type": self.media_type,
            "sha256": self.sha256(),
        }

    def jpeg_block(self, max_edge: int = 640, quality: int = 60) -> Dict[str, Any]:
        """集成契约第 1 节 I1：事件流用的缩略图块（长边 max_edge、JPEG q=quality）。

        契约第 1 节写的是「长边 640」，因此长边恒等于 max_edge：大于则缩小，
        小于则等比放大（局部截图帧可能小于 640，放大保证前端字段恒定、不出现小图）。
        返回字段与契约一致：media_type / width / height / data_uri（width/height 是缩图后的尺寸）。
        """
        from PIL import Image  # 惰性导入：与其它图像函数保持一致

        image = self.image
        longest = max(int(image.width), int(image.height))
        edge = int(max_edge)
        if edge > 0 and longest > 0 and longest != edge:
            ratio = float(edge) / float(longest)
            size = (
                max(1, int(round_half_up(image.width * ratio))),
                max(1, int(round_half_up(image.height * ratio))),
            )
            image = image.resize(size, Image.LANCZOS)
        buf = io.BytesIO()
        image.convert("RGB").save(buf, format="JPEG", quality=int(quality), optimize=True)
        return {
            "media_type": "image/jpeg",
            "width": int(image.width),
            "height": int(image.height),
            "data_uri": "data:image/jpeg;base64," + base64.b64encode(buf.getvalue()).decode("ascii"),
        }


# ---------------------------------------------------------------------------
# 坐标换算（第 1.1 节）
# ---------------------------------------------------------------------------


def image_to_screen(x: float, y: float, frame: CaptureFrame) -> Tuple[int, int]:
    """image 空间 -> 屏幕空间。调用方必须先做边界检查。"""
    sx = frame.origin[0] + round_half_up(float(x) / float(frame.scale))
    sy = frame.origin[1] + round_half_up(float(y) / float(frame.scale))
    return int(sx), int(sy)


def screen_to_image(sx: float, sy: float, frame: CaptureFrame) -> Tuple[int, int]:
    ix = round_half_up((float(sx) - frame.origin[0]) * float(frame.scale))
    iy = round_half_up((float(sy) - frame.origin[1]) * float(frame.scale))
    return int(ix), int(iy)


def check_image_bounds(x: float, y: float, frame: CaptureFrame, what: str = "坐标") -> None:
    """越界一律拒绝，绝不 clamp（协议第 1.1 节 / 第 8 节第 2 条）。"""
    w, h = frame.width, frame.height
    if not (0 <= x <= w - 1):
        raise CuaError(
            E_OUT_OF_BOUNDS,
            "%s x=%s 超出图像宽度 %s" % (what, _fmt_num(x), w),
            hint="坐标必须落在 image.width/image.height 内（允许区间 0..%d）" % (w - 1),
            context={"axis": "x", "value": x, "image_width": w, "image_height": h},
        )
    if not (0 <= y <= h - 1):
        raise CuaError(
            E_OUT_OF_BOUNDS,
            "%s y=%s 超出图像高度 %s" % (what, _fmt_num(y), h),
            hint="坐标必须落在 image.width/image.height 内（允许区间 0..%d）" % (h - 1),
            context={"axis": "y", "value": y, "image_width": w, "image_height": h},
        )


def to_screen_point(x: float, y: float, frame: CaptureFrame, what: str = "坐标") -> Tuple[int, int]:
    """边界检查 + 换算，一步到位（执行器唯一入口）。"""
    check_image_bounds(x, y, frame, what=what)
    return image_to_screen(x, y, frame)


def _fmt_num(value: float) -> str:
    if isinstance(value, float) and value.is_integer():
        return str(int(value))
    return str(value)


def image_rect_to_screen_rect(
    rect: Sequence[float], frame: CaptureFrame, what: str = "region", clip: bool = False
) -> Tuple[int, int, int, int]:
    """image 空间的 {x,y,width,height} -> 屏幕矩形（用于局部截图）。"""
    x, y, w, h = float(rect[0]), float(rect[1]), float(rect[2]), float(rect[3])
    if w < 1 or h < 1:
        raise CuaError(
            E_OUT_OF_BOUNDS,
            "%s 宽高必须为正：width=%s height=%s" % (what, _fmt_num(w), _fmt_num(h)),
            hint="局部截图的 width/height 最小为 schema 规定的 16",
            context={"rect": list(rect)},
        )
    if not clip:
        check_image_bounds(x, y, frame, what=what)
        check_image_bounds(x + w - 1, y + h - 1, frame, what=what + " 右下角")
        return (
            int(frame.origin[0] + round_half_up(x / frame.scale)),
            int(frame.origin[1] + round_half_up(y / frame.scale)),
            max(1, round_half_up(w / frame.scale)),
            max(1, round_half_up(h / frame.scale)),
        )
    # clip 模式：把区域裁到屏幕内，绝不放大到屏幕外
    sx = frame.origin[0] + round_half_up(x / frame.scale)
    sy = frame.origin[1] + round_half_up(y / frame.scale)
    sw = max(1, round_half_up(w / frame.scale))
    sh = max(1, round_half_up(h / frame.scale))
    scr_w, scr_h = frame.screen_size if frame.screen_size[0] else (frame.width, frame.height)
    sx0, sy0 = max(0, sx), max(0, sy)
    sx1, sy1 = min(scr_w, sx + sw), min(scr_h, sy + sh)
    if sx1 <= sx0 or sy1 <= sy0:
        raise CuaError(
            E_OUT_OF_BOUNDS,
            "局部截图区域完全落在屏幕之外：%s" % (list(rect),),
            hint="请给出落在当前 image 内的区域",
        )
    return int(sx0), int(sy0), int(sx1 - sx0), int(sy1 - sy0)


# ---------------------------------------------------------------------------
# 截图提供者
# ---------------------------------------------------------------------------


def clamp_region_to_frame(region: Dict[str, float], frame: CaptureFrame, min_side: int = 16) -> Optional[Dict[str, int]]:
    """把 image 空间的矩形裁进当前帧；完全越界返回 None。"""
    x = max(0, int(round_half_up(float(region.get("x", 0)))))
    y = max(0, int(round_half_up(float(region.get("y", 0)))))
    w = int(round_half_up(float(region.get("width", 0))))
    h = int(round_half_up(float(region.get("height", 0))))
    if x >= frame.width or y >= frame.height or x + w <= 0 or y + h <= 0:
        return None
    x = min(x, max(0, frame.width - min_side))
    y = min(y, max(0, frame.height - min_side))
    w = max(min_side, min(w, frame.width - x))
    h = max(min_side, min(h, frame.height - y))
    if w < min_side or h < min_side or x >= frame.width or y >= frame.height:
        return None
    return {"x": x, "y": y, "width": w, "height": h}


class ScreenshotProvider:
    """截图接口。capture(region=...) 中的 region 是 image 空间坐标（协议第 3.8 节）。"""

    name = "base"

    def capture(
        self,
        region: Optional[Dict[str, float]] = None,
        base_frame: Optional[CaptureFrame] = None,
    ) -> CaptureFrame:  # pragma: no cover - 接口
        raise NotImplementedError

    def close(self) -> None:
        return None


class PyAutoGUIScreenshot(ScreenshotProvider):
    """真实截图（全屏 / 局部放大）。已知限制：pyautogui 在 Windows 上只报主屏几何，
    多显示器虚拟桌面合并坐标系未实测（设计如此）。"""

    name = "pyautogui"

    def __init__(self, cfg: Optional[Config] = None) -> None:
        self.cfg = cfg or Config()
        self._pg = None

    @property
    def pg(self):
        if self._pg is None:
            self._pg = require_pyautogui()
        return self._pg

    def _screen_size(self) -> Tuple[int, int]:
        size = self.pg.size()
        return int(size.width), int(size.height)

    def capture(
        self,
        region: Optional[Dict[str, float]] = None,
        base_frame: Optional[CaptureFrame] = None,
    ) -> CaptureFrame:
        from PIL import Image

        screen_w, screen_h = self._screen_size()
        if region is None:
            img = self.pg.screenshot()
            origin = (0, 0)
            region_screen = (0, 0, screen_w, screen_h)
            label = "full"
        else:
            if base_frame is None:
                raise CuaError(
                    E_INTERNAL,
                    "局部截图需要当前帧做坐标换算",
                    hint="调用方必须传入 base_frame（协议第 3.8 节：region 是 image 空间坐标）",
                )
            rect = image_rect_to_screen_rect(
                (region["x"], region["y"], region["width"], region["height"]),
                base_frame,
                what="screenshot.region",
            )
            # 真实屏幕边界裁剪后再抓图
            rect = _clip_rect_to_screen(rect, base_frame)
            img = self.pg.screenshot(region=rect)
            zoom = float(self.cfg.screenshot_zoom)
            if zoom and abs(zoom - 1.0) > 1e-6:
                img = img.resize(
                    (max(1, round_half_up(img.width * zoom)), max(1, round_half_up(img.height * zoom))),
                    Image.LANCZOS,
                )
            origin = (rect[0], rect[1])
            region_screen = rect
            label = "region"
        return _finish_frame(img, origin, region_screen, screen_w, screen_h, self.cfg, label)


def _clip_rect_to_screen(rect: Tuple[int, int, int, int], frame: CaptureFrame) -> Tuple[int, int, int, int]:
    scr_w, scr_h = frame.screen_size if frame.screen_size[0] else (frame.width, frame.height)
    x, y, w, h = rect
    x0, y0 = max(0, x), max(0, y)
    x1, y1 = min(scr_w, x + w), min(scr_h, y + h)
    if x1 <= x0 or y1 <= y0:
        raise CuaError(E_OUT_OF_BOUNDS, "局部截图区域完全落在屏幕之外：%s" % (list(rect),))
    return int(x0), int(y0), int(x1 - x0), int(y1 - y0)


def _finish_frame(
    img: Any,
    origin: Tuple[int, int],
    region_screen: Tuple[int, int, int, int],
    screen_w: int,
    screen_h: int,
    cfg: Config,
    label: str,
) -> CaptureFrame:
    """按协议第 1.1 节计算 scale = image.width / 捕获区域宽，并按需等比缩放送模型。"""
    from PIL import Image

    region_w = max(1, int(region_screen[2]))
    if img.width > cfg.image_max_width:
        ratio = cfg.image_max_width / float(img.width)
        img = img.resize(
            (int(cfg.image_max_width), max(1, round_half_up(img.height * ratio))),
            Image.LANCZOS,
        )
    scale = img.width / float(region_w)
    return CaptureFrame(
        image=img,
        origin=(int(origin[0]), int(origin[1])),
        scale=float(scale),
        screen_size=(int(screen_w), int(screen_h)),
        media_type="image/png",
        region_screen=tuple(int(v) for v in region_screen),
        label=label,
    )


# ---------------------------------------------------------------------------
# 离线合成屏幕（无显示器自测用）
# ---------------------------------------------------------------------------


def paint_base(width: int, height: int, shift: int = 0, tone: int = 0) -> Any:
    """确定性背景：竖条渐变 + 一个随 shift 移动的方块（用于产生可控的画面变化）。"""
    from PIL import Image, ImageDraw

    img = Image.new("RGB", (width, height), (24 + tone, 28 + tone, 34 + tone))
    draw = ImageDraw.Draw(img)
    for x in range(0, width, 16):
        shade = 30 + (x * 7 // max(1, width)) + tone
        draw.rectangle([x, 0, x + 7, height], fill=(shade, shade + 4, shade + 10))
    bx = (shift * 37) % max(1, width - 120)
    by = (shift * 23) % max(1, height - 80)
    draw.rectangle([bx, by, bx + 110, by + 70], fill=(200, 90, 60))
    draw.rectangle([8, 8, 240, 40], fill=(240, 240, 240))
    draw.text((16, 16), "CUA offline frame #%d" % shift, fill=(10, 10, 10))
    return img


def paint_plain(width: int, height: int, tone: int = 0) -> Any:
    """无强对比内容的底图：用于构造「背景被压暗」这类弱变化。"""
    from PIL import Image, ImageDraw

    img = Image.new("RGB", (width, height), (40 + tone, 46 + tone, 54 + tone))
    draw = ImageDraw.Draw(img)
    draw.rectangle([0, 0, width, 36], fill=(110, 110, 110))
    draw.rectangle([0, 0, width, 3], fill=(90, 90, 150))
    return img


def paint_popup(width: int, height: int, shift: int = 0) -> Any:
    """模态框样张：背景整体压暗（弱变化）+ 中央高对比块（强变化，第 7 节附加信号）。"""
    from PIL import Image, ImageDraw

    img = paint_plain(width, height).point(lambda v: int(v * 0.70))
    draw = ImageDraw.Draw(img)
    w = int(width * 0.42)
    h = int(height * 0.30)
    x0 = (width - w) // 2
    y0 = (height - h) // 2
    draw.rectangle([x0, y0, x0 + w, y0 + h], fill=(250, 250, 250))
    draw.rectangle([x0, y0, x0 + w, y0 + 34], fill=(40, 90, 200))
    draw.text((x0 + 12, y0 + 12), "Confirm", fill=(255, 255, 255))
    draw.rectangle([x0 + 20, y0 + h - 46, x0 + 120, y0 + h - 14], fill=(210, 210, 210))
    draw.rectangle([x0 + w - 140, y0 + h - 46, x0 + w - 20, y0 + h - 14], fill=(60, 130, 240))
    # 会动的进度块：让连续两帧之间仍能稳定识别出「模态框还在」
    mx = x0 + 24 + (shift * 17) % max(1, w - 120)
    draw.rectangle([mx, y0 + 48, mx + 90, y0 + 64], fill=(60, 130, 240))
    return img


class MockScreenshot(ScreenshotProvider):
    """离线合成屏幕。scenario 决定帧序列：

    - moving（默认）：每帧方块位移，画面持续变化；
    - frozen：所有帧完全相同（用于 stall 检测）；
    - popup：第 2 帧起弹出模态框；
    - alt：偶数帧 moving、奇数帧 frozen（用于混合场景）。
    """

    name = "mock"

    def __init__(
        self,
        cfg: Optional[Config] = None,
        scenario: str = "moving",
        screen_size: Tuple[int, int] = (1920, 1080),
        painter: Optional[Callable[[int, int, int], Any]] = None,
    ) -> None:
        self.cfg = cfg or Config()
        self.scenario = scenario
        self._screen_size = (int(screen_size[0]), int(screen_size[1]))
        self._painter = painter
        self.capture_count = 0

    def capture(
        self,
        region: Optional[Dict[str, float]] = None,
        base_frame: Optional[CaptureFrame] = None,
    ) -> CaptureFrame:
        from PIL import Image

        n = self.capture_count
        self.capture_count += 1
        screen_w, screen_h = self._screen_size
        if region is None:
            img = self._paint(n, screen_w, screen_h)
            origin = (0, 0)
            region_screen = (0, 0, screen_w, screen_h)
            label = "full"
        else:
            if base_frame is None:
                raise CuaError(
                    E_INTERNAL,
                    "局部截图需要当前帧做坐标换算",
                    hint="调用方必须传入 base_frame",
                )
            rect = image_rect_to_screen_rect(
                (region["x"], region["y"], region["width"], region["height"]),
                base_frame,
                what="screenshot.region",
            )
            rect = _clip_rect_to_screen(rect, base_frame)
            ix0, iy0 = screen_to_image(rect[0], rect[1], base_frame)
            ix1, iy1 = screen_to_image(rect[0] + rect[2], rect[1] + rect[3], base_frame)
            ix0 = max(0, min(ix0, base_frame.width))
            iy0 = max(0, min(iy0, base_frame.height))
            ix1 = max(ix0, min(ix1, base_frame.width))
            iy1 = max(iy0, min(iy1, base_frame.height))
            crop = base_frame.image.crop((ix0, iy0, ix1, iy1))
            zoom = float(self.cfg.screenshot_zoom)
            img = crop.resize(
                (max(1, round_half_up(crop.width * zoom)), max(1, round_half_up(crop.height * zoom))),
                Image.LANCZOS,
            )
            origin = (rect[0], rect[1])
            region_screen = rect
            label = "region"
        return _finish_frame(img, origin, region_screen, screen_w, screen_h, self.cfg, label)

    def _paint(self, n: int, width: int, height: int) -> Any:
        if self._painter is not None:
            return self._painter(n, width, height)
        if self.scenario == "frozen":
            return paint_base(width, height, shift=0)
        if self.scenario == "popup":
            return paint_plain(width, height) if n < 1 else paint_popup(width, height, shift=n)
        if self.scenario == "popup_flip":
            return paint_popup(width, height, shift=n) if n % 2 == 1 else paint_plain(width, height)
        if self.scenario == "alt":
            return paint_base(width, height, shift=n if n % 2 == 0 else 0)
        return paint_base(width, height, shift=n)


# ---------------------------------------------------------------------------
# 帧差分 / 感知哈希 / 弹窗启发式（第 4、7 节）
# ---------------------------------------------------------------------------


def perceptual_hash(image: Any) -> int:
    """dHash：9x8 灰度差分 -> 64bit。用于「这一下到底有没有生效」的客观判断。"""
    gray = image.convert("L").resize((9, 8))
    px = list(gray.getdata())
    bits = 0
    idx = 0
    for row in range(8):
        for col in range(8):
            left = px[row * 9 + col]
            right = px[row * 9 + col + 1]
            if left > right:
                bits |= 1 << idx
            idx += 1
    return bits


def hamming(a: int, b: int) -> int:
    return bin(int(a) ^ int(b)).count("1")


@dataclass
class FrameDiff:
    screen_changed: bool
    change_ratio: float
    bbox: Optional[Tuple[int, int, int, int]]   # (x0, y0, x1, y1) 含端点
    bbox_area_ratio: float
    fill_ratio: float
    popup_suspected: bool
    signals: List[str] = field(default_factory=list)
    hash_before: int = 0
    hash_after: int = 0
    threshold: int = 8
    strong_threshold: int = 45
    salient_ratio: float = 0.0   # 强变化像素占比（用于弹窗形状判定）

    def observed(self) -> Dict[str, Any]:
        return {
            "screen_changed": bool(self.screen_changed),
            "change_ratio": round(float(self.change_ratio), 6),
        }

    def to_dict(self) -> Dict[str, Any]:
        return {
            "screen_changed": bool(self.screen_changed),
            "change_ratio": round(float(self.change_ratio), 6),
            "bbox": list(self.bbox) if self.bbox else None,
            "bbox_area_ratio": round(float(self.bbox_area_ratio), 6),
            "fill_ratio": round(float(self.fill_ratio), 6),
            "popup_suspected": bool(self.popup_suspected),
            "salient_ratio": round(float(self.salient_ratio), 6),
            "signals": list(self.signals),
            "hash_before": self.hash_before,
            "hash_after": self.hash_after,
        }

    def crop_hint(self, pad: int = 12) -> Optional[Dict[str, int]]:
        """第 7 节处置优先级 1：把疑似弹窗区域单独裁剪放大。"""
        if not self.bbox:
            return None
        x0, y0, x1, y1 = self.bbox
        return {
            "x": max(0, x0 - pad),
            "y": max(0, y0 - pad),
            "width": max(16, (x1 - x0 + 1) + 2 * pad),
            "height": max(16, (y1 - y0 + 1) + 2 * pad),
        }


def diff_frames(
    before: Optional[Any],
    after: Any,
    cfg: Optional[Config] = None,
    pixel_threshold: int = 8,
    strong_threshold: int = 45,
) -> FrameDiff:
    """两帧差分 + 第 7 节弹窗启发式。before 为 None 时视为首帧（无变化）。

    采用两级阈值（设计如此）：弱变化（>8）算 change_ratio；强变化（>45）用于判定
    「模态级突变」的形状——否则整屏被压暗会把 bbox 撑满全屏，导致弹窗规则永不命中。
    """
    cfg = cfg or Config()
    if before is None:
        return FrameDiff(False, 0.0, None, 0.0, 0.0, False, ["first_frame"],
                         perceptual_hash(after), perceptual_hash(after), pixel_threshold)
    import numpy as np

    b = before.convert("RGB")
    a = after.convert("RGB")
    if a.size != b.size:
        a = a.resize(b.size)
    arr_b = np.asarray(b, dtype=np.int16)
    arr_a = np.asarray(a, dtype=np.int16)
    delta = np.abs(arr_a - arr_b).max(axis=2)
    weak = delta > int(pixel_threshold)       # 弱变化：change_ratio / screen_changed（协议第 4 节口径）
    strong = delta > int(strong_threshold)    # 强变化：弹窗形状判定（设计如此，见 README）
    changed = int(weak.sum())
    total = int(weak.size) or 1
    ratio = changed / float(total)
    hb, ha = perceptual_hash(b), perceptual_hash(a)
    screen_changed = bool(hamming(hb, ha) > 0 or ratio > 0.001)

    bbox: Optional[Tuple[int, int, int, int]] = None
    bbox_area_ratio = 0.0
    fill_ratio = 0.0
    signals: List[str] = []
    popup = False
    salient_pixels = int(strong.sum())
    salient_ratio = salient_pixels / float(total)
    shape_mask = strong if salient_pixels else weak   # 无强变化时退化为弱变化，保证启发式仍可用
    if int(shape_mask.sum()):
        rows = np.flatnonzero(shape_mask.any(axis=1))
        cols = np.flatnonzero(shape_mask.any(axis=0))
        y0, y1 = int(rows[0]), int(rows[-1])
        x0, x1 = int(cols[0]), int(cols[-1])
        bbox = (x0, y0, x1, y1)
        bw, bh = (x1 - x0 + 1), (y1 - y0 + 1)
        bbox_area_ratio = (bw * bh) / float(total)
        fill_ratio = int(shape_mask.sum()) / float(max(1, bw * bh))
        if cfg.popup_enabled:
            fills_screen = (bw >= b.size[0] - 2) and (bh >= b.size[1] - 2)
            if (
                cfg.popup_area_min_ratio <= bbox_area_ratio <= cfg.popup_area_max_ratio
                and fill_ratio > cfg.popup_fill_min_ratio
                and not fills_screen
            ):
                popup = True
                signals.append("bbox_rule")
            # 附加信号（不单独触发 popup_suspected）
            cx, cy = b.size[0] / 2.0, b.size[1] / 2.0
            if x0 <= cx <= x1 and y0 <= cy <= y1:
                signals.append("center_block")
            lum_b = arr_b.mean(axis=2)
            lum_a = arr_a.mean(axis=2)
            outside = np.ones(shape_mask.shape, dtype=bool)
            outside[y0:y1 + 1, x0:x1 + 1] = False
            subtle = outside & weak
            if bool(subtle.any()):
                d_out = float((lum_b[subtle] - lum_a[subtle]).mean())
                if d_out > 10.0:
                    signals.append("background_darkened")
                    if popup:
                        signals.append("modal_like")
    return FrameDiff(screen_changed, ratio, bbox, bbox_area_ratio, fill_ratio, popup, signals,
                     hb, ha, pixel_threshold, strong_threshold, salient_ratio)

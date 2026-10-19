"""键鼠执行器：把协议第 3 节的 9 种动作落到 pyautogui。

安全不变量（协议第 8 节，无条件成立）：
  1. pyautogui.FAILSAFE 永久开启，且不作为可配置项暴露；
  2. 坐标越界一律拒绝（E_OUT_OF_BOUNDS），绝不 clamp / 取模 / 翻转；
  3. 每步之间强制 sleep 至少 150ms；
  4. 任何终止路径都释放所有仍按下的修饰键；
  5. 危险组合键默认关闭（allow_dangerous_hotkeys 才放行）；
  6. risk=high 只「标记」，由调用方（loop）决定是否放行。

dry_run=True 时完全不触碰 pyautogui：所有输入落到 DryRunDevice 记录，
但坐标越界、键名白名单、参数区间等语义检查照样执行。
"""

from __future__ import annotations

import json
import time
from contextlib import contextmanager
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Optional, Sequence, Tuple

from .action_parser import ParsedStep
from .config import (
    DANGEROUS_HOTKEY_PATTERNS,
    KEY_WHITELIST,
    MODIFIER_KEYS,
    PYAUTOGUI_KEY_MAP,
    Config,
    destructive_hits,
)
from .errors import (
    E_ARG_RANGE,
    E_EXECUTOR_FAILSAFE,
    E_INTERNAL,
    E_KEY_NOT_ALLOWED,
    E_NO_FOCUS,
    E_SCHEMA,
    E_UNKNOWN_ACTION,
    CuaError,
)
from .screenshot import (
    CaptureFrame,
    FrameDiff,
    ScreenshotProvider,
    diff_frames,
    image_rect_to_screen_rect,
    to_screen_point,
)

MODIFIERS = ("ctrl", "alt", "shift", "win")


def _pyautogui_key(key: str) -> str:
    k = key.lower()
    return PYAUTOGUI_KEY_MAP.get(k, k)


# ---------------------------------------------------------------------------
# 输入设备
# ---------------------------------------------------------------------------


class InputDevice:
    """键鼠设备接口。真实实现 = PyAutoGUIDevice；自测 = RecordingDevice/DryRunDevice。"""

    name = "base"

    def move_to(self, x: int, y: int, duration: float = 0.0) -> None:  # pragma: no cover
        raise NotImplementedError

    def click(self, x: int, y: int, button: str = "left", clicks: int = 1) -> None:  # pragma: no cover
        raise NotImplementedError

    def mouse_down(self, button: str = "left") -> None:  # pragma: no cover
        raise NotImplementedError

    def mouse_up(self, button: str = "left") -> None:  # pragma: no cover
        raise NotImplementedError

    def scroll(self, amount: int) -> None:  # pragma: no cover
        raise NotImplementedError

    def hscroll(self, amount: int) -> None:  # pragma: no cover
        raise NotImplementedError

    def key_down(self, key: str) -> None:  # pragma: no cover
        raise NotImplementedError

    def key_up(self, key: str) -> None:  # pragma: no cover
        raise NotImplementedError

    def press(self, key: str) -> None:  # pragma: no cover
        raise NotImplementedError

    def write(self, text: str, interval: float = 0.0) -> None:  # pragma: no cover
        raise NotImplementedError

    def hotkey(self, *keys: str) -> None:  # pragma: no cover
        raise NotImplementedError

    def position(self) -> Tuple[int, int]:
        return (0, 0)

    def size(self) -> Tuple[int, int]:
        return (0, 0)

    def set_clipboard(self, text: str) -> Optional[str]:
        """写入剪贴板，返回原内容（用于恢复）。不支持时返回 None。"""
        return None

    def get_clipboard(self) -> Optional[str]:
        return None


class RecordingDevice(InputDevice):
    """只记录、不执行（离线自测用）。"""

    name = "recording"

    def __init__(self, size: Tuple[int, int] = (1920, 1080)) -> None:
        self.ops: List[Dict[str, Any]] = []
        self._pos: Tuple[int, int] = (0, 0)
        self._size = size
        self.clipboard: Optional[str] = None
        self.fail_next = False
        self.fail_on: Optional[str] = None

    def _record(self, op: str, **kwargs: Any) -> None:
        if self.fail_next or (self.fail_on is not None and self.fail_on == op):
            self.fail_next = False
            raise CuaError(E_EXECUTOR_FAILSAFE, "模拟 FailSafe：鼠标被甩到左上角",
                           hint="立即停手并释放所有按键")
        self.ops.append(dict(op=op, **kwargs))

    def move_to(self, x: int, y: int, duration: float = 0.0) -> None:
        self._record("move_to", x=x, y=y, duration=duration)
        self._pos = (int(x), int(y))

    def click(self, x: int, y: int, button: str = "left", clicks: int = 1) -> None:
        self._record("click", x=x, y=y, button=button, clicks=clicks)
        self._pos = (int(x), int(y))

    def mouse_down(self, button: str = "left") -> None:
        self._record("mouse_down", button=button)

    def mouse_up(self, button: str = "left") -> None:
        self._record("mouse_up", button=button)

    def scroll(self, amount: int) -> None:
        self._record("scroll", amount=amount)

    def hscroll(self, amount: int) -> None:
        self._record("hscroll", amount=amount)

    def key_down(self, key: str) -> None:
        self._record("key_down", key=key)

    def key_up(self, key: str) -> None:
        self._record("key_up", key=key)

    def press(self, key: str) -> None:
        self._record("press", key=key)

    def write(self, text: str, interval: float = 0.0) -> None:
        self._record("write", text=text, interval=interval)

    def hotkey(self, *keys: str) -> None:
        self._record("hotkey", keys=list(keys))

    def position(self) -> Tuple[int, int]:
        return self._pos

    def size(self) -> Tuple[int, int]:
        return self._size

    def set_clipboard(self, text: str) -> Optional[str]:
        old = self.clipboard
        self.clipboard = text
        self._record("set_clipboard", text=text)
        return old

    def get_clipboard(self) -> Optional[str]:
        return self.clipboard

    # 断言辅助 ---------------------------------------------------------
    def ops_of(self, name: str) -> List[Dict[str, Any]]:
        return [o for o in self.ops if o["op"] == name]

    def clear(self) -> None:
        self.ops.clear()


class DryRunDevice(RecordingDevice):
    """--dry-run 专用：只打印将执行的动作，绝不触发真实输入。"""

    name = "dry_run"

    def __init__(self, size: Tuple[int, int] = (1920, 1080), echo: bool = False) -> None:
        super().__init__(size=size)
        self.echo = echo

    def _record(self, op: str, **kwargs: Any) -> None:
        super()._record(op, **kwargs)
        if self.echo:
            payload = json.dumps(kwargs, ensure_ascii=False, sort_keys=True)
            print("[dry-run] %s %s" % (op, payload), flush=True)


class PyAutoGUIDevice(InputDevice):
    """真实输入设备。FAILSAFE 永久开启（协议第 8 节第 1 条）。"""

    name = "pyautogui"

    def __init__(self) -> None:
        import pyautogui  # 惰性导入

        pyautogui.FAILSAFE = True      # 永久开启，不暴露配置
        pyautogui.PAUSE = 0.0          # 节奏由执行器统一控制
        self._pg = pyautogui
        self._clipboard_supported = True

    # FailSafeException -> E_EXECUTOR_FAILSAFE
    def _guard(self, fn: Callable[..., Any], *args: Any, **kwargs: Any) -> Any:
        try:
            return fn(*args, **kwargs)
        except Exception as exc:
            if type(exc).__name__ == "FailSafeException":
                raise CuaError(
                    E_EXECUTOR_FAILSAFE,
                    "鼠标被甩到屏幕左上角，触发 pyautogui FAILSAFE",
                    hint="已停手并释放所有按键，请人工接管",
                    cause=exc,
                )
            raise

    def move_to(self, x: int, y: int, duration: float = 0.0) -> None:
        self._guard(self._pg.moveTo, int(x), int(y), duration=float(duration))

    def click(self, x: int, y: int, button: str = "left", clicks: int = 1) -> None:
        self._guard(self._pg.click, int(x), int(y), clicks=int(clicks), button=button)

    def mouse_down(self, button: str = "left") -> None:
        self._guard(self._pg.mouseDown, button=button)

    def mouse_up(self, button: str = "left") -> None:
        self._guard(self._pg.mouseUp, button=button)

    def scroll(self, amount: int) -> None:
        self._guard(self._pg.scroll, int(amount))

    def hscroll(self, amount: int) -> None:
        self._guard(self._pg.hscroll, int(amount))

    def key_down(self, key: str) -> None:
        self._guard(self._pg.keyDown, key)

    def key_up(self, key: str) -> None:
        self._guard(self._pg.keyUp, key)

    def press(self, key: str) -> None:
        self._guard(self._pg.press, key)

    def write(self, text: str, interval: float = 0.0) -> None:
        self._guard(self._pg.write, text, interval=float(interval))

    def hotkey(self, *keys: str) -> None:
        self._guard(self._pg.hotkey, *keys)

    def position(self) -> Tuple[int, int]:
        p = self._guard(self._pg.position)
        return int(p.x), int(p.y)

    def size(self) -> Tuple[int, int]:
        s = self._guard(self._pg.size)
        return int(s.width), int(s.height)

    def set_clipboard(self, text: str) -> Optional[str]:
        """写入剪贴板并返回原内容；写后读不回（剪贴板不可用）返回 None 让调用方退化。"""
        try:
            import pyperclip

            try:
                old = pyperclip.paste()
            except Exception:
                old = None
            pyperclip.copy(text)
            if pyperclip.paste() != text:
                return None
            return old
        except Exception:
            return None

    def get_clipboard(self) -> Optional[str]:
        try:
            import pyperclip

            return pyperclip.paste()
        except Exception:
            return None


# ---------------------------------------------------------------------------
# 执行结果
# ---------------------------------------------------------------------------


@dataclass
class ExecutionOutcome:
    result: Dict[str, Any]
    frame: Optional[CaptureFrame] = None       # 执行后的当前帧（供下一轮 Observation）
    diff: Optional[FrameDiff] = None

    @property
    def ok(self) -> bool:
        return bool(self.result.get("ok"))

    def to_dict(self) -> Dict[str, Any]:
        return dict(self.result)


class Executor:
    """协议第 3 节 9 种动作的执行器。"""

    def __init__(
        self,
        cfg: Optional[Config] = None,
        provider: Optional[ScreenshotProvider] = None,
        device: Optional[InputDevice] = None,
        dry_run: Optional[bool] = None,
        sleep_fn: Optional[Callable[[float], None]] = None,
        echo: bool = False,
    ) -> None:
        self.cfg = cfg or Config()
        self.provider = provider
        self.dry_run = self.cfg.dry_run if dry_run is None else bool(dry_run)
        if device is not None:
            self.device = device
        elif self.dry_run:
            self.device = DryRunDevice(echo=echo)
        else:
            self.device = PyAutoGUIDevice()
        self._sleep = sleep_fn or time.sleep
        self._held: List[str] = []

    # -- 修饰键安全 ------------------------------------------------------
    @contextmanager
    def _modifiers(self, mods: Sequence[str]):
        pressed: List[str] = []
        try:
            for m in mods:
                self.device.key_down(_pyautogui_key(m))
                self._held.append(m)
                pressed.append(m)
            yield
        finally:
            for m in reversed(pressed):
                try:
                    self.device.key_up(_pyautogui_key(m))
                finally:
                    if m in self._held:
                        self._held.remove(m)

    def release_all_modifiers(self) -> List[str]:
        """第 8 节第 7 条：任何终止路径都要补 keyUp。返回被释放的键。"""
        released: List[str] = []
        for m in reversed(list(self._held)):
            try:
                self.device.key_up(_pyautogui_key(m))
                released.append(m)
            except Exception:
                # 释放失败也不能吞掉原错误；继续释放剩下的键
                released.append(m)
        self._held.clear()
        return released

    # -- 主入口 ----------------------------------------------------------
    def execute(self, step: Any, frame: CaptureFrame, raise_on_error: bool = False) -> ExecutionOutcome:
        action = step.action if isinstance(step, ParsedStep) else (step or {}).get("action")
        raw_step = step.step if isinstance(step, ParsedStep) else (step or {})
        if not isinstance(action, dict):
            raise CuaError(E_SCHEMA, "无可执行动作（action 为 null 或缺失）",
                           hint="action 恒为必填的动作对象（契约 2026-10-11 裁定），收尾请用 type=done")

        started_at = time.time()
        result: Dict[str, Any] = {
            "ok": False,
            "step": raw_step.get("step"),
            "action": action,
            "started_at": started_at,
            "duration_ms": 0,
            "error": None,
            "observed": {"screen_changed": False, "change_ratio": 0.0, "cursor": None},
            "meta": {
                "screen_point": None,
                "coord_rounded": bool(getattr(step, "meta", {}).get("coord_rounded", False)),
                "guardrail_required": str(raw_step.get("risk") or "low") == "high",
                "risk": raw_step.get("risk") or "low",
                "dry_run": self.dry_run,
                "min_sleep_s": self.cfg.min_step_sleep_s,
            },
        }
        new_frame: Optional[CaptureFrame] = None
        error: Optional[CuaError] = None
        try:
            new_frame = self._dispatch(action, frame, result)
            result["ok"] = True
        except CuaError as exc:
            error = exc
            result["error"] = exc.to_dict()
            result["meta"]["error_context"] = exc.context
        except Exception as exc:  # 未预期异常同样走「先释放按键」的路径
            error = CuaError(E_INTERNAL, "%s: %s" % (type(exc).__name__, exc),
                             hint="未预期异常，已记录完整堆栈", cause=exc)
            result["error"] = error.to_dict()
        finally:
            # 任何终止路径：先释放所有按键，再保证步间间隔 >= 150ms（第 8 节第 3、7 条）
            released = self.release_all_modifiers()
            if released:
                result["meta"]["released_modifiers"] = released
            self._sleep(max(0.0, float(self.cfg.min_step_sleep_s)))
            result["duration_ms"] = int((time.time() - started_at) * 1000)

        if error is not None:
            if raise_on_error:
                raise error
            return ExecutionOutcome(result=result, frame=None)

        # 执行后截一帧，算 observed（协议第 4 节）
        if self.provider is not None and (new_frame is None or action.get("type") != "screenshot"):
            try:
                after = self.provider.capture()
            except CuaError:
                after = None
            if after is not None:
                diff = diff_frames(frame.image, after.image, self.cfg)
                result["observed"] = diff.observed()
                result["meta"]["frame_diff"] = diff.to_dict()
                try:
                    cursor = self.device.position()
                    result["observed"]["cursor"] = {"x": int(cursor[0]), "y": int(cursor[1])}
                except Exception:
                    result["observed"]["cursor"] = None
                result["meta"]["screen_after"] = {
                    "origin": {"x": after.origin[0], "y": after.origin[1]},
                    "scale": round(float(after.scale), 6),
                    "width": after.width,
                    "height": after.height,
                }
                new_frame = after
                return ExecutionOutcome(result=result, frame=new_frame, diff=diff)
        return ExecutionOutcome(result=result, frame=new_frame)

    # -- 分发 ------------------------------------------------------------
    def _dispatch(self, action: Dict[str, Any], frame: CaptureFrame, result: Dict[str, Any]) -> Optional[CaptureFrame]:
        atype = action.get("type")
        handler = {
            "click": self._do_click,
            "type": self._do_type,
            "hotkey": self._do_hotkey,
            "drag": self._do_drag,
            "scroll": self._do_scroll,
            "move": self._do_move,
            "wait": self._do_wait,
            "screenshot": self._do_screenshot,
            "done": self._do_done,
        }.get(atype)
        if handler is None:
            raise CuaError(
                E_UNKNOWN_ACTION,
                "未注册的动作类型：%r" % (atype,),
                hint="支持的动作类型：click/type/hotkey/drag/scroll/move/wait/screenshot/done",
            )
        return handler(action, frame, result)

    # -- 9 种动作 --------------------------------------------------------
    def _check_modifiers(self, mods: Any) -> List[str]:
        if mods is None:
            return []
        if not isinstance(mods, list):
            raise CuaError(E_ARG_RANGE, "modifiers 必须是数组", hint="取值是 ctrl/alt/shift/win 的子集，最多 3 个")
        for m in mods:
            if m not in MODIFIERS:
                raise CuaError(E_KEY_NOT_ALLOWED, "modifiers 含未允许的键：%r" % (m,),
                               hint="允许的修饰键：%s" % ", ".join(MODIFIERS))
        if len(mods) > 3:
            raise CuaError(E_ARG_RANGE, "modifiers 最多 3 个，实际 %d" % len(mods),
                           hint="最多同时按 3 个修饰键")
        return list(mods)

    def _do_click(self, action: Dict[str, Any], frame: CaptureFrame, result: Dict[str, Any]) -> None:
        point = to_screen_point(action["x"], action["y"], frame, what="click 坐标")
        button = action.get("button") or "left"
        clicks = int(action.get("clicks") or 1)
        mods = self._check_modifiers(action.get("modifiers"))
        result["meta"]["screen_point"] = {"x": point[0], "y": point[1]}
        with self._modifiers(mods):
            self.device.click(point[0], point[1], button=button, clicks=clicks)
        return None

    def _do_type(self, action: Dict[str, Any], frame: CaptureFrame, result: Dict[str, Any]) -> None:
        text = action.get("text")
        if not isinstance(text, str):
            raise CuaError(E_ARG_RANGE, "type.text 必须是字符串", hint="请给出要输入的文本")
        target = action.get("target", "focused")
        interval = float(action.get("interval_ms") or 0) / 1000.0
        if isinstance(target, dict):
            if target.get("editable") is False:
                raise CuaError(E_NO_FOCUS, "type.target 明确标记为不可编辑",
                               hint="先点 target 再输入")
            if "x" in target and "y" in target:
                point = to_screen_point(target["x"], target["y"], frame, what="type.target 坐标")
                result["meta"]["screen_point"] = {"x": point[0], "y": point[1]}
                self.device.click(point[0], point[1], button="left", clicks=1)
            else:
                raise CuaError(E_ARG_RANGE, "type.target 对象必须包含 x/y",
                               hint="target 只支持 \"focused\" 或 {x, y}")
        elif target not in (None, "", "focused"):
            raise CuaError(E_ARG_RANGE, "type.target 不支持字符串 %r" % (target,),
                           hint="target 只支持 \"focused\" 或 {x, y}")

        if action.get("clear_first"):
            with self._modifiers(["ctrl"]):
                self.device.press("a")
        self._type_text(text, interval, result)
        if action.get("submit"):
            self.device.press("enter")
        return None

    def _type_text(self, text: str, interval: float, result: Dict[str, Any]) -> None:
        """非空文本一律优先走剪贴板粘贴；剪贴板不可用才退化（task-25，2026-10-11）。

        实测：pyautogui.write 是逐字符 press，真实 Windows 上会丢键——在记事本里输入
        "hello from CUA" 落成 "hellofromCUA"（两个空格丢失；窗口标题 "*hellofromCUA - Notepad"）。
        粘贴是原子操作，不丢字符且更快，所以 ASCII 也走剪贴板。
        """
        if not text:
            result["meta"]["type_path"] = "empty"
            return
        if self.cfg.type_use_clipboard_for_unicode:
            old = self.device.set_clipboard(text)
            if old is not None or self.device.get_clipboard() == text:
                with self._modifiers(["ctrl"]):
                    self.device.press("v")
                result["meta"]["type_path"] = "clipboard"
                return
        # 退化路径 1：纯 ASCII 且无换行 -> 整段 write（旧行为，保留）
        simple = all(32 <= ord(ch) <= 126 for ch in text) and "\n" not in text and "\r" not in text
        if simple:
            self.device.write(text, interval=interval)
            result["meta"]["type_path"] = "write"
            return
        # 退化路径 2：逐字符（换行 -> enter），不支持的非 ASCII 会被跳过并记录
        skipped: List[str] = []
        for ch in text:
            if ch in ("\n", "\r"):
                self.device.press("enter")
            elif 32 <= ord(ch) <= 126:
                self.device.write(ch, interval=interval)
            else:
                skipped.append(ch)
        result["meta"]["type_path"] = "char_by_char"
        if skipped:
            result["meta"]["type_skipped_chars"] = len(skipped)

    def _do_hotkey(self, action: Dict[str, Any], frame: CaptureFrame, result: Dict[str, Any]) -> None:
        keys = [str(k).lower() for k in action["keys"]]
        if not 1 <= len(keys) <= 3:
            raise CuaError(E_ARG_RANGE, "hotkey.keys 元素个数必须在 1~3，实际 %d" % len(keys),
                           hint="最多同时按 3 个键")
        for k in keys:
            if k not in KEY_WHITELIST:
                raise CuaError(E_KEY_NOT_ALLOWED, "键名不在白名单：%r" % (k,),
                               hint="白名单：a-z 0-9 F1-F12 ctrl alt shift win cmd enter tab esc space "
                                    "backspace delete up down left right home end pageup pagedown insert")
        self._check_dangerous_hotkey(keys)
        repeat = int(action.get("repeat") or 1)
        for _ in range(repeat):
            self.device.hotkey(*[_pyautogui_key(k) for k in keys])
        result["meta"]["hotkey"] = keys
        return None

    def _check_dangerous_hotkey(self, keys: Sequence[str]) -> None:
        if self.cfg.allow_dangerous_hotkeys:
            return
        as_set = set(keys)
        for pattern in DANGEROUS_HOTKEY_PATTERNS:
            if set(pattern) == as_set:
                raise CuaError(
                    E_KEY_NOT_ALLOWED,
                    "危险组合键默认关闭：%s" % "+".join(keys),
                    hint="如确需执行，请在配置里显式打开 allow_dangerous_hotkeys",
                    context={"keys": list(keys)},
                )

    def _do_drag(self, action: Dict[str, Any], frame: CaptureFrame, result: Dict[str, Any]) -> None:
        start = action["from"]
        end = action["to"]
        point_a = to_screen_point(start["x"], start["y"], frame, what="drag.from 坐标")
        point_b = to_screen_point(end["x"], end["y"], frame, what="drag.to 坐标")
        duration = float(action.get("duration_ms", 600)) / 1000.0
        if not 0.1 <= duration <= 5.0:
            raise CuaError(E_ARG_RANGE, "drag.duration_ms 必须在 100~5000，实际 %s" % action.get("duration_ms"),
                           hint="协议要求必须给时长（100~5000ms），瞬时拖拽会被当成点击")
        button = action.get("button") or "left"
        mods = self._check_modifiers(action.get("modifiers"))
        result["meta"]["screen_point"] = {"x": point_a[0], "y": point_a[1]}
        result["meta"]["screen_point_to"] = {"x": point_b[0], "y": point_b[1]}
        with self._modifiers(mods):
            self.device.move_to(point_a[0], point_a[1], duration=0.0)
            self.device.mouse_down(button)
            try:
                self.device.move_to(point_b[0], point_b[1], duration=duration)
            finally:
                self.device.mouse_up(button)
        return None

    def _do_scroll(self, action: Dict[str, Any], frame: CaptureFrame, result: Dict[str, Any]) -> None:
        point = to_screen_point(action["x"], action["y"], frame, what="scroll 锚点")
        dx = int(action.get("dx") or 0)
        dy = int(action.get("dy") or 0)
        if not -20 <= dx <= 20 or not -20 <= dy <= 20:
            raise CuaError(E_ARG_RANGE, "scroll 的 dx/dy 必须在 -20~20，实际 dx=%s dy=%s" % (dx, dy),
                           hint="合法区间 -20~20，正数 = 向右 / 向下")
        unit = action.get("unit") or "line"
        if unit not in ("line", "page"):
            raise CuaError(E_ARG_RANGE, "scroll.unit 只允许 line / page，实际 %r" % (unit,),
                           hint="unit 取值：line（默认）或 page")
        factor = float(self.cfg.scroll_page_lines) if unit == "page" else 1.0
        result["meta"]["screen_point"] = {"x": point[0], "y": point[1]}
        self.device.move_to(point[0], point[1], duration=0.0)
        if dy:
            # pyautogui.scroll 正数 = 向上，协议 dy 正数 = 向下
            self.device.scroll(int(round(-dy * factor)))
        if dx:
            # pyautogui.hscroll 正数 = 向右，与协议一致
            self.device.hscroll(int(round(dx * factor)))
        return None

    def _do_move(self, action: Dict[str, Any], frame: CaptureFrame, result: Dict[str, Any]) -> None:
        point = to_screen_point(action["x"], action["y"], frame, what="move 坐标")
        duration = float(action.get("duration_ms", 200)) / 1000.0
        if duration < 0:
            raise CuaError(E_ARG_RANGE, "move.duration_ms 不能为负", hint="取值 0~3000")
        result["meta"]["screen_point"] = {"x": point[0], "y": point[1]}
        self.device.move_to(point[0], point[1], duration=duration)
        return None

    def _do_wait(self, action: Dict[str, Any], frame: CaptureFrame, result: Dict[str, Any]) -> None:
        ms = int(action["ms"])
        if not 100 <= ms <= 10000:
            raise CuaError(E_ARG_RANGE, "wait.ms 必须在 100~10000，实际 %s" % ms, hint="合法区间 100~10000")
        self._sleep(ms / 1000.0)
        result["meta"]["waited_ms"] = ms
        return None

    def _do_screenshot(self, action: Dict[str, Any], frame: CaptureFrame, result: Dict[str, Any]) -> Optional[CaptureFrame]:
        region = action.get("region")
        if self.provider is None:
            raise CuaError(E_INTERNAL, "没有截图提供者，无法执行 screenshot 动作",
                           hint="请注入 ScreenshotProvider")
        if region is None:
            new_frame = self.provider.capture()
            result["meta"]["recapture"] = "full"
        else:
            if region.get("width", 0) < 16 or region.get("height", 0) < 16:
                raise CuaError(E_ARG_RANGE, "screenshot.region 的 width/height 最小为 16",
                               hint="协议规定 width/height >= 16")
            # 越界必须由执行器自己拒绝（协议 1.1 / 8.2），不能依赖某个提供者恰好做了校验：
            # 不同 provider 对越界区域的反应不同（有的报错、有的静默裁剪、有的会抛底层异常）。
            image_rect_to_screen_rect(
                (region.get("x", 0), region.get("y", 0), region.get("width", 0), region.get("height", 0)),
                frame,
                what="screenshot.region",
            )
            new_frame = self.provider.capture(region=region, base_frame=frame)
            result["meta"]["recapture"] = "region"
            result["meta"]["region_image_space"] = dict(region)
        result["meta"]["image"] = new_frame.image_block()
        result["observed"] = {"screen_changed": True, "change_ratio": 0.0, "cursor": None}
        return new_frame

    def _do_done(self, action: Dict[str, Any], frame: CaptureFrame, result: Dict[str, Any]) -> None:
        result["meta"]["done"] = {
            "status": action.get("status"),
            "summary": action.get("summary"),
            "artifacts": action.get("artifacts") or [],
        }
        result["observed"] = {"screen_changed": False, "change_ratio": 0.0, "cursor": None}
        return None


def destructive_guard(step: Any) -> List[str]:
    """协议第 7 节硬约束：命中断词表的文本。扫描模型给出的可见文本字段。"""
    raw = step.step if isinstance(step, ParsedStep) else (step or {})
    texts: List[str] = []
    for key in ("thought", "notes"):
        if isinstance(raw.get(key), str):
            texts.append(raw[key])
    action = raw.get("action") or {}
    if isinstance(action, dict):
        for key, value in action.items():
            if isinstance(value, str):
                texts.append(value)
            elif isinstance(value, list):
                texts.extend([v for v in value if isinstance(v, str)])
    final = raw.get("final")
    if isinstance(final, dict) and isinstance(final.get("summary"), str):
        texts.append(final["summary"])
    hits: List[str] = []
    for t in texts:
        for hit in destructive_hits(t):
            if hit not in hits:
                hits.append(hit)
    return hits

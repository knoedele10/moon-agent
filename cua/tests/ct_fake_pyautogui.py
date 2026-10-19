# -*- coding: utf-8 -*-
"""记录式 pyautogui 假件：CUA 契约测试的安全网 + 真实输入调用的证据记录器。

verifier 独立编写（不读取 cua/ 实现）。作用：
1. 在任何 cua.* 模块导入之前装进 sys.modules["pyautogui"]，使整套契约测试
   在物理上不可能移动真实鼠标/键盘（契约 §8 的安全前提，也避免验收动到使用者桌面）。
2. 记录每一次输入调用（calls），供 --dry-run / --mock 用例断言「零真实输入」。
3. 支持注入故障（fail_next / fail_after），用于取证「失败终止时必须释放修饰键」（契约 §8.7）。
4. pressed 跟踪当前仍按下的键，测试直接断言终态为空集。

只依赖标准库；screenshot() 延迟使用 Pillow（已装）。
"""
from __future__ import annotations

import sys
import types

MOUSE_MUTATING = frozenset({
    "moveTo", "moveRel", "move", "click", "doubleClick", "tripleClick",
    "rightClick", "middleClick", "mouseDown", "mouseUp",
    "dragTo", "dragRel", "drag", "scroll", "hscroll", "vscroll",
})
KEYBOARD_MUTATING = frozenset({
    "keyDown", "keyUp", "press", "hotkey", "write", "typewrite", "hold",
})
INPUT_MUTATING = MOUSE_MUTATING | KEYBOARD_MUTATING

KEYBOARD_KEYS = [
    chr(9), chr(10), chr(13), " ", "!", '"', "#", "$", "%", "&", "'", "(", ")", "*", "+",
    ",", "-", ".", "/", "0", "1", "2", "3", "4", "5", "6", "7", "8", "9", ":", ";",
    "<", "=", ">", "?", "@", "[", chr(92), "]", "^", "_", chr(96), "a", "b", "c", "d", "e",
    "f", "g", "h", "i", "j", "k", "l", "m", "n", "o", "p", "q", "r", "s", "t", "u",
    "v", "w", "x", "y", "z", "{", "|", "}", "~", "accept", "add", "alt", "altleft",
    "altright", "apps", "backspace", "browserback", "browserfavorites",
    "browserforward", "browserhome", "browserrefresh", "browsersearch",
    "browserstop", "capslock", "clear", "convert", "ctrl", "ctrlleft", "ctrlright",
    "decimal", "del", "delete", "divide", "down", "end", "enter", "esc", "escape",
    "execute", "f1", "f10", "f11", "f12", "f13", "f14", "f15", "f16", "f17", "f18",
    "f19", "f2", "f20", "f21", "f22", "f23", "f24", "f3", "f4", "f5", "f6", "f7",
    "f8", "f9", "final", "fn", "help", "home", "insert", "left", "linefeed",
    "medianexttrack", "mediaplaypause", "mediaprevioustrack", "mediastop",
    "multiply", "nexttrack", "nonconvert", "numlock", "pagedown", "pageup", "pause",
    "playpause", "prevtrack", "print", "printscreen", "right", "scrolllock",
    "select", "separator", "shift", "shiftleft", "shiftright", "sleep", "space",
    "stop", "subtract", "tab", "up", "volumeDown", "volumeMute", "volumeUp",
]


class FailSafeException(Exception):
    """与 pyautogui.FailSafeException 同名（契约 §5 E_EXECUTOR_FAILSAFE 的触发源）。"""


class Size(tuple):
    """复刻 pyautogui.size() 的返回值：namedtuple(width, height)，可下标/解包且有 .width/.height。"""

    def __new__(cls, width, height):
        return super().__new__(cls, (int(width), int(height)))

    width = property(lambda self: self[0])
    height = property(lambda self: self[1])

    def __repr__(self):
        return "Size(width=%d, height=%d)" % (self[0], self[1])


class Point(tuple):
    """复刻 pyautogui.position() 的返回值：namedtuple(x, y)。"""

    def __new__(cls, x, y):
        return super().__new__(cls, (int(x), int(y)))

    x = property(lambda self: self[0])
    y = property(lambda self: self[1])

    def __repr__(self):
        return "Point(x=%d, y=%d)" % (self[0], self[1])


class FakePyAutoGUI(types.ModuleType):
    """可记录的 pyautogui 替身。未显式实现的名字一律返回记录式 no-op。"""

    def __init__(self, name="pyautogui", screen=(1920, 1080)):
        super().__init__(name)
        self.calls = []
        self.pressed = set()
        self.mouse = (0, 0)
        self.FAILSAFE = True
        self.PAUSE = 0.0
        self.MINIMUM_DURATION = 0.0
        self.MINIMUM_SLEEP = 0.0
        self.DARWIN_CATCH_UP_TIME = 0.0
        self.KEYBOARD_KEYS = list(KEYBOARD_KEYS)
        self.FailSafeException = FailSafeException
        self._screen = tuple(int(v) for v in screen)
        self._fail_next = {}
        self._fail_after = None

    # ---------------- 记录 / 故障注入 / 观测 ----------------
    def _record(self, name, *args, **kwargs):
        self.calls.append((name, args, dict(kwargs)))
        n = self._fail_next.get(name, 0)
        if n:
            self._fail_next[name] = n - 1
            raise FailSafeException("fake: injected failure on " + str(name))
        if self._fail_after is not None and name in INPUT_MUTATING:
            self._fail_after -= 1
            if self._fail_after <= 0:
                self._fail_after = None
                raise FailSafeException("fake: armed failure after N input calls")

    def fail_next(self, name, times=1):
        self._fail_next[name] = self._fail_next.get(name, 0) + int(times)
        return self

    def fail_after(self, n):
        self._fail_after = int(n)
        return self

    def reset(self):
        self.calls = []
        self.pressed = set()
        self._fail_next = {}
        self._fail_after = None
        self.mouse = (0, 0)
        return self

    def call_names(self):
        return [c[0] for c in self.calls]

    def input_calls(self):
        return [c for c in self.calls if c[0] in INPUT_MUTATING]

    def has_real_input(self):
        return bool(self.input_calls())

    def balanced_keys(self):
        """返回仍未配对的按键集合（正常终态应为空）。"""
        return set(self.pressed)

    def unpaired_keydowns(self):
        down = [c[1][0] for c in self.calls if c[0] == "keyDown" and c[1]]
        up = [c[1][0] for c in self.calls if c[0] == "keyUp" and c[1]]
        leftover = list(down)
        for k in up:
            if k in leftover:
                leftover.remove(k)
        return leftover

    # ---------------- 屏幕 / 鼠标 ----------------
    def size(self):
        self._record("size")
        return Size(*self._screen)

    def position(self):
        self._record("position")
        return Point(*self.mouse)

    def onScreen(self, x, y=None):
        self._record("onScreen", x, y)
        if y is None:
            x, y = x
        return 0 <= x < self._screen[0] and 0 <= y < self._screen[1]

    def moveTo(self, x=None, y=None, duration=0.0, **kwargs):
        self._record("moveTo", x, y, duration=duration)
        if x is not None and y is not None:
            self.mouse = (x, y)

    def moveRel(self, xOffset=0, yOffset=0, duration=0.0, **kwargs):
        self._record("moveRel", xOffset, yOffset, duration=duration)
        self.mouse = (self.mouse[0] + xOffset, self.mouse[1] + yOffset)

    move = moveRel

    def click(self, x=None, y=None, clicks=1, interval=0.0, button="left", duration=0.0, **kwargs):
        self._record("click", x, y, clicks=clicks, button=button, duration=duration)
        if x is not None and y is not None:
            self.mouse = (x, y)

    def doubleClick(self, x=None, y=None, interval=0.0, button="left", **kwargs):
        self._record("doubleClick", x, y, button=button)

    def tripleClick(self, x=None, y=None, interval=0.0, button="left", **kwargs):
        self._record("tripleClick", x, y, button=button)

    def rightClick(self, x=None, y=None, **kwargs):
        self._record("rightClick", x, y)

    def middleClick(self, x=None, y=None, **kwargs):
        self._record("middleClick", x, y)

    def mouseDown(self, x=None, y=None, button="left", **kwargs):
        self._record("mouseDown", x, y, button=button)

    def mouseUp(self, x=None, y=None, button="left", **kwargs):
        self._record("mouseUp", x, y, button=button)

    def dragTo(self, x=None, y=None, duration=0.0, button="left", **kwargs):
        self._record("dragTo", x, y, duration=duration, button=button)

    def dragRel(self, xOffset=0, yOffset=0, duration=0.0, button="left", **kwargs):
        self._record("dragRel", xOffset, yOffset, duration=duration, button=button)

    def scroll(self, clicks, x=None, y=None, **kwargs):
        self._record("scroll", clicks, x, y)

    def hscroll(self, clicks, x=None, y=None, **kwargs):
        self._record("hscroll", clicks, x, y)

    def vscroll(self, clicks, x=None, y=None, **kwargs):
        self._record("vscroll", clicks, x, y)

    # ---------------- 键盘 ----------------
    def keyDown(self, key, **kwargs):
        self._record("keyDown", key)
        self.pressed.add(str(key).lower())

    def keyUp(self, key, **kwargs):
        self._record("keyUp", key)
        self.pressed.discard(str(key).lower())

    def press(self, keys, presses=1, interval=0.0, **kwargs):
        self._record("press", keys, presses=presses)

    def hotkey(self, *keys, **kwargs):
        self._record("hotkey", *keys)

    def write(self, message, interval=0.0, **kwargs):
        self._record("write", message)

    typewrite = write

    # ---------------- 截图 ----------------
    def screenshot(self, region=None, **kwargs):
        self._record("screenshot", region)
        try:
            from PIL import Image
        except Exception:
            return None
        img = Image.new("RGB", self._screen, (18, 18, 18))
        if region:
            try:
                x, y, w, h = [int(v) for v in region]
                img = img.crop((x, y, x + w, y + h))
            except Exception:
                pass
        return img

    def sleep(self, seconds):
        self._record("sleep", seconds)

    # ---------------- 未知名字 ----------------
    def __getattr__(self, name):
        if name.startswith("__") and name.endswith("__"):
            raise AttributeError(name)

        def _stub(*args, **kwargs):
            self._record(name, *args, **kwargs)
            return None

        return _stub


MODULE = None


def install(force=False):
    """把假件装进 sys.modules['pyautogui']，返回假件实例。"""
    global MODULE
    if MODULE is not None and not force:
        return MODULE
    m = FakePyAutoGUI()
    MODULE = m
    sys.modules["pyautogui"] = m
    return m


def get():
    return MODULE


def uninstall():
    global MODULE
    if MODULE is not None:
        sys.modules.pop("pyautogui", None)
        MODULE = None

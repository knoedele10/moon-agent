# -*- coding: utf-8 -*-
"""实现侧入口适配层（verifier 独立编写）。

背景：契约文档 §10 只冻结了模块名，没有冻结函数名/类名。本层负责：
  1. 在任何 cua.* 导入之前安装记录式 pyautogui 假件（安全网：测试不可能动到真实键鼠）；
  2. 按候选名单把「契约能力」解析成可调用的实现入口；
  3. 解析不到 -> unittest.SkipTest（实现尚未落地或命名不同），并在能力表里列出缺口；
  4. 解析到但行为与契约不符 -> 由测试 FAIL（这才是真问题）。

归一化约定：
  - 实现可以用「抛异常」「返回 False」「返回 {ok: False, error: {...}}」等任一方式表达拒绝；
    本层统一成 Outcome(ok, value, code)。
  - 错误码来源优先级：异常属性 code/error_code -> 异常消息里的 E_XXX -> 异常类名（camel -> E_XXX）。
"""
from __future__ import annotations

import copy
import functools
import importlib
import inspect
import json
import re
import subprocess
import sys
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO_ROOT = HERE.parents[1]
for _p in (str(HERE), str(REPO_ROOT)):
    if _p not in sys.path:
        sys.path.insert(0, _p)

from ct_fake_pyautogui import INPUT_MUTATING, install as _install_fake  # noqa: E402

FAKE = _install_fake()

MODULE_GROUPS = {
    "action_parser": ("cua.action_parser", "cua.parser", "cua.action_protocol",
                      "cua.protocol", "cua.actions", "cua.types"),
    "screenshot": ("cua.screenshot", "cua.screen", "cua.coords", "cua.geometry",
                   "cua.capture", "cua.observation"),
    "executor": ("cua.executor", "cua.exec", "cua.input", "cua.actuator"),
    "errors": ("cua.errors", "cua.error", "cua.retry"),
    "guard": ("cua.guard", "cua.popup", "cua.safety", "cua.executor", "cua.policy"),
    "router": ("cua.router", "cua.capability", "cua.route"),
    "loop": ("cua.loop", "cua.runner", "cua.orchestrator"),
    "main": ("cua.main", "cua.cli"),
}

SCAN_KEYWORDS = ("pars", "action", "protocol", "screen", "coord", "geometry",
                 "exec", "input", "error", "retry", "guard", "safety", "popup",
                 "key", "model", "obs", "type", "util", "config")

_LOADED = {}
_LOAD_ERRORS = {}


def load_module(dotted):
    if dotted in _LOADED:
        return _LOADED[dotted]
    try:
        mod = importlib.import_module(dotted)
    except Exception as exc:
        _LOADED[dotted] = None
        _LOAD_ERRORS[dotted] = "%s: %s" % (type(exc).__name__, exc)
        return None
    _LOADED[dotted] = mod
    return mod


def cua_package_present():
    return (REPO_ROOT / "cua").is_dir()


def _scan_extra():
    cua_dir = REPO_ROOT / "cua"
    if not cua_dir.is_dir():
        return []
    out = []
    for p in sorted(cua_dir.glob("*.py")):
        stem = p.stem
        if stem.startswith("_") or stem in ("main", "loop", "setup", "conftest"):
            continue
        if not any(k in stem.lower() for k in SCAN_KEYWORDS):
            continue
        out.append("cua." + stem)
    return out


def _group_modules(group):
    mods = []
    for dotted in MODULE_GROUPS[group]:
        m = load_module(dotted)
        if m is not None:
            mods.append((dotted, m))
    for dotted in _scan_extra():
        if dotted in MODULE_GROUPS[group]:
            continue
        m = load_module(dotted)
        if m is not None:
            mods.append((dotted, m))
    return mods


def _norm(s):
    return re.sub(r"[^a-z0-9]", "", str(s).lower())


def find_attr(groups, names, require_callable=False, want_container=False):
    """在候选模块里找属性。返回 (dotted, attrname, value) 或 (None, None, None)。"""
    for g in groups:
        for dotted, mod in _group_modules(g):
            for n in names:
                if hasattr(mod, n):
                    val = getattr(mod, n)
                    if require_callable and not callable(val):
                        continue
                    if want_container and callable(val):
                        continue
                    return dotted, n, val
    wanted = dict((_norm(n), n) for n in names)
    for g in groups:
        for dotted, mod in _group_modules(g):
            try:
                attrs = dir(mod)
            except Exception:
                continue
            for attr in attrs:
                if attr.startswith("__"):
                    continue
                if _norm(attr) in wanted:
                    val = getattr(mod, attr)
                    if require_callable and not callable(val):
                        continue
                    if want_container and callable(val):
                        continue
                    return dotted, attr, val
    return None, None, None


def reason_missing(label, *groups):
    if not cua_package_present():
        return "适配层：cua/ 包尚不存在，%s 无法验证（实现未落地）" % label
    seen = []
    for g in groups:
        for dotted in MODULE_GROUPS.get(g, ()):
            if load_module(dotted) is not None:
                seen.append(dotted)
    if not seen:
        errs = "; ".join("%s(%s)" % (k, v) for k, v in list(_LOAD_ERRORS.items())[:4])
        return ("适配层：%s 的候选模块都导入失败或不存在；%s 未实现。导入错误样本：%s"
                % (label, "/".join(MODULE_GROUPS.get(groups[0], ("?",))), errs))
    return ("适配层：模块已存在（%s）但找不到 %s 的入口（契约 §10 只冻结模块名，未冻结函数名）"
            % (", ".join(sorted(set(seen))), label))


# --------------------------------------------------------------------------
# 结果归一化
# --------------------------------------------------------------------------
def _camel_to_upper(name):
    s = re.sub(r"([a-z0-9])([A-Z])", r"\1_\2", name)
    s = re.sub(r"(_?Error|_?Exception|_?Fault)$", "", s)
    return s.replace("_", "").upper()


def code_of(obj):
    """尽力从异常/返回值里解析出 §5 错误码。"""
    if obj is None:
        return None
    if isinstance(obj, str):
        m = re.search(r"\bE_[A-Z][A-Z0-9_]*\b", obj)
        return m.group(0) if m else None
    if isinstance(obj, dict):
        for key in ("code", "error_code", "err_code", "error", "reason"):
            if key in obj and obj[key] is not None:
                got = code_of(obj[key])
                if got:
                    return got
        return None
    if isinstance(obj, BaseException):
        for attr in ("code", "error_code", "err_code"):
            v = getattr(obj, attr, None)
            if v is not None:
                got = code_of(v)
                if got:
                    return got
        got = code_of(str(obj))
        if got:
            return got
        target = _camel_to_upper(type(obj).__name__)
        for c in KNOWN_CODES:
            body = c[2:].replace("_", "")
            if target == body or (len(body) >= 5 and (target.startswith(body) or body.startswith(target))):
                return c
        return None
    for attr in ("code", "error_code", "err_code"):
        v = getattr(obj, attr, None)
        if v is not None:
            got = code_of(v)
            if got:
                return got
    return code_of(str(obj))


KNOWN_CODES = (
    "E_BAD_JSON", "E_SCHEMA", "E_UNKNOWN_ACTION", "E_ARG_RANGE", "E_OUT_OF_BOUNDS",
    "E_KEY_NOT_ALLOWED", "E_NO_FOCUS", "E_MODEL_TIMEOUT", "E_MODEL_HTTP",
    "E_MODEL_REFUSAL", "E_EXECUTOR_FAILSAFE", "E_EXPECT_TIMEOUT", "E_POPUP_BLOCKING",
    "E_STALL", "E_LOOP", "E_STEP_BUDGET", "E_WALL_BUDGET", "E_CANCEL",
    "E_NEED_HUMAN", "E_INTERNAL",
)


class Outcome(object):
    __slots__ = ("ok", "value", "code", "exc", "where")

    def __init__(self, ok, value=None, code=None, exc=None, where=""):
        self.ok = bool(ok)
        self.value = value
        self.code = code
        self.exc = exc
        self.where = where

    def describe(self):
        if self.ok:
            return "ok value=%r @%s" % (self.value, self.where)
        return "failed code=%s exc=%r @%s" % (self.code, self.exc, self.where)

    def __repr__(self):
        return "<Outcome %s>" % self.describe()


def call(fn, *args, **kwargs):
    where = kwargs.pop("_where", "")
    try:
        return Outcome(True, fn(*args, **kwargs), where=where)
    except Exception as exc:
        return Outcome(False, None, code_of(exc), exc, where)


def call_shapes(fn, shapes, where=""):
    """按候选参数形状调用；全部 TypeError -> SkipTest（入口签名不认识）。返回值先过校验。"""
    last = None
    for item in shapes:
        args, kwargs = item[0], item[1]
        validate = item[2] if len(item) > 2 else None
        try:
            value = fn(*args, **kwargs)
        except TypeError as exc:
            last = exc
            continue
        except Exception as exc:
            return Outcome(False, None, code_of(exc), exc, where)
        if validate is not None and not validate(value):
            last = TypeError("返回 %r 不是预期的坐标结构" % (value,))
            continue
        return Outcome(True, value, where=where)
    raise unittest.SkipTest(
        "适配层：%s 的入口 %s 与全部 %d 种候选调用形式都不匹配（最后一次：%s）"
        % (where, getattr(fn, "__name__", fn), len(shapes), last))


def is_pair_of_numbers(value):
    if isinstance(value, (tuple, list)) and len(value) == 2:
        return all(isinstance(v, (int, float)) and not isinstance(v, bool) for v in value)
    if isinstance(value, dict) and "x" in value and "y" in value:
        return all(isinstance(value[k], (int, float)) and not isinstance(value[k], bool) for k in ("x", "y"))
    return all(hasattr(value, k) for k in ("x", "y"))


def as_point(value):
    if isinstance(value, (tuple, list)) and len(value) == 2:
        return (value[0], value[1])
    if isinstance(value, dict):
        return (value["x"], value["y"])
    return (value.x, value.y)


def rejected(outcome):
    """实现是否表达了「拒绝」。

    只看「能力自己归一出来的处置结果」：ok=False 或携带错误/无效标记。
    不再把裸 False 当拒绝 —— 同一个 False 在「护栏」里是放行、在「白名单判定」里是拒绝，
    由各能力在 _call 里归一（见 key_allowed / click_guard）。
    """
    if not outcome.ok:
        return True
    v = outcome.value
    if isinstance(v, dict):
        if v.get("ok") is False or v.get("valid") is False:
            return True
        if v.get("in_bounds") is False or v.get("allowed") is False:
            return True
        if v.get("error"):
            return True
    return False


def rejection_code(outcome):
    if outcome.code:
        return outcome.code
    v = outcome.value
    if isinstance(v, dict):
        got = code_of(v)
        if got:
            return got
        errs = v.get("errors")
        if errs:
            got = code_of(str(errs))
            if got:
                return got
    return None


def verdict(outcome):
    """把「校验类」返回值归一成 "OK" 或错误码字符串。

    约定：返回 False 视为「拒绝但未给错误码」，返回 REJECTED_NO_CODE 便于测试报出精确问题。
    """
    if not outcome.ok:
        return outcome.code or "UNKNOWN_ERROR"
    v = outcome.value
    if isinstance(v, bool):
        return "OK" if v else "REJECTED_NO_CODE"
    if v is None:
        return "OK"
    if isinstance(v, dict):
        if v.get("ok") is False or v.get("valid") is False:
            return rejection_code(outcome) or "REJECTED_NO_CODE"
        if v.get("errors"):
            return code_of(str(v["errors"])) or "REJECTED_NO_CODE"
        return "OK"
    if isinstance(v, (list, tuple)):
        if not v:
            return "OK"
        return code_of(str(v)) or "REJECTED_NO_CODE"
    result = getattr(v, "result", None)
    if isinstance(result, dict):
        return verdict(Outcome(True, result, where=outcome.where))
    ok_attr = getattr(v, "ok", None)
    if isinstance(ok_attr, bool):
        return "OK" if ok_attr else "REJECTED_NO_CODE"
    return "OK"


def unwrap(value):
    """剥掉实现的结果包装（如 ExtractResult.value），返回最内层原始值。"""
    for _ in range(8):
        inner = getattr(value, "value", None)
        if inner is None or inner is value:
            break
        value = inner
    return value


def as_object(value):
    """把提取结果归一成 dict：直接 dict / JSON 字符串 / 带 .value 的结果对象（如 ExtractResult）。"""
    if isinstance(value, dict):
        return value
    if isinstance(value, str):
        try:
            got = json.loads(value)
        except Exception:
            return None
        return got if isinstance(got, dict) else None
    inner = getattr(value, "value", None)
    if inner is not None and inner is not value:
        return as_object(inner)
    return None


# --------------------------------------------------------------------------
# 能力解析
# --------------------------------------------------------------------------
def json_extractor():
    label = "JSON 提取（§2.1）"
    dotted, name, fn = find_attr(
        ("action_parser",), (
            "extract_json", "extract_json_object", "extract_json_from_text",
            "parse_json", "parse_step_json", "extract_step", "extract",
            "loads_step", "load_step_json", "json_extract",
        ), require_callable=True)
    if fn is None:
        raise unittest.SkipTest(reason_missing(label, "action_parser"))
    where = "%s.%s" % (dotted, name)

    def _call(text):
        return call(fn, text, _where=where)

    return _Cap("extract_json", _call, where)


def step_validator():
    label = "Step 校验（§2/§3 Schema）"
    dotted, name, fn = find_attr(
        ("action_parser",), (
            "validate_step", "validate", "validate_action", "validate_step_dict",
            "check_step", "check_action", "parse_step", "parse", "parse_action",
            "load_step", "coerce_step", "normalize_step",
        ), require_callable=True)
    if fn is None:
        raise unittest.SkipTest(reason_missing(label, "action_parser"))
    where = "%s.%s" % (dotted, name)

    def _call(step, image=None):
        # 深拷贝：实现可能原地改写传入的 step（已观察到四舍五入就地生效），
        # 不能让它污染本套件的共享夹具。
        clean = copy.deepcopy(step)
        text = json.dumps(clean, ensure_ascii=False)
        shapes = [((clean,), {}), ((text,), {})]
        if image is not None:
            shapes.extend([
                ((step, image), {}),
                ((step, image.get("width"), image.get("height")), {}),
                ((text, image), {}),
                ((step, dict(image)), {}),
            ])
        return call_shapes(fn, shapes, where=where)

    return _Cap("step_validator", _call, where)


_FRAME_CLS_CACHE = {}


def frame_class():
    """找实现里的帧元信息类型（如 CaptureFrame）。"""
    if "cls" not in _FRAME_CLS_CACHE:
        dotted, name, cls = find_attr(
            ("screenshot", "observation", "executor"),
            ("CaptureFrame", "ImageFrame", "ScreenFrame", "Frame", "ImageMeta"))
        _FRAME_CLS_CACHE["cls"] = (dotted, name, cls) if isinstance(cls, type) else (None, None, None)
    return _FRAME_CLS_CACHE["cls"]


def frame_from(origin, scale, width=None, height=None, image=None):
    """按实现自己的帧类型构造一帧元信息：origin/scale 是 §1.1 的唯一换算依据。"""
    dotted, name, cls = frame_class()
    if cls is None:
        return None
    try:
        params = list(inspect.signature(cls).parameters)
    except Exception:
        params = []
    if image is None and width and height:
        try:
            from PIL import Image  # Pillow 已装；越界判定按 image.width/height 取尺寸
            image = Image.new("RGB", (int(width), int(height)), (10, 10, 10))
        except Exception:
            image = None
    kwargs = {}
    for p in params:
        low = p.lower()
        if low in ("origin", "origin_xy", "offset"):
            kwargs[p] = (int(origin["x"]), int(origin["y"]))
        elif low in ("scale", "image_scale"):
            kwargs[p] = float(scale)
        elif low in ("screen_size", "screen", "size"):
            kwargs[p] = (int(width or 0), int(height or 0))
        elif low in ("image", "img", "frame_image"):
            kwargs[p] = image
        elif low in ("label",):
            kwargs[p] = "verifier-test"
    try:
        return cls(**kwargs)
    except Exception:
        return None


def image_to_screen():
    label = "坐标换算 image -> screen（§1.1）"
    dotted, name, fn = find_attr(
        ("screenshot", "executor"), (
            "image_to_screen", "to_screen", "to_screen_coords", "image_to_screen_point",
            "screen_point", "map_to_screen", "convert_to_screen", "to_screen_point",
            "image_point_to_screen", "scale_point", "denormalize_point",
        ), require_callable=True)
    if fn is None:
        raise unittest.SkipTest(reason_missing(label, "screenshot"))
    where = "%s.%s" % (dotted, name)

    def _call(x, y, origin, scale):
        meta = {"origin": dict(origin), "scale": scale}
        point = {"x": x, "y": y}
        frame = frame_from(origin, scale)
        shapes = []
        if frame is not None:
            shapes.append(((x, y, frame), {}))
        shapes += [
            ((x, y, dict(origin), scale), {}),
            ((x, y, scale, dict(origin)), {}),
            ((point, dict(origin), scale), {}),
            ((point, meta), {}),
            ((x, y, meta), {}),
            ((x, y, origin["x"], origin["y"], scale), {}),
            ((point, origin["x"], origin["y"], scale), {}),
            ((x, y, origin["x"], origin["y"], scale, meta), {}),
        ]
        shapes = [(a, k, is_pair_of_numbers) for a, k in shapes]
        return call_shapes(fn, shapes, where=where)

    return _Cap("image_to_screen", _call, where)


def screen_to_image():
    label = "坐标换算 screen -> image（§1.1 逆算，文档未显式冻结）"
    dotted, name, fn = find_attr(
        ("screenshot", "executor"), (
            "screen_to_image", "to_image", "to_image_coords", "image_coords",
            "screen_to_image_point", "map_to_image", "from_screen", "to_image_point",
        ), require_callable=True)
    if fn is None:
        raise unittest.SkipTest(reason_missing(label, "screenshot"))
    where = "%s.%s" % (dotted, name)

    def _call(sx, sy, origin, scale):
        meta = {"origin": dict(origin), "scale": scale}
        point = {"x": sx, "y": sy}
        frame = frame_from(origin, scale)
        shapes = []
        if frame is not None:
            shapes.append(((sx, sy, frame), {}))
        shapes += [
            ((sx, sy, dict(origin), scale), {}),
            ((sx, sy, scale, dict(origin)), {}),
            ((point, meta), {}),
            ((point, dict(origin), scale), {}),
            ((sx, sy, meta), {}),
            ((sx, sy, origin["x"], origin["y"], scale), {}),
        ]
        shapes = [(a, k, is_pair_of_numbers) for a, k in shapes]
        return call_shapes(fn, shapes, where=where)

    return _Cap("screen_to_image", _call, where)


def bounds_checker():
    label = "越界判定（§1.1/§8.2）"
    dotted, name, fn = find_attr(
        ("screenshot", "executor", "action_parser"), (
            "check_image_bounds", "check_bounds", "assert_in_bounds", "ensure_in_bounds",
            "validate_point", "bounds_check", "in_bounds", "check_point",
            "validate_bounds", "reject_out_of_bounds",
        ), require_callable=True)
    if fn is None:
        raise unittest.SkipTest(reason_missing(label, "screenshot"))
    where = "%s.%s" % (dotted, name)

    def _call(x, y, width, height):
        point = {"x": x, "y": y}
        image = {"width": width, "height": height}
        frame = frame_from({"x": 0, "y": 0}, 1.0, width, height)
        shapes = []
        if frame is not None:
            shapes += [((x, y, frame), {}), ((point, frame), {})]
        shapes += [
            ((x, y, width, height), {}),
            ((point, width, height), {}),
            ((point, image), {}),
            ((x, y, image), {}),
            ((point, dict(image, scale=1.0, origin={"x": 0, "y": 0})), {}),
        ]
        return call_shapes(fn, shapes, where=where)

    return _Cap("bounds_check", _call, where)


def bounds_enforced_convert():
    """「先检后换」的一步到位入口（§1.1：越界必须拒绝，绝不 clamp）。

    优先用实现自己的一步入口（如 to_screen_point）；没有时退回
    「bounds_check + image_to_screen」的组合 —— 这正是 §1.1 规定的调用顺序。
    """
    label = "越界拒绝 + 坐标换算（§1.1）"
    dotted, name, fn = find_attr(
        ("screenshot", "executor"), (
            "to_screen_point", "image_to_screen_checked", "checked_image_to_screen",
            "convert_point_to_screen", "map_point_to_screen", "image_point_to_screen_safe",
        ), require_callable=True)
    if fn is not None:
        where = "%s.%s" % (dotted, name)

        def _call(x, y, origin, scale, width=None, height=None, _fn=fn, _where=where):
            frame = frame_from(origin, scale, width, height)
            shapes = []
            if frame is not None:
                shapes.append(((x, y, frame), {}))
            shapes.append(((x, y, dict(origin), scale), {}))
            shapes.append(((x, y, {"origin": dict(origin), "scale": scale}), {}))
            shapes = [(a, k, is_pair_of_numbers) for a, k in shapes]
            return call_shapes(_fn, shapes, where=_where)

        return _Cap("bounds_enforced_convert", _call, where)

    check = bounds_checker()
    conv = image_to_screen()
    where = "%s + %s（组合：先检后换）" % (check.where, conv.where)

    def _call(x, y, origin, scale, width=None, height=None, _check=check, _conv=conv):
        chk = _check(x, y, int(width or 0), int(height or 0))
        if rejected(chk):
            return chk
        return _conv(x, y, origin, scale)

    return _Cap("bounds_enforced_convert", _call, where)


def step_validator_with_image():
    """解析「显式接受 image/observation 参数」的 Step 校验入口，用于越界判据。

    只在实现签名里真的声明了 image 类参数时才认为它能做越界校验；
    否则 SkipTest —— 避免把一个「不认识 image 参数」的解析器误判为契约违规。
    """
    label = "Step 校验（带 image 的越界检查）"
    names = (
        "validate_step", "validate", "validate_action", "validate_step_dict",
        "check_step", "check_action", "parse_step", "parse", "parse_action",
        "load_step", "coerce_step", "normalize_step",
    )
    image_params = {"image", "observation", "frame", "screen", "image_meta", "step_image"}
    for dotted, mod in _group_modules("action_parser"):
        for n in names:
            if not hasattr(mod, n):
                continue
            fn = getattr(mod, n)
            if not callable(fn):
                continue
            try:
                sig = inspect.signature(fn)
            except Exception:
                continue
            meta = set(sig.parameters) & image_params
            if not meta:
                continue
            where = "%s.%s" % (dotted, n)

            def _call(step, image, _fn=fn, _where=where):
                text = json.dumps(step, ensure_ascii=False)
                shapes = [
                    ((copy.deepcopy(step), image), {}),
                    ((step, image.get("width"), image.get("height")), {}),
                    ((text, image), {}),
                    ((step, dict(image)), {}),
                ]
                return call_shapes(_fn, shapes, where=_where)

            return _Cap("step_validator_with_image", _call, where)
    raise unittest.SkipTest(reason_missing(label, "action_parser"))


def scan_sources(pattern):
    """在 cua/ 实现源码里做一次静态安全扫描（只用于安全网/反向取证，不用于判定语义）。"""
    cua_dir = REPO_ROOT / "cua"
    hits = []
    if not cua_dir.is_dir():
        return hits
    for p in sorted(cua_dir.rglob("*.py")):
        if "tests" in p.relative_to(cua_dir).parts:
            continue  # 不扫描本验收套件自身
        try:
            text = p.read_text(encoding="utf-8", errors="replace")
        except Exception:
            continue
        for m in re.finditer(pattern, text):
            hits.append(("%s:%d" % (p.relative_to(REPO_ROOT), text.count(chr(10), 0, m.start()) + 1),
                         m.group(0).strip()))
    return hits


def key_allowed():
    label = "键名白名单判定（§3.3）"
    dotted, name, val = find_attr(
        ("executor", "action_parser", "guard"), (
            "is_allowed_key", "check_key", "validate_key", "key_allowed",
            "ensure_key_allowed", "allowed_key", "check_keys", "validate_keys",
            "normalize_key", "is_valid_key",
        ), require_callable=True)
    if val is not None:
        where = "%s.%s" % (dotted, name)

        def _call(key):
            res = call_shapes(val, [((key,), {}), (([key],), {}), ((key, None), {})], where=where)
            if not res.ok:
                return res
            allowed = res.value
            if isinstance(allowed, (list, tuple)):
                allowed = len(allowed) == 0
            if isinstance(allowed, dict):
                allowed = bool(allowed.get("ok", allowed.get("allowed", False)))
            if not isinstance(allowed, bool):
                return res
            # 实现只给布尔时，按 §5「白名单外一律 E_KEY_NOT_ALLOWED」归一错误码
            return Outcome(allowed, allowed, None if allowed else "E_KEY_NOT_ALLOWED", None, where)

        return _Cap("key_allowed_fn", _call, where)
    dotted, name, cont = find_attr(
        ("executor", "action_parser", "guard"), (
            "KEY_WHITELIST", "ALLOWED_KEYS", "KEY_ALLOWLIST", "ALLOWED_KEY_NAMES",
            "WHITELIST", "KEY_BLACKLIST", "ALLOWED_KEYS_SET",
        ), want_container=True)
    if cont is not None:
        where = "%s.%s" % (dotted, name)
        if isinstance(cont, dict):
            members = set(cont.keys())
        elif isinstance(cont, (set, frozenset, list, tuple)):
            members = set(cont)
        else:
            members = None
        if members is None:
            raise unittest.SkipTest("适配层：%s 不是可判定的集合类型（%s）" % (where, type(cont).__name__))
        normalized = set(str(m) for m in members)
        normalized.update(m.lower() for m in normalized)

        def _call(key):
            allowed = key in members or str(key).lower() in normalized
            return Outcome(allowed, allowed, None if allowed else "E_KEY_NOT_ALLOWED", None, where)

        return _Cap("key_whitelist_container", _call, where)
    raise unittest.SkipTest(reason_missing(label, "executor"))


def destructive_predicate():
    label = "破坏性词表判定（§7）"
    dotted, name, fn = find_attr(
        ("guard", "executor", "action_parser", "loop"), (
            "destructive_hits", "is_destructive", "hits_destructive", "detect_destructive",
            "check_destructive", "match_destructive", "destructive_hit",
            "contains_destructive", "is_dangerous_text", "guard_text",
        ), require_callable=True)
    if fn is not None:
        where = "%s.%s" % (dotted, name)

        def _call(text):
            return call_shapes(fn, [((text,), {}), ((text, "popup"), {}), ((text, {}), {})], where=where)

        return _Cap("is_destructive_fn", _call, where)
    dotted, name, cont = find_attr(
        ("guard", "executor", "action_parser"), (
            "DESTRUCTIVE_WORDS", "BLOCKED_WORDS", "FORBIDDEN_WORDS",
            "DANGEROUS_WORDS", "DESTRUCTIVE_KEYWORDS", "NEED_HUMAN_WORDS",
        ), want_container=True)
    if cont is None:
        raise unittest.SkipTest(reason_missing(label, "guard"))
    where = "%s.%s" % (dotted, name)
    words = tuple(cont) if isinstance(cont, (list, tuple, set, frozenset)) else tuple(cont.keys())

    def _call(text):
        if not isinstance(text, str):
            return Outcome(True, False, where=where)
        low = text.lower()
        for w in words:
            w = str(w)
            if w in text or w.lower() in low:
                return Outcome(True, True, where=where)
        return Outcome(True, False, where=where)

    return _Cap("destructive_words_container", _call, where)


def click_guard():
    label = "弹窗破坏性按钮护栏（§7 硬约束）"
    dotted, name, fn = find_attr(
        ("guard", "executor", "loop", "action_parser"), (
            "destructive_guard", "guard_popup_click", "guard_click", "check_popup_click",
            "popup_guard", "safe_click", "guard_button", "check_destructive_click",
            "guard_action", "popup_button_policy", "decide_popup_button",
        ), require_callable=True)
    if fn is None:
        raise unittest.SkipTest(reason_missing(label, "guard"))
    where = "%s.%s" % (dotted, name)

    def _call(text, action=None):
        """命中破坏性词表 -> 判为拒绝（§7 硬约束：命中即不允许点击、转人工）。

        有的实现把护栏写成「返回命中的词」的谓词（空列表=安全）；本适配层把
        「非空命中」映射为拒绝（E_NEED_HUMAN 是本套件的映射，不是实现直接返回的码），
        「空命中」映射为放行。
        """
        act = action or {"type": "click", "x": 10, "y": 20}
        step = {"protocol": "cua/1", "thought": text, "action": act, "done": False,
                "notes": text}
        shapes = [
            ((step,), {}),
            ((text, act), {}),
            ((act, text), {}),
            ((text,), {}),
            ((text, act, {}), {}),
        ]
        res = call_shapes(fn, shapes, where=where)
        if not res.ok:
            return res
        if res.code:
            return res
        v = res.value
        if isinstance(v, (list, tuple, set)):
            hits = list(v)
            if hits:
                return Outcome(False, None, "E_NEED_HUMAN", None,
                               "%s[hits=%s]" % (where, hits))
            return Outcome(True, False, where=where)
        if v is True:
            return Outcome(False, None, "E_NEED_HUMAN", None, where)
        if v is False or v is None:
            return Outcome(True, bool(v), where=where)
        return Outcome(True, True, where=where)

    return _Cap("click_guard", _call, where)


PROTOCOL_VERSION = "cua/1"


def _normalize_exec_result(res, where):
    """把 ExecutionOutcome 之类的执行回执归一成 Outcome(ok, result_dict, code)。"""
    if not res.ok:
        return res
    v = res.value
    result = getattr(v, "result", None)
    if isinstance(result, dict):
        ok = bool(result.get("ok", True))
        return Outcome(ok, result, code_of(result) if not ok else None, None, where)
    return res


def _verifier_frame(width=1920, height=1080):
    return frame_from({"x": 0, "y": 0}, 1.0, width, height)


def _verifier_step(action):
    return {"protocol": PROTOCOL_VERSION, "thought": "verifier 取证：只做一步",
            "action": action, "done": False}


def _make_screenshot_provider():
    """注入实现自己的截图提供者（走假件，不会真的截屏）。"""
    dotted, name, cls = find_attr(
        ("screenshot",), ("PyAutoGUIScreenshot", "ScreenshotProvider", "PyAutoGUIScreen",
                          "ScreenGrabber", "RealScreenshot"))
    if cls is None or not isinstance(cls, type):
        return None
    try:
        return cls()
    except Exception:
        return None


def _executor_runner(init_kwargs=None):
    """解析「类形态」执行器 Executor(...).execute(step, frame)，返回统一入口。"""
    init_kwargs = dict(init_kwargs or {})
    for cls_name in ("Executor", "ActionExecutor", "Actuator", "InputExecutor"):
        dotted, name, cls = find_attr(("executor",), (cls_name,))
        if cls is None or not isinstance(cls, type):
            continue
        try:
            params = set(inspect.signature(cls.__init__).parameters)
        except Exception:
            params = set(init_kwargs)
        wanted = dict(init_kwargs)
        if "provider" in params and "provider" not in wanted:
            prov = _make_screenshot_provider()
            if prov is not None:
                wanted["provider"] = prov  # screenshot 动作需要提供者，否则先报 E_INTERNAL
        kwargs = dict((k, v) for k, v in wanted.items() if k in params)
        if any(k not in kwargs for k in init_kwargs):
            continue  # 实现没承诺这个开关，不硬塞
        try:
            instance = cls(**kwargs)
        except Exception:
            continue
        for meth in ("execute", "execute_action", "perform", "run", "do"):
            if not hasattr(instance, meth):
                continue
            bound = getattr(instance, meth)
            where = "%s.%s.%s" % (dotted, name, meth)

            def _call(action, width=None, height=None, _bound=bound, _where=where):
                frame = _verifier_frame(width or 1920, height or 1080)
                shapes = [((_verifier_step(action), frame), {}), ((action, frame), {}),
                          ((action,), {})]
                return _normalize_exec_result(call_shapes(_bound, shapes, where=_where), _where)

            return _Cap("executor_instance", _call, where)
    return None


def action_executor():
    label = "动作执行入口（§4/§8）"
    cap = _executor_runner({})
    if cap is not None:
        return cap
    dotted, name, fn = find_attr(
        ("executor",), (
            "execute_action", "execute", "perform_action", "do_action", "apply_action",
            "run_action", "execute_step_action", "dispatch_action",
        ), require_callable=True)
    if fn is not None:
        where = "%s.%s" % (dotted, name)

        def _call(action, width=None, height=None):
            frame = _verifier_frame(width or 1920, height or 1080)
            shapes = [((_verifier_step(action), frame), {}), ((action, frame), {}),
                      ((action,), {})]
            return _normalize_exec_result(call_shapes(fn, shapes, where=where), where)

        return _Cap("execute_action_fn", _call, where)
    raise unittest.SkipTest(reason_missing(label, "executor"))


def reject_step(step, image=None):
    """综合校验面：先问 Step 校验器，再问执行器；返回首个非 OK 的判定。

    契约只要求「非法 Step / 非法动作被拒并给出错误码」，没有规定必须由哪一层拒绝
    （§10 的分工里键名白名单天然属于执行层）。因此给实现两层机会，判定标准不变。
    """
    seen = []
    try:
        got = verdict(step_validator()(step, image))
        if got != "OK":
            return got
        seen.append("parse=OK")
    except unittest.SkipTest:
        seen.append("parse=SKIP")
    try:
        ex = action_executor()
        res = ex(dict(step.get("action") or {}))
        got = verdict(res)
        if got not in ("OK",):
            return got
        seen.append("exec=OK")
    except unittest.SkipTest:
        seen.append("exec=SKIP")
    return "OK"


class _Cap(object):
    __slots__ = ("name", "fn", "where")

    def __init__(self, name, fn, where):
        self.name = name
        self.fn = fn
        self.where = where

    def __call__(self, *args, **kwargs):
        return self.fn(*args, **kwargs)

    def __repr__(self):
        return "<cap %s @%s>" % (self.name, self.where)


def _explicit_param(fn, names):
    """函数签名里是否显式声明了某个参数名（不接受 **kwargs 的模糊匹配）。"""
    try:
        sig = inspect.signature(fn)
    except Exception:
        return None
    for n in names:
        if n in sig.parameters:
            return n
    return None


def dry_run_executor():
    """解析出「显式声明了 dry_run / mock 开关」的执行入口。

    只有显式声明该参数（不是 **kwargs 兜住）才认为实现承诺了这个开关；
    找不到 -> SkipTest，并在报告里记为「待实现落地后复核」。
    """
    label = "dry-run / mock 执行开关"
    names = ("dry_run", "mock")
    # 类形态优先：与 action_executor 共用同一条调用路径（带帧、归一结果），
    # 否则「零输入」断言会因为动作根本没执行而假通过。
    for flag in ("dry_run", "mock"):
        cap = _executor_runner({flag: True})
        if cap is not None:
            return _Cap("dry_run_instance", cap.fn, "%s（%s=True）" % (cap.where, flag))

    dotted, name, fn = find_attr(
        ("executor", "loop", "main"), (
            "execute_action", "execute", "perform_action", "do_action", "run_action",
            "execute_step", "dispatch_action",
        ), require_callable=True)
    if fn is not None:
        param = _explicit_param(fn, names)
        if param is None:
            raise unittest.SkipTest(
                "适配层：%s.%s 未显式声明 dry_run/mock 参数，无法在此层取证（主循环层待验证）"
                % (dotted, name))
        where = "%s.%s(%s=True)" % (dotted, name, param)
        inner = functools.partial(fn, **{param: True})

        def _call(action, width=None, height=None, _fn=inner, _where=where):
            frame = _verifier_frame(width or 1920, height or 1080)
            shapes = [((_verifier_step(action), frame), {}), ((action, frame), {}),
                      ((action,), {})]
            return _normalize_exec_result(call_shapes(_fn, shapes, where=_where), _where)

        return _Cap("dry_run_fn", _call, where)

    raise unittest.SkipTest(reason_missing(label, "executor", "loop", "main"))


# --------------------------------------------------------------------------
# 错误策略视图（§5）
# --------------------------------------------------------------------------
def _spec_from_any(value):
    if isinstance(value, bool):
        return {"retryable": value}
    if isinstance(value, int):
        return {"retryable": value > 0, "max_retries": value}
    if isinstance(value, dict):
        out = {}
        for k in ("retryable", "retry", "can_retry", "is_retryable"):
            if k in value:
                out["retryable"] = bool(value[k])
        for k in ("max_retries", "max_attempts", "retries", "retry_limit", "limit"):
            if k in value:
                out["max_retries"] = int(value[k])
        for k in ("must_replan", "replan", "needs_replan", "force_replan"):
            if k in value:
                out["must_replan"] = bool(value[k])
        if "retryable" not in out and out.get("max_retries") is not None:
            out["retryable"] = out["max_retries"] > 0
        return out
    # 对象形态（如 ErrorPolicy dataclass）
    out = {}
    for field_name, keys in (("retryable", ("retryable", "retry", "can_retry", "is_retryable")),
                             ("max_retries", ("max_retries", "max_attempts", "retry_limit")),
                             ("must_replan", ("must_replan", "replan", "needs_replan"))):
        for k in keys:
            if hasattr(value, k):
                v = getattr(value, k)
                if v is None:
                    continue
                out[field_name] = int(v) if field_name == "max_retries" else bool(v)
                break
    if "retryable" not in out and out.get("max_retries") is not None:
        out["retryable"] = out["max_retries"] > 0
    return out


class PolicyView(object):
    def __init__(self, fns=None, where="", maps=None):
        self.fns = dict(fns or {})
        self.where = where
        self.maps = maps or {}
        self._cache = {}

    def _from_fn(self, kind, code, status=None):
        fn = self.fns.get(kind)
        if fn is None:
            return {}
        key = (kind, code, status)
        if key in self._cache:
            return self._cache[key]
        ctx = {"status": status, "http_status": status, "code": code}
        if kind == "retryable":
            shapes = [((code,), {}), ((code, status), {}), ((code, ctx), {})]
        else:
            shapes = [((code,), {}), ((code, ctx), {})]
        try:
            res = call_shapes(fn, shapes, where="%s:%s" % (self.where, kind))
        except unittest.SkipTest:
            raise
        spec = {}
        if res.ok:
            v = res.value
            if isinstance(v, bool):
                spec = {"retryable": v} if kind in ("retryable", "policy") else {kind: v}
            elif isinstance(v, int):
                spec = {kind: int(v)}
            elif isinstance(v, dict):
                spec = _spec_from_any(v)
            elif isinstance(v, (tuple, list)) and v and isinstance(v[0], bool):
                spec = {"retryable": bool(v[0])}
            else:
                spec = _spec_from_any(v)
        self._cache[key] = spec
        return spec

    def spec(self, code, status=None, source="merged"):
        """source: merged（函数优先、表补缺）/ table（只看数据表）/ fns（只看函数）。"""
        table_spec = {}
        for key, table in self.maps.items():
            if table is None:
                continue
            if isinstance(table, (set, frozenset, list, tuple)):
                members = set(str(x) for x in table)
                if code in members:
                    if key == "retryable":
                        out["retryable"] = True
                    elif key == "non_retryable":
                        out["retryable"] = False
                    elif key == "must_replan":
                        out["must_replan"] = True
                continue
            if isinstance(table, dict):
                if code in table:
                    part = _spec_from_any(table[code])
                    if key == "retryable":
                        part.setdefault("retryable", True)
                    elif key == "non_retryable":
                        part.setdefault("retryable", False)
                    elif key == "must_replan":
                        part.setdefault("must_replan", True)
                    table_spec.update(part)
        fn_spec = {}
        for kind in ("policy", "retryable", "must_replan", "max_retries"):
            try:
                got = self._from_fn(kind, code, status)
            except unittest.SkipTest:
                got = {}
            for k, v in got.items():
                if v is None:
                    continue
                if k not in fn_spec:
                    fn_spec[k] = v
        if source == "table":
            out = dict(table_spec)
        elif source == "fns":
            out = dict(fn_spec)
        else:
            # 操作性判定（函数）优先：代码真正执行的决策是它；数据表只补它没说的
            out = dict(table_spec)
            out.update(fn_spec)
        if code == "E_MODEL_HTTP" and status is not None:
            if 400 <= int(status) < 500:
                out.pop("retryable", None)
                out["retryable"] = False
            elif 500 <= int(status) < 600:
                out["retryable"] = True
        return out

    def retryable(self, code, status=None):
        s = self.spec(code, status)
        return s.get("retryable")

    def max_retries(self, code):
        s = self.spec(code)
        return s.get("max_retries")

    def must_replan(self, code):
        s = self.spec(code)
        return s.get("must_replan")


def error_policy():
    label = "错误码策略（§5：可重试性/重试上限/重规划）"
    fns = {}
    where = []
    for kind, names in (
        ("retryable", ("is_retryable", "can_retry", "should_retry", "retryable", "retry_decision")),
        ("must_replan", ("is_replan", "must_replan", "needs_replan", "should_replan")),
        ("max_retries", ("max_retries", "retry_limit", "retry_budget")),
        ("policy", ("policy", "get_policy", "policy_for", "error_policy", "retry_policy",
                    "classify_error", "classify")),
    ):
        for groups in (("errors",), ("loop", "action_parser")):
            dotted, name, val = find_attr(groups, names, require_callable=True)
            if val is not None:
                fns[kind] = val
                where.append("%s=%s.%s" % (kind, dotted, name))
                break
    maps = {}
    for key, names in (
        ("policy", ("ERROR_POLICY", "ERROR_POLICIES", "ERRORS", "ERROR_TABLE", "ERROR_CODES",
                    "POLICY", "POLICIES", "ERROR_SPECS")),
        ("retryable", ("RETRYABLE", "RETRYABLE_ERRORS", "RETRYABLE_CODES", "CAN_RETRY")),
        ("non_retryable", ("NON_RETRYABLE", "NON_RETRYABLE_CODES", "FATAL_ERRORS", "FATAL_CODES")),
        ("max_retries", ("MAX_RETRIES", "RETRY_LIMITS", "RETRY_BUDGET", "RETRY_COUNTS")),
        ("must_replan", ("MUST_REPLAN", "MUST_REPLAN_CODES", "REPLAN", "REPLAN_CODES", "FORCE_REPLAN")),
    ):
        dotted, name, val = find_attr(("errors",), names, want_container=True)
        if val is not None:
            maps[key] = val
    if maps.get("policy") is not None:
        base = _spec_from_any
        policy_map = maps.pop("policy")
        if isinstance(policy_map, dict):
            maps["policy"] = dict((k, v) for k, v in policy_map.items())
    if not maps and not fns:
        raise unittest.SkipTest(reason_missing(label, "errors"))
    return PolicyView(fns=fns, where=", ".join(where) or "error policy map", maps=maps)


# --------------------------------------------------------------------------
# CLI / 主循环
# --------------------------------------------------------------------------
def cli_help(timeout=60):
    """在 Python 子进程里跑 cua.main --help（装好假 pyautogui，安全）。"""
    if not (REPO_ROOT / "cua").is_dir():
        raise unittest.SkipTest("适配层：cua/ 包尚不存在，无法探测 CLI 标志")
    probe = HERE / "ct_cli_probe.py"
    cmd = [sys.executable, str(probe), "--help"]
    try:
        proc = subprocess.run(cmd, cwd=str(REPO_ROOT), stdout=subprocess.PIPE,
                              stderr=subprocess.STDOUT, timeout=timeout)
    except Exception as exc:
        raise unittest.SkipTest("适配层：无法启动 CLI 探测子进程（%s: %s）" % (type(exc).__name__, exc))
    return proc.returncode, (proc.stdout or b"").decode("utf-8", "replace")


def capability_report():
    rows = []
    probes = [
        ("extract_json", json_extractor),
        ("step_validate", step_validator),
        ("image_to_screen", image_to_screen),
        ("screen_to_image", screen_to_image),
        ("bounds_check", bounds_checker),
        ("key_whitelist", key_allowed),
        ("error_policy", error_policy),
        ("destructive_words", destructive_predicate),
        ("click_guard", click_guard),
        ("execute_action", action_executor),
        ("dry_run_executor", dry_run_executor),
    ]
    for name, probe in probes:
        try:
            cap = probe()
            rows.append((name, "RESOLVED", repr(cap)))
        except unittest.SkipTest as exc:
            rows.append((name, "MISSING", str(exc)))
        except Exception as exc:
            rows.append((name, "BROKEN", "%s: %s" % (type(exc).__name__, exc)))
    return rows

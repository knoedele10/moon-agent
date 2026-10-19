# -*- coding: utf-8 -*-
"""CUA 契约参考实现（verifier 独立编写，只依据冻结文档）。

唯一依据：
  - docs/cua/03-action-protocol.md
  - docs/cua/action.schema.json

刻意不读 cua/ 下的实现代码，以保持验收独立性。三个用途：
  1. 让契约测试套件在实现尚未落地时就已可执行、可证伪（而不是一片空 skip）；
  2. 把文档里的字面规则（§1.1 坐标、§2.1 解析、§3 动作取值范围、§5 错误码、
     §7 破坏性词表、§8 安全不变量）变成可执行判据；
  3. 实现落地后，同一批判据同时跑「参考实现」与「真实实现」，差异即契约漂移证据。

已知契约内部不一致（详见 docs/verify/task-7-cua-contract-tests.md）：
  (a) md §2 表格原写「done=true 时 action 可为 null」；Schema 的 action.oneOf 不接受 null。
      Lead 于 2026-10-11 裁定：以 Schema 为准，action 永远是必填的动作对象，不可为 null；
      final.status/summary 是终态唯一权威来源；若同时给了 action.type=done，
      其 status/summary 必须与 final 一致，不一致记 E_SCHEMA。本参考实现按该裁定执行。
  (b) md §2.1 说解析器对「数值坐标」宽容（接受小数并四舍五入）；Schema 要求 integer。
      本参考实现按 §2.1 的显式宽容接受小数坐标（该宽容只在文档里点明了坐标，不外推到其它数值字段）。
  (c) md 说小数「四舍五入」；Python 内置 round() 是银行家舍入（.5 向偶）。本参考实现取
      文档字面含义：round half up（正数即 .5 进位）。
"""
from __future__ import annotations

import json
import math
import re
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
SCHEMA_PATH = REPO_ROOT / "docs" / "cua" / "action.schema.json"
PROTOCOL_DOC = REPO_ROOT / "docs" / "cua" / "03-action-protocol.md"

PROTOCOL_VERSION = "cua/1"

ACTION_TYPES = (
    "click", "type", "hotkey", "drag", "scroll", "move", "wait", "screenshot", "done",
)

ALLOWED_BUTTONS = ("left", "right", "middle")
ALLOWED_MODIFIERS = ("ctrl", "alt", "shift", "win")
ALLOWED_UNITS = ("line", "page")
ALLOWED_STATUS = ("success", "failed", "need_human")
ALLOWED_RISK = ("low", "medium", "high")


class ContractError(Exception):
    """参考实现抛出的契约错误；code 来自 §5 错误码表。"""

    def __init__(self, code, message=""):
        super().__init__("%s: %s" % (code, message))
        self.code = code
        self.message = message


# --------------------------------------------------------------------------
# §5 错误码表（冻结）：可重试性 / 最大重试次数 / 是否必须重规划
# max_retries 语义 = 「同一动作再来一次」的最大重试次数（不是总尝试次数）。
# --------------------------------------------------------------------------
ERROR_POLICY = {
    "E_BAD_JSON": dict(retryable=True, max_retries=2, must_replan=False),
    "E_SCHEMA": dict(retryable=True, max_retries=2, must_replan=False),
    "E_UNKNOWN_ACTION": dict(retryable=False, max_retries=0, must_replan=False),
    "E_ARG_RANGE": dict(retryable=True, max_retries=1, must_replan=False),
    "E_OUT_OF_BOUNDS": dict(retryable=True, max_retries=1, must_replan=False),
    "E_KEY_NOT_ALLOWED": dict(retryable=True, max_retries=1, must_replan=False),
    "E_NO_FOCUS": dict(retryable=True, max_retries=1, must_replan=False),
    "E_MODEL_TIMEOUT": dict(retryable=True, max_retries=2, must_replan=False),
    "E_MODEL_HTTP": dict(retryable=None, max_retries=None, must_replan=False,
                         retryable_4xx=False, retryable_5xx=True),
    "E_MODEL_REFUSAL": dict(retryable=False, max_retries=0, must_replan=False),
    "E_EXECUTOR_FAILSAFE": dict(retryable=False, max_retries=0, must_replan=False),
    "E_EXPECT_TIMEOUT": dict(retryable=True, max_retries=1, must_replan=False),
    "E_POPUP_BLOCKING": dict(retryable=True, max_retries=2, must_replan=False),
    "E_STALL": dict(retryable=False, max_retries=0, must_replan=True),
    "E_LOOP": dict(retryable=False, max_retries=0, must_replan=True),
    "E_STEP_BUDGET": dict(retryable=False, max_retries=0, must_replan=False),
    "E_WALL_BUDGET": dict(retryable=False, max_retries=0, must_replan=False),
    "E_CANCEL": dict(retryable=False, max_retries=0, must_replan=False),
    "E_NEED_HUMAN": dict(retryable=False, max_retries=0, must_replan=False),
    "E_INTERNAL": dict(retryable=False, max_retries=0, must_replan=False),
}
ALL_CODES = tuple(ERROR_POLICY.keys())
MUST_REPLAN_CODES = frozenset(c for c, p in ERROR_POLICY.items() if p["must_replan"])
RETRYABLE_CODES = frozenset(c for c, p in ERROR_POLICY.items() if p["retryable"] is True)
NON_RETRYABLE_CODES = frozenset(c for c, p in ERROR_POLICY.items() if p["retryable"] is False)
MAX_RETRIES = dict((c, p["max_retries"]) for c, p in ERROR_POLICY.items() if p["max_retries"] is not None)


# --------------------------------------------------------------------------
# §3.3 键名白名单
# --------------------------------------------------------------------------
def _build_key_whitelist():
    keys = set()
    for ch in "abcdefghijklmnopqrstuvwxyz":
        keys.add(ch)
    for d in "0123456789":
        keys.add(d)
    for i in range(1, 13):
        keys.add("F%d" % i)
    for k in ("ctrl", "alt", "shift", "win", "cmd"):
        keys.add(k)
    for k in ("enter", "tab", "esc", "space", "backspace", "delete", "up", "down",
              "left", "right", "home", "end", "pageup", "pagedown", "insert"):
        keys.add(k)
    return frozenset(keys)


KEY_WHITELIST = _build_key_whitelist()


def is_allowed_key(key):
    return isinstance(key, str) and key in KEY_WHITELIST


# --------------------------------------------------------------------------
# §7 破坏性词表（命中即转人工，不允许点击）
# --------------------------------------------------------------------------
DESTRUCTIVE_WORDS_ZH = (
    "删除", "卸载", "格式化", "清空", "重置", "覆盖", "支付", "付款",
    "确认购买", "允许", "授权", "安装", "同意",
)
DESTRUCTIVE_WORDS_EN = (
    "Delete", "Uninstall", "Format", "Erase", "Reset", "Overwrite",
    "Pay", "Purchase", "Allow", "Grant", "Install", "Agree",
)
DESTRUCTIVE_WORDS = DESTRUCTIVE_WORDS_ZH + DESTRUCTIVE_WORDS_EN


def is_destructive(text):
    """中英文任一命中即真；英文大小写不敏感；用子串匹配（文档要求「命中即转人工」，取保守侧）。"""
    if not isinstance(text, str):
        return False
    for w in DESTRUCTIVE_WORDS_ZH:
        if w in text:
            return True
    low = text.lower()
    for w in DESTRUCTIVE_WORDS_EN:
        if w.lower() in low:
            return True
    return False


def destructive_hits(text):
    hits = []
    if not isinstance(text, str):
        return hits
    for w in DESTRUCTIVE_WORDS_ZH:
        if w in text:
            hits.append(w)
    low = text.lower()
    for w in DESTRUCTIVE_WORDS_EN:
        if w.lower() in low:
            hits.append(w)
    return hits


# --------------------------------------------------------------------------
# §2.1 JSON 提取（宽进严出）
# --------------------------------------------------------------------------
FENCE = chr(96) * 3
_FENCE_RE = re.compile(re.escape(FENCE) + r"[ \t]*([A-Za-z0-9_+-]*)[ \t]*\r?\n?(.*?)" + re.escape(FENCE), re.S)


def strip_code_fence(text):
    """剥离第一个 Markdown 代码围栏，返回围栏内容；无围栏返回 None。"""
    if not isinstance(text, str):
        return None
    m = _FENCE_RE.search(text)
    if m:
        return m.group(2)
    return None


def balanced_object(text):
    """从第一个左花括号扫到与之配对的右花括号，正确跳过字符串内花括号与转义。找不到返回 None。"""
    if not isinstance(text, str):
        return None
    start = text.find("{")
    if start < 0:
        return None
    depth = 0
    in_str = False
    esc = False
    for i in range(start, len(text)):
        ch = text[i]
        if in_str:
            if esc:
                esc = False
            elif ch == "\\":
                esc = True
            elif ch == '"':
                in_str = False
            continue
        if ch == '"':
            in_str = True
        elif ch == "{":
            depth += 1
        elif ch == "}":
            depth -= 1
            if depth == 0:
                return text[start:i + 1]
    return None


def extract_json(text):
    """按 §2.1 的顺序提取 JSON；全部失败抛 E_BAD_JSON。"""
    if not isinstance(text, str):
        raise ContractError("E_BAD_JSON", "输入不是字符串")
    fenced = strip_code_fence(text)
    if fenced is not None:
        try:
            return json.loads(fenced)
        except Exception:
            pass
    try:
        return json.loads(text)
    except Exception:
        pass
    for cand in (fenced, text):
        if not cand:
            continue
        chunk = balanced_object(cand)
        if chunk is None:
            continue
        try:
            return json.loads(chunk)
        except Exception:
            continue
    raise ContractError("E_BAD_JSON", "无法从模型输出中提取 JSON 对象")


# --------------------------------------------------------------------------
# §1.1 坐标换算
# --------------------------------------------------------------------------
def round_half_up(value):
    """文档字面含义的「四舍五入」（与 Python 内置 round 的银行家舍入不同）。"""
    return int(math.floor(float(value) + 0.5))


def image_to_screen(x, y, origin, scale):
    """screen_x = origin.x + round(x / scale)。origin 为 dict(x=, y=)。"""
    if scale is None or scale <= 0:
        raise ContractError("E_ARG_RANGE", "scale 必须为正数")
    ox = origin["x"] if isinstance(origin, dict) else origin[0]
    oy = origin["y"] if isinstance(origin, dict) else origin[1]
    return (ox + round_half_up(float(x) / scale), oy + round_half_up(float(y) / scale))


def screen_to_image(sx, sy, origin, scale):
    """正向公式的自然逆运算（文档未显式冻结逆公式，此处由 §1.1 推导）。"""
    ox = origin["x"] if isinstance(origin, dict) else origin[0]
    oy = origin["y"] if isinstance(origin, dict) else origin[1]
    return (round_half_up((sx - ox) * scale), round_half_up((sy - oy) * scale))


def check_bounds(x, y, width, height):
    """§1.1/§8.2：超出 image 边界一律拒绝，绝不 clamp。合法区间为 [0, width) x [0, height)。"""
    if not (0 <= x < width) or not (0 <= y < height):
        raise ContractError(
            "E_OUT_OF_BOUNDS",
            "x=%r,y=%r 超出图像 %sx%s" % (x, y, width, height),
        )


# --------------------------------------------------------------------------
# §2 / §3 Schema 校验与错误分类（单故障判据；多故障时优先级文档未规定）
# --------------------------------------------------------------------------
_JSONSCHEMA_VALIDATOR = None
_JSONSCHEMA_ERROR = None


def _get_validator():
    global _JSONSCHEMA_VALIDATOR, _JSONSCHEMA_ERROR
    if _JSONSCHEMA_VALIDATOR is not None or _JSONSCHEMA_ERROR is not None:
        return _JSONSCHEMA_VALIDATOR
    try:
        import jsonschema
        schema = json.loads(SCHEMA_PATH.read_text(encoding="utf-8"))
        _JSONSCHEMA_VALIDATOR = jsonschema.Draft202012Validator(schema)
    except Exception as exc:  # pragma: no cover - 环境缺依赖时
        _JSONSCHEMA_ERROR = exc
    return _JSONSCHEMA_VALIDATOR


def schema_available():
    return _get_validator() is not None


def schema_errors(step):
    v = _get_validator()
    if v is None:
        raise RuntimeError("jsonschema 不可用：%s" % (_JSONSCHEMA_ERROR,))
    return sorted(v.iter_errors(step), key=lambda e: list(e.path))


def schema_valid(step):
    return not schema_errors(step)


def _is_num(v):
    return isinstance(v, (int, float)) and not isinstance(v, bool)


def _in(v, lo, hi):
    return _is_num(v) and lo <= v <= hi


def _int_in(v, lo, hi):
    """非坐标数值字段按 schema 要求必须是整数（§2.1 的小数宽容只给了坐标）。"""
    return isinstance(v, int) and not isinstance(v, bool) and lo <= v <= hi


def _check_modifiers(mods):
    if not isinstance(mods, list) or any(not isinstance(m, str) for m in mods):
        return "E_SCHEMA"
    if len(mods) > 3:
        return "E_ARG_RANGE"
    if any(m not in ALLOWED_MODIFIERS for m in mods):
        return "E_SCHEMA"
    return None


def classify_action(action):
    """返回错误码或 None（合法）。fixture 均为单故障，故判据顺序不影响结论。"""
    if not isinstance(action, dict):
        return "E_SCHEMA"
    atype = action.get("type")
    if not isinstance(atype, str) or atype == "":
        return "E_SCHEMA"
    if atype not in ACTION_TYPES:
        return "E_UNKNOWN_ACTION"

    if atype == "click":
        if not (_is_num(action.get("x")) and _is_num(action.get("y"))):
            return "E_SCHEMA"
        if "button" in action and action["button"] not in ALLOWED_BUTTONS:
            return "E_SCHEMA"
        if "clicks" in action:
            if not _int_in(action["clicks"], 1, 2):
                return "E_ARG_RANGE"
        if "modifiers" in action:
            return _check_modifiers(action["modifiers"])
        return None

    if atype == "type":
        text = action.get("text")
        if not isinstance(text, str):
            return "E_SCHEMA"
        if len(text) > 2000:
            return "E_SCHEMA"
        if "clear_first" in action and not isinstance(action["clear_first"], bool):
            return "E_SCHEMA"
        if "submit" in action and not isinstance(action["submit"], bool):
            return "E_SCHEMA"
        if "interval_ms" in action and not _int_in(action["interval_ms"], 0, 500):
            return "E_ARG_RANGE"
        if "target" in action:
            t = action["target"]
            if t is not None and not isinstance(t, (str, dict)):
                return "E_SCHEMA"
        return None

    if atype == "hotkey":
        keys = action.get("keys")
        if not isinstance(keys, list) or any(not isinstance(k, str) for k in keys):
            return "E_SCHEMA"
        if len(keys) < 1 or len(keys) > 3:
            return "E_ARG_RANGE"
        for k in keys:
            if not is_allowed_key(k):
                return "E_KEY_NOT_ALLOWED"
        if "repeat" in action and not _int_in(action["repeat"], 1, 20):
            return "E_ARG_RANGE"
        return None

    if atype == "drag":
        for side in ("from", "to"):
            p = action.get(side)
            if not isinstance(p, dict) or not (_is_num(p.get("x")) and _is_num(p.get("y"))):
                return "E_SCHEMA"
        if "duration_ms" in action and not _int_in(action["duration_ms"], 100, 5000):
            return "E_ARG_RANGE"
        if "button" in action and action["button"] not in ALLOWED_BUTTONS:
            return "E_SCHEMA"
        if "modifiers" in action:
            return _check_modifiers(action["modifiers"])
        return None

    if atype == "scroll":
        if not (_is_num(action.get("x")) and _is_num(action.get("y"))):
            return "E_SCHEMA"
        for axis in ("dx", "dy"):
            if axis in action and not _int_in(action[axis], -20, 20):
                return "E_ARG_RANGE"
        if "unit" in action and action["unit"] not in ALLOWED_UNITS:
            return "E_SCHEMA"
        return None

    if atype == "move":
        if not (_is_num(action.get("x")) and _is_num(action.get("y"))):
            return "E_SCHEMA"
        if "duration_ms" in action and not _int_in(action["duration_ms"], 0, 3000):
            return "E_ARG_RANGE"
        return None

    if atype == "wait":
        if "ms" not in action:
            return "E_SCHEMA"
        if not _int_in(action["ms"], 100, 10000):
            return "E_ARG_RANGE"
        if "reason" in action and not isinstance(action["reason"], str):
            return "E_SCHEMA"
        return None

    if atype == "screenshot":
        region = action.get("region")
        if region is None:
            return None
        if not isinstance(region, dict):
            return "E_SCHEMA"
        for f in ("x", "y", "width", "height"):
            if f not in region or not _is_num(region[f]):
                return "E_SCHEMA"
        for f in ("width", "height"):
            if not _int_in(region[f], 16, 1 << 30):
                return "E_ARG_RANGE"
        return None

    if atype == "done":
        if action.get("status") not in ALLOWED_STATUS:
            return "E_SCHEMA"
        if not isinstance(action.get("summary"), str):
            return "E_SCHEMA"
        if "artifacts" in action:
            arts = action["artifacts"]
            if not isinstance(arts, list) or any(not isinstance(a, str) for a in arts):
                return "E_SCHEMA"
        return None

    return "E_UNKNOWN_ACTION"


def classify_step(step):
    """整步校验：返回错误码或 None。单故障 fixture 下判据顺序无关。"""
    if not isinstance(step, dict):
        return "E_SCHEMA"
    if step.get("protocol") != PROTOCOL_VERSION:
        return "E_SCHEMA"
    thought = step.get("thought")
    if not isinstance(thought, str) or thought == "" or len(thought) > 1200:
        return "E_SCHEMA"
    done = step.get("done")
    if not isinstance(done, bool):
        return "E_SCHEMA"
    if "risk" in step and step["risk"] not in ALLOWED_RISK:
        return "E_SCHEMA"

    action = step.get("action")
    if action is None:
        # 契约冲突点：md §2 说 done=true 时 action 可为 null；Schema 的 oneOf 不接受 null。
        # 此处按 §9「以 Schema 为准」判 E_SCHEMA；测试把该案例放进「冲突」分组。
        return "E_SCHEMA"
    code = classify_action(action)
    if code:
        return code

    if done:
        final = step.get("final")
        if not isinstance(final, dict):
            return "E_SCHEMA"
        if final.get("status") not in ALLOWED_STATUS:
            return "E_SCHEMA"
        if not isinstance(final.get("summary"), str):
            return "E_SCHEMA"
        ans = final.get("answer")
        if ans is not None and not isinstance(ans, str):
            return "E_SCHEMA"
        # Lead 裁定（2026-10-11）：final.status/summary 是终态唯一权威来源；
        # 若同时给了 action.type=done，其 status/summary 必须与 final 一致。
        act = step.get("action")
        if isinstance(act, dict) and act.get("type") == "done":
            if act.get("status") != final.get("status") or act.get("summary") != final.get("summary"):
                return "E_SCHEMA"

    expect = step.get("expect")
    if expect is not None:
        if not isinstance(expect, dict):
            return "E_SCHEMA"
        if "what" in expect and not isinstance(expect["what"], str):
            return "E_SCHEMA"
        if "must_change" in expect and not isinstance(expect["must_change"], bool):
            return "E_SCHEMA"
        if "timeout_ms" in expect and not _int_in(expect["timeout_ms"], 0, 30000):
            return "E_SCHEMA"
    return None


def step_ok(step):
    return classify_step(step) is None

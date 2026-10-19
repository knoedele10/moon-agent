"""Step 解析与 Schema 校验（协议第 2 节，宽进严出）。

- 宽进：按协议 2.1 的顺序提取 JSON：全文直解 -> 剥代码围栏 -> 第一个平衡对象扫描；
- 严出：按 docs/cua/action.schema.json 校验（自研校验子集，不引入新依赖）；
- 宽容点：数值坐标接受整数或小数（小数四舍五入并在 meta 记 coord_rounded）；
  未知字段忽略；未知 action.type 一定报 E_UNKNOWN_ACTION。

错误码映射（协议第 5 节）：
  - 提取失败/非 JSON        -> E_BAD_JSON
  - 结构不符（缺字段/类型错/枚举错/长度错） -> E_SCHEMA
  - action 内数值区间越界（schema 的 minimum/maximum） -> E_ARG_RANGE
  - action.type 未注册      -> E_UNKNOWN_ACTION
"""

from __future__ import annotations

import json
import os
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Dict, List, Optional, Sequence, Tuple

from .config import ACTION_TYPES, Config, PROTOCOL_VERSION
from .errors import (
    E_ARG_RANGE,
    E_BAD_JSON,
    E_INTERNAL,
    E_SCHEMA,
    E_UNKNOWN_ACTION,
    CuaError,
)
from .screenshot import round_half_up

_FENCE = chr(96) * 3  # 三个反引号，避免在源码里直接写出来
_FENCE_RE = re.compile(_FENCE + r"[ \t]*(?:json|JSON)?[ \t]*\r?\n?(.*?)" + _FENCE, re.S)

#: 允许小数坐标的字段（协议 2.1 宽容点）。
_POINT_KEYS = ("from", "to")
_AXIS_KEYS = ("x", "y")
_RECT_KEYS = ("x", "y", "width", "height")


# ---------------------------------------------------------------------------
# 1. 宽进提取
# ---------------------------------------------------------------------------


@dataclass
class ExtractResult:
    value: Any
    method: str          # direct / fence / balanced
    raw: str


def _try_load(text: str) -> Optional[Any]:
    try:
        return json.loads(text)
    except Exception:
        return None


def balanced_object_scan(text: str) -> Optional[str]:
    """从第一个 { 开始扫到与之配对的 }；正确跳过字符串内的花括号与转义。"""
    start = text.find("{")
    if start < 0:
        return None
    depth = 0
    in_string = False
    escaped = False
    for i in range(start, len(text)):
        ch = text[i]
        if in_string:
            if escaped:
                escaped = False
            elif ch == "\\":
                escaped = True
            elif ch == '"':
                in_string = False
            continue
        if ch == '"':
            in_string = True
        elif ch == "{":
            depth += 1
        elif ch == "}":
            depth -= 1
            if depth == 0:
                return text[start:i + 1]
    return None


def extract_json(text: str) -> ExtractResult:
    """按协议 2.1 三步提取，全部失败抛 E_BAD_JSON。"""
    if text is None:
        raise CuaError(E_BAD_JSON, "模型输出为空", hint="请只输出一个 JSON 对象")
    stripped = text.strip()
    if not stripped:
        raise CuaError(E_BAD_JSON, "模型输出为空", hint="请只输出一个 JSON 对象")

    value = _try_load(stripped)
    if value is not None:
        return ExtractResult(value, "direct", stripped)

    fence = _FENCE_RE.search(stripped)
    if fence:
        inner = fence.group(1).strip()
        value = _try_load(inner)
        if value is not None:
            return ExtractResult(value, "fence", inner)

    balanced = balanced_object_scan(stripped)
    if balanced is not None:
        value = _try_load(balanced)
        if value is not None:
            return ExtractResult(value, "balanced", balanced)

    raise CuaError(
        E_BAD_JSON,
        "模型输出无法解析成 JSON（已尝试：全文直解 / 剥代码围栏 / 平衡括号扫描）",
        hint="请只输出一个 JSON 对象，不要包含解释文字或多余代码块",
        context={"head": stripped[:200]},
    )


# ---------------------------------------------------------------------------
# 2. 自研 Schema 校验子集
# ---------------------------------------------------------------------------


def default_schema_path() -> Path:
    env = os.environ.get("CUA_SCHEMA_PATH")
    if env:
        return Path(env)
    return Path(__file__).resolve().parent.parent / "docs" / "cua" / "action.schema.json"


_SCHEMA_CACHE: Dict[str, Any] = {}


def load_schema(path: Optional[Path] = None) -> Dict[str, Any]:
    """读取冻结的 action.schema.json（带缓存）。"""
    p = Path(path) if path else default_schema_path()
    key = str(p)
    if key not in _SCHEMA_CACHE:
        try:
            with open(p, "r", encoding="utf-8") as fh:
                _SCHEMA_CACHE[key] = json.load(fh)
        except Exception as exc:
            raise CuaError(
                E_INTERNAL,
                "无法读取 action.schema.json：%s" % exc,
                hint="契约文件位于 docs/cua/action.schema.json，可用 CUA_SCHEMA_PATH 覆盖",
                cause=exc,
            )
    return _SCHEMA_CACHE[key]


@dataclass
class ValidationIssue:
    path: str
    keyword: str
    message: str

    def __str__(self) -> str:
        return "%s [%s] %s" % (self.path, self.keyword, self.message)


def _json_type(value: Any) -> str:
    if value is None:
        return "null"
    if isinstance(value, bool):
        return "boolean"
    if isinstance(value, int):
        return "integer"
    if isinstance(value, float):
        return "number"
    if isinstance(value, str):
        return "string"
    if isinstance(value, list):
        return "array"
    if isinstance(value, dict):
        return "object"
    return "unknown"


def _type_matches(value: Any, expected: str) -> bool:
    actual = _json_type(value)
    if expected == "number":
        return actual in ("integer", "number")
    if expected == "integer":
        return actual == "integer"
    return actual == expected


class MiniValidator:
    """JSON Schema draft 2020-12 的所需子集。

    支持的keyword：$ref(#/$defs/...)、type、required、properties、additionalProperties、
    items、minItems、maxItems、minLength、maxLength、minimum、maximum、enum、const、
    oneOf、anyOf、allOf、if/then/else。
    未知 keyword 一律忽略（与 additionalProperties 忽略未知字段的向前兼容策略一致）。
    """

    def __init__(self, schema: Dict[str, Any]) -> None:
        self.root = schema

    # -- 公共入口 ---------------------------------------------------------
    def validate(self, instance: Any) -> List[ValidationIssue]:
        issues: List[ValidationIssue] = []
        self._validate(instance, self.root, "$", issues)
        return issues

    def matches(self, instance: Any, schema: Dict[str, Any]) -> bool:
        """仅判断是否通过（结果丢弃，但必须真正收集错误——否则任何分支都会「通过」）。"""
        issues: List[ValidationIssue] = []
        self._validate(instance, schema, "$", issues, collect=True)
        return not issues

    # -- 递归 -------------------------------------------------------------
    def _resolve(self, schema: Dict[str, Any]) -> Dict[str, Any]:
        seen = 0
        while isinstance(schema, dict) and "$ref" in schema:
            ref = schema["$ref"]
            if not isinstance(ref, str) or not ref.startswith("#/"):
                return schema
            node: Any = self.root
            for part in ref[2:].split("/"):
                node = node.get(part, {}) if isinstance(node, dict) else {}
            schema = node
            seen += 1
            if seen > 16:
                break
        return schema if isinstance(schema, dict) else {}

    def _validate(
        self,
        instance: Any,
        schema: Any,
        path: str,
        issues: List[ValidationIssue],
        collect: bool = True,
    ) -> None:
        if not isinstance(schema, dict):
            return
        schema = self._resolve(schema)

        def add(keyword: str, message: str) -> None:
            if collect:
                issues.append(ValidationIssue(path, keyword, message))

        # const / enum
        if "const" in schema and instance != schema["const"]:
            add("const", "必须等于 %r，实际为 %r" % (schema["const"], instance))
        if "enum" in schema and instance not in schema["enum"]:
            add("enum", "必须是 %s 之一，实际为 %r" % (schema["enum"], instance))

        # type
        expected = schema.get("type")
        if expected is not None:
            types = expected if isinstance(expected, list) else [expected]
            if not any(_type_matches(instance, t) for t in types):
                add("type", "类型必须是 %s，实际为 %s" % (types, _json_type(instance)))
                return

        # 数值
        if isinstance(instance, (int, float)) and not isinstance(instance, bool):
            if "minimum" in schema and instance < schema["minimum"]:
                add("minimum", "必须 >= %s，实际为 %s" % (schema["minimum"], instance))
            if "maximum" in schema and instance > schema["maximum"]:
                add("maximum", "必须 <= %s，实际为 %s" % (schema["maximum"], instance))
            if "exclusiveMinimum" in schema and instance <= schema["exclusiveMinimum"]:
                add("exclusiveMinimum", "必须 > %s，实际为 %s" % (schema["exclusiveMinimum"], instance))
            if "exclusiveMaximum" in schema and instance >= schema["exclusiveMaximum"]:
                add("exclusiveMaximum", "必须 < %s，实际为 %s" % (schema["exclusiveMaximum"], instance))

        # 字符串
        if isinstance(instance, str):
            if "minLength" in schema and len(instance) < schema["minLength"]:
                add("minLength", "长度必须 >= %s，实际 %d" % (schema["minLength"], len(instance)))
            if "maxLength" in schema and len(instance) > schema["maxLength"]:
                add("maxLength", "长度必须 <= %s，实际 %d" % (schema["maxLength"], len(instance)))

        # 对象
        if isinstance(instance, dict):
            for key in schema.get("required", []) or []:
                if key not in instance:
                    add("required", "缺少必填字段 %r" % key)
            props = schema.get("properties") or {}
            for key, subschema in props.items():
                if key in instance:
                    self._validate(instance[key], subschema, "%s.%s" % (path, key), issues, collect)
            ap = schema.get("additionalProperties")
            if ap is False:
                for key in instance:
                    if key not in props:
                        add("additionalProperties", "不允许出现未知字段 %r" % key)

        # 数组
        if isinstance(instance, list):
            if "minItems" in schema and len(instance) < schema["minItems"]:
                add("minItems", "元素个数必须 >= %s，实际 %d" % (schema["minItems"], len(instance)))
            if "maxItems" in schema and len(instance) > schema["maxItems"]:
                add("maxItems", "元素个数必须 <= %s，实际 %d" % (schema["maxItems"], len(instance)))
            items = schema.get("items")
            if isinstance(items, dict):
                for idx, item in enumerate(instance):
                    self._validate(item, items, "%s[%d]" % (path, idx), issues, collect)

        # oneOf / anyOf / allOf
        if "oneOf" in schema:
            self._validate_one_of(instance, schema["oneOf"], path, issues, collect)
        if "anyOf" in schema:
            matched = [sub for sub in schema["anyOf"] if self.matches(instance, sub)]
            if not matched:
                best = self._pick_most_specific(instance, schema["anyOf"])
                sub_issues: List[ValidationIssue] = []
                if best is not None:
                    self._validate(instance, best, path, sub_issues, collect=True)
                if sub_issues:
                    if collect:
                        issues.extend(sub_issues)
                else:
                    add("anyOf", "不满足任何一个允许的形式")
        if "allOf" in schema:
            for sub in schema["allOf"]:
                self._validate(instance, sub, path, issues, collect)

        # if / then / else
        if "if" in schema:
            if self.matches(instance, schema["if"]):
                if "then" in schema:
                    self._validate(instance, schema["then"], path, issues, collect)
            elif "else" in schema:
                self._validate(instance, schema["else"], path, issues, collect)

    def _validate_one_of(
        self,
        instance: Any,
        branches: Sequence[Dict[str, Any]],
        path: str,
        issues: List[ValidationIssue],
        collect: bool,
    ) -> None:
        matched = [sub for sub in branches if self.matches(instance, sub)]
        if len(matched) == 1:
            return
        if len(matched) > 1:
            if collect:
                issues.append(ValidationIssue(path, "oneOf", "同时匹配 %d 个分支，必须恰好匹配 1 个" % len(matched)))
            return
        # 全不匹配：优先回喂「type 对得上但字段不合格」的那个分支，报错更精确
        if collect:
            chosen = self._pick_branch_by_type(instance, branches)
            if chosen is not None:
                sub_issues: List[ValidationIssue] = []
                self._validate(instance, chosen, path, sub_issues, collect=True)
                if sub_issues:
                    issues.extend(sub_issues)
                    return
            issues.append(ValidationIssue(path, "oneOf", "不匹配任何一种已注册的动作形式"))

    @staticmethod
    def _branch_type_matches(branch: Dict[str, Any], json_type: str) -> bool:
        expected = branch.get("type")
        if expected is None:
            return True
        types = expected if isinstance(expected, list) else [expected]
        if json_type == "integer" and "number" in types:
            return True
        return json_type in types

    def _pick_most_specific(self, instance: Any, branches: Sequence[Dict[str, Any]]) -> Optional[Dict[str, Any]]:
        """anyOf 失败时挑一个最贴合的分支，把它的具体错误回喂给模型。"""
        jt = _json_type(instance)
        pool = [b for b in branches if self._branch_type_matches(b, jt)] or list(branches)
        best: Optional[Dict[str, Any]] = None
        best_count: Optional[int] = None
        for branch in pool:
            sub: List[ValidationIssue] = []
            self._validate(instance, branch, "$", sub, collect=True)
            count = len(sub)
            if best_count is None or count < best_count:
                best, best_count = branch, count
        return best

    @staticmethod
    def _pick_branch_by_type(instance: Any, branches: Sequence[Dict[str, Any]]) -> Optional[Dict[str, Any]]:
        if not isinstance(instance, dict):
            return None
        want = instance.get("type")
        for sub in branches:
            const = ((sub.get("properties") or {}).get("type") or {}).get("const")
            if const is not None and const == want:
                return sub
        return None


def validate_with_jsonschema(instance: Any, schema: Optional[Dict[str, Any]] = None) -> Optional[List[str]]:
    """若环境里装了 jsonschema，则用它交叉校验；没装返回 None（不引入新依赖）。"""
    try:
        import jsonschema  # noqa: WPS433
    except Exception:
        return None
    sch = schema or load_schema()
    try:
        validator = jsonschema.Draft202012Validator(sch)
        return ["%s: %s" % ("/".join(str(p) for p in err.absolute_path), err.message)
                for err in validator.iter_errors(instance)]
    except Exception as exc:  # pragma: no cover
        return ["jsonschema 调用失败: %s" % exc]


# ---------------------------------------------------------------------------
# 3. 宽容归一化 + Step 解析
# ---------------------------------------------------------------------------


@dataclass
class ParsedStep:
    raw_text: str
    step: Dict[str, Any]
    meta: Dict[str, Any] = field(default_factory=dict)

    @property
    def action(self) -> Optional[Dict[str, Any]]:
        return self.step.get("action")

    @property
    def done(self) -> bool:
        return bool(self.step.get("done"))

    @property
    def final(self) -> Optional[Dict[str, Any]]:
        return self.step.get("final")

    @property
    def risk(self) -> str:
        return str(self.step.get("risk") or "low")

    @property
    def expect(self) -> Optional[Dict[str, Any]]:
        return self.step.get("expect")

    def to_dict(self) -> Dict[str, Any]:
        return dict(self.step)


def _normalize_point(obj: Dict[str, Any], rounded: List[str]) -> None:
    for key in _AXIS_KEYS:
        if key in obj and isinstance(obj[key], float):
            obj[key] = round_half_up(obj[key])
            rounded.append(key)


def normalize_coordinates(action: Optional[Dict[str, Any]]) -> Tuple[Optional[Dict[str, Any]], List[str]]:
    """协议 2.1 宽容点：数值坐标接受整数或小数，小数四舍五入。返回被取整的字段路径。"""
    rounded: List[str] = []
    if not isinstance(action, dict):
        return action, rounded
    if "x" in action or "y" in action:
        _normalize_point(action, rounded)
    for key in _POINT_KEYS:
        node = action.get(key)
        if isinstance(node, dict):
            before = len(rounded)
            _normalize_point(node, rounded)
            if len(rounded) > before:
                rounded[-1] = "%s.%s" % (key, rounded[-1])
    region = action.get("region")
    if isinstance(region, dict):
        for key in _RECT_KEYS:
            if key in region and isinstance(region[key], float):
                region[key] = round_half_up(region[key])
                rounded.append("region.%s" % key)
    return action, rounded


def ignored_top_level_fields(step: Dict[str, Any], schema: Optional[Dict[str, Any]] = None) -> List[str]:
    sch = schema or load_schema()
    known = set((sch.get("properties") or {}).keys())
    return sorted(k for k in step.keys() if k not in known)


def _issues_to_error(step: Dict[str, Any], issues: Sequence[ValidationIssue]) -> CuaError:
    """把校验问题映射成协议错误码：action 内数值区间 -> E_ARG_RANGE，其余 -> E_SCHEMA。"""
    arg_issues = [
        i for i in issues
        if i.keyword in ("minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum")
        and (i.path.startswith("$.action") or i.path.startswith("$['action']"))
    ]
    detail = "; ".join(str(i) for i in issues[:6])
    if arg_issues and len(arg_issues) == len(issues):
        return CuaError(
            E_ARG_RANGE,
            "动作参数越界：%s" % "; ".join(str(i) for i in arg_issues),
            hint="请按 action.schema.json 给出的区间重新取值",
            context={"issues": [str(i) for i in issues], "action": step.get("action")},
        )
    return CuaError(
        E_SCHEMA,
        "Step 不符合 cua/1 协议：%s" % detail,
        hint="请只输出协议规定的字段与取值，参考 action.schema.json",
        context={"issues": [str(i) for i in issues]},
    )


def validate_step_dict(
    step: Any,
    cfg: Optional[Config] = None,
    schema: Optional[Dict[str, Any]] = None,
) -> Dict[str, Any]:
    """校验（并做宽容归一化）一个已解析的 Step 字典；返回归一化后的字典。"""
    cfg = cfg or Config()
    sch = schema or load_schema()
    if not isinstance(step, dict):
        raise CuaError(E_SCHEMA, "Step 顶层必须是 JSON 对象，实际为 %s" % _json_type(step),
                       hint="请输出形如 {protocol, thought, action, done} 的对象")

    # 未知 action.type 一定报错（协议 2.1 / 第 9 节）
    action = step.get("action")
    if isinstance(action, dict) and "type" in action and action["type"] not in ACTION_TYPES:
        raise CuaError(
            E_UNKNOWN_ACTION,
            "未注册的动作类型：%r" % action.get("type"),
            hint="支持的动作类型清单：%s" % ", ".join(ACTION_TYPES),
            context={"type": action.get("type")},
        )

    action, rounded = normalize_coordinates(step.get("action"))
    step = dict(step)
    step["action"] = action

    issues = MiniValidator(sch).validate(step)
    if issues:
        raise _issues_to_error(step, issues)

    # 2026-10-11 裁定（契约第 2 节「终态字段的权威性」）：
    # action 恒为必填的动作对象，不可为 null；final.status/summary 是终态唯一权威来源；
    # 若同时给了 action.type="done"，其 status/summary 必须与 final 完全一致。
    action = step.get("action")
    final = step.get("final")
    if isinstance(action, dict) and action.get("type") == "done" and isinstance(final, dict):
        mismatched = []
        for field_name in ("status", "summary"):
            if action.get(field_name) != final.get(field_name):
                mismatched.append(field_name)
        if mismatched:
            raise CuaError(
                E_SCHEMA,
                "action.type=done 的 %s 与顶层 final 不一致" % "/".join(mismatched),
                hint="final.status/final.summary 是终态唯一权威来源，action 里的同名值必须与之一致",
                context={"action": action, "final": final},
            )
    return step


def parse_step(
    text: str,
    cfg: Optional[Config] = None,
    schema: Optional[Dict[str, Any]] = None,
) -> ParsedStep:
    """模型原文 -> ParsedStep（提取 + 校验 + 宽容归一化）。"""
    cfg = cfg or Config()
    extracted = extract_json(text)
    raw_value = extracted.value
    raw_action = raw_value.get("action") if isinstance(raw_value, dict) else None
    coord_rounded = _has_float_coord(raw_action)
    step = validate_step_dict(raw_value, cfg=cfg, schema=schema)
    meta: Dict[str, Any] = {
        "extraction": extracted.method,
        "coord_rounded": coord_rounded,
        "ignored_fields": ignored_top_level_fields(step, schema),
        "protocol": PROTOCOL_VERSION,
    }
    return ParsedStep(raw_text=text, step=step, meta=meta)


def _has_float_coord(action: Any) -> bool:
    if not isinstance(action, dict):
        return False
    if any(isinstance(action.get(k), float) for k in _AXIS_KEYS):
        return True
    for key in _POINT_KEYS:
        node = action.get(key)
        if isinstance(node, dict) and any(isinstance(node.get(k), float) for k in _AXIS_KEYS):
            return True
    region = action.get("region")
    if isinstance(region, dict) and any(isinstance(region.get(k), float) for k in _RECT_KEYS):
        return True
    return False


def step_fingerprint(step: Dict[str, Any]) -> str:
    """用于 loop 检测的动作指纹（协议第 5 节 E_LOOP：同一（动作，画面哈希）重复）。"""
    action = step.get("action")
    return json.dumps(action, sort_keys=True, ensure_ascii=False)

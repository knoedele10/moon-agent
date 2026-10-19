"""CUA 错误码、重试与重规划策略。

严格对应 docs/cua/03-action-protocol.md 第 5 节「错误码表（冻结）」。
设计要点（协议第 5 节末段）：
  - 可重试（retryable=True）＝ 同一动作再来一次；
  - E_STALL / E_LOOP ＝ 语义上认定这条路走不通，必须换策略（replan），
    绝不能被当成超时去反复重试。
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Dict, Optional

# ---------------------------------------------------------------------------
# 错误码常量（第 5 节 21 条，穷举）
# 2026-10-11 契约 §3.1：新增 E_MODEL_EMPTY（空 content 不是合法回复，不送解析器）
# ---------------------------------------------------------------------------

E_BAD_JSON = "E_BAD_JSON"
E_SCHEMA = "E_SCHEMA"
E_UNKNOWN_ACTION = "E_UNKNOWN_ACTION"
E_ARG_RANGE = "E_ARG_RANGE"
E_OUT_OF_BOUNDS = "E_OUT_OF_BOUNDS"
E_KEY_NOT_ALLOWED = "E_KEY_NOT_ALLOWED"
E_NO_FOCUS = "E_NO_FOCUS"
E_MODEL_TIMEOUT = "E_MODEL_TIMEOUT"
E_MODEL_HTTP = "E_MODEL_HTTP"
E_MODEL_REFUSAL = "E_MODEL_REFUSAL"
E_MODEL_EMPTY = "E_MODEL_EMPTY"
E_EXECUTOR_FAILSAFE = "E_EXECUTOR_FAILSAFE"
E_EXPECT_TIMEOUT = "E_EXPECT_TIMEOUT"
E_POPUP_BLOCKING = "E_POPUP_BLOCKING"
E_STALL = "E_STALL"
E_LOOP = "E_LOOP"
E_STEP_BUDGET = "E_STEP_BUDGET"
E_WALL_BUDGET = "E_WALL_BUDGET"
E_CANCEL = "E_CANCEL"
E_NEED_HUMAN = "E_NEED_HUMAN"
E_INTERNAL = "E_INTERNAL"

#: 策略分类
CAT_RETRY = "retry"      # 同一动作重试
CAT_REPLAN = "replan"    # 必须换策略重规划
CAT_HUMAN = "human"      # 交还控制权 / 转人工
CAT_STOP = "stop"        # 终止
CAT_BUDGET = "budget"    # 预算耗尽，优雅退出


@dataclass(frozen=True)
class ErrorPolicy:
    code: str
    retryable: Optional[bool]  # None 表示取决于上下文（目前只有 E_MODEL_HTTP）
    max_retries: int
    category: str
    disposition: str


#: 与第 5 节表格逐行对应。
ERROR_TABLE: Dict[str, ErrorPolicy] = {
    E_BAD_JSON: ErrorPolicy(E_BAD_JSON, True, 2, CAT_RETRY,
                            "回喂解析错误 + 要求「只输出一个 JSON 对象」"),
    E_SCHEMA: ErrorPolicy(E_SCHEMA, True, 2, CAT_RETRY,
                          "回喂 schema 校验失败字段"),
    E_UNKNOWN_ACTION: ErrorPolicy(E_UNKNOWN_ACTION, False, 0, CAT_STOP,
                                  "回喂支持的动作类型清单"),
    E_ARG_RANGE: ErrorPolicy(E_ARG_RANGE, True, 1, CAT_RETRY, "回喂合法区间"),
    E_OUT_OF_BOUNDS: ErrorPolicy(E_OUT_OF_BOUNDS, True, 1, CAT_RETRY,
                                 "回喂当前 image.width/height"),
    E_KEY_NOT_ALLOWED: ErrorPolicy(E_KEY_NOT_ALLOWED, True, 1, CAT_RETRY, "回喂白名单"),
    E_NO_FOCUS: ErrorPolicy(E_NO_FOCUS, True, 1, CAT_RETRY, "先点 target 再输入"),
    E_MODEL_TIMEOUT: ErrorPolicy(E_MODEL_TIMEOUT, True, 2, CAT_RETRY,
                                 "指数退避重试；连续失败 -> E_NEED_HUMAN"),
    E_MODEL_HTTP: ErrorPolicy(E_MODEL_HTTP, None, 2, CAT_RETRY,
                              "4xx 多为 key/额度/模型名问题，重试无意义，直接上报；5xx 退避重试"),
    E_MODEL_REFUSAL: ErrorPolicy(E_MODEL_REFUSAL, False, 0, CAT_HUMAN,
                                 "记 need_human，不换个说法反复试探"),
    E_MODEL_EMPTY: ErrorPolicy(E_MODEL_EMPTY, False, 0, CAT_HUMAN,
                               "空回复不是合法动作；检查 model_max_tokens（云端建议 >= 4096）后重跑"),
    E_EXECUTOR_FAILSAFE: ErrorPolicy(E_EXECUTOR_FAILSAFE, False, 0, CAT_HUMAN,
                                     "立即停手，释放所有按键，交还控制权"),
    E_EXPECT_TIMEOUT: ErrorPolicy(E_EXPECT_TIMEOUT, True, 1, CAT_RETRY,
                                  "再截一帧确认；仍不符预期记 E_STALL"),
    E_POPUP_BLOCKING: ErrorPolicy(E_POPUP_BLOCKING, True, 2, CAT_RETRY, "走第 7 节弹窗策略"),
    E_STALL: ErrorPolicy(E_STALL, False, 0, CAT_REPLAN,
                         "判定卡住，强制重规划（换策略，而不是重试同一动作）"),
    E_LOOP: ErrorPolicy(E_LOOP, False, 0, CAT_REPLAN,
                        "强制中断当前策略，回退到上一个稳定状态"),
    E_STEP_BUDGET: ErrorPolicy(E_STEP_BUDGET, False, 0, CAT_BUDGET, "优雅退出，返回已完成的进度"),
    E_WALL_BUDGET: ErrorPolicy(E_WALL_BUDGET, False, 0, CAT_BUDGET, "优雅退出，返回已完成的进度"),
    E_CANCEL: ErrorPolicy(E_CANCEL, False, 0, CAT_STOP, "释放按键、停手"),
    E_NEED_HUMAN: ErrorPolicy(E_NEED_HUMAN, False, 0, CAT_HUMAN, "暂停并提示用户接管"),
    E_INTERNAL: ErrorPolicy(E_INTERNAL, False, 0, CAT_STOP, "记录完整堆栈，停手"),
}

#: 冻结错误码全集，用于自检断言「第 5 节一条不漏」。
ERROR_CODES = tuple(ERROR_TABLE.keys())

_INTERNAL_POLICY = ERROR_TABLE[E_INTERNAL]


def policy(code: str) -> ErrorPolicy:
    """取错误码策略；未注册的码按 E_INTERNAL 处理（不抛异常，避免二级故障）。"""
    return ERROR_TABLE.get(code, _INTERNAL_POLICY)


def is_retryable(code: str, http_status: Optional[int] = None) -> bool:
    """是否「同一动作再来一次」。E_MODEL_HTTP 取决于状态码：5xx 是，4xx 否。"""
    pol = policy(code)
    if pol.code == E_MODEL_HTTP and http_status is not None:
        return int(http_status) >= 500
    return bool(pol.retryable)


def max_retries(code: str) -> int:
    return policy(code).max_retries


def category(code: str) -> str:
    return policy(code).category


def is_replan(code: str) -> bool:
    """E_STALL / E_LOOP 才要求强制重规划。"""
    return policy(code).category == CAT_REPLAN


def is_human(code: str) -> bool:
    return policy(code).category == CAT_HUMAN


# ---------------------------------------------------------------------------
# 异常
# ---------------------------------------------------------------------------


class CuaError(Exception):
    """带协议错误码的异常。

    to_dict() 的字段严格对齐协议第 4 节失败样例：
    {code, message, retryable, hint}
    """

    def __init__(
        self,
        code: str,
        message: str,
        hint: Optional[str] = None,
        retryable: Optional[bool] = None,
        context: Optional[Dict[str, Any]] = None,
        cause: Optional[BaseException] = None,
    ) -> None:
        super().__init__("%s: %s" % (code, message))
        self.code = code
        self.message = message
        self.hint = hint
        self._retryable = retryable
        self.context: Dict[str, Any] = dict(context or {})
        self.cause = cause

    @property
    def retryable(self) -> bool:
        if self._retryable is not None:
            return bool(self._retryable)
        status = self.context.get("http_status")
        return is_retryable(self.code, status)

    @property
    def policy(self) -> ErrorPolicy:
        return policy(self.code)

    def to_dict(self) -> Dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            "retryable": self.retryable,
            "hint": self.hint,
        }

    def feedback_message(self) -> str:
        """回喂给模型的一行错误说明（第 5 节「默认处置」）。"""
        parts = ["上一次输出被拒绝：%s %s" % (self.code, self.message)]
        if self.hint:
            parts.append("修正建议：%s" % self.hint)
        parts.append("请只输出一个 JSON 对象，字段严格遵守 cua/1 协议。")
        return " ".join(parts)

    def __repr__(self) -> str:  # pragma: no cover - 仅调试
        return "CuaError(%s, %r)" % (self.code, self.message)


def make_error(code: str, message: str, hint: Optional[str] = None, **context: Any) -> CuaError:
    return CuaError(code, message, hint=hint, context=context or None)


def from_exception(exc: BaseException, code: str = E_INTERNAL) -> CuaError:
    if isinstance(exc, CuaError):
        return exc
    return CuaError(
        code,
        "%s: %s" % (type(exc).__name__, exc),
        hint="未预期异常，已记录完整堆栈",
        cause=exc,
    )


# ---------------------------------------------------------------------------
# 重试计数（可重试 vs 必须重规划分开）
# ---------------------------------------------------------------------------


@dataclass
class RetryTracker:
    """按错误码累计重试次数。

    - retryable 的错误：计数未超上限 -> 允许「同一动作再试一次」；
    - replan 的错误（E_STALL/E_LOOP）：永远不进入重试路径，直接返回 replan。
    """

    counts: Dict[str, int] = field(default_factory=dict)

    def record(self, code: str) -> int:
        self.counts[code] = self.counts.get(code, 0) + 1
        return self.counts[code]

    def count(self, code: str) -> int:
        return self.counts.get(code, 0)

    def reset(self, code: Optional[str] = None) -> None:
        if code is None:
            self.counts.clear()
        else:
            self.counts.pop(code, None)

    def decision(self, code: str, http_status: Optional[int] = None) -> str:
        """返回 'retry' 或 'replan' 或 'give_up'。"""
        if is_replan(code):
            return "replan"
        if is_retryable(code, http_status) and self.count(code) < max_retries(code):
            return "retry"
        return "give_up"

    def snapshot(self) -> Dict[str, int]:
        return dict(self.counts)

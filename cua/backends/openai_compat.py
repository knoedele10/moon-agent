"""OpenAI 兼容多模态客户端（可直接指向 Ollama 的 /v1 端点）。

错误映射（协议第 5 节 + 契约 §3.1）：
  - 超时            -> E_MODEL_TIMEOUT（可重试，指数退避由调用方控制）
  - 4xx / 5xx       -> E_MODEL_HTTP（5xx 可重试，4xx 不可）
  - 连接失败        -> E_MODEL_HTTP（retryable=True，服务可能只是没起来）
  - 疑似拒绝执行    -> E_MODEL_REFUSAL（不重试，转 need_human）
  - 空 content      -> E_MODEL_EMPTY（不重试、不送解析器；含代理传来的空 content 标记）

契约 §3.1：deepseek-flash 等 reasoning 模型在 max_tokens 过小时会返回空 content。
空回复必须在客户端层就被拦下，否则会被报成「模型返回结构异常」，把 max_tokens 这个真因掩盖掉。
"""

from __future__ import annotations

from typing import Any, Dict, List, Optional

from ..config import Config
from ..errors import (
    E_INTERNAL,
    E_MODEL_EMPTY,
    E_MODEL_HTTP,
    E_MODEL_REFUSAL,
    E_MODEL_TIMEOUT,
    CuaError,
)
from .base import ModelClient

#: cua-go 模型代理把「上游返回空 content」映射成 HTTP 502，body.error.message 里固定带这个标记
#: （2026-10-11 与 cua-go 对齐的字样；CUA 侧据此映射为 E_MODEL_EMPTY，而不是 E_MODEL_HTTP，
#: 否则会被 5xx 重试逻辑反复重试同一件事）。
EMPTY_CONTENT_MARKER = "E_MODEL_EMPTY"

#: 拒绝话术的启发式（设计如此，仅覆盖最明显的几种）
_REFUSAL_MARKERS = (
    "i can't help",
    "i cannot help",
    "i'm unable to",
    "i am unable to",
    "as an ai",
    "我不能协助",
    "我无法协助",
    "抱歉，我不能",
)


class OpenAIChatClient(ModelClient):
    name = "openai-compat"
    supports_vision = True

    def __init__(self, cfg: Optional[Config] = None) -> None:
        self.cfg = cfg or Config()
        self._client = None
        self.last_latency_ms: Optional[int] = None

    @property
    def client(self):
        if self._client is None:
            try:
                import openai
            except Exception as exc:  # pragma: no cover
                raise CuaError(E_INTERNAL, "未安装 openai SDK：%s" % exc,
                               hint="pip install openai，或使用 --mock 离线模式")
            self._client = openai.OpenAI(
                base_url=self.cfg.model_base_url,
                api_key=self.cfg.model_api_key,
                timeout=self.cfg.model_timeout_s,
                max_retries=0,  # 重试策略统一由本框架控制
            )
        return self._client

    def decide(self, messages: List[Dict[str, Any]]) -> str:
        import time

        try:
            import openai
        except Exception as exc:  # pragma: no cover
            raise CuaError(E_INTERNAL, "未安装 openai SDK：%s" % exc)
        started = time.time()
        try:
            resp = self.client.chat.completions.create(
                model=self.cfg.model_name,
                messages=messages,
                temperature=self.cfg.model_temperature,
                max_tokens=self.cfg.model_max_tokens,
            )
        except openai.APITimeoutError as exc:
            raise CuaError(E_MODEL_TIMEOUT, "模型响应超时（%.1fs）" % self.cfg.model_timeout_s,
                           hint="指数退避后重试；连续失败转 E_NEED_HUMAN", cause=exc)
        except openai.APIStatusError as exc:
            status = getattr(exc, "status_code", None)
            detail = _status_error_detail(exc)
            if status_error_code(detail) == E_MODEL_EMPTY:
                # 代理把「上游空 content」映射成 502 + EMPTY_CONTENT_MARKER：
                # 必须当 E_MODEL_EMPTY 处理，绝不能退化成 E_MODEL_HTTP 被 5xx 重试逻辑反复重试。
                raise CuaError(
                    E_MODEL_EMPTY,
                    detail or "模型返回空 content（%s）" % EMPTY_CONTENT_MARKER,
                    hint="上游/代理报告空 content：检查 model_max_tokens（云端建议 >= 4096）后重跑",
                    context={"http_status": status, "upstream_message": detail},
                    cause=exc,
                )
            # 契约第 3 节：必须带出上游真实原因（上下文超限不许被说成 key 问题）
            raise CuaError(
                E_MODEL_HTTP,
                "模型服务返回 HTTP %s%s" % (status, ("：" + detail) if detail else ""),
                hint="4xx 请按上游返回的真实原因处理（模型名/额度/上下文超限等），重试无意义；5xx 可退避重试",
                context={"http_status": status, "upstream_message": detail},
                cause=exc,
            )
        except openai.APIConnectionError as exc:
            raise CuaError(
                E_MODEL_HTTP,
                "无法连接模型服务：%s" % self.cfg.model_base_url,
                hint="请确认服务已启动（Ollama 默认 http://127.0.0.1:11434/v1）",
                retryable=True,
                cause=exc,
            )
        except CuaError:
            raise
        except Exception as exc:
            raise CuaError(E_INTERNAL, "模型调用未预期失败：%s" % exc, cause=exc)
        finally:
            self.last_latency_ms = int((time.time() - started) * 1000)

        text = require_content(resp, self.cfg)
        if _looks_like_refusal(text):
            raise CuaError(E_MODEL_REFUSAL, "模型拒绝执行该任务",
                           hint="记 need_human，不换个说法反复试探")
        return text

    def close(self) -> None:
        if self._client is not None:
            try:
                self._client.close()
            except Exception:
                pass


def _status_error_detail(exc: Any) -> str:
    """从 APIStatusError 里尽量挖出上游真实原因（契约第 3 节）。

    依次尝试：body.error.message（OpenAI 标准错误体，Go 代理也用它带出上游原因）
    -> body.message -> body 原文 -> exc.message -> str(exc)。取不到就返回空串。
    """
    body = getattr(exc, "body", None)
    text = ""
    if isinstance(body, dict):
        err = body.get("error")
        if isinstance(err, dict):
            text = str(err.get("message") or "")
        elif isinstance(err, str):
            text = err
        if not text:
            text = str(body.get("message") or "")
    elif isinstance(body, str):
        text = body
    if not text:
        text = str(getattr(exc, "message", "") or "")
    if not text:
        text = str(exc)
    return " ".join(text.split())[:400]


def status_error_code(detail: str) -> str:
    """按上游 message 判定状态码错误码。

    带 EMPTY_CONTENT_MARKER（E_MODEL_EMPTY）-> E_MODEL_EMPTY，否则 E_MODEL_HTTP。
    """
    return E_MODEL_EMPTY if EMPTY_CONTENT_MARKER in (detail or "") else E_MODEL_HTTP


def require_content(resp: Any, cfg: Optional[Config] = None) -> str:
    """取出模型回复文本；空 / 全空白 / 缺 content 一律抛 E_MODEL_EMPTY（契约 §3.1）。

    这是「空 content 不得进入动作解析器」的唯一入口：解析器只应看到非空文本。
    """
    text = _extract_text(resp)
    if text is None or not text.strip():
        conf = cfg or Config()
        raise CuaError(
            E_MODEL_EMPTY,
            "模型返回空回复：content 为空或全空白（常见于 max_tokens 过小，reasoning 吃掉了输出预算）",
            hint="云端模型建议 model_max_tokens >= 4096；空回复不是合法动作，不会送进解析器",
            context={"model": conf.model_name, "max_tokens": conf.model_max_tokens},
        )
    return text


def _extract_text(resp: Any) -> Optional[str]:
    try:
        choices = getattr(resp, "choices", None) or []
        if not choices:
            return None
        message = getattr(choices[0], "message", None)
        content = getattr(message, "content", None)
        if isinstance(content, str):
            return content
        if isinstance(content, list):
            parts = []
            for item in content:
                if isinstance(item, dict) and item.get("type") == "text":
                    parts.append(item.get("text", ""))
                else:
                    text = getattr(item, "text", None)
                    if text:
                        parts.append(text)
            return "".join(parts) or None
        return None
    except Exception:
        return None


def _looks_like_refusal(text: str) -> bool:
    lowered = (text or "").lower()
    if not lowered.strip():
        return False
    if lowered.lstrip().startswith("{"):
        return False
    return any(marker in lowered for marker in _REFUSAL_MARKERS)

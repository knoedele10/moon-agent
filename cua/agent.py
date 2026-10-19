"""视觉推理 Agent（协议第 2 节 L2 层的生产者）。

负责：组装 messages -> 调模型 -> 解析/校验 Step；
对 E_BAD_JSON / E_SCHEMA 按协议第 5 节「最多重试 2 次」，把错误信息回喂给模型。
其余错误（越界、参数越界、限流等）由主循环统一处理。
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional

from .action_parser import ParsedStep, parse_step
from .backends.base import ModelClient
from .config import Config
from .errors import (
    E_BAD_JSON,
    E_SCHEMA,
    RetryTracker,
    CuaError,
    max_retries,
)
from .observation import observation_for_model
from .prompts import build_messages, build_text_messages, error_feedback, system_prompt

#: 直接在 Agent 内部重问模型的错误码（纯「输出格式」问题）
AGENT_RETRY_CODES = (E_BAD_JSON, E_SCHEMA)


@dataclass
class AgentTrace:
    attempts: int = 0
    reasks: List[Dict[str, Any]] = field(default_factory=list)
    prompt_source: str = ""


class VisionAgent:
    def __init__(self, client: ModelClient, cfg: Optional[Config] = None,
                 logger: Optional[Any] = None) -> None:
        self.client = client
        self.cfg = cfg or Config()
        self.logger = logger
        self.last_trace: Optional[AgentTrace] = None

    # ------------------------------------------------------------------
    def decide(
        self,
        observation: Dict[str, Any],
        frame: Optional[Any] = None,
        feedback: Optional[str] = None,
        include_image: Optional[bool] = None,
    ) -> ParsedStep:
        sys_text, source = system_prompt()
        obs_text = observation_for_model(observation)
        want_image = self.client.supports_vision if include_image is None else bool(include_image)
        image_url = None
        if want_image and frame is not None:
            image_url = frame.data_url()
        if want_image:
            messages = build_messages(obs_text, image_url, feedback, system=sys_text)
        else:
            messages = build_text_messages(obs_text, feedback, system=sys_text)

        tracker = RetryTracker()
        trace = AgentTrace(prompt_source=source)
        fb = feedback
        while True:
            trace.attempts += 1
            raw = self.client.decide(messages)
            try:
                parsed = parse_step(raw, cfg=self.cfg)
                self.last_trace = trace
                self._log({"event": "model_ok", "extraction": parsed.meta.get("extraction"),
                           "prompt_source": source, "attempts": trace.attempts})
                return parsed
            except CuaError as exc:
                if exc.code in AGENT_RETRY_CODES and tracker.count(exc.code) < max_retries(exc.code):
                    tracker.record(exc.code)
                    fb = error_feedback(exc)
                    trace.reasks.append({"code": exc.code, "message": exc.message})
                    self._log({"event": "model_reask", "code": exc.code, "message": exc.message})
                    if want_image:
                        messages = build_messages(obs_text, image_url, fb, system=sys_text)
                    else:
                        messages = build_text_messages(obs_text, fb, system=sys_text)
                    continue
                self.last_trace = trace
                raise

    # ------------------------------------------------------------------
    def _log(self, payload: Dict[str, Any]) -> None:
        if self.logger is not None:
            self.logger.event("agent", **payload)

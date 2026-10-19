"""模型客户端接口。"""

from __future__ import annotations

from typing import Any, Dict, List


class ModelClient:
    """一次 decide = 一次模型调用。实现负责把超时/HTTP 错误映射成协议错误码。"""

    name = "base"
    supports_vision = True

    def decide(self, messages: List[Dict[str, Any]]) -> str:  # pragma: no cover - 接口
        raise NotImplementedError

    def close(self) -> None:
        return None

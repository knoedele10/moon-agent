"""模型客户端后端：OpenAI 兼容多模态 API + mock。"""

from __future__ import annotations

from .base import ModelClient
from .mock import MockClient
from .openai_compat import OpenAIChatClient

__all__ = ["ModelClient", "MockClient", "OpenAIChatClient", "build_client"]


def build_client(cfg):
    """按配置选择后端：mock 优先，否则 OpenAI 兼容。"""
    if cfg.use_mock_model:
        return MockClient(script=[], name="mock-auto")
    return OpenAIChatClient(cfg)

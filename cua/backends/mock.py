"""脚本化假模型：--mock 用，保证离线可自测。

脚本是一串「模型原文」，每次 decide 取一条；条目可以是：
  - 字符串：原样返回（用于覆盖纯 JSON / 围栏 / 带前言 / 坏 JSON 等解析路径）；
  - {"raise": "E_MODEL_TIMEOUT", "message": "..."}：抛出对应错误（用于覆盖模型侧错误路径）。
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any, Dict, List, Optional, Sequence

from ..config import Config
from ..errors import E_INTERNAL, CuaError


class MockClient:
    name = "mock"
    supports_vision = False

    def __init__(self, script: Sequence[Any], loop: bool = False, name: Optional[str] = None) -> None:
        if name:
            self.name = name
        self.script: List[Any] = list(script)
        self.loop = bool(loop)
        self.calls: List[Dict[str, Any]] = []
        self.index = 0

    # -- 构造 -----------------------------------------------------------
    @classmethod
    def from_json_file(cls, path: str, loop: bool = False) -> "MockClient":
        with open(path, "r", encoding="utf-8") as fh:
            data = json.load(fh)
        if isinstance(data, dict) and "script" in data:
            return cls(data["script"], loop=bool(data.get("loop", loop)))
        if not isinstance(data, list):
            raise CuaError(E_INTERNAL, "mock 脚本必须是 JSON 数组或 {script:[...]}")
        return cls(data, loop=loop)

    # -- 调用 -----------------------------------------------------------
    def decide(self, messages: List[Dict[str, Any]]) -> str:
        self.calls.append({"messages": messages, "index": self.index})
        if self.index >= len(self.script):
            if not self.loop:
                raise CuaError(E_INTERNAL, "mock 脚本已用尽（第 %d 次调用）" % (self.index + 1),
                               hint="请补足 --mock 脚本条目，或设置 loop=True")
            self.index = 0
        entry = self.script[self.index]
        self.index += 1
        if isinstance(entry, dict) and "raise" in entry:
            raise CuaError(entry["raise"], entry.get("message", "mock 注入错误"),
                           hint=entry.get("hint"), context=entry.get("context"))
        if isinstance(entry, str):
            return entry
        return json.dumps(entry, ensure_ascii=False)

    @property
    def exhausted(self) -> bool:
        return self.index >= len(self.script)

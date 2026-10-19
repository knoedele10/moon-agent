"""Observation 组装（协议第 1 节，L1 层）。"""

from __future__ import annotations

import json
import time
from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional

from .config import Config, PROTOCOL_VERSION
from .screenshot import CaptureFrame


@dataclass
class History:
    """最近 6~10 步的压缩历史（协议第 1 节：只给压缩摘要，不回灌完整截图）。"""

    limit: int = 8
    entries: List[Dict[str, Any]] = field(default_factory=list)

    def add(self, step: int, action: Optional[Dict[str, Any]], ok: bool,
            screen_changed: bool, error_code: Optional[str] = None) -> None:
        entry: Dict[str, Any] = {
            "step": int(step),
            "action": action,
            "ok": bool(ok),
            "screen_changed": bool(screen_changed),
        }
        if error_code:
            entry["error_code"] = error_code
        self.entries.append(entry)
        if len(self.entries) > self.limit:
            self.entries = self.entries[-self.limit:]

    def tail(self) -> List[Dict[str, Any]]:
        return list(self.entries)


def build_observation(
    cfg: Config,
    task: str,
    step: int,
    frame: CaptureFrame,
    last_action_result: Optional[Dict[str, Any]] = None,
    history: Optional[History] = None,
    popup_suspected: bool = False,
    steps_left: Optional[int] = None,
    seconds_left: Optional[float] = None,
    notes: Optional[str] = None,
    screen_index: int = 0,
    dpi_scale: Optional[float] = None,
) -> Dict[str, Any]:
    """组装协议第 1 节 Observation 字典。"""
    obs: Dict[str, Any] = {
        "protocol": PROTOCOL_VERSION,
        "step": int(step),
        "task": task,
        "screen": {
            "index": int(screen_index),
            "width": int(frame.screen_size[0] or frame.width),
            "height": int(frame.screen_size[1] or frame.height),
            "dpi_scale": float(dpi_scale if dpi_scale is not None else 1.0),
        },
        "image": frame.image_block(),
        "last_action_result": last_action_result,
        "history_tail": history.tail() if history else [],
        "flags": {
            "popup_suspected": bool(popup_suspected),
            "failsafe_armed": True,
            "steps_left": int(steps_left if steps_left is not None else cfg.step_budget),
            "seconds_left": float(round(seconds_left, 3)) if seconds_left is not None else float(cfg.wall_budget_s),
        },
    }
    if notes:
        obs["notes"] = notes
    return obs


def observation_for_model(obs: Dict[str, Any]) -> str:
    """给模型的文本：去掉图片二进制相关信息（图片另走 image_url），保留坐标空间三要素。"""
    view = dict(obs)
    image = dict(view.get("image") or {})
    image.pop("sha256", None)
    image.pop("media_type", None)
    view["image"] = image
    return json.dumps(view, ensure_ascii=False, indent=1)

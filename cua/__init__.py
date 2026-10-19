"""blog-my 视觉 CUA 最小可运行框架。

冻结契约：docs/cua/03-action-protocol.md 与 docs/cua/action.schema.json。
本包只实现协议，不改协议；契约有歧义时按协议第 9 节「以 Schema 为准」。

本模块只暴露版本号与协议常量，不做任何副作用（不导入 pyautogui、不截屏、不动鼠标）。
"""

from __future__ import annotations

__version__ = "0.1.0"

PROTOCOL_VERSION = "cua/1"

__all__ = ["__version__", "PROTOCOL_VERSION"]

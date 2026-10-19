"""视觉推理 Agent 的提示词（协议第 2 节输出契约）。

对齐说明：docs/cua/02-agent-prompt.md 是系统提示词的权威来源（由 cua-arch 产出）。
本文件做两件事：
  1. 若 docs/cua/02-agent-prompt.md 存在，直接使用其正文作为系统提示词（source 会记录在日志里）；
  2. 否则使用下面的占位提示词，占位提示词同样是「只输出一个符合 cua/1 的 JSON 对象」，
     足以让 mock 与真实 Ollama 模型产生可解析的输出。

【待与 02 对齐】：占位提示词与 02-agent-prompt.md 的具体措辞可能不同，
02 文件产出后自动生效，无需改本文件。
"""

from __future__ import annotations

import json
import os
import re
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple

from .config import ACTION_TYPES, Config, PROTOCOL_VERSION
from .errors import CuaError

#: 02 号文档（系统提示词权威来源）
PROMPT_DOC = Path(__file__).resolve().parent.parent / "docs" / "cua" / "02-agent-prompt.md"

_FENCE = chr(96) * 3
#: 02 号文档第 1 节把完整系统提示词放在一个 text 代码块里，这里逐字取出该块。
_PROMPT_BLOCK_RE = re.compile(_FENCE + r"[ \t]*text[ \t]*\r?\n(.*?)" + _FENCE, re.S)

PLACEHOLDER_SYSTEM_PROMPT = """你是 blog-my 的视觉 CUA（Computer-Use Agent）。你每一步只能输出一个 JSON 对象，不要输出任何解释文字。

协议版本固定为 cua/1。输出结构：
{
  "protocol": "cua/1",
  "thought": "一句话说清：我看到什么 -> 所以我做什么（不超过 1200 字）",
  "action": { "type": "...", ... },
  "expect": { "what": "预期界面变化", "timeout_ms": 8000, "must_change": true },
  "risk": "low",
  "done": false,
  "final": null
}

允许的 9 种动作（穷举，未知 type 会被拒绝）：
1. click：{"type":"click","x":0,"y":0,"button":"left","clicks":1,"modifiers":[]}
2. type：{"type":"type","text":"...","clear_first":false,"submit":false,"interval_ms":0}
3. hotkey：{"type":"hotkey","keys":["ctrl","s"],"repeat":1}（单键也走这里，如 {"keys":["enter"]}）
4. drag：{"type":"drag","from":{"x":0,"y":0},"to":{"x":0,"y":0},"duration_ms":600}
5. scroll：{"type":"scroll","x":0,"y":0,"dx":0,"dy":-3,"unit":"line"}
6. move：{"type":"move","x":0,"y":0,"duration_ms":200}
7. wait：{"type":"wait","ms":1500,"reason":"等待加载"}
8. screenshot：{"type":"screenshot","region":null} 或 region={"x":0,"y":0,"width":200,"height":120}
9. done：{"type":"done","status":"success","summary":"...","artifacts":[]}

坐标规则（最容易出错）：
- 所有坐标都是当前这张 image 的像素坐标，左上角 (0,0)，右下为正；
- 执行器按 screen = origin + round(coord / scale) 换算，origin/scale 见 observation.image；
- 坐标超出 image 边界会被直接拒绝，不会被夹到边界。

其他硬性要求：
- thought 必填，不超过 1200 字；
- done=true 时必须给出 final：{"status":"success|failed|need_human","summary":"...","answer":null}；
- 任务完成用 {"type":"done",...}；需要验证码/密码/UAC/支付时用 status=need_human；
- 只输出一个 JSON 对象，不要 Markdown 代码块以外的任何文字。
"""


def repo_root() -> Path:
    return Path(__file__).resolve().parent.parent


def prompt_doc_path() -> Path:
    override = os.environ.get("CUA_PROMPT_DOC")
    return Path(override) if override else PROMPT_DOC


def extract_prompt_block(text: str) -> Optional[str]:
    """从 02 号文档里取出第 1 节的 text 代码块（即「可直接复制」的系统提示词）。"""
    match = _PROMPT_BLOCK_RE.search(text or "")
    if not match:
        return None
    block = match.group(1).strip()
    return block or None


def system_prompt() -> Tuple[str, str]:
    """返回 (提示词正文, 来源)。

    02 号文档存在且含 text 代码块时，逐字使用该代码块（与文档保持同步）；
    否则回落到本文件的占位提示词。
    """
    path = prompt_doc_path()
    if path.is_file():
        try:
            text = path.read_text(encoding="utf-8")
            block = extract_prompt_block(text)
            if block:
                return block, str(path)
            stripped = text.strip()
            if stripped:
                return stripped, str(path)
        except Exception:
            pass
    return PLACEHOLDER_SYSTEM_PROMPT, "cua/prompts.py（占位，待与 docs/cua/02-agent-prompt.md 对齐）"


def error_feedback(err: CuaError, extra: Optional[str] = None) -> str:
    """把错误回喂给模型（协议第 5 节默认处置）。"""
    parts = [err.feedback_message()]
    if extra:
        parts.append(extra)
    return "\n".join(parts)


def build_messages(
    obs_text: str,
    image_data_url: Optional[str] = None,
    feedback: Optional[str] = None,
    system: Optional[str] = None,
) -> List[Dict[str, Any]]:
    """构造 OpenAI 兼容的 chat messages（文本 + 可选图片）。"""
    sys_text = system if system is not None else system_prompt()[0]
    content: List[Dict[str, Any]] = [{"type": "text", "text": obs_text}]
    if image_data_url:
        content.append({"type": "image_url", "image_url": {"url": image_data_url}})
    if feedback:
        content.append({"type": "text", "text": feedback})
    return [
        {"role": "system", "content": sys_text},
        {"role": "user", "content": content},
    ]


def build_text_messages(obs_text: str, feedback: Optional[str] = None,
                        system: Optional[str] = None) -> List[Dict[str, str]]:
    """纯文本模式（mock 后端与不支持视觉的模型）。"""
    sys_text = system if system is not None else system_prompt()[0]
    user = obs_text if not feedback else obs_text + "\n\n" + feedback
    return [
        {"role": "system", "content": sys_text},
        {"role": "user", "content": user},
    ]

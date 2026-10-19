"""CUA 运行配置与冻结常量。

对应 docs/cua/03-action-protocol.md：
  - 第 3.3 节 键名白名单
  - 第 5 节 预算与错误
  - 第 7 节 破坏性词表
  - 第 8 节 安全不变量

约定（仓库文档规范）：中文；只有实测过的写实测，没跑过的标「设计如此」或「待验证」。
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field, replace
from typing import Any, Dict, List, Optional, Sequence, Tuple

# ---------------------------------------------------------------------------
# 协议常量
# ---------------------------------------------------------------------------

PROTOCOL_VERSION = "cua/1"

#: 第 3.3 节键名白名单（小写）。白名单之外一律 E_KEY_NOT_ALLOWED。
KEY_WHITELIST: frozenset = frozenset(
    list("abcdefghijklmnopqrstuvwxyz")
    + list("0123456789")
    + ["f%d" % i for i in range(1, 13)]
    + [
        "ctrl",
        "alt",
        "shift",
        "win",
        "cmd",
        "enter",
        "tab",
        "esc",
        "space",
        "backspace",
        "delete",
        "up",
        "down",
        "left",
        "right",
        "home",
        "end",
        "pageup",
        "pagedown",
        "insert",
    ]
)

#: 修饰键集合（需要「按下后必须保证释放」的键）。
MODIFIER_KEYS: frozenset = frozenset(["ctrl", "alt", "shift", "win", "cmd"])

#: pyautogui 键名映射：协议键名 -> pyautogui 键名。
PYAUTOGUI_KEY_MAP: Dict[str, str] = {
    "win": "win",
    "cmd": "win",
    "enter": "enter",
    "tab": "tab",
    "esc": "esc",
    "space": "space",
    "backspace": "backspace",
    "delete": "delete",
    "pageup": "pageup",
    "pagedown": "pagedown",
    "insert": "insert",
}

#: 第 8 节第 5 条：默认关闭的危险组合键（全部小写、已排序的元组）。
#: 需要显式打开 allow_dangerous_hotkeys 才允许执行。
DANGEROUS_HOTKEY_PATTERNS: Tuple[Tuple[str, ...], ...] = (
    ("alt", "f4"),
    ("ctrl", "alt", "delete"),
    ("ctrl", "alt", "del"),
    ("win", "l"),
    ("win", "r"),
    ("win", "x"),
    ("win", "e"),
    ("win", "d"),
    ("win", "i"),
    ("win", "s"),
    ("win", "a"),
    ("win", "p"),
    ("win", "u"),
    ("win", "q"),
    ("ctrl", "shift", "esc"),
)

#: 第 7 节破坏性词表（命中即转人工）。中英文均含，子串匹配、大小写不敏感。
DESTRUCTIVE_WORDS: Tuple[str, ...] = (
    "删除",
    "卸载",
    "格式化",
    "清空",
    "重置",
    "覆盖",
    "支付",
    "付款",
    "确认购买",
    "允许",
    "授权",
    "安装",
    "同意",
    "delete",
    "uninstall",
    "format",
    "erase",
    "reset",
    "overwrite",
    "pay",
    "purchase",
    "allow",
    "grant",
    "install",
    "agree",
)

#: 弹窗默认优先选择的安全按钮词表（第 7 节处置优先级第 2 条）。
SAFE_POPUP_WORDS: Tuple[str, ...] = (
    "取消",
    "关闭",
    "稍后",
    "返回",
    "以后再说",
    "cancel",
    "close",
    "later",
    "dismiss",
    "no",
    "x",
)

def destructive_hits(text: str) -> List[str]:
    """返回 text 中命中的破坏性词（第 7 节）。子串匹配、大小写不敏感。"""
    if not text:
        return []
    lowered = text.lower()
    hits: List[str] = []
    for word in DESTRUCTIVE_WORDS:
        if word.lower() in lowered:
            hits.append(word)
    return hits


#: 9 种动作类型（第 3 节穷举）。
ACTION_TYPES: Tuple[str, ...] = (
    "click",
    "type",
    "hotkey",
    "drag",
    "scroll",
    "move",
    "wait",
    "screenshot",
    "done",
)


# ---------------------------------------------------------------------------
# 配置
# ---------------------------------------------------------------------------


@dataclass
class Config:
    """一次 CUA 运行的全部可调项。

    注意：pyautogui.FAILSAFE 不在这里——第 8 节第 1 条要求它永久开启，
    因此它不作为可配置项暴露（见 executor.PyAutoGUIDevice）。
    """

    # --- 任务与预算 ---
    task: str = ""
    step_budget: int = 40              # 第 8 节第 4 条，默认 40
    wall_budget_s: float = 300.0       # 第 8 节第 4 条，默认 300s
    min_step_sleep_s: float = 0.15     # 第 8 节第 3 条，至少 150ms

    # --- 模型（OpenAI 兼容）---
    model_base_url: str = "http://127.0.0.1:11434/v1"
    model_api_key: str = "ollama"
    model_name: str = "qwen2.5vl:7b"
    model_timeout_s: float = 60.0
    model_max_retries: int = 2
    model_temperature: float = 0.0
    model_max_tokens: int = 4096     # 契约 §3.1：云端默认 >= 4096（1024 会让 reasoning 模型间歇性返回空）

    # --- 运行模式 ---
    dry_run: bool = False              # 只打印将执行的动作，不真按键
    use_mock_model: bool = False       # 脚本化假模型
    use_mock_screen: bool = False      # 合成屏幕帧，不调真实截图 API
    script: Optional[str] = None       # --mock 脚本文件（JSON 数组，每项一段模型原文）

    # --- 图像 ---
    image_max_width: int = 1472        # 送模型前等比缩放上限（设计如此，控制 token）
    screenshot_zoom: float = 2.0       # 局部截图时的放大倍数（设计如此）
    screenshot_format: str = "PNG"

    # --- 检测阈值 ---
    stall_window: int = 5              # 连续 N 步几乎无变化 -> E_STALL
    stall_change_ratio: float = 0.005  # 「几乎无变化」的像素占比阈值（设计如此）
    loop_repeat_threshold: int = 3     # 同一（动作，画面哈希）重复 >= 3 -> E_LOOP
    max_replans: int = 2               # 允许的最大强制重规划次数
    popup_enabled: bool = True
    popup_area_min_ratio: float = 0.03
    popup_area_max_ratio: float = 0.80
    popup_fill_min_ratio: float = 0.25
    popup_max_esc: int = 2             # 第 7 节：esc 最多两次，再无效转人工
    popup_auto_crop: bool = True       # 第 7 节处置优先级 1：疑似弹窗区域裁剪放大
    scroll_page_lines: int = 5         # unit=page 时 1 页折算的滚动行数（设计如此）
    observation_history_tail: int = 8
    expect_poll_interval_s: float = 0.25

    # --- 安全 ---
    allow_dangerous_hotkeys: bool = False   # 第 8 节第 5 条
    guard_high_risk: bool = True            # 第 8 节第 6 条：risk=high 需放行
    # task-25（2026-10-11）：语义已扩展为「输入优先走剪贴板」——非空文本一律写剪贴板再 Ctrl+V（ASCII 也是）。
    # 置 False 只保留旧的逐字符 write 路径（无剪贴板/离线环境）。字段名保留以免破坏既有配置与文档引用。
    type_use_clipboard_for_unicode: bool = True

    # --- 预算分档（协议第 8 节第 4 条，2026-10-11 裁定）---
    # cloud：40 步 / 300s；local：本地 VLM 单步 9~78s，墙钟默认 1800s 且截图长边收到 1280。
    budget_profile: str = "cloud"

    # --- 路由 ---
    enable_gui_fallback: bool = True
    capability_probes: List[Dict[str, Any]] = field(default_factory=list)

    # --- 事件流（集成契约第 1 节，2026-10-11）---
    # --events stdout：stdout 只输出 JSONL 事件（逐行 flush），人类可读输出全部改走 stderr。
    events_stdout: bool = False
    # --events-images：每产生一帧，在 frame 之后追加一条 frame_image（需与 events_stdout 同用）。
    events_images: bool = False
    event_image_max_edge: int = 640    # frame_image 长边上限（契约第 1 节：缩到长边 640）
    event_image_quality: int = 60      # frame_image JPEG 质量（契约第 1 节：q60）

    # --- 日志 ---
    log_path: str = os.path.join("cua", "logs", "cua.jsonl")
    verbose: bool = True

    # ------------------------------------------------------------------
    def with_(self, **kwargs: Any) -> "Config":
        """返回一份改动副本（便于 selfcheck 构造不同配置）。"""
        return replace(self, **kwargs)

    def clamped_sleep(self, seconds: float) -> float:
        return max(float(seconds), float(self.min_step_sleep_s))


#: 预算分档（协议第 8 节第 4 条，2026-10-11 裁定：40/300 只对云端 API 后端成立）。
#: 本地 VLM 实测 9~78 秒/步，300 秒只够 4~12 步，因此本地档墙钟默认 >= 1800s，
#: 并把送模型的截图长边收到 896~1280 之间（默认取上限 1280）以压低单步耗时。
BUDGET_PROFILES: Dict[str, Dict[str, Any]] = {
    "cloud": {
        "step_budget": 40,
        "wall_budget_s": 300.0,
        "image_max_width": 1472,
        # 契约 §3.1：云端 reasoning 模型在 max_tokens 过小时会返回空 content（实测 80 -> 空 / 4000 -> 正常）
        "model_max_tokens": 4096,
        "description": "云端 API 后端：40 步 / 300s / 截图长边 1472 / max_tokens 4096",
    },
    "local": {
        "step_budget": 40,
        "wall_budget_s": 1800.0,
        "image_max_width": 1280,
        # 本地 Ollama 小 VLM 保持小值，避免无谓地拉长单步耗时
        "model_max_tokens": 1024,
        "description": "本地 VLM 后端：40 步 / 1800s / 截图长边 1280（本地单步 9~78s）/ max_tokens 1024",
    },
}

LOCAL_HOSTS = ("127.0.0.1", "localhost", "0.0.0.0", "::1", "host.docker.internal")


def is_local_model_url(url: str) -> bool:
    if not url:
        return False
    for host in LOCAL_HOSTS:
        if host in url:
            return True
    return False


def resolve_budget_profile(requested: Optional[str], base_url: str = "") -> str:
    """'auto' -> 本地地址走 local 档，否则 cloud 档；非法值回落 cloud。"""
    name = (requested or "cloud").strip().lower()
    if name == "auto":
        return "local" if is_local_model_url(base_url) else "cloud"
    return name if name in BUDGET_PROFILES else "cloud"


def apply_budget_profile(cfg: Config, profile: Optional[str] = None) -> Config:
    """按分档覆盖 step_budget / wall_budget_s / image_max_width。"""
    name = resolve_budget_profile(profile or cfg.budget_profile, cfg.model_base_url)
    preset = BUDGET_PROFILES.get(name) or BUDGET_PROFILES["cloud"]
    return cfg.with_(
        budget_profile=name,
        step_budget=int(preset["step_budget"]),
        wall_budget_s=float(preset["wall_budget_s"]),
        image_max_width=int(preset["image_max_width"]),
        model_max_tokens=int(preset["model_max_tokens"]),
    )


def load_config_from_env(base: Optional[Config] = None) -> Config:
    """从环境变量叠加配置；未设置的保持默认。"""
    cfg = base or Config()
    env_map = {
        "CUA_MODEL_BASE_URL": ("model_base_url", str),
        "CUA_MODEL_API_KEY": ("model_api_key", str),
        "CUA_MODEL_NAME": ("model_name", str),
        "CUA_MODEL_TIMEOUT_S": ("model_timeout_s", float),
        "CUA_STEP_BUDGET": ("step_budget", int),
        "CUA_WALL_BUDGET_S": ("wall_budget_s", float),
        "CUA_LOG_PATH": ("log_path", str),
        "CUA_BUDGET_PROFILE": ("budget_profile", str),
    }
    updates: Dict[str, Any] = {}
    for env_key, (attr, cast) in env_map.items():
        raw = os.environ.get(env_key)
        if raw:
            try:
                updates[attr] = cast(raw)
            except (TypeError, ValueError):
                continue
    if os.environ.get("CUA_DRY_RUN") in ("1", "true", "TRUE"):
        updates["dry_run"] = True
    # 第 8 节第 5 条的危险组合键默认关闭；真实使用（如 Win+R 启动程序）需要显式放行。
    if os.environ.get("CUA_ALLOW_DANGEROUS_HOTKEYS") in ("1", "true", "TRUE"):
        updates["allow_dangerous_hotkeys"] = True
    cfg = cfg.with_(**updates)
    # 分档优先于默认值；调用方随后可再用显式参数覆盖
    return apply_budget_profile(cfg, cfg.budget_profile)


# ---------------------------------------------------------------------------
# 路由级别（第 6 节）
# ---------------------------------------------------------------------------

ROUTE_L0 = "L0"  # 结构化接口 REST/GraphQL/SDK/CLI/socket
ROUTE_L1 = "L1"  # 浏览器 DOM 层 CDP/Playwright
ROUTE_L2 = "L2"  # 操作系统命令（白名单）
ROUTE_L3 = "L3"  # GUI 视觉操控（本协议，兜底）

ROUTE_LEVELS: Tuple[str, ...] = (ROUTE_L0, ROUTE_L1, ROUTE_L2, ROUTE_L3)

ROUTE_DESCRIPTIONS: Dict[str, str] = {
    ROUTE_L0: "结构化接口（最快最稳，首选）",
    ROUTE_L1: "浏览器 DOM 层（CDP/Playwright）",
    ROUTE_L2: "操作系统命令（白名单约束）",
    ROUTE_L3: "GUI 视觉操控（最慢最脆，兜底）",
}

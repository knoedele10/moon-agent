"""cua 包的命令行入口。

用法（全部离线可跑，无显示器、不动真实鼠标）：
  python -m cua.main --selfcheck                # 全链路离线自检（解析/校验/换算/错误路径/端到端）
  python -m cua.main --dry-run --mock           # 脚本化假模型 + 只打印将执行的动作
  python -m cua.main --dry-run --mock --script cua/mock/demo.json
  python -m cua.main --events stdout            # stdout 逐行 JSONL 事件流（人类可读输出改走 stderr）
  python -m cua.main --events stdout --events-images   # 每帧再追加 frame_image（长边 640 / JPEG q60）
  python -m cua.main --task "把标题改成 Hello"  # 接真实模型（需要模型服务）

--mock / --selfcheck 默认使用合成屏幕（MockScreenshot），绝不调用真实截图 API；
要对着真屏幕跑加 --real-screen。
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
from pathlib import Path
from typing import Any, Callable, Dict, List, Optional, Sequence, Tuple

from . import __version__
from .action_parser import (
    MiniValidator,
    default_schema_path,
    extract_json,
    load_schema,
    parse_step,
    validate_step_dict,
    validate_with_jsonschema,
)
from .agent import VisionAgent
from .backends.mock import MockClient
from .backends.openai_compat import OpenAIChatClient
from .config import (
    ACTION_TYPES,
    BUDGET_PROFILES,
    DESTRUCTIVE_WORDS,
    KEY_WHITELIST,
    PROTOCOL_VERSION,
    Config,
    apply_budget_profile,
    destructive_hits,
    load_config_from_env,
    resolve_budget_profile,
)
from .errors import (
    E_ARG_RANGE,
    E_BAD_JSON,
    E_CANCEL,
    E_EXECUTOR_FAILSAFE,
    E_EXPECT_TIMEOUT,
    E_INTERNAL,
    E_KEY_NOT_ALLOWED,
    E_LOOP,
    E_MODEL_EMPTY,
    E_MODEL_HTTP,
    E_MODEL_REFUSAL,
    E_MODEL_TIMEOUT,
    E_NEED_HUMAN,
    E_OUT_OF_BOUNDS,
    E_POPUP_BLOCKING,
    E_SCHEMA,
    E_STALL,
    E_STEP_BUDGET,
    E_UNKNOWN_ACTION,
    E_WALL_BUDGET,
    ERROR_CODES,
    ERROR_TABLE,
    CuaError,
    RetryTracker,
    is_replan,
    is_retryable,
    max_retries,
)
from .executor import (
    DryRunDevice,
    ExecutionOutcome,
    Executor,
    RecordingDevice,
    destructive_guard,
)
from .loop import (
    ControlLoop,
    EventStreamLogger,
    JsonlLogger,
    LoopDetector,
    MemoryLogger,
    StallDetector,
    TeeLogger,
)
from .observation import History, build_observation
from .router import Router, StaticProbe, default_router
from .screenshot import (
    CaptureFrame,
    MockScreenshot,
    PyAutoGUIScreenshot,
    clamp_region_to_frame,
    diff_frames,
    image_rect_to_screen_rect,
    image_to_screen,
    paint_base,
    paint_plain,
    paint_popup,
    round_half_up,
    screen_to_image,
    to_screen_point,
)

_FENCE = chr(96) * 3

DEMO_TASK = "离线演示：点击搜索框 -> 输入关键词 -> 提交 -> 结束"


def _demo_script() -> List[str]:
    steps = [
        {"protocol": PROTOCOL_VERSION, "thought": "先看一眼界面，点击搜索框",
         "action": {"type": "click", "x": 640.4, "y": 210.6}, "expect": None, "risk": "low", "done": False},
        {"protocol": PROTOCOL_VERSION, "thought": "输入关键词（支持中文走剪贴板）",
         "action": {"type": "type", "text": "hello cua", "clear_first": True, "submit": False},
         "risk": "low", "done": False},
        {"protocol": PROTOCOL_VERSION, "thought": "用快捷键提交",
         "action": {"type": "hotkey", "keys": ["ctrl", "s"], "repeat": 1}, "risk": "low", "done": False},
        {"protocol": PROTOCOL_VERSION, "thought": "等待界面响应",
         "action": {"type": "wait", "ms": 200, "reason": "等待界面更新"}, "risk": "low", "done": False},
        {"protocol": PROTOCOL_VERSION, "thought": "任务完成",
         "action": {"type": "done", "status": "success", "summary": "离线演示跑通", "artifacts": []},
         "risk": "low", "done": True,
         "final": {"status": "success", "summary": "离线演示跑通", "answer": None}},
    ]
    script: List[str] = []
    # 第 1 条故意带前言 + 代码围栏 + 小数坐标，用来覆盖「宽进提取」与 coord_rounded
    script.append("让我先看看界面。" + _FENCE + "json\n" + json.dumps(steps[0], ensure_ascii=False) + "\n" + _FENCE)
    for step in steps[1:]:
        script.append(json.dumps(step, ensure_ascii=False))
    return script


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="python -m cua.main",
        description="blog-my CUA 最小可运行框架（协议 %s）" % PROTOCOL_VERSION,
    )
    p.add_argument("--task", default=None, help="任务描述")
    p.add_argument("--mock", action="store_true", help="使用脚本化假模型（离线）")
    p.add_argument("--script", default=None, help="--mock 的脚本文件（JSON 数组）")
    p.add_argument("--dry-run", action="store_true", help="只打印将执行的动作，不真按键")
    p.add_argument("--selfcheck", action="store_true", help="离线全链路自检")
    p.add_argument("--json", dest="as_json", action="store_true", help="输出机器可读报告")
    p.add_argument("--events", default=None, choices=["stdout"],
                   help="事件流输出（集成契约第 1 节 I1）：stdout=stdout 逐行 JSONL；开启后 stdout 无人类可读文本")
    p.add_argument("--events-images", dest="events_images", action="store_true",
                   help="--events stdout 时每产生一帧追加 frame_image（长边 640 / JPEG q60）")
    p.add_argument("--max-steps", type=int, default=None, help="步数预算（默认 40）")
    p.add_argument("--max-seconds", type=float, default=None, help="墙钟预算秒（默认 300）")
    p.add_argument("--model", default=None, help="模型名（OpenAI 兼容）")
    p.add_argument("--base-url", default=None, help="模型服务 base_url（默认 Ollama /v1）")
    p.add_argument("--api-key", default=None, help="模型服务 api key")
    p.add_argument("--model-max-tokens", dest="model_max_tokens", type=int, default=None,
                   help="模型单次回复 token 上限（覆盖分档；契约 §3.1：云端建议 >= 4096）")
    p.add_argument("--log", dest="log_path", default=None, help="JSONL 日志路径")
    p.add_argument("--no-log", action="store_true", help="不写日志文件（仅内存）")
    p.add_argument("--mock-scenario", default="moving",
                   choices=["moving", "frozen", "popup", "popup_flip", "alt"],
                   help="合成屏幕场景（--mock/--selfcheck 用）")
    p.add_argument("--real-screen", action="store_true", help="即使 --mock 也使用真实截图")
    p.add_argument("--budget-profile", default=None, choices=["cloud", "local", "auto"],
                   help="预算分档（cloud 40步/300s，local 40步/1800s+截图长边1280，auto 按 base_url 判断）")
    p.add_argument("--allow-high-risk", action="store_true", help="放行 risk=high 的动作（护栏）")
    p.add_argument("--version", action="version", version="cua %s" % __version__)
    return p


def config_from_args(args: argparse.Namespace) -> Config:
    cfg = load_config_from_env()   # 内部已按 budget_profile 分档
    updates: Dict[str, Any] = {}
    if args.model:
        updates["model_name"] = args.model
    if args.base_url:
        updates["model_base_url"] = args.base_url
    if args.api_key:
        updates["model_api_key"] = args.api_key
    if args.log_path:
        updates["log_path"] = args.log_path
    if args.no_log:
        updates["log_path"] = None
    if args.budget_profile:
        updates["budget_profile"] = args.budget_profile
    updates["dry_run"] = bool(args.dry_run)
    updates["use_mock_model"] = bool(args.mock)
    updates["use_mock_screen"] = bool(args.mock or args.selfcheck) and not args.real_screen
    updates["script"] = args.script
    updates["events_stdout"] = bool(getattr(args, "events", None) == "stdout")
    updates["events_images"] = bool(getattr(args, "events_images", False))
    cfg = cfg.with_(**updates)
    # 分档先于显式预算参数：显式 --max-steps / --max-seconds 永远优先
    cfg = apply_budget_profile(cfg, cfg.budget_profile)
    if args.max_steps is not None:
        cfg = cfg.with_(step_budget=int(args.max_steps))
    if args.max_seconds is not None:
        cfg = cfg.with_(wall_budget_s=float(args.max_seconds))
    explicit_tokens = getattr(args, "model_max_tokens", None)
    if explicit_tokens is not None:
        cfg = cfg.with_(model_max_tokens=int(explicit_tokens))
    return cfg


def _build_provider(cfg: Config, scenario: str):
    if cfg.use_mock_screen:
        return MockScreenshot(cfg, scenario=scenario)
    return PyAutoGUIScreenshot(cfg)


def _build_client(cfg: Config, script_path: Optional[str]):
    if cfg.use_mock_model:
        if script_path:
            return MockClient.from_json_file(script_path)
        return MockClient(_demo_script(), loop=False)
    return OpenAIChatClient(cfg)


def _force_utf8_stdio() -> Dict[str, str]:
    """把 stdout / stderr 固定成 UTF-8（task-21 / 集成契约事件流编码）。

    管道下 Windows 默认用 ANSI 代码页（本机实测 gbk）。事件流用
    json.dumps(..., ensure_ascii=False) 写中文，按 GBK 落字节后 Go 按 UTF-8 读
    会被静默替换成 U+FFFD（emoji 更会直接 UnicodeEncodeError 崩溃）。
    因此在写第一个事件之前 reconfigure，保证 stdout 字节就是 UTF-8。
    reconfigure 是就地生效，已持有的流引用同样被修正。
    """
    info: Dict[str, str] = {}
    for name in ("stdout", "stderr"):
        stream = getattr(sys, name, None)
        reconfigure = getattr(stream, "reconfigure", None)
        if stream is None or reconfigure is None:
            info[name] = "unsupported"
            continue
        try:
            reconfigure(encoding="utf-8", errors="strict")
            info[name] = str(getattr(stream, "encoding", "?"))
        except Exception as exc:  # pragma: no cover - 极端环境
            info[name] = "failed:%s" % exc
    return info


def run_once(args: argparse.Namespace, cfg: Config) -> int:
    provider = _build_provider(cfg, args.mock_scenario)
    client = _build_client(cfg, args.script)
    # 集成契约第 1 节 I1：--events stdout 时 stdout 只允许 JSONL。
    # task-21：事件流开启时先把 stdout/stderr 固定为 UTF-8，再捕获流引用。
    if cfg.events_stdout:
        _force_utf8_stdio()
    # events_sink 直接握住真实 stdout；人类可读输出（含诊断）走 human 流。
    real_stdout = sys.stdout
    events_sink = EventStreamLogger(real_stdout) if cfg.events_stdout else None
    file_sink: Any = JsonlLogger(cfg.log_path, enabled=True) if cfg.log_path else MemoryLogger()
    logger: Any = TeeLogger([file_sink, events_sink]) if events_sink is not None else file_sink
    human = sys.stderr if cfg.events_stdout else real_stdout
    agent = VisionAgent(client, cfg, logger=logger)
    executor = Executor(cfg, provider=provider)
    loop = ControlLoop(cfg, agent=agent, executor=executor, provider=provider, logger=logger,
                       events=events_sink, allow_high_risk=bool(args.allow_high_risk))
    task = args.task or DEMO_TASK
    try:
        if cfg.events_stdout:
            # 兜底：任何漏网的人类可读打印都不许落到 stdout（契约第 1 节）
            sys.stdout = sys.stderr
        report = loop.run(task)
    finally:
        sys.stdout = real_stdout

    if args.as_json:
        print(json.dumps(report.to_dict(), ensure_ascii=False, indent=1), file=human)
    else:
        print("=== CUA 运行 ===", file=human)
        print("任务：%s" % task, file=human)
        print("模式：dry_run=%s mock_model=%s mock_screen=%s" % (
            cfg.dry_run, cfg.use_mock_model, cfg.use_mock_screen), file=human)
        print("模型：%s（%s）" % (getattr(client, "name", "?"), getattr(cfg, "model_name", "-")), file=human)
        print("日志：%s" % (cfg.log_path or "（内存）"), file=human)
        print(file=human)
        print("--- 逐步轨迹 ---", file=human)
        for item in report.trace:
            print("[step %s] risk=%s extraction=%s coord_rounded=%s" % (
                item.get("step"), item.get("risk"), item.get("extraction"), item.get("coord_rounded")),
                file=human)
            print("         thought: %s" % (item.get("thought") or ""), file=human)
            print("         action : %s" % json.dumps(item.get("action"), ensure_ascii=False), file=human)
            print("         result : ok=%s error=%s screen_changed=%s duration=%sms" % (
                item.get("ok"), item.get("error_code"), item.get("screen_changed"), item.get("duration_ms")),
                file=human)
        print(file=human)
        print("--- 报告 ---", file=human)
        for line in report.summary_lines():
            print(line, file=human)
        if isinstance(executor.device, (DryRunDevice, RecordingDevice)):
            print(file=human)
            print("--- 设备操作记录（%s，未触碰真实鼠标/键盘）---" % executor.device.name, file=human)
            for idx, op in enumerate(executor.device.ops, 1):
                print("[%2d] %s" % (idx, json.dumps(op, ensure_ascii=False, sort_keys=True)), file=human)
    if logger is not None:
        logger.close()
    return 0 if report.ok else 1


# ---------------------------------------------------------------------------
# 离线自检
# ---------------------------------------------------------------------------


class Checks:
    def __init__(self) -> None:
        self.rows: List[Tuple[str, str, bool, str]] = []
        self.section = "未命名"

    def begin(self, name: str) -> None:
        self.section = name

    def note(self, text: str) -> None:
        self.rows.append((self.section, text, True, "__NOTE__"))

    def ok(self, name: str, detail: Any = "") -> None:
        self.rows.append((self.section, name, True, "" if detail is None else str(detail)))

    def fail(self, name: str, detail: Any = "") -> None:
        self.rows.append((self.section, name, False, str(detail)))

    def expect(self, name: str, cond: bool, detail: Any = "") -> bool:
        if cond:
            self.ok(name, detail)
        else:
            self.fail(name, detail)
        return bool(cond)

    def check(self, name: str, fn: Callable[[], Any]) -> bool:
        try:
            detail = fn()
            self.ok(name, "" if detail is None or detail is True else detail)
            return True
        except Exception as exc:
            self.fail(name, "%s: %s" % (type(exc).__name__, exc))
            return False

    def render(self) -> Tuple[bool, List[str]]:
        lines: List[str] = []
        current = None
        passed = failed = 0
        for section, name, good, detail in self.rows:
            if section != current:
                current = section
                lines.append("")
                lines.append("== %s ==" % section)
            if detail == "__NOTE__":
                lines.append("      · %s" % name)
                continue
            mark = "PASS" if good else "FAIL"
            if good:
                passed += 1
            else:
                failed += 1
            suffix = ("  -> %s" % detail) if detail else ""
            lines.append("  [%s] %s%s" % (mark, name, suffix))
        lines.append("")
        lines.append("== 结论 ==")
        lines.append("  通过 %d 项，失败 %d 项" % (passed, failed))
        lines.append("  自检完成时间：%s" % time.strftime("%Y-%m-%d %H:%M:%S"))
        lines.append("  说明：本自检全程使用 MockScreenshot + DryRunDevice/RecordingDevice，")
        lines.append("        不截图、不移动鼠标、不按键；真实 pyautogui 仅被导入用于断言 FAILSAFE=True。")
        return failed == 0, lines


def _req(cond: Any, message: str) -> Any:
    if not cond:
        raise AssertionError(message)
    return True


def _step_json(action: Any, **overrides: Any) -> str:
    step = {"protocol": PROTOCOL_VERSION, "thought": "自检用例", "action": action, "done": False}
    step.update(overrides)
    return json.dumps(step, ensure_ascii=False)


def _frame(width: int, height: int, scale: float, origin: Tuple[int, int],
           screen: Tuple[int, int] = (1920, 1080), color: int = 0, label: str = "test") -> CaptureFrame:
    from PIL import Image

    img = Image.new("RGB", (width, height), (color, color, color))
    return CaptureFrame(image=img, origin=origin, scale=scale, screen_size=screen, label=label)


def _e2e(
    script: Sequence[Any],
    cfg: Optional[Config] = None,
    scenario: str = "moving",
    painter: Optional[Callable[[int, int, int], Any]] = None,
    clock: Optional[Callable[[], float]] = None,
    real_sleep: bool = False,
    allow_high_risk: bool = False,
):
    cfg = cfg or Config()
    provider = MockScreenshot(cfg, scenario=scenario, painter=painter)
    device = DryRunDevice(size=(1920, 1080))
    sleep_fn = (lambda s: time.sleep(s)) if real_sleep else (lambda s: None)
    executor = Executor(cfg, provider=provider, device=device, dry_run=True, sleep_fn=sleep_fn)
    logger = MemoryLogger()
    agent = VisionAgent(MockClient(list(script), loop=True), cfg, logger=logger)
    loop = ControlLoop(cfg, agent=agent, executor=executor, provider=provider, logger=logger,
                       clock=clock, sleep_fn=sleep_fn, allow_high_risk=allow_high_risk)
    report = loop.run("自检任务")
    return report, logger, device, provider, executor


def _mock_cfg(**kw: Any) -> Config:
    base = Config(use_mock_model=True, use_mock_screen=True, dry_run=True, log_path=None)
    base.log_path = None
    return base.with_(**kw)


def run_selfcheck(args: argparse.Namespace) -> Tuple[bool, List[str]]:
    c = Checks()
    header: List[str] = []
    header.append("=== CUA 离线自检（协议 %s，cua %s）===" % (PROTOCOL_VERSION, __version__))
    header.append("工作目录：%s" % os.getcwd())
    header.append("Python：%s" % sys.version.replace("\n", " "))
    header.append("模式：无显示器 / 不动真实鼠标 / 不截图（MockScreenshot + DryRunDevice）")
    header.append("建议对照命令：python -m cua.main --selfcheck")
    header.append("")

    # ---------------- 1 环境 ----------------
    c.begin("1 环境与依赖")
    c.expect("Python >= 3.10", sys.version_info >= (3, 10), sys.version.split()[0])

    def _deps():
        import PIL
        import numpy

        import pyautogui

        pyautogui.FAILSAFE = True
        return "pyautogui=%s FAILSAFE=%s Pillow=%s numpy=%s" % (
            pyautogui.__version__, pyautogui.FAILSAFE, PIL.__version__, numpy.__version__)

    c.check("pyautogui / Pillow / numpy 可用且 FAILSAFE=True", _deps)

    def _jsonschema():
        import importlib.metadata as md

        try:
            return "jsonschema=%s（仅用于交叉校验，不是硬依赖）" % md.version("jsonschema")
        except Exception:
            return "未安装 jsonschema（自研校验子集独立工作）"

    c.check("jsonschema 可选依赖状态", _jsonschema)

    def _schema_file():
        p = default_schema_path()
        _req(p.is_file(), "契约文件不存在：%s" % p)
        return str(p)

    c.check("冻结契约 action.schema.json 存在", _schema_file)
    c.note("真实鼠标/键盘全程未被调用（设备为 DryRunDevice / RecordingDevice）")
    c.note("网络未被调用（无 API 探针；模型为 MockClient）")

    # ---------------- 2 契约一致性 ----------------
    c.begin("2 契约一致性")
    schema = load_schema()
    branches = (schema.get("properties") or {}).get("action", {}).get("oneOf", [])

    def _actions_match():
        consts = sorted((b.get("properties") or {}).get("type", {}).get("const") for b in branches)
        _req(consts == sorted(ACTION_TYPES), "schema oneOf 分支 %s 与 ACTION_TYPES %s 不一致" % (consts, ACTION_TYPES))
        return "9 种动作与 schema oneOf 逐项一致"

    c.check("动作类型与 schema 一致", _actions_match)

    def _error_codes_in_doc():
        doc = Path("docs/cua/03-action-protocol.md")
        _req(doc.is_file(), "缺少协议文档")
        text = doc.read_text(encoding="utf-8")
        missing = [code for code in ERROR_CODES if code not in text]
        _req(not missing, "协议文档未出现：%s" % missing)
        return "%d 个错误码全部出现在冻结契约第 5 节" % len(ERROR_CODES)

    c.check("错误码表与协议文档逐条对齐", _error_codes_in_doc)

    def _utf8_event_roundtrip():
        import subprocess

        root = Path(__file__).resolve().parents[1]
        task = "中文任务🙂 含 emoji：新建一个名为测试的文件夹"
        env = dict(os.environ)
        # 故意清掉这两个变量：复现「管道下默认 gbk」的真实环境（cua-go 侧才设它们）
        for key in ("PYTHONIOENCODING", "PYTHONUTF8"):
            env.pop(key, None)
        proc = subprocess.run(
            [sys.executable, "-m", "cua.main", "--dry-run", "--mock", "--no-log",
             "--events", "stdout", "--task", task],
            cwd=str(root), capture_output=True, env=env,
        )
        _req(proc.returncode == 0,
             "子进程退出码 %s：%s" % (proc.returncode,
                                   proc.stderr.decode("utf-8", errors="replace")[-200:]))
        raw = proc.stdout
        try:
            text = raw.decode("utf-8", errors="strict")
        except UnicodeDecodeError as exc:
            raise AssertionError("stdout 不是合法 UTF-8：%s" % exc)
        _req("\ufffd" not in text, "stdout 解码后含 U+FFFD %d 个" % text.count("\ufffd"))
        events = [json.loads(line) for line in text.splitlines() if line.strip()]
        starts = [e for e in events if e.get("kind") == "run_start"]
        _req(len(starts) == 1, "run_start 数量异常：%d" % len(starts))
        _req(starts[0].get("task") == task,
             "run_start.task 与提交任务不逐码点相同：%r" % (starts[0].get("task"),))
        thoughts = [e.get("thought") for e in events if e.get("kind") == "step" and e.get("thought")]
        # _demo_script() 第 1 条带前言 + 代码围栏，必须用协议自己的宽进提取器
        expected = [extract_json(raw_step).value.get("thought") for raw_step in _demo_script()]
        _req(bool(thoughts) and thoughts[0] == expected[0],
             "step.thought 码点不一致：%r != %r" % (thoughts[:1], expected[:1]))
        return "%d 字节严格 UTF-8；task 与 %d 个中文 thought 逐码点相同，U+FFFD=0" % (
            len(raw), len(thoughts))

    c.check("事件流 UTF-8 往返（task-21）", _utf8_event_roundtrip)

    def _prompt_alignment():
        from .prompts import PLACEHOLDER_SYSTEM_PROMPT, extract_prompt_block, prompt_doc_path, system_prompt

        text, source = system_prompt()
        doc = prompt_doc_path()
        if not doc.is_file():
            _req(text == PLACEHOLDER_SYSTEM_PROMPT, "缺少 02 文档时应使用占位提示词")
            return "02-agent-prompt.md 尚未产出，使用 cua/prompts.py 占位（来源：%s）" % source
        block = extract_prompt_block(doc.read_text(encoding="utf-8"))
        _req(block is not None, "02 文档缺少 text 代码块（第 1 节系统提示词）")
        _req(text == block, "系统提示词与 02 文档第 1 节 text 代码块不一致")
        _req(source == str(doc), "提示词来源应为 02 文档，实际 %s" % source)
        return "系统提示词逐字取自 %s 第 1 节 text 代码块（%d 字）" % (doc.name, len(text))

    c.check("系统提示词与 02-agent-prompt.md 对齐", _prompt_alignment)
    c.expect("错误码总数为 21", len(ERROR_CODES) == 21, len(ERROR_CODES))

    def _policy_table():
        checks = [
            (E_BAD_JSON, True, 2, "retry"),
            (E_SCHEMA, True, 2, "retry"),
            (E_UNKNOWN_ACTION, False, 0, "stop"),
            (E_ARG_RANGE, True, 1, "retry"),
            (E_OUT_OF_BOUNDS, True, 1, "retry"),
            (E_KEY_NOT_ALLOWED, True, 1, "retry"),
            ("E_NO_FOCUS", True, 1, "retry"),
            (E_MODEL_TIMEOUT, True, 2, "retry"),
            (E_MODEL_REFUSAL, False, 0, "human"),
            (E_MODEL_EMPTY, False, 0, "human"),
            (E_EXECUTOR_FAILSAFE, False, 0, "human"),
            (E_EXPECT_TIMEOUT, True, 1, "retry"),
            (E_POPUP_BLOCKING, True, 2, "retry"),
            (E_STALL, False, 0, "replan"),
            (E_LOOP, False, 0, "replan"),
            (E_STEP_BUDGET, False, 0, "budget"),
            (E_WALL_BUDGET, False, 0, "budget"),
            (E_CANCEL, False, 0, "stop"),
            (E_NEED_HUMAN, False, 0, "human"),
            (E_INTERNAL, False, 0, "stop"),
        ]
        bad = []
        for code, retryable, limit, category in checks:
            pol = ERROR_TABLE[code]
            if (pol.retryable, pol.max_retries, pol.category) != (retryable, limit, category):
                bad.append("%s -> %s" % (code, pol))
        _req(not bad, "策略表不符：%s" % bad)
        return "重试上限与分类逐条核对通过（E_MODEL_HTTP 单独按状态码判定）"

    c.check("错误策略表（可重试/上限/分类）", _policy_table)
    c.expect("E_MODEL_HTTP 5xx 可重试", is_retryable("E_MODEL_HTTP", 503) is True)
    c.expect("E_MODEL_HTTP 4xx 不可重试", is_retryable("E_MODEL_HTTP", 404) is False)
    c.expect("E_STALL 属于重规划而非重试", is_replan(E_STALL) and not is_retryable(E_STALL))
    c.expect("E_LOOP 属于重规划而非重试", is_replan(E_LOOP) and not is_retryable(E_LOOP))

    # ---------------- 3 宽进提取 ----------------
    c.begin("3 Step 提取（协议 2.1 宽进）")
    ok_action = {"type": "wait", "ms": 500}
    payload = _step_json(ok_action)

    def _extract_direct():
        r = extract_json(payload)
        _req(r.method == "direct", "方法应为 direct，实际 %s" % r.method)
        return "全文直解"

    c.check("全文直解", _extract_direct)

    def _extract_fence():
        r = extract_json("说明文字\n" + _FENCE + "json\n" + payload + "\n" + _FENCE + "\n后面的解释")
        _req(r.method == "fence", "方法应为 fence，实际 %s" % r.method)
        _req(r.value["action"]["type"] == "wait", "内容错误")
        return "剥代码围栏（含前后说明文字）"

    c.check("剥代码围栏", _extract_fence)

    def _extract_balanced():
        text = "我的想法是 " + payload + " 就这些"
        r = extract_json(text)
        _req(r.method == "balanced", "方法应为 balanced，实际 %s" % r.method)
        return "第一个平衡对象扫描"

    c.check("平衡括号扫描", _extract_balanced)

    def _extract_braces_in_string():
        tricky = _step_json({"type": "type", "text": "a {b} c \"d\" } {", "submit": False})
        r = extract_json("前言 " + tricky + " 后记")
        _req(r.value["action"]["text"] == "a {b} c \"d\" } {", "字符串内的花括号被误判")
        return "正确跳过字符串内的花括号与转义"

    c.check("字符串内花括号/转义", _extract_braces_in_string)

    def _extract_escape():
        tricky = json.dumps({"protocol": PROTOCOL_VERSION, "thought": "t",
                             "action": {"type": "type", "text": "back\\slash \"quote\""},
                             "done": False})
        r = extract_json("前缀 " + tricky + " 后缀")
        _req("back" in r.value["action"]["text"], "转义解析失败")
        return "反斜杠与引号转义"

    c.check("反斜杠/引号转义", _extract_escape)

    def _extract_bad():
        for bad in ("", "   ", "完全不是 JSON", "{未闭合", None):
            try:
                extract_json(bad)
            except CuaError as exc:
                if exc.code != E_BAD_JSON:
                    raise AssertionError("错误码应为 E_BAD_JSON，实际 %s" % exc.code)
                continue
            raise AssertionError("未报错：%r" % (bad,))
        return "空/无 JSON/未闭合 -> E_BAD_JSON"

    c.check("提取失败报 E_BAD_JSON", _extract_bad)

    # ---------------- 4 Schema 校验 ----------------
    c.begin("4 Schema 校验（协议第 2、3 节）")
    valid_actions = [
        {"type": "click", "x": 100, "y": 80, "button": "right", "clicks": 2, "modifiers": ["ctrl", "shift"]},
        {"type": "type", "text": "hello", "clear_first": True, "submit": True, "interval_ms": 20,
         "target": "focused"},
        {"type": "hotkey", "keys": ["ctrl", "shift", "s"], "repeat": 2},
        {"type": "drag", "from": {"x": 1, "y": 2}, "to": {"x": 30, "y": 40}, "duration_ms": 600,
         "button": "left", "modifiers": ["alt"]},
        {"type": "scroll", "x": 960, "y": 540, "dx": 0, "dy": -3, "unit": "line"},
        {"type": "move", "x": 300, "y": 400, "duration_ms": 200},
        {"type": "wait", "ms": 1500, "reason": "等待加载"},
        {"type": "screenshot", "region": {"x": 10, "y": 10, "width": 200, "height": 120}},
        {"type": "done", "status": "success", "summary": "完成", "artifacts": ["a.txt"]},
    ]

    def _valid_all():
        for action in valid_actions:
            if action["type"] == "done":
                text = _step_json(action, done=True, final={"status": "success", "summary": "完成"})
            else:
                text = _step_json(action)
            parsed = parse_step(text, cfg=Config())
            _req(parsed.action["type"] == action["type"], "动作类型不符")
        return "9 种动作全部通过校验"

    c.check("9 种动作合法样例全部通过", _valid_all)

    unknown_extra = _step_json({"type": "wait", "ms": 300, "x-future": {"a": 1}}, future_field="yes")

    def _unknown_fields():
        parsed = parse_step(unknown_extra, cfg=Config())
        _req("future_field" in parsed.meta["ignored_fields"], "未知顶层字段未被记录")
        return "未知字段被忽略（向前兼容），并记录到 meta：%s" % parsed.meta["ignored_fields"]

    c.check("未知字段忽略", _unknown_fields)

    def _coord_rounded():
        parsed = parse_step(_step_json({"type": "click", "x": 640.4, "y": 210.6}), cfg=Config())
        _req(parsed.action["x"] == 640 and parsed.action["y"] == 211, "小数坐标未四舍五入")
        _req(parsed.meta["coord_rounded"] is True, "meta.coord_rounded 未置 True")
        return "640.4 -> 640，210.6 -> 211，meta.coord_rounded=True"

    c.check("小数坐标宽容取整", _coord_rounded)

    invalid_cases: List[Tuple[str, str, str]] = [
        ("缺 protocol", json.dumps({"thought": "t", "action": {"type": "wait", "ms": 500}, "done": False}),
         E_SCHEMA),
        ("协议版本错误", _step_json({"type": "wait", "ms": 500}, protocol="cua/2"), E_SCHEMA),
        ("thought 为空", _step_json({"type": "wait", "ms": 500}, thought=""), E_SCHEMA),
        ("thought 超长", _step_json({"type": "wait", "ms": 500}, thought="x" * 1201), E_SCHEMA),
        ("clicks=3", _step_json({"type": "click", "x": 1, "y": 1, "clicks": 3}), E_SCHEMA),
        ("modifiers 4 个", _step_json({"type": "click", "x": 1, "y": 1,
                                       "modifiers": ["ctrl", "alt", "shift", "win"]}), E_SCHEMA),
        ("scroll dy=99", _step_json({"type": "scroll", "x": 1, "y": 1, "dy": 99}), E_ARG_RANGE),
        ("wait ms=50", _step_json({"type": "wait", "ms": 50}), E_ARG_RANGE),
        ("hotkey keys=[]", _step_json({"type": "hotkey", "keys": []}), E_SCHEMA),
        ("screenshot region 宽 10", _step_json({"type": "screenshot",
                                              "region": {"x": 0, "y": 0, "width": 10, "height": 10}}),
         E_ARG_RANGE),
        ("done=true 缺 final", json.dumps({"protocol": PROTOCOL_VERSION, "thought": "t",
                                           "action": {"type": "done", "status": "success", "summary": "s"},
                                           "done": True}), E_SCHEMA),
        ("未知 action.type", _step_json({"type": "exec", "cmd": "dir"}), E_UNKNOWN_ACTION),
        ("顶层是数组", "[1, 2, 3]", E_SCHEMA),
        ("click 缺 y", _step_json({"type": "click", "x": 1}), E_SCHEMA),
        ("interval_ms=999", _step_json({"type": "type", "text": "a", "interval_ms": 999}), E_ARG_RANGE),
        ("done=false 且没有 action", json.dumps({"protocol": PROTOCOL_VERSION, "thought": "t", "done": False}),
         E_SCHEMA),
    ]

    def _invalid_all():
        mismatched = []
        for label, text, want in invalid_cases:
            try:
                parse_step(text, cfg=Config())
            except CuaError as exc:
                if exc.code != want:
                    mismatched.append("%s：期望 %s 实际 %s（%s）" % (label, want, exc.code, exc.message[:60]))
                continue
            mismatched.append("%s：未报错" % label)
        _req(not mismatched, "; ".join(mismatched))
        return "%d 条非法样例全部按预期报错" % len(invalid_cases)

    c.check("非法样例错误码映射", _invalid_all)

    def _cross_jsonschema():
        if validate_with_jsonschema({}) is None:
            return "jsonschema 未安装，跳过交叉校验（自研子集仍是唯一权威路径）"
        disagree = []
        samples: List[Tuple[str, Any, bool]] = []
        for action in valid_actions:
            if action["type"] == "done":
                text = _step_json(action, done=True, final={"status": "success", "summary": "完成"})
            else:
                text = _step_json(action)
            samples.append(("valid-" + action["type"], json.loads(text), True))
        for label, text, _ in invalid_cases:
            try:
                value = json.loads(text)
            except Exception:
                continue
            samples.append(("invalid-" + label, value, False))
        for label, value, expect_valid in samples:
            mine = not MiniValidator(schema).validate(value)
            theirs = not validate_with_jsonschema(value, schema)
            if bool(mine) != bool(theirs) or bool(mine) != bool(expect_valid):
                disagree.append("%s：自研=%s jsonschema=%s 期望=%s" % (label, mine, theirs, expect_valid))
        _req(not disagree, "; ".join(disagree))
        return "自研校验子集与 jsonschema Draft2020-12 判定一致（%d 个样例）" % len(samples)

    c.check("与 jsonschema 交叉校验", _cross_jsonschema)

    def _terminal_ruling():
        # 2026-10-11 裁定：action 恒为必填动作对象，不可为 null
        for label, text in [
            ("done=false", json.dumps({"protocol": PROTOCOL_VERSION, "thought": "t", "action": None,
                                       "done": False})),
            ("done=true 且给 final",
             json.dumps({"protocol": PROTOCOL_VERSION, "thought": "t", "action": None, "done": True,
                         "final": {"status": "success", "summary": "s"}})),
        ]:
            try:
                parse_step(text, cfg=Config())
                raise AssertionError("action=null（%s）应报 E_SCHEMA" % label)
            except CuaError as exc:
                _req(exc.code == E_SCHEMA, "action=null（%s）期望 E_SCHEMA 实际 %s" % (label, exc.code))
        # done=true 缺 final -> E_SCHEMA（Schema allOf）
        try:
            parse_step(_step_json({"type": "done", "status": "success", "summary": "s"}, done=True),
                       cfg=Config())
            raise AssertionError("done=true 缺 final 应报 E_SCHEMA")
        except CuaError as exc:
            _req(exc.code == E_SCHEMA, "缺 final 期望 E_SCHEMA 实际 %s" % exc.code)
        # action.type=done 与 final 一致 -> 通过
        parse_step(json.dumps({"protocol": PROTOCOL_VERSION, "thought": "t", "done": True,
                               "action": {"type": "done", "status": "success", "summary": "s"},
                               "final": {"status": "success", "summary": "s"}}), cfg=Config())
        # 不一致 -> E_SCHEMA
        try:
            parse_step(json.dumps({"protocol": PROTOCOL_VERSION, "thought": "t", "done": True,
                                   "action": {"type": "done", "status": "success", "summary": "A"},
                                   "final": {"status": "success", "summary": "B"}}), cfg=Config())
            raise AssertionError("action.type=done 与 final 不一致应报 E_SCHEMA")
        except CuaError as exc:
            _req(exc.code == E_SCHEMA, "不一致期望 E_SCHEMA 实际 %s" % exc.code)
        # done=true 配非 done 动作 + final -> 合法
        parsed = parse_step(json.dumps({"protocol": PROTOCOL_VERSION, "thought": "t", "done": True,
                                        "action": {"type": "click", "x": 10, "y": 10},
                                        "final": {"status": "success", "summary": "s"}}), cfg=Config())
        _req(parsed.action["type"] == "click", "done=true 配 click 应被接受")
        _req("strict_schema" not in Config().__dataclass_fields__, "兼容开关 strict_schema 应已移除")
        return "action=null 恒 E_SCHEMA；缺 final E_SCHEMA；done/final 不一致 E_SCHEMA；done+任意动作合法"

    c.check("终态字段裁定（2026-10-11）", _terminal_ruling)

    # ---------------- 5 坐标换算 ----------------
    c.begin("5 坐标换算（协议 1.1）")
    full = _frame(1472, 828, 1472 / 1920.0, (0, 0))
    region = _frame(800, 600, 2.0, (100, 50))

    def _coord_protocol_example():
        pt = image_to_screen(640, 210, full)
        _req(pt == (835, 274), "协议样例应得 (835,274)，实际 %s" % (pt,))
        return "image(640,210) + scale=0.7667 -> screen(835,274)，与协议第 4 节样例一致"

    c.check("协议第 4 节换算样例", _coord_protocol_example)

    def _coord_region():
        pt = image_to_screen(10, 20, region)
        _req(pt == (105, 60), "局部帧换算应为 (105,60)，实际 %s" % (pt,))
        back = screen_to_image(105, 60, region)
        _req(back == (10, 20), "逆换算应回到 (10,20)，实际 %s" % (back,))
        return "局部帧 origin=(100,50) scale=2.0：(10,20) -> (105,60) 且可逆"

    c.check("局部帧 origin/scale 换算与逆换算", _coord_region)

    def _round_half_up_cases():
        cases = [(0.5, 1), (1.5, 2), (2.5, 3), (-0.5, 0), (-1.5, -1), (-2.5, -2)]
        bad = [(v, round_half_up(v), want) for v, want in cases if round_half_up(v) != want]
        _req(not bad, "四舍五入不一致：%s" % bad)
        return "round_half_up 对 .5 一律向上（int(floor(x+0.5))）"

    c.check("round_half_up 确定性", _round_half_up_cases)

    def _region_to_screen():
        rect = image_rect_to_screen_rect((10, 20, 100, 60), region, what="region")
        _req(rect == (105, 60, 50, 30), "局部截图区域换算错误：%s" % (rect,))
        return "image 空间 region -> 屏幕矩形 %s" % (rect,)

    c.check("局部截图区域换算", _region_to_screen)

    # ---------------- 6 越界拒绝 ----------------
    c.begin("6 越界拒绝（绝不 clamp，协议 1.1 / 8.2）")
    bounds_frame = _frame(1472, 828, 1.0, (0, 0), screen=(1472, 828))
    for label, x, y in [("x=width", 1472, 10), ("x=-1", -1, 10),
                        ("y=height", 10, 828), ("y=-1", 10, -1)]:
        def _bounds_case(x=x, y=y, label=label):
            try:
                to_screen_point(x, y, bounds_frame)
            except CuaError as exc:
                _req(exc.code == E_OUT_OF_BOUNDS, "期望 E_OUT_OF_BOUNDS 实际 %s" % exc.code)
                return "拒绝 %s：%s" % (label, exc.message)
            raise AssertionError("%s 未被拒绝" % label)

        c.check("越界拒绝 %s" % label, _bounds_case)

    def _bounds_message():
        err = _capture_error(lambda: to_screen_point(1600, 100, _frame(1472, 828, 1.0, (0, 0)), what="坐标"))
        _req("x=1600" in err.message, "错误消息应回显实际值：%s" % err.message)
        _req("1472" in err.message, "错误消息应给出当前图像宽度：%s" % err.message)
        return err.message

    c.check("越界错误消息回显实际值与图像尺寸", _bounds_message)

    def _executor_bounds_noop():
        cfg = _mock_cfg()
        provider = MockScreenshot(cfg)
        device = RecordingDevice()
        executor = Executor(cfg, provider=provider, device=device, dry_run=True, sleep_fn=lambda s: None)
        frame = provider.capture()
        outcome = executor.execute(json.loads(_step_json({"type": "click", "x": 99999, "y": 10})), frame)
        _req(outcome.result["ok"] is False, "越界动作不应 ok")
        _req(outcome.result["error"]["code"] == E_OUT_OF_BOUNDS, "错误码不符")
        _req(device.ops == [], "越界时不应产生任何设备操作，实际 %s" % device.ops)
        return "越界点击未产生任何设备操作（绝不 clamp）"

    c.check("越界时不产生设备操作", _executor_bounds_noop)

    class _BlindProvider:
        """故意不做任何越界校验的提供者：用来证明拒绝发生在执行器内部。"""

        def __init__(self, base):
            self.base = base
            self.calls = []

        def capture(self, region=None, base_frame=None):
            self.calls.append(region)
            return self.base.capture()

    def _executor_region_bounds():
        cfg = _mock_cfg()
        mock = MockScreenshot(cfg)
        provider = _BlindProvider(mock)
        device = RecordingDevice()
        executor = Executor(cfg, provider=provider, device=device, dry_run=True, sleep_fn=lambda s: None)
        frame = mock.capture()
        step = json.loads(_step_json({"type": "screenshot",
                                      "region": {"x": frame.width - 10, "y": 10, "width": 100, "height": 100}}))
        outcome = executor.execute(step, frame)
        _req(outcome.result["error"]["code"] == E_OUT_OF_BOUNDS,
             "区域越界应报 E_OUT_OF_BOUNDS，实际 %s" % outcome.result["error"])
        _req(provider.calls == [], "越界区域不应被交给截图提供者，实际调用 %s" % provider.calls)
        # 贴边的合法区域必须放行（像素区间 [x, x+width) 允许 x+width == image.width）
        ok_step = json.loads(_step_json({"type": "screenshot",
                                         "region": {"x": frame.width - 100, "y": frame.height - 100,
                                                    "width": 100, "height": 100}}))
        ok_outcome = executor.execute(ok_step, frame)
        _req(ok_outcome.ok, "贴边合法区域被误拒：%s" % ok_outcome.result.get("error"))
        _req(len(provider.calls) == 1, "合法区域应恰好调用提供者一次")
        return "越界区域由执行器自己拒绝（未触达 provider）；贴边合法区域（x+width==image.width）放行"

    c.check("局部截图区域越界由执行器拒绝", _executor_region_bounds)

    # ---------------- 7 执行器 ----------------
    c.begin("7 执行器（9 种动作 / 安全不变量）")

    def _exec_all_actions():
        cfg = _mock_cfg()
        provider = MockScreenshot(cfg, scenario="moving")
        device = RecordingDevice()
        executor = Executor(cfg, provider=provider, device=device, dry_run=True, sleep_fn=lambda s: None)
        frame = provider.capture()
        plan = [
            ({"type": "click", "x": 100, "y": 80, "button": "left", "clicks": 1}, "click"),
            ({"type": "type", "text": "abc", "submit": True}, "set_clipboard"),
            ({"type": "hotkey", "keys": ["ctrl", "s"], "repeat": 1}, "hotkey"),
            ({"type": "drag", "from": {"x": 10, "y": 10}, "to": {"x": 100, "y": 100}, "duration_ms": 600}, "mouse_up"),
            ({"type": "scroll", "x": 500, "y": 400, "dy": -3, "unit": "line"}, "scroll"),
            ({"type": "move", "x": 300, "y": 400, "duration_ms": 200}, "move_to"),
            ({"type": "wait", "ms": 120}, "__sleep__"),
            ({"type": "screenshot", "region": None}, "__capture__"),
            ({"type": "done", "status": "success", "summary": "done"}, "__noop__"),
        ]
        for action, expect_op in plan:
            device.clear()
            if action["type"] == "done":
                text = _step_json(action, done=True, final={"status": "success", "summary": "done"})
            else:
                text = _step_json(action)
            step = json.loads(text)
            outcome = executor.execute(step, frame)
            if not outcome.ok:
                raise AssertionError("动作 %s 执行失败：%s" % (action["type"], outcome.result.get("error")))
            if expect_op == "__capture__":
                _req(outcome.frame is not None, "screenshot 未返回新帧")
                continue
            if expect_op == "__noop__":
                continue
            if expect_op == "__sleep__":
                continue
            ops = [o["op"] for o in device.ops]
            _req(expect_op in ops, "动作 %s 未产生 %s，实际 %s" % (action["type"], expect_op, ops))
        return "9 种动作均可执行并产生预期设备操作"

    c.check("9 种动作逐个执行", _exec_all_actions)

    def _result_shape():
        cfg = _mock_cfg()
        provider = MockScreenshot(cfg)
        device = RecordingDevice()
        executor = Executor(cfg, provider=provider, device=device, dry_run=True, sleep_fn=lambda s: None)
        frame = provider.capture()
        outcome = executor.execute(json.loads(_step_json({"type": "click", "x": 10, "y": 10},
                                                       step=7, risk="low")), frame)
        keys = set(outcome.result.keys())
        _req({"ok", "step", "action", "started_at", "duration_ms", "error", "observed", "meta"} <= keys,
             "ActionResult 字段缺失：%s" % sorted(keys))
        _req(set(outcome.result["observed"]) == {"screen_changed", "change_ratio", "cursor"},
             "observed 字段不符：%s" % sorted(outcome.result["observed"]))
        _req(outcome.result["step"] == 7, "step 未回传")
        return "ActionResult 字段与协议第 4 节一致"

    c.check("ActionResult 结构", _result_shape)

    def _modifier_balance():
        cfg = _mock_cfg()
        provider = MockScreenshot(cfg)
        device = RecordingDevice()
        executor = Executor(cfg, provider=provider, device=device, dry_run=True, sleep_fn=lambda s: None)
        frame = provider.capture()
        executor.execute(json.loads(_step_json({"type": "click", "x": 10, "y": 10,
                                                "modifiers": ["ctrl", "shift"]})), frame)
        downs = [o["key"] for o in device.ops_of("key_down")]
        ups = [o["key"] for o in device.ops_of("key_up")]
        _req(sorted(downs) == sorted(ups) == ["ctrl", "shift"], "修饰键未成对：down=%s up=%s" % (downs, ups))
        _req(executor._held == [], "执行后仍有按住未释放的键：%s" % executor._held)
        return "修饰键 key_down/key_up 成对且执行后无残留"

    c.check("修饰键成对释放", _modifier_balance)

    def _release_on_error():
        cfg = _mock_cfg()
        provider = MockScreenshot(cfg)
        device = RecordingDevice()
        executor = Executor(cfg, provider=provider, device=device, dry_run=True, sleep_fn=lambda s: None)
        frame = provider.capture()
        device.fail_on = "click"   # 让 ctrl 真的按下之后再触发 FAILSAFE
        outcome = executor.execute(json.loads(_step_json({"type": "click", "x": 10, "y": 10,
                                                          "modifiers": ["ctrl"]})), frame)
        _req(outcome.result["error"]["code"] == E_EXECUTOR_FAILSAFE, "FAILSAFE 未映射成 E_EXECUTOR_FAILSAFE")
        ups = [o["key"] for o in device.ops_of("key_up")]
        _req("ctrl" in ups, "异常路径未释放 ctrl：%s" % ups)
        return "FAILSAFE -> E_EXECUTOR_FAILSAFE，且 ctrl 已释放"

    c.check("异常路径释放修饰键", _release_on_error)

    def _drag_release_on_error():
        cfg = _mock_cfg()
        provider = MockScreenshot(cfg)
        device = RecordingDevice()
        executor = Executor(cfg, provider=provider, device=device, dry_run=True, sleep_fn=lambda s: None)
        frame = provider.capture()
        original = device.move_to

        def boom(x, y, duration=0.0):
            if duration and duration > 0:
                raise RuntimeError("模拟拖拽中途失败")
            return original(x, y, duration)

        device.move_to = boom  # type: ignore
        outcome = executor.execute(json.loads(_step_json(
            {"type": "drag", "from": {"x": 10, "y": 10}, "to": {"x": 200, "y": 200},
             "duration_ms": 300})), frame)
        _req(outcome.result["ok"] is False, "拖动失败应不 ok")
        _req([o["op"] for o in device.ops_of("mouse_down")] == ["mouse_down"], "mouse_down 缺失")
        _req([o["op"] for o in device.ops_of("mouse_up")] == ["mouse_up"], "失败时未抬起鼠标")
        return "拖拽中途异常也会 mouse_up"

    c.check("拖拽异常仍抬起鼠标", _drag_release_on_error)

    def _hotkey_rules():
        cfg = _mock_cfg()
        provider = MockScreenshot(cfg)
        device = RecordingDevice()
        executor = Executor(cfg, provider=provider, device=device, dry_run=True, sleep_fn=lambda s: None)
        frame = provider.capture()
        bad_key = executor.execute(json.loads(_step_json({"type": "hotkey", "keys": ["f13"]})), frame)
        _req(bad_key.result["error"]["code"] == E_KEY_NOT_ALLOWED, "白名单外键名未拒绝")
        dangerous = executor.execute(json.loads(_step_json({"type": "hotkey", "keys": ["alt", "f4"]})), frame)
        _req(dangerous.result["error"]["code"] == E_KEY_NOT_ALLOWED, "危险组合键未拦截")
        allow_cfg = cfg.with_(allow_dangerous_hotkeys=True)
        executor2 = Executor(allow_cfg, provider=provider, device=device, dry_run=True, sleep_fn=lambda s: None)
        allowed = executor2.execute(json.loads(_step_json({"type": "hotkey", "keys": ["alt", "f4"]})), frame)
        _req(allowed.ok, "显式 allowlist 后应放行")
        return "白名单外与危险组合键被拦截；allowlist 打开后放行"

    c.check("键名白名单与危险组合键", _hotkey_rules)

    def _type_paths():
        cfg = _mock_cfg()
        provider = MockScreenshot(cfg)
        device = RecordingDevice()
        executor = Executor(cfg, provider=provider, device=device, dry_run=True, sleep_fn=lambda s: None)
        frame = provider.capture()
        device.clear()
        executor.execute(json.loads(_step_json({"type": "type", "text": "abc", "submit": True,
                                               "target": {"x": 5, "y": 5}})), frame)
        ops = [o["op"] for o in device.ops]
        _req("click" in ops and "press" in ops, "target 未先点击或 submit 未回车：%s" % ops)
        return "target/submit 路径正确（输入路径见下面 3 项 task-25 自检）"

    c.check("type target/submit 路径", _type_paths)

    def _type_ascii_clipboard():
        cfg = _mock_cfg()
        provider = MockScreenshot(cfg)
        device = RecordingDevice()
        executor = Executor(cfg, provider=provider, device=device, dry_run=True, sleep_fn=lambda s: None)
        frame = provider.capture()
        device.clear()
        outcome = executor.execute(json.loads(_step_json({"type": "type", "text": "hello from CUA"})), frame)
        ops = [o["op"] for o in device.ops]
        _req("set_clipboard" in ops, "ASCII 也应先写剪贴板：%s" % ops)
        _req("write" not in ops, "ASCII 不应再走逐字符 write：%s" % ops)
        _req(any(o["op"] == "press" and o.get("key") == "v" for o in device.ops),
             "ASCII 未走 Ctrl+V：%s" % ops)
        _req(outcome.result["meta"]["type_path"] == "clipboard",
             "type_path 应为 clipboard，实际 %s" % outcome.result["meta"].get("type_path"))
        device.clear()
        ex2 = executor.execute(json.loads(_step_json({"type": "type", "text": "中文输入"})), frame)
        _req(ex2.result["meta"]["type_path"] == "clipboard", "中文仍应走剪贴板")
        return "ASCII 与中文都走剪贴板（set_clipboard -> ctrl+v），不再逐字符 write"

    c.check("type ASCII 也走剪贴板（task-25）", _type_ascii_clipboard)

    def _type_clipboard_fallback():
        class _NoClipboard(RecordingDevice):
            def set_clipboard(self, text: str) -> Optional[str]:
                return None

            def get_clipboard(self) -> Optional[str]:
                return None

        cfg = _mock_cfg()
        provider = MockScreenshot(cfg)
        frame = provider.capture()
        device = _NoClipboard()
        executor = Executor(cfg, provider=provider, device=device, dry_run=True, sleep_fn=lambda s: None)
        outcome = executor.execute(json.loads(_step_json({"type": "type", "text": "hello from CUA"})), frame)
        ops = [o["op"] for o in device.ops]
        _req(ops == ["write"], "剪贴板不可用时应退化为整段 write：%s" % ops)
        _req(outcome.result["meta"]["type_path"] == "write",
             "退化时 type_path 应为 write，实际 %s" % outcome.result["meta"].get("type_path"))
        cfg2 = cfg.with_(type_use_clipboard_for_unicode=False)
        device2 = RecordingDevice()
        executor2 = Executor(cfg2, provider=provider, device=device2, dry_run=True, sleep_fn=lambda s: None)
        out2 = executor2.execute(json.loads(_step_json({"type": "type", "text": "abc"})), frame)
        _req(out2.result["meta"]["type_path"] == "write", "开关关闭时应走 write 退化路径")
        return "剪贴板不可用 / 开关关闭 -> 逐字符 write（type_path=write）"

    c.check("剪贴板不可用时退化", _type_clipboard_fallback)

    def _type_clear_first():
        cfg = _mock_cfg()
        provider = MockScreenshot(cfg)
        device = RecordingDevice()
        executor = Executor(cfg, provider=provider, device=device, dry_run=True, sleep_fn=lambda s: None)
        frame = provider.capture()
        device.clear()
        executor.execute(json.loads(_step_json({"type": "type", "text": "hello from CUA",
                                               "clear_first": True})), frame)
        ops = [o["op"] for o in device.ops]
        _req(ops[:3] == ["key_down", "press", "key_up"], "clear_first 应先按住 ctrl 再按 a：%s" % ops)
        _req(device.ops[0].get("key") == "ctrl" and device.ops[1].get("key") == "a",
             "clear_first 的第一次按键应为 ctrl+a：%s" % ops)
        _req("set_clipboard" in ops and any(o["op"] == "press" and o.get("key") == "v" for o in device.ops),
             "clear_first 之后仍应粘贴：%s" % ops)
        return "clear_first -> ctrl+a -> 剪贴板粘贴（语义与顺序保持）"

    c.check("clear_first 仍先 ctrl+a", _type_clear_first)

    def _scroll_mapping():
        cfg = _mock_cfg()
        provider = MockScreenshot(cfg)
        device = RecordingDevice()
        executor = Executor(cfg, provider=provider, device=device, dry_run=True, sleep_fn=lambda s: None)
        frame = provider.capture()
        device.clear()
        executor.execute(json.loads(_step_json({"type": "scroll", "x": 10, "y": 10, "dy": -3, "unit": "line"})), frame)
        _req([o["amount"] for o in device.ops_of("scroll")] == [3],
             "协议 dy=-3（向上）应映射为 pyautogui.scroll(+3)：%s" % device.ops)
        device.clear()
        executor.execute(json.loads(_step_json({"type": "scroll", "x": 10, "y": 10, "dx": 2, "unit": "page"})), frame)
        _req([o["amount"] for o in device.ops_of("hscroll")] == [2 * cfg.scroll_page_lines],
             "page 单位应按页行数放大：%s" % device.ops)
        return "dy 正负映射与 page 放大均正确（page=%d 行）" % cfg.scroll_page_lines

    c.check("scroll 方向与单位映射", _scroll_mapping)

    def _pacing():
        cfg = _mock_cfg()
        provider = MockScreenshot(cfg)
        device = RecordingDevice()
        executor = Executor(cfg, provider=provider, device=device, dry_run=True)  # 真实 sleep
        frame = provider.capture()
        outcome = executor.execute(json.loads(_step_json({"type": "move", "x": 10, "y": 10})), frame)
        _req(outcome.result["duration_ms"] >= 150, "步间 sleep 不足 150ms：%sms" % outcome.result["duration_ms"])
        return "实测单步耗时 %sms（>=150ms）" % outcome.result["duration_ms"]

    c.check("每步 sleep >= 150ms（实测计时）", _pacing)

    def _failsafe_always_on():
        import pyautogui

        _req(pyautogui.FAILSAFE is True, "pyautogui.FAILSAFE 必须为 True")
        _req("failsafe" not in [f for f in Config().__dataclass_fields__],
             "FAILSAFE 不应暴露成配置项（协议第 8 节第 1 条）")
        return "FAILSAFE=True 且不是配置项"

    c.check("FAILSAFE 常开且不可配置", _failsafe_always_on)

    def _screenshot_region_zoom():
        # image_max_width=1920 让全屏帧 scale=1.0，便于直接核对放大倍数
        cfg = _mock_cfg(image_max_width=1920)
        provider = MockScreenshot(cfg)
        device = RecordingDevice()
        executor = Executor(cfg, provider=provider, device=device, dry_run=True, sleep_fn=lambda s: None)
        base = provider.capture()
        _req(abs(base.scale - 1.0) < 1e-6, "样例前提失败：全屏帧 scale 应为 1.0，实际 %s" % base.scale)
        outcome = executor.execute(json.loads(_step_json(
            {"type": "screenshot", "region": {"x": 100, "y": 100, "width": 200, "height": 100}})), base)
        new = outcome.frame
        _req(new is not None, "未返回新帧")
        _req(new.label == "region", "新帧应为局部帧")
        _req(abs(new.scale - cfg.screenshot_zoom) < 0.05,
             "局部帧 scale 应约等于放大倍数：%s" % new.scale)
        _req(new.origin == (100, 100), "局部帧 origin 应为区域左上角在屏幕中的位置：%s" % (new.origin,))
        # 换算一致性（协议 1.1）：局部帧里的 (0,0) 必须落回该区域左上角
        _req(image_to_screen(0, 0, new) == (100, 100), "局部帧坐标换算不一致")
        # 若全屏帧本身已被缩小，则 scale 必须等于 放大倍数 * 基准 scale（协议公式）
        cfg2 = _mock_cfg()
        provider2 = MockScreenshot(cfg2)
        executor2 = Executor(cfg2, provider=provider2, device=RecordingDevice(), dry_run=True,
                             sleep_fn=lambda s: None)
        base2 = provider2.capture()
        out2 = executor2.execute(json.loads(_step_json(
            {"type": "screenshot", "region": {"x": 100, "y": 100, "width": 200, "height": 100}})), base2)
        expect2 = cfg2.screenshot_zoom * base2.scale
        _req(abs(out2.frame.scale - expect2) < 0.05,
             "缩小基准下的 scale 应为 zoom*base_scale=%.3f，实际 %.3f" % (expect2, out2.frame.scale))
        return "region -> origin=%s scale=%.3f（= zoom %.1f × 基准 scale %.3f，协议公式一致）" % (
            new.origin, new.scale, cfg.screenshot_zoom, base2.scale)

    c.check("screenshot 局部放大与坐标空间切换", _screenshot_region_zoom)

    # ---------------- 8 错误与重试策略 ----------------
    c.begin("8 错误对象与重试策略")

    def _error_dict():
        err = CuaError(E_OUT_OF_BOUNDS, "x=1600 超出图像宽度 1472",
                       hint="坐标必须落在 image.width/image.height 内")
        d = err.to_dict()
        _req(set(d) == {"code", "message", "retryable", "hint"}, "错误对象字段不符：%s" % sorted(d))
        _req(d["retryable"] is True, "E_OUT_OF_BOUNDS 应可重试")
        return "to_dict 字段与协议第 4 节失败样例一致"

    c.check("CuaError.to_dict", _error_dict)

    def _empty_content_is_model_empty():
        from .backends.openai_compat import require_content, status_error_code
        from .errors import E_MODEL_EMPTY as CODE

        class _Msg:
            def __init__(self, content: Any) -> None:
                self.content = content

        class _Choice:
            def __init__(self, content: Any) -> None:
                self.message = _Msg(content)

        class _Resp:
            def __init__(self, choices: Any) -> None:
                self.choices = choices

        cases = [
            ("content 为空串", _Resp([_Choice("")])),
            ("content 全空白", _Resp([_Choice("   \n\t ")])),
            ("content=None", _Resp([_Choice(None)])),
            ("没有 choices", _Resp([])),
        ]
        bad = []
        for label, resp in cases:
            try:
                require_content(resp, Config())
            except CuaError as exc:
                if exc.code != CODE:
                    bad.append("%s -> %s" % (label, exc.code))
                continue
            bad.append("%s 未报错（空回复流进了后续流程）" % label)
        _req(not bad, "; ".join(bad))
        normal = require_content(_Resp([_Choice('{"protocol": "cua/1", "done": false}')]), Config())
        _req(normal.lstrip().startswith("{"), "正常 content 被误判为空：%r" % normal[:40])
        _req(status_error_code("模型返回空 content（E_MODEL_EMPTY）：多为 max_tokens 过小") == CODE,
             "代理传来的空 content 标记未被识别为 E_MODEL_EMPTY")
        _req(status_error_code("invalid api key") == E_MODEL_HTTP,
             "普通上游错误被误判成 E_MODEL_EMPTY")
        return "空串/全空白/None/缺 choices -> %s；代理标记识别；普通上游错误仍 E_MODEL_HTTP" % CODE

    c.check("空 content 映射为 E_MODEL_EMPTY（契约 §3.1）", _empty_content_is_model_empty)

    def _retry_tracker():
        t = RetryTracker()
        seq = []
        for _ in range(3):
            seq.append(t.decision(E_BAD_JSON))
            t.record(E_BAD_JSON)
        _req(seq == ["retry", "retry", "give_up"], "E_BAD_JSON 重试序列错误：%s" % seq)
        t2 = RetryTracker()
        _req(t2.decision(E_STALL) == "replan", "E_STALL 必须走重规划")
        _req(t2.decision(E_LOOP) == "replan", "E_LOOP 必须走重规划")
        t3 = RetryTracker()
        _req(t3.decision(E_OUT_OF_BOUNDS) == "retry", "E_OUT_OF_BOUNDS 首次应可重试")
        t3.record(E_OUT_OF_BOUNDS)
        _req(t3.decision(E_OUT_OF_BOUNDS) == "give_up", "E_OUT_OF_BOUNDS 只允许 1 次重试")
        return "E_BAD_JSON 2 次 / E_OUT_OF_BOUNDS 1 次 / E_STALL,E_LOOP 直接重规划"

    c.check("RetryTracker 决策", _retry_tracker)
    c.expect("E_SCHEMA 重试上限为 2", max_retries(E_SCHEMA) == 2)
    c.expect("反馈文本要求只输出一个 JSON",
             "只输出一个 JSON 对象" in CuaError(E_BAD_JSON, "坏的").feedback_message())

    # ---------------- 9 弹窗 / 卡死 / 循环 / 破坏性词 ----------------
    c.begin("9 弹窗与卡死检测（协议第 7 节）")
    cfg_pop = _mock_cfg()
    base_img = paint_plain(1920, 1080)
    popup_img = paint_popup(1920, 1080, shift=1)

    def _popup_positive():
        d = diff_frames(base_img, popup_img, cfg_pop)
        _req(d.popup_suspected, "模态框样张应被判为疑似弹窗：%s" % d.to_dict())
        _req(0.03 <= d.bbox_area_ratio <= 0.80, "bbox 面积比不在 3%%~80%%：%s" % d.bbox_area_ratio)
        _req(d.fill_ratio > 0.25, "填充率不足：%s" % d.fill_ratio)
        _req("center_block" in d.signals, "缺少 center_block 信号：%s" % d.signals)
        _req("background_darkened" in d.signals, "缺少 background_darkened 信号：%s" % d.signals)
        return "bbox=%s 面积比=%.3f 填充率=%.2f 信号=%s" % (d.bbox, d.bbox_area_ratio, d.fill_ratio, d.signals)

    c.check("模态框样张判为疑似弹窗", _popup_positive)

    def _popup_negative_same():
        d = diff_frames(base_img, base_img, cfg_pop)
        _req(not d.popup_suspected and not d.screen_changed, "同帧不应有变化")
        return "同帧：无变化、非弹窗"

    c.check("同帧反例", _popup_negative_same)

    def _popup_negative_tiny():
        from PIL import ImageDraw

        img = base_img.copy()
        ImageDraw.Draw(img).rectangle([5, 5, 5, 5], fill=(255, 255, 255))
        d = diff_frames(base_img, img, cfg_pop)
        _req(not d.popup_suspected, "单像素变化不应判为弹窗")
        return "极小变化反例：bbox 面积比 %.6f" % d.bbox_area_ratio

    c.check("微小变化反例", _popup_negative_tiny)

    def _popup_negative_fullscreen():
        from PIL import Image

        img = Image.new("RGB", (1920, 1080), (255, 255, 255))
        d = diff_frames(base_img, img, cfg_pop)
        _req(not d.popup_suspected, "整屏突变不应判为弹窗（bbox 贴满全屏）")
        return "整屏突变反例：面积比 %.3f" % d.bbox_area_ratio

    c.check("整屏变化反例", _popup_negative_fullscreen)

    def _destructive_words():
        zh = destructive_hits("确认删除该文件？")
        en = destructive_hits("Click Delete to continue")
        _req("删除" in zh and "delete" in en, "破坏性词表未命中：%s / %s" % (zh, en))
        _req(destructive_hits("取消并关闭窗口") == [], "安全文案被误判")
        hits = destructive_guard(json.loads(_step_json({"type": "click", "x": 1, "y": 1}, thought="点击删除按钮")))
        _req("删除" in hits, "destructive_guard 未命中 thought：%s" % hits)
        return "中英文命中：%s / %s；安全文案不误伤；destructive_guard=%s" % (zh, en, hits)

    c.check("破坏性词表", _destructive_words)

    def _stall_detector():
        det = StallDetector(window=3, threshold=0.005)
        d = diff_frames(base_img, base_img, cfg_pop)
        flags = [det.update(d) for _ in range(3)]
        _req(flags == [False, False, True], "stall 触发时机错误：%s" % flags)
        det2 = StallDetector(window=2, threshold=0.005)
        det2.update(diff_frames(base_img, popup_img, cfg_pop))
        _req(det2.consecutive == 0, "有变化时不应累计")
        return "连续 N 步无变化才触发；有变化即清零"

    c.check("StallDetector", _stall_detector)

    def _loop_detector():
        det = LoopDetector(threshold=3)
        flags = [det.hit("A", "hash1") for _ in range(3)]
        _req(flags == [False, False, True], "loop 触发时机错误：%s" % flags)
        _req(det.hit("A", "hash2") is False, "画面哈希不同不应计入同一 key")
        return "同一（动作，画面哈希）第 3 次触发 E_LOOP；画面变化即分离"

    c.check("LoopDetector", _loop_detector)

    def _clamp_region():
        frame = _frame(100, 100, 1.0, (0, 0), screen=(200, 200))
        r = clamp_region_to_frame({"x": 90, "y": 90, "width": 90, "height": 90}, frame)
        _req(r is not None and r["x"] + r["width"] <= 100 and r["y"] + r["height"] <= 100,
             "裁剪结果越界：%s" % r)
        _req(clamp_region_to_frame({"x": 500, "y": 500, "width": 10, "height": 10}, frame) is None,
             "完全越界应返回 None")
        return "弹窗裁剪提示会被裁进当前帧：%s" % r

    c.check("弹窗裁剪区域裁剪", _clamp_region)

    # ---------------- 10 路由 ----------------
    c.begin("10 API-first 路由（协议第 6 节）")

    def _route_default_gui():
        r = default_router(_mock_cfg())
        d = r.route("测试任务")
        _req(d.level == "L3", "无探针时应落 L3，实际 %s" % d.level)
        _req({"level", "reason", "probe_result", "candidates"} <= set(d.to_dict()), "路由记录字段缺失")
        return "无任何 API 探针 -> L3（%s）" % d.reason

    c.check("默认落到 GUI 兜底", _route_default_gui)

    def _route_l0_first():
        r = Router([StaticProbe(True, level="L0", name="github-api", reason="探测到 GitHub API")], _mock_cfg())
        d = r.route("建 issue")
        _req(d.level == "L0", "应命中 L0，实际 %s" % d.level)
        _req(d.probe_result and d.probe_result["reachable"] is True, "探针结果未记录")
        return "L0 可用即停：%s" % d.reason

    c.check("L0 命中即停", _route_l0_first)

    def _route_skip_unreachable():
        probes = [StaticProbe(False, level="L0", name="api"), StaticProbe(True, level="L1", name="cdp")]
        d = Router(probes, _mock_cfg()).route("填表")
        _req(d.level == "L1", "应跳过不可用的 L0，实际 %s" % d.level)
        _req(len(d.candidates) == 2, "候选探测记录数不符：%s" % d.candidates)
        return "L0 不可用 -> L1；两次探测结果都落记录"

    c.check("跳过不可用级别", _route_skip_unreachable)

    def _route_no_gui():
        cfg = _mock_cfg(enable_gui_fallback=False)
        try:
            Router([], cfg).route("x")
            raise AssertionError("关闭兜底且无 API 时应报错")
        except CuaError as exc:
            _req(exc.code == E_NEED_HUMAN, "期望 E_NEED_HUMAN 实际 %s" % exc.code)
        return "关闭 GUI 兜底且无 API -> E_NEED_HUMAN"

    c.check("关闭兜底的行为", _route_no_gui)

    # ---------------- 11 端到端 ----------------
    c.begin("11 端到端（mock 模型 + 合成屏幕 + dry-run）")

    def _e2e_happy():
        script = [
            "我先看看界面。",  # 坏 JSON -> 回喂重问
            _FENCE + "json\n" + json.dumps({
                "protocol": PROTOCOL_VERSION, "thought": "点击搜索框",
                "action": {"type": "click", "x": 640.4, "y": 210.6}, "done": False}) + "\n" + _FENCE,
            json.dumps({"protocol": PROTOCOL_VERSION, "thought": "输入关键词",
                        "action": {"type": "type", "text": "hello cua", "clear_first": True}, "done": False}),
            json.dumps({"protocol": PROTOCOL_VERSION, "thought": "完成",
                        "action": {"type": "done", "status": "success", "summary": "跑通"},
                        "done": True, "final": {"status": "success", "summary": "跑通"}}),
        ]
        report, logger, device, provider, executor = _e2e(script, _mock_cfg())
        _req(report.ok, "任务应成功：code=%s msg=%s" % (report.terminal_code, report.terminal_message))
        _req(report.steps == 3, "步数应为 3，实际 %s" % report.steps)
        reasks = [e for e in logger.events if e.get("kind") == "agent" and e.get("event") == "model_reask"]
        _req(len(reasks) == 1, "应发生 1 次回喂重问，实际 %s" % len(reasks))
        first = report.results[0]
        _req(first["meta"]["coord_rounded"] is True, "小数坐标未记录 coord_rounded")
        _req(first["meta"]["screen_point"] == {"x": 835, "y": 275},
             "坐标换算不符：%s" % first["meta"]["screen_point"])
        ops = [o["op"] for o in device.ops]
        _req("click" in ops and "set_clipboard" in ops and "press" in ops,
             "设备操作缺失（task-25 起 type 走剪贴板，不再有 write）：%s" % ops)
        return "3 步成功；坏 JSON 回喂 1 次；640.4/210.6 -> screen(835,275)"

    c.check("正常任务闭环", _e2e_happy)

    def _e2e_out_of_bounds_recovery():
        bad = json.dumps({"protocol": PROTOCOL_VERSION, "thought": "点错位置",
                          "action": {"type": "click", "x": 99999, "y": 10}, "done": False})
        good = json.dumps({"protocol": PROTOCOL_VERSION, "thought": "修正坐标",
                           "action": {"type": "click", "x": 100, "y": 100}, "done": False})
        done = json.dumps({"protocol": PROTOCOL_VERSION, "thought": "完成",
                           "action": {"type": "done", "status": "success", "summary": "ok"},
                           "done": True, "final": {"status": "success", "summary": "ok"}})
        report, logger, device, provider, executor = _e2e([bad, good, done], _mock_cfg())
        _req(report.ok, "越界后应能恢复：%s" % report.terminal_message)
        _req(report.results[0]["error"]["code"] == E_OUT_OF_BOUNDS, "第一条应报越界")
        _req(report.results[0]["error"]["retryable"] is True, "E_OUT_OF_BOUNDS 应标记可重试")
        retries = [e for e in logger.events if e.get("kind") == "executor_retry"]
        _req(len(retries) == 1 and retries[0]["code"] == E_OUT_OF_BOUNDS, "未走执行器重试路径")
        return "越界 -> 回喂当前 image 尺寸 -> 修正成功（未 clamp）"

    c.check("越界回喂后恢复", _e2e_out_of_bounds_recovery)

    def _e2e_stall_replan():
        cfg = _mock_cfg(stall_window=3, loop_repeat_threshold=99)
        script = [json.dumps({"protocol": PROTOCOL_VERSION, "thought": "移动 %d" % i,
                              "action": {"type": "move", "x": 100 + 10 * i, "y": 100}, "done": False})
                  for i in range(8)]
        report, logger, device, provider, executor = _e2e(script, cfg, scenario="frozen")
        _req(report.terminal_code == E_STALL, "冻结画面应最终报 E_STALL，实际 %s" % report.terminal_code)
        _req(report.replans == cfg.max_replans, "重规划次数应为 %d，实际 %s" % (cfg.max_replans, report.replans))
        replans = [e for e in logger.events if e.get("kind") == "replan"]
        _req(len(replans) == cfg.max_replans, "重规划事件数不符：%s" % len(replans))
        return "冻结画面：%d 步后强制重规划 %d 次 -> 终止 E_STALL" % (report.steps, report.replans)

    c.check("E_STALL 强制重规划", _e2e_stall_replan)

    def _e2e_loop_detection():
        cfg = _mock_cfg(stall_window=99, loop_repeat_threshold=3)
        same = json.dumps({"protocol": PROTOCOL_VERSION, "thought": "重复同一步",
                           "action": {"type": "move", "x": 100, "y": 100}, "done": False})
        report, logger, device, provider, executor = _e2e([same], cfg, scenario="frozen")
        _req(report.terminal_code == E_LOOP, "应报 E_LOOP，实际 %s" % report.terminal_code)
        _req(report.steps == 5, "第 3 次命中后重规划两次应停在第 5 步，实际 %s" % report.steps)
        return "同一（动作，画面哈希）第 3 次触发 -> 重规划 2 次 -> E_LOOP（%d 步）" % report.steps

    c.check("E_LOOP 中断策略", _e2e_loop_detection)

    def _e2e_step_budget():
        cfg = _mock_cfg(step_budget=2, stall_window=99, loop_repeat_threshold=99)
        script = [json.dumps({"protocol": PROTOCOL_VERSION, "thought": "移动 %d" % i,
                              "action": {"type": "move", "x": 50 + 7 * i, "y": 60}, "done": False})
                  for i in range(6)]
        report, logger, device, provider, executor = _e2e(script, cfg)
        _req(report.terminal_code == E_STEP_BUDGET, "应报 E_STEP_BUDGET，实际 %s" % report.terminal_code)
        _req(report.steps == 2, "步数预算 2 应恰好停在第 2 步，实际 %s" % report.steps)
        return "步数预算 40 -> 2（自检用）时优雅退出"

    c.check("步数预算", _e2e_step_budget)

    def _e2e_wall_budget():
        cfg = _mock_cfg(step_budget=40, wall_budget_s=10)
        state = {"t": 0.0}

        def clock():
            state["t"] += 50.0
            return state["t"]

        script = [json.dumps({"protocol": PROTOCOL_VERSION, "thought": "移动",
                              "action": {"type": "move", "x": 10, "y": 10}, "done": False})]
        report, logger, device, provider, executor = _e2e(script, cfg, clock=clock)
        _req(report.terminal_code == E_WALL_BUDGET, "应报 E_WALL_BUDGET，实际 %s" % report.terminal_code)
        return "墙钟预算与步数预算双重约束（注入时钟：10s 预算）"

    c.check("墙钟预算", _e2e_wall_budget)

    def _e2e_high_risk():
        script = [json.dumps({"protocol": PROTOCOL_VERSION, "thought": "危险动作",
                              "action": {"type": "click", "x": 10, "y": 10}, "risk": "high", "done": False})]
        report, logger, device, provider, executor = _e2e(script, _mock_cfg())
        _req(report.terminal_code == E_NEED_HUMAN, "risk=high 应转人工，实际 %s" % report.terminal_code)
        _req(device.ops == [], "未放行的动作不应有任何设备操作")
        return "risk=high 默认需护栏放行，未放行即 E_NEED_HUMAN"

    c.check("risk=high 护栏", _e2e_high_risk)

    def _e2e_destructive_popup():
        cfg = _mock_cfg(stall_window=99, loop_repeat_threshold=99, popup_auto_crop=False)
        script = [
            json.dumps({"protocol": PROTOCOL_VERSION, "thought": "点击确定",
                        "action": {"type": "click", "x": 960, "y": 540}, "done": False}),
            json.dumps({"protocol": PROTOCOL_VERSION, "thought": "点击删除按钮",
                        "action": {"type": "click", "x": 960, "y": 540}, "done": False}),
        ]
        report, logger, device, provider, executor = _e2e(script, cfg, scenario="popup")
        _req(report.terminal_code == E_NEED_HUMAN,
             "疑似弹窗上的破坏性按钮应转人工，实际 %s（%s）" % (report.terminal_code, report.terminal_message))
        _req("破坏性词表" in report.terminal_message, "终止说明未提到破坏性词表：%s" % report.terminal_message)
        popups = [e for e in report.events if e.get("code") == E_POPUP_BLOCKING]
        _req(len(popups) >= 1, "未记录 E_POPUP_BLOCKING 事件")
        return "疑似弹窗 + 破坏性按钮 -> 不点击，直接 E_NEED_HUMAN"

    c.check("弹窗破坏性按钮硬约束", _e2e_destructive_popup)

    def _e2e_popup_escalation():
        cfg = _mock_cfg(stall_window=99, loop_repeat_threshold=99, popup_auto_crop=False)
        script = [json.dumps({"protocol": PROTOCOL_VERSION, "thought": "点击弹窗按钮",
                              "action": {"type": "click", "x": 960, "y": 540}, "done": False})]
        report, logger, device, provider, executor = _e2e(script, cfg, scenario="popup_flip")
        _req(report.terminal_code == E_NEED_HUMAN, "弹窗持续应最终转人工，实际 %s" % report.terminal_code)
        esc = [e for e in report.events if e.get("code") == E_POPUP_BLOCKING and e.get("action") == "esc"]
        _req(len(esc) == cfg.popup_max_esc, "esc 次数应为 %d，实际 %s" % (cfg.popup_max_esc, len(esc)))
        _req("esc" in report.terminal_message, "终止说明应提到 esc：%s" % report.terminal_message)
        return "弹窗持续 -> 按 %d 次 esc -> 仍无效转人工" % len(esc)

    c.check("弹窗 esc 升级转人工", _e2e_popup_escalation)

    def _e2e_model_timeout():
        cfg = _mock_cfg()
        script = [{"raise": E_MODEL_TIMEOUT, "message": "模拟模型超时"}]
        report, logger, device, provider, executor = _e2e(script, cfg)
        _req(report.terminal_code == E_NEED_HUMAN,
             "模型连续超时应转人工，实际 %s" % report.terminal_code)
        retries = [e for e in logger.events if e.get("kind") == "model_retry"]
        _req(len(retries) == max_retries(E_MODEL_TIMEOUT), "超时重试次数应为 2，实际 %s" % len(retries))
        return "E_MODEL_TIMEOUT 退避重试 2 次 -> E_NEED_HUMAN"

    c.check("模型超时路径", _e2e_model_timeout)

    def _e2e_need_human_final():
        script = [json.dumps({"protocol": PROTOCOL_VERSION, "thought": "需要验证码",
                              "action": {"type": "done", "status": "need_human", "summary": "出现验证码"},
                              "done": True, "final": {"status": "need_human", "summary": "出现验证码"}})]
        report, logger, device, provider, executor = _e2e(script, _mock_cfg())
        _req(report.terminal_code == E_NEED_HUMAN and report.final_status == "need_human",
             "need_human 结束路径错误：%s" % report.to_dict())
        _req(device.ops == [], "结束步骤不应有设备操作")
        return "模型自报 need_human：正确地交还控制权"

    c.check("need_human 收尾", _e2e_need_human_final)

    def _e2e_cancel():
        cfg = _mock_cfg(loop_repeat_threshold=99, stall_window=99)
        provider = MockScreenshot(cfg)
        device = DryRunDevice()
        noop = lambda s: None
        executor = Executor(cfg, provider=provider, device=device, dry_run=True, sleep_fn=noop)
        logger = MemoryLogger()
        move = json.dumps({"protocol": PROTOCOL_VERSION, "thought": "移动",
                           "action": {"type": "move", "x": 10, "y": 10}, "done": False})
        agent = VisionAgent(MockClient([move], loop=True), cfg, logger=logger)
        loop = ControlLoop(cfg, agent=agent, executor=executor, provider=provider,
                           logger=logger, sleep_fn=noop)
        original = device._record
        state = {"n": 0}

        def record_then_cancel(op, **kwargs):
            original(op, **kwargs)
            state["n"] += 1
            loop.cancel()

        device._record = record_then_cancel  # type: ignore
        report = loop.run("取消测试")
        _req(report.terminal_code == E_CANCEL, "应报 E_CANCEL，实际 %s" % report.terminal_code)
        _req(report.steps == 1, "取消应停在已完成的那一步，实际 %s" % report.steps)
        return "loop.cancel() -> E_CANCEL（第 1 步后停手并释放按键）"

    c.check("手动取消路径 E_CANCEL", _e2e_cancel)

    def _e2e_dry_run_no_ops_of_real_device():
        cfg = _mock_cfg()
        script = [json.dumps({"protocol": PROTOCOL_VERSION, "thought": "移动",
                              "action": {"type": "move", "x": 10, "y": 10}, "done": False})]
        report, logger, device, provider, executor = _e2e(script, cfg)
        _req(isinstance(executor.device, DryRunDevice), "自检必须使用 DryRunDevice")
        _req(executor.dry_run is True, "自检必须处于 dry_run 模式")
        _req(len(device.ops) >= 1, "dry-run 也应记录将执行的动作")
        return "全程 DryRunDevice：%d 条模拟操作，真实 pyautogui 未被调用" % len(device.ops)

    c.check("dry-run 空转验证", _e2e_dry_run_no_ops_of_real_device)

    # ---------------- 12 契约不一致登记 ----------------
    c.begin("12 已知边界与待验证项（只登记，不改契约）")
    c.note("2026-10-11 裁定：action 恒为必填动作对象（不可为 null），final.status/summary 是终态唯一权威来源。")
    c.note("E_ARG_RANGE 与 E_SCHEMA 的边界：action 内 minimum/maximum 越界 -> E_ARG_RANGE，其余结构不符 -> E_SCHEMA。")
    c.note("弹窗判定使用两级阈值（弱>8 计 change_ratio，强>45 定形状），否则整屏压暗会把 bbox 撑满。")
    c.note("多显示器虚拟桌面合并坐标系未实测（pyautogui 只报主屏几何）——待验证。")
    c.note("弹窗按钮文字识别依赖模型给出的文本字段，未接入 OCR —— 待验证。")
    c.note("本地 VLM 真实端到端（Ollama qwen2.5vl:7b）未在本自检中跑过 —— 待验证。")

    # ---------------- 13 预算分档（2026-10-11 裁定） ----------------
    c.begin("13 预算分档（2026-10-11 裁定）")

    def _profile_values():
        cloud = BUDGET_PROFILES["cloud"]
        local = BUDGET_PROFILES["local"]
        _req(cloud["step_budget"] == 40 and cloud["wall_budget_s"] == 300.0,
             "云端档应保持 40 步 / 300s：%s" % cloud)
        _req(local["wall_budget_s"] >= 1800.0, "本地档墙钟不应低于 1800s：%s" % local["wall_budget_s"])
        _req(896 <= local["image_max_width"] <= 1280,
             "本地档截图长边应在 896~1280：%s" % local["image_max_width"])
        return "cloud=%s；local=%s" % (BUDGET_PROFILES["cloud"]["description"],
                                       BUDGET_PROFILES["local"]["description"])

    c.check("分档取值", _profile_values)

    def _profile_defaults():
        cfg = Config()
        _req(cfg.budget_profile == "cloud" and cfg.step_budget == 40 and cfg.wall_budget_s == 300.0,
             "默认档应为 cloud 40/300")
        local_cfg = apply_budget_profile(cfg, "local")
        _req(local_cfg.budget_profile == "local" and local_cfg.wall_budget_s == 1800.0,
             "local 档未生效：%s" % local_cfg.wall_budget_s)
        _req(local_cfg.image_max_width == 1280, "local 档截图长边未收紧：%s" % local_cfg.image_max_width)
        return "Config() 默认 cloud(40/300/1472)；local -> 40/%.0f/%d" % (
            local_cfg.wall_budget_s, local_cfg.image_max_width)

    c.check("默认值与分档应用", _profile_defaults)

    def _profile_max_tokens():
        cloud = BUDGET_PROFILES["cloud"]
        local = BUDGET_PROFILES["local"]
        _req(int(cloud["model_max_tokens"]) >= 4096,
             "云端档 model_max_tokens 应 >= 4096：%s" % cloud["model_max_tokens"])
        _req(int(local["model_max_tokens"]) < 4096,
             "本地档应保持小值：%s" % local["model_max_tokens"])
        _req(Config().model_max_tokens >= 4096,
             "Config() 默认 model_max_tokens 应 >= 4096：%s" % Config().model_max_tokens)
        cli = config_from_args(build_parser().parse_args(["--mock", "--dry-run", "--budget-profile", "cloud"]))
        _req(cli.model_max_tokens >= 4096,
             "CLI（cloud 档）model_max_tokens 应 >= 4096：%s" % cli.model_max_tokens)
        local_cfg = apply_budget_profile(Config(), "local")
        _req(local_cfg.model_max_tokens == int(local["model_max_tokens"]),
             "local 档未应用 model_max_tokens：%s" % local_cfg.model_max_tokens)
        override = config_from_args(build_parser().parse_args(
            ["--mock", "--dry-run", "--budget-profile", "local", "--model-max-tokens", "2048"]))
        _req(override.model_max_tokens == 2048,
             "显式 --model-max-tokens 应覆盖分档：%s" % override.model_max_tokens)
        return "cloud=%d / local=%d；Config() 默认 -> %d；显式 --model-max-tokens 可覆盖分档" % (
            cloud["model_max_tokens"], local["model_max_tokens"], Config().model_max_tokens)

    c.check("max_tokens 分档（契约 §3.1）", _profile_max_tokens)

    def _profile_auto_and_precedence():
        _req(resolve_budget_profile("auto", "http://127.0.0.1:11434/v1") == "local", "本地地址应走 local")
        _req(resolve_budget_profile("auto", "https://api.openai.com/v1") == "cloud", "远端地址应走 cloud")
        _req(resolve_budget_profile("不存在的档", "http://x") == "cloud", "非法档应回落 cloud")
        args = build_parser().parse_args(["--mock", "--dry-run", "--budget-profile", "local"])
        cfg = config_from_args(args)
        _req(cfg.budget_profile == "local" and cfg.wall_budget_s == 1800.0, "CLI 分档未生效")
        args2 = build_parser().parse_args(["--mock", "--budget-profile", "local", "--max-seconds", "45",
                                           "--max-steps", "7"])
        cfg2 = config_from_args(args2)
        _req(cfg2.wall_budget_s == 45.0 and cfg2.step_budget == 7,
             "显式预算参数应覆盖分档：%s/%s" % (cfg2.wall_budget_s, cfg2.step_budget))
        return "auto 按 base_url 判定；显式 --max-seconds/--max-steps 覆盖分档"

    c.check("auto 判定与显式覆盖优先级", _profile_auto_and_precedence)

    def _local_profile_in_loop():
        cfg = apply_budget_profile(_mock_cfg(), "local")
        _req(cfg.wall_budget_s == 1800.0 and cfg.image_max_width == 1280, "local 档未应用到运行配置")
        report, logger, device, provider, executor = _e2e(
            [json.dumps({"protocol": PROTOCOL_VERSION, "thought": "移动",
                         "action": {"type": "move", "x": 10, "y": 10}, "done": False})], cfg)
        frame = provider.capture()
        _req(frame.screen_size[0] == 1920, "样例前提：合成屏幕宽 1920")
        _req(frame.width <= 1280, "本地档截图长边应被收到 1280 以内：%s" % frame.width)
        return "local 档在完整闭环中生效（合成屏 1920 -> 送模型宽 %d，墙钟 1800s）" % frame.width

    c.check("local 档在闭环中生效", _local_profile_in_loop)

    def _terminal_action_executed():
        script = [json.dumps({"protocol": PROTOCOL_VERSION, "thought": "点完就结束",
                              "action": {"type": "click", "x": 100, "y": 100}, "done": True,
                              "final": {"status": "success", "summary": "点完就结束"}})]
        report, logger, device, provider, executor = _e2e(script, _mock_cfg())
        _req(report.ok, "done=true + 合法动作 + final 应成功：%s" % report.terminal_message)
        _req([o["op"] for o in device.ops] == ["click"],
             "终态动作应被执行一次：%s" % device.ops)
        return "done=true 配 click：动作执行后按 final 收尾"

    c.check("终态动作仍被执行", _terminal_action_executed)

    ok, lines = c.render()
    return ok, header + lines


def _capture_error(fn: Callable[[], Any]) -> CuaError:
    try:
        fn()
    except CuaError as exc:
        return exc
    raise AssertionError("预期抛出 CuaError")


def main(argv: Optional[Sequence[str]] = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if args.selfcheck:
        _ok, lines = run_selfcheck(args)
        print("\n".join(lines))
        return 0 if _ok else 1
    if args.events_images and args.events != "stdout":
        print("--events-images 需要与 --events stdout 同用（集成契约第 1 节）。", file=sys.stderr)
        return 2
    cfg = config_from_args(args)
    if not args.task and not (cfg.use_mock_model or cfg.dry_run):
        print("请给出 --task，或使用 --selfcheck / --dry-run --mock 离线自检。", file=sys.stderr)
        parser.print_help(sys.stderr)
        return 2
    return run_once(args, cfg)


if __name__ == "__main__":
    sys.exit(main())

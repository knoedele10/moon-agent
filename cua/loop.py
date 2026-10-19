"""主循环、预算约束、卡死/循环检测与弹窗策略（协议第 4、5、7、8 节）。

闭环：L1 Observation -> L2 Step -> L3 ActionResult -> L1 -> ... 直到 done 或触发终止条件。

终止规则：
  - 步数预算与墙钟预算双重约束，任一超限即优雅退出（E_STEP_BUDGET / E_WALL_BUDGET）；
  - E_STALL / E_LOOP 不是重试，而是强制重规划（E_REPLAN，默认最多 2 次），
    重规划次数用尽才终止；
  - 任何终止路径都先释放所有修饰键（第 8 节第 7 条）。
"""

from __future__ import annotations

import json
import os
import sys
import time
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Optional, Tuple

from .action_parser import ParsedStep, parse_step, step_fingerprint
from .agent import VisionAgent
from .backends.mock import MockClient
from .config import PROTOCOL_VERSION, Config
from .errors import (
    E_ARG_RANGE,
    E_CANCEL,
    E_EXECUTOR_FAILSAFE,
    E_EXPECT_TIMEOUT,
    E_INTERNAL,
    E_KEY_NOT_ALLOWED,
    E_LOOP,
    E_MODEL_HTTP,
    E_MODEL_TIMEOUT,
    E_NEED_HUMAN,
    E_NO_FOCUS,
    E_OUT_OF_BOUNDS,
    E_POPUP_BLOCKING,
    E_STALL,
    E_STEP_BUDGET,
    E_WALL_BUDGET,
    RetryTracker,
    CuaError,
)
from .executor import ExecutionOutcome, Executor, destructive_guard
from .observation import History, build_observation
from .prompts import error_feedback
from .router import Router, default_router
from .screenshot import (
    CaptureFrame,
    FrameDiff,
    MockScreenshot,
    PyAutoGUIScreenshot,
    ScreenshotProvider,
    clamp_region_to_frame,
    diff_frames,
    system_dpi_scale,
)

#: 执行器级错误：成功一步后重置计数，保证「每步」的重试预算（协议第 5 节）
EXECUTOR_RETRY_CODES = (
    E_OUT_OF_BOUNDS,
    E_ARG_RANGE,
    E_KEY_NOT_ALLOWED,
    E_NO_FOCUS,
    E_EXPECT_TIMEOUT,
    E_POPUP_BLOCKING,
)


# ---------------------------------------------------------------------------
# 日志
# ---------------------------------------------------------------------------


def _event_record(kind: str, fields: Dict[str, Any]) -> Dict[str, Any]:
    """现有 JSONL 日志记录 schema：{"ts": 秒.毫秒, "kind": 事件名, ...字段}。

    集成契约第 1 节要求 stdout 事件流复用这套 schema，因此所有 sink 共用本函数。
    """
    record: Dict[str, Any] = {"ts": round(time.time(), 3), "kind": kind}
    record.update(_jsonable(fields))
    return record


def _write_jsonl(stream: Any, record: Dict[str, Any]) -> None:
    """写一行 JSONL 并立即 flush（不依赖 PYTHONUNBUFFERED）。"""
    stream.write(json.dumps(record, ensure_ascii=False) + "\n")
    stream.flush()


class JsonlLogger:
    """JSONL 事件日志（编排审计用）。"""

    def __init__(self, path: Optional[str] = None, enabled: bool = True, echo: bool = False) -> None:
        self.path = path
        self.enabled = enabled
        self.echo = echo
        self._fh = None
        if enabled and path:
            directory = os.path.dirname(os.path.abspath(path))
            if directory:
                os.makedirs(directory, exist_ok=True)
            self._fh = open(path, "a", encoding="utf-8")

    def event(self, kind: str, **fields: Any) -> Dict[str, Any]:
        record = _event_record(kind, fields)
        self._emit(record)
        return record

    def _emit(self, record: Dict[str, Any]) -> None:
        if self.echo:
            _write_jsonl(sys.stdout, record)
        if self._fh is not None:
            _write_jsonl(self._fh, record)

    def close(self) -> None:
        if self._fh is not None:
            self._fh.close()
            self._fh = None


class MemoryLogger:
    def __init__(self) -> None:
        self.events: List[Dict[str, Any]] = []

    def event(self, kind: str, **fields: Any) -> Dict[str, Any]:
        record = _event_record(kind, fields)
        self._emit(record)
        return record

    def _emit(self, record: Dict[str, Any]) -> None:
        self.events.append(record)

    def close(self) -> None:
        return None


#: 集成契约第 1 节冻结的事件名（stdout 事件流只允许这些；frame_image 为可选附加）。
#: 审计日志文件不受此限制（保留全部既有事件，行为不回归）。
EVENT_STREAM_KINDS: frozenset = frozenset((
    "run_start",
    "frame",
    "observation",
    "agent",
    "step",
    "action_result",
    "replan",
    "expect_timeout",
    "executor_retry",
    "guardrail",
    "run_end",
    "frame_image",
))


class EventStreamLogger:
    """集成契约第 1 节 I1：stdout 事件流（逐行 JSONL，逐行 flush）。

    只写 JSONL，不做任何人类可读打印；record schema 与 JsonlLogger 完全一致。
    只转发契约冻结的 kind（EVENT_STREAM_KINDS）；审计日志文件仍记录全部事件。
    stream 默认取 sys.stdout，调用方可在重定向 sys.stdout 之前先捕获真实流。
    """

    def __init__(self, stream: Any = None) -> None:
        self.stream = stream if stream is not None else sys.stdout

    def event(self, kind: str, **fields: Any) -> Dict[str, Any]:
        record = _event_record(kind, fields)
        self._emit(record)
        return record

    def _emit(self, record: Dict[str, Any]) -> None:
        if record.get("kind") not in EVENT_STREAM_KINDS:
            return
        _write_jsonl(self.stream, record)

    def close(self) -> None:
        try:
            self.stream.flush()
        except Exception:
            pass


class TeeLogger:
    """把同一条 record 分发给多个 sink（文件 JSONL + stdout 事件流）。

    同一条事件只生成一次 record，保证各 sink 的 ts/字段完全一致。
    """

    def __init__(self, sinks: List[Any]) -> None:
        self.sinks = [s for s in sinks if s is not None]

    def event(self, kind: str, **fields: Any) -> Dict[str, Any]:
        record = _event_record(kind, fields)
        for sink in self.sinks:
            sink._emit(record)
        return record

    def close(self) -> None:
        for sink in self.sinks:
            try:
                sink.close()
            except Exception:
                pass


def _jsonable(value: Any) -> Any:
    if isinstance(value, dict):
        return {str(k): _jsonable(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [_jsonable(v) for v in value]
    if isinstance(value, (str, int, float, bool)) or value is None:
        return value
    return str(value)


# ---------------------------------------------------------------------------
# 检测器
# ---------------------------------------------------------------------------


class StallDetector:
    """协议第 5 节 E_STALL：连续 N 步画面几乎无变化。"""

    def __init__(self, window: int = 5, threshold: float = 0.005) -> None:
        self.window = int(window)
        self.threshold = float(threshold)
        self.consecutive = 0

    def update(self, diff: Optional[FrameDiff]) -> bool:
        if diff is None:
            return False
        if diff.change_ratio < self.threshold:
            self.consecutive += 1
        else:
            self.consecutive = 0
        return self.consecutive >= self.window

    def reset(self) -> None:
        self.consecutive = 0


class LoopDetector:
    """协议第 5 节 E_LOOP：同一（动作，画面哈希）重复 3 次以上。"""

    def __init__(self, threshold: int = 3) -> None:
        self.threshold = int(threshold)
        self.counts: Dict[Tuple[str, str], int] = {}

    def hit(self, action_fingerprint: str, screen_hash: str) -> bool:
        key = (action_fingerprint, screen_hash)
        self.counts[key] = self.counts.get(key, 0) + 1
        return self.counts[key] >= self.threshold

    def reset(self) -> None:
        self.counts.clear()


@dataclass
class PopupState:
    active: bool = False
    consecutive: int = 0
    esc_sent: int = 0
    crop: Optional[Dict[str, int]] = None
    auto_cropped: bool = False


# ---------------------------------------------------------------------------
# 运行报告
# ---------------------------------------------------------------------------


@dataclass
class RunReport:
    task: str = ""
    ok: bool = False
    terminal_code: Optional[str] = None
    terminal_message: str = ""
    final: Optional[Dict[str, Any]] = None
    final_status: Optional[str] = None
    steps: int = 0
    replans: int = 0
    results: List[Dict[str, Any]] = field(default_factory=list)
    trace: List[Dict[str, Any]] = field(default_factory=list)
    events: List[Dict[str, Any]] = field(default_factory=list)
    route: Optional[Dict[str, Any]] = None
    started_at: float = 0.0
    duration_ms: int = 0
    log_path: Optional[str] = None
    popup_events: int = 0
    expect_timeouts: int = 0

    def to_dict(self) -> Dict[str, Any]:
        return {
            "task": self.task,
            "ok": self.ok,
            "terminal_code": self.terminal_code,
            "terminal_message": self.terminal_message,
            "final": self.final,
            "final_status": self.final_status,
            "steps": self.steps,
            "replans": self.replans,
            "route": self.route,
            "duration_ms": self.duration_ms,
            "steps_detail": self.trace,
            "events": self.events,
            "log_path": self.log_path,
            "popup_events": self.popup_events,
            "expect_timeouts": self.expect_timeouts,
            "results": self.results,
        }

    def summary_lines(self) -> List[str]:
        lines = [
            "任务：%s" % self.task,
            "结果：%s" % ("成功" if self.ok else "未成功"),
            "终止码：%s" % (self.terminal_code or "（正常结束）"),
            "终止说明：%s" % (self.terminal_message or "-"),
            "步数：%d  重规划：%d  弹窗事件：%d  用时：%.1fs" % (
                self.steps, self.replans, self.popup_events, self.duration_ms / 1000.0),
        ]
        if self.route:
            lines.append("路由：%s（%s）" % (self.route.get("level"), self.route.get("reason")))
        if self.final:
            lines.append("final：%s" % json.dumps(self.final, ensure_ascii=False))
        return lines


# ---------------------------------------------------------------------------
# 主循环
# ---------------------------------------------------------------------------


class ControlLoop:
    def __init__(
        self,
        cfg: Optional[Config] = None,
        agent: Optional[VisionAgent] = None,
        executor: Optional[Executor] = None,
        provider: Optional[ScreenshotProvider] = None,
        router: Optional[Router] = None,
        logger: Optional[Any] = None,
        events: Optional[Any] = None,
        clock: Optional[Callable[[], float]] = None,
        allow_high_risk: bool = False,
        sleep_fn: Optional[Callable[[float], None]] = None,
    ) -> None:
        self.cfg = cfg or Config()
        self._clock = clock or time.monotonic
        self._sleep = sleep_fn or time.sleep
        self.provider = provider or (
            MockScreenshot(self.cfg) if self.cfg.use_mock_screen else PyAutoGUIScreenshot(self.cfg)
        )
        self.executor = executor or Executor(self.cfg, provider=self.provider)
        self.agent = agent
        self.router = router or default_router(self.cfg)
        self.logger = logger
        #: stdout 事件流 sink（集成契约第 1 节 I1）；frame_image 只写这里，不写审计日志文件
        self.events = events
        self.allow_high_risk = bool(allow_high_risk)
        self._cancelled = False
        self.report: Optional[RunReport] = None

    # -- 外部控制 --------------------------------------------------------
    def cancel(self) -> None:
        self._cancelled = True

    def _log(self, kind: str, **fields: Any) -> None:
        if self.logger is not None:
            self.logger.event(kind, **fields)

    def _log_frame(self, step: int, frame: CaptureFrame, label: Optional[str] = None) -> None:
        """记录一帧：先 frame（既有行为），再在 --events-images 下追加 frame_image。"""
        self._log("frame", step=step, image=frame.image_block(), label=label or frame.label)
        if self.events is not None and self.cfg.events_images:
            block = frame.jpeg_block(self.cfg.event_image_max_edge, self.cfg.event_image_quality)
            self.events.event("frame_image", step=step, **block)

    def _log_frame_if_events(self, step: int, frame: CaptureFrame) -> None:
        """--events-images 下为「执行后新产生的一帧」补发 frame + frame_image。

        默认关闭，因此不改变既有 JSONL 日志内容（不回归）。
        """
        if self.events is not None and self.cfg.events_images:
            self._log_frame(step, frame)

    # -- 主流程 ----------------------------------------------------------
    def run(self, task: Optional[str] = None) -> RunReport:
        cfg = self.cfg
        task = task if task is not None else cfg.task
        t0 = self._clock()
        report = RunReport(task=task, started_at=time.time(), log_path=cfg.log_path)
        self.report = report
        history = History(limit=cfg.observation_history_tail)
        retry = RetryTracker()
        stall = StallDetector(window=cfg.stall_window, threshold=cfg.stall_change_ratio)
        loopd = LoopDetector(threshold=cfg.loop_repeat_threshold)
        popup = PopupState()
        terminal: Optional[CuaError] = None
        last_result: Optional[Dict[str, Any]] = None
        feedback: Optional[str] = None
        notes: Optional[str] = None
        replans = 0
        step = 0
        dpi = system_dpi_scale() if not cfg.use_mock_screen else 1.0

        try:
            decision = self.router.route(task)
            report.route = decision.to_dict()
            self._log("run_start", task=task, route=decision.to_dict(),
                      dry_run=cfg.dry_run, mock_screen=cfg.use_mock_screen,
                      step_budget=cfg.step_budget, wall_budget_s=cfg.wall_budget_s)
            frame = self.provider.capture()
            self._log_frame(0, frame)

            while True:
                if self._cancelled:
                    terminal = CuaError(E_CANCEL, "用户手动中断", hint="已释放按键并停手")
                    break
                elapsed = self._clock() - t0
                if step >= cfg.step_budget:
                    terminal = CuaError(E_STEP_BUDGET, "超出步数预算 %d" % cfg.step_budget,
                                        hint="优雅退出，返回已完成的进度")
                    break
                if elapsed >= cfg.wall_budget_s:
                    terminal = CuaError(E_WALL_BUDGET, "超出时间预算 %.1fs" % cfg.wall_budget_s,
                                        hint="优雅退出，返回已完成的进度")
                    break
                if self.agent is None:
                    raise CuaError(E_INTERNAL, "没有注入 VisionAgent", hint="ControlLoop 需要 agent")
                step += 1

                # 第 7 节处置 1：疑似弹窗 -> 把可疑区域裁剪放大作为下一步 Observation 主体
                if popup.active and popup.crop and cfg.popup_auto_crop and not popup.auto_cropped:
                    try:
                        region = clamp_region_to_frame(popup.crop, frame)
                        if region is None:
                            raise CuaError(E_INTERNAL, "弹窗裁剪区域越界")
                        frame = self.provider.capture(region=region, base_frame=frame)
                        popup.auto_cropped = True
                        self._log("popup_crop", region=popup.crop, image=frame.image_block())
                    except CuaError as exc:
                        self._log("popup_crop_failed", error=exc.to_dict())

                steps_left = max(0, cfg.step_budget - step)
                seconds_left = max(0.0, cfg.wall_budget_s - (self._clock() - t0))
                obs = build_observation(
                    cfg, task, step, frame,
                    last_action_result=last_result,
                    history=history,
                    popup_suspected=popup.active,
                    steps_left=steps_left,
                    seconds_left=seconds_left,
                    notes=notes,
                    dpi_scale=dpi,
                )
                self._log("observation", step=step, observation=obs)

                try:
                    parsed = self.agent.decide(obs, frame, feedback=feedback)
                except CuaError as exc:
                    decision_kind = retry.decision(exc.code, exc.context.get("http_status"))
                    if decision_kind == "retry" and exc.code in (E_MODEL_TIMEOUT, E_MODEL_HTTP):
                        retry.record(exc.code)
                        backoff = min(8.0, 0.5 * (2 ** (retry.count(exc.code) - 1)))
                        self._log("model_retry", step=step, code=exc.code, backoff_s=backoff)
                        self._sleep(backoff)
                        continue
                    terminal = self._escalate_model_error(exc)
                    break
                feedback = None
                entry = self._trace_entry(step, parsed)
                report.trace.append(entry)
                self._log("step", step=step, thought=parsed.step.get("thought"),
                          action=parsed.action, risk=parsed.risk, meta=parsed.meta)

                # --- 终止判断：done ---
                if parsed.done:
                    report.final, report.final_status, terminal = self._finish_from_step(parsed, entry)
                    report.ok = report.final_status == "success" and terminal is None
                    # 2026-10-11 裁定：done=true 允许配任意合法动作；若给了非 done 的动作，
                    # 它仍然是「这一步要执行的动作」，执行完再收尾。
                    terminal_action = (parsed.action or {}).get("type")
                    if terminal is None and terminal_action not in (None, "done"):
                        outcome = self.executor.execute(parsed, frame)
                        report.results.append(outcome.result)
                        entry["ok"] = bool(outcome.result.get("ok"))
                        entry["duration_ms"] = outcome.result.get("duration_ms")
                        entry["screen_changed"] = bool(
                            (outcome.result.get("observed") or {}).get("screen_changed"))
                        self._log("terminal_action_result", step=step, result=outcome.result)
                        if not outcome.ok:
                            err = outcome.result.get("error") or {}
                            entry["error_code"] = err.get("code")
                            terminal = CuaError(err.get("code") or E_INTERNAL,
                                                err.get("message") or "终态动作执行失败",
                                                hint=err.get("hint"),
                                                retryable=err.get("retryable"))
                            report.ok = False
                    break

                # --- 第 8 节第 6 条：risk=high 需要护栏放行 ---
                if parsed.risk == "high" and cfg.guard_high_risk and not self.allow_high_risk:
                    terminal = CuaError(
                        E_NEED_HUMAN,
                        "动作 risk=high，默认需要护栏放行",
                        hint="请人工确认，或以 allow_high_risk 显式放行",
                        context={"action": parsed.action},
                    )
                    entry["error_code"] = E_NEED_HUMAN
                    break

                # --- 第 7 节硬约束：疑似弹窗下的破坏性按钮 ---
                hits = destructive_guard(parsed)
                if hits and popup.active:
                    terminal = CuaError(
                        E_NEED_HUMAN,
                        "疑似弹窗上的按钮文字命中破坏性词表：%s" % "、".join(hits),
                        hint="不点击，直接交还控制权（协议第 7 节硬约束）",
                        context={"hits": hits, "action": parsed.action},
                    )
                    entry["error_code"] = E_NEED_HUMAN
                    break

                # --- 第 5 节 E_LOOP：同一（动作，画面哈希）重复 ---
                if loopd.hit(step_fingerprint(parsed.step), frame.sha256()):
                    if replans < cfg.max_replans:
                        replans += 1
                        report.replans = replans
                        notes = self._replan_note(E_LOOP, replans)
                        self._log("replan", step=step, code=E_LOOP, count=replans, note=notes)
                        report.events.append({"step": step, "code": E_LOOP, "action": "replan"})
                        continue
                    terminal = CuaError(E_LOOP, "同一动作与画面重复达到 %d 次" % cfg.loop_repeat_threshold,
                                        hint="强制中断当前策略，回退到上一个稳定状态")
                    entry["error_code"] = E_LOOP
                    break

                # --- 执行 ---
                outcome: ExecutionOutcome = self.executor.execute(parsed, frame)
                result = outcome.result
                last_result = result
                report.results.append(result)
                entry["ok"] = bool(result.get("ok"))
                entry["screen_changed"] = bool((result.get("observed") or {}).get("screen_changed"))
                entry["duration_ms"] = result.get("duration_ms")
                self._log("action_result", step=step, result=result)

                if not outcome.ok:
                    err = result.get("error") or {}
                    code = err.get("code") or E_INTERNAL
                    entry["error_code"] = code
                    exc = CuaError(code, err.get("message") or "", hint=err.get("hint"),
                                   retryable=err.get("retryable"),
                                   context={"http_status": err.get("http_status")})
                    if code == E_EXECUTOR_FAILSAFE:
                        terminal = exc
                        break
                    decision_kind = retry.decision(code, err.get("http_status"))
                    if decision_kind == "retry":
                        retry.record(code)
                        feedback = self._executor_feedback(exc, frame)
                        self._log("executor_retry", step=step, code=code, feedback=feedback)
                        continue
                    terminal = exc
                    break

                # 成功一步：重置执行器级重试计数（协议第 5 节的「每步」预算）
                for code in EXECUTOR_RETRY_CODES:
                    retry.reset(code)
                history.add(step, parsed.action, True, entry["screen_changed"])
                if outcome.frame is not None:
                    frame = outcome.frame
                    # --events-images 时补发 frame + frame_image（默认关闭，不改变既有日志）
                    self._log_frame_if_events(step, frame)

                # --- expect 校验（协议第 2 节 / 第 5 节 E_EXPECT_TIMEOUT）---
                if parsed.expect:
                    ok_expect, frame, expect_diff = self._await_expect(frame, parsed.expect)
                    if not ok_expect:
                        report.expect_timeouts += 1
                        if report.expect_timeouts <= 1:
                            feedback = ("上一步的 expect 未满足：must_change=%s，已再截一帧确认仍未变化。"
                                        "请换一种策略。" % parsed.expect.get("must_change"))
                            entry["error_code"] = E_EXPECT_TIMEOUT
                            self._log("expect_timeout", step=step, expect=parsed.expect)
                            continue
                        if replans < cfg.max_replans:
                            replans += 1
                            report.replans = replans
                            notes = self._replan_note(E_STALL, replans)
                            entry["error_code"] = E_STALL
                            self._log("replan", step=step, code=E_STALL, count=replans, note=notes)
                            continue
                        terminal = CuaError(E_STALL, "expect 连续未满足，判定卡住",
                                            hint="强制重规划（换策略，而不是重试同一动作）")
                        entry["error_code"] = E_STALL
                        break

                # --- 弹窗与卡死检测 ---
                diff = outcome.diff
                if diff is not None:
                    popup_err = self._popup_policy(diff, popup, step, report)
                    if popup_err is not None:
                        terminal = popup_err
                        break
                    if stall.update(diff):
                        if replans < cfg.max_replans:
                            replans += 1
                            report.replans = replans
                            notes = self._replan_note(E_STALL, replans)
                            stall.reset()
                            self._log("replan", step=step, code=E_STALL, count=replans, note=notes)
                            report.events.append({"step": step, "code": E_STALL, "action": "replan"})
                            continue
                        terminal = CuaError(
                            E_STALL,
                            "连续 %d 步画面几乎无变化（change_ratio < %.3f）"
                            % (cfg.stall_window, cfg.stall_change_ratio),
                            hint="判定卡住，强制重规划（换策略，而不是重试同一动作）",
                        )
                        entry["error_code"] = E_STALL
                        break
                else:
                    stall.reset()

        except KeyboardInterrupt:
            terminal = CuaError(E_CANCEL, "收到 Ctrl+C，用户手动中断", hint="已释放按键并停手")
        except CuaError as exc:
            terminal = exc
        finally:
            self.executor.release_all_modifiers()
            report.steps = step
            report.duration_ms = int((time.time() - report.started_at) * 1000)
            if terminal is not None:
                report.terminal_code = terminal.code
                report.terminal_message = terminal.message
                report.ok = False
                self._log("run_end", code=terminal.code, message=terminal.message,
                          steps=step, replans=replans)
            else:
                self._log("run_end", code=None, ok=report.ok, steps=step, replans=replans)
            try:
                self.provider.close()
            except Exception:
                pass
            if self.logger is not None:
                self.logger.close()
        return report

    # ------------------------------------------------------------------
    def _escalate_model_error(self, exc: CuaError) -> CuaError:
        """协议第 5 节：模型超时连续失败 -> E_NEED_HUMAN；4xx 直接上报。"""
        if exc.code == E_MODEL_TIMEOUT:
            return CuaError(E_NEED_HUMAN, "模型连续超时：%s" % exc.message,
                            hint="请检查模型服务后人工接管")
        if exc.code == E_MODEL_HTTP:
            status = exc.context.get("http_status")
            if status is not None and int(status) < 500:
                return CuaError(E_NEED_HUMAN, "模型服务 4xx（%s）：%s" % (status, exc.message),
                                hint="4xx 多为 key/额度/模型名问题，重试无意义，直接上报")
            return CuaError(E_NEED_HUMAN, "模型服务 5xx：%s" % exc.message,
                            hint="退避重试仍失败，转人工")
        return exc

    def _executor_feedback(self, exc: CuaError, frame: CaptureFrame) -> str:
        extra = "当前 image 尺寸：width=%d height=%d（坐标必须落在该范围内，超界会被直接拒绝）。" % (
            frame.width, frame.height)
        return error_feedback(exc, extra)

    def _replan_note(self, code: str, count: int) -> str:
        return ("上次同类操作被判定为「%s」（已重规划 %d 次）。禁止重复上一步的相同动作与坐标，"
                "请换一种策略：例如重新截局部放大看清目标、改用快捷键、或先滚动/移动再点击。"
                % (code, count))

    def _trace_entry(self, step: int, parsed: ParsedStep) -> Dict[str, Any]:
        return {
            "step": step,
            "thought": parsed.step.get("thought"),
            "action": parsed.action,
            "risk": parsed.risk,
            "done": parsed.done,
            "extraction": parsed.meta.get("extraction"),
            "coord_rounded": parsed.meta.get("coord_rounded"),
            "ok": None,
            "error_code": None,
            "screen_changed": None,
        }

    def _finish_from_step(self, parsed: ParsedStep, entry: Dict[str, Any]) -> Tuple[
        Optional[Dict[str, Any]], Optional[str], Optional[CuaError]
    ]:
        action = parsed.action or {}
        final = dict(parsed.final or {})
        if not final and action.get("type") == "done":
            final = {
                "status": action.get("status", "success"),
                "summary": action.get("summary", ""),
                "artifacts": action.get("artifacts") or [],
            }
        status = final.get("status") or "success"
        entry["ok"] = status == "success"
        entry["final_status"] = status
        if status == "success":
            return final, status, None
        if status == "need_human":
            return final, status, CuaError(E_NEED_HUMAN, final.get("summary") or "模型请求人工介入",
                                           hint="暂停并提示用户接管")
        # status=failed：模型自己判定失败，不算内部错误，只记 ok=False 与 final
        return final, status, None

    def _await_expect(
        self, frame: CaptureFrame, expect: Dict[str, Any]
    ) -> Tuple[bool, CaptureFrame, Optional[FrameDiff]]:
        """等待 expect；超时后按协议「再截一帧确认」。"""
        timeout_ms = int(expect.get("timeout_ms") or 0)
        must_change = bool(expect.get("must_change"))
        deadline = self._clock() + max(0.0, timeout_ms / 1000.0)
        current = frame
        last_diff: Optional[FrameDiff] = None
        while True:
            new = self.provider.capture()
            last_diff = diff_frames(frame.image, new.image, self.cfg)
            current = new
            if last_diff.screen_changed or not must_change:
                return True, current, last_diff
            if self._clock() >= deadline:
                break
            self._sleep(self.cfg.expect_poll_interval_s)
        # 协议第 5 节：再截一帧确认
        self._sleep(self.cfg.expect_poll_interval_s)
        new = self.provider.capture()
        last_diff = diff_frames(frame.image, new.image, self.cfg)
        if last_diff.screen_changed or not must_change:
            return True, new, last_diff
        return False, new, last_diff

    def _popup_policy(
        self, diff: FrameDiff, popup: PopupState, step: int, report: RunReport
    ) -> Optional[CuaError]:
        """第 7 节处置优先级：裁剪放大 -> 非破坏性选择 -> esc -> 转人工。"""
        if not diff.popup_suspected:
            popup.active = False
            popup.consecutive = 0
            popup.esc_sent = 0
            popup.crop = None
            popup.auto_cropped = False
            return None
        popup.active = True
        popup.consecutive += 1
        popup.crop = diff.crop_hint()
        report.popup_events += 1
        report.events.append({"step": step, "code": E_POPUP_BLOCKING, "action": "detected",
                              "diff": diff.to_dict()})
        self._log("popup_detected", step=step, diff=diff.to_dict(), consecutive=popup.consecutive)
        if popup.consecutive >= 2:
            if popup.esc_sent < self.cfg.popup_max_esc:
                popup.esc_sent += 1
                self._inject_esc(step, report, popup.esc_sent)
                return None
            return CuaError(
                E_NEED_HUMAN,
                "疑似弹窗持续遮挡，已按 %d 次 esc 仍未消失" % popup.esc_sent,
                hint="判断不了就直接交还控制权（协议第 7 节处置优先级第 3 条）",
                context={"popup_state": popup.__dict__},
            )
        return None

    def _inject_esc(self, step: int, report: RunReport, nth: int) -> None:
        """注入一次 esc（第 7 节：判断不了 -> 按一次 esc）。"""
        raw = {
            "protocol": PROTOCOL_VERSION,
            "thought": "（策略注入）检测到疑似弹窗，先按一次 esc",
            "action": {"type": "hotkey", "keys": ["esc"]},
            "done": False,
        }
        parsed = parse_step(json.dumps(raw, ensure_ascii=False), cfg=self.cfg)
        frame = self.provider.capture()
        outcome = self.executor.execute(parsed, frame)
        outcome.result["meta"]["injected"] = {"reason": E_POPUP_BLOCKING, "attempt": nth}
        report.results.append(outcome.result)
        report.events.append({"step": step, "code": E_POPUP_BLOCKING, "action": "esc", "attempt": nth})
        self._log("popup_esc", step=step, attempt=nth, result=outcome.result)

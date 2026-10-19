"""API-first 路由骨架（协议第 6 节）。

四级能力路由，从上往下命中第一个可用的就停：
  L0 结构化接口 -> L1 浏览器 DOM -> L2 系统命令（白名单）-> L3 GUI 视觉操控（兜底）。

判定规则要点：
  - 有能力探测：要实际探一次（探到 401 也说明接口存在；DNS 失败说明不存在），结果落日志；
  - 每次路由都记录 route = {level, reason, probe_result}，让「为什么用了慢路径」事后可审计；
  - 默认没有可用 API 才落到 GUI。
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional, Sequence, Tuple

from .config import (
    ROUTE_DESCRIPTIONS,
    ROUTE_L0,
    ROUTE_L1,
    ROUTE_L2,
    ROUTE_L3,
    ROUTE_LEVELS,
    Config,
)
from .errors import E_NEED_HUMAN, CuaError


@dataclass
class ProbeResult:
    reachable: bool
    detail: str
    status: Optional[int] = None

    def to_dict(self) -> Dict[str, Any]:
        return {"reachable": bool(self.reachable), "status": self.status, "detail": self.detail}


class CapabilityProbe:
    """能力探测接口。子类只需实现 probe()。"""

    level = ROUTE_L0
    name = "base"
    reason = ""

    def probe(self) -> ProbeResult:  # pragma: no cover - 接口
        raise NotImplementedError

    def to_dict(self) -> Dict[str, Any]:
        return {"level": self.level, "name": self.name, "reason": self.reason}


class HttpProbe(CapabilityProbe):
    """HTTP 探测：401/403 也算「接口存在」（协议第 6 节判定规则 1）。"""

    def __init__(self, url: str, level: str = ROUTE_L0, name: Optional[str] = None,
                 reason: str = "", timeout: float = 3.0, method: str = "GET",
                 expected_status: Optional[Sequence[int]] = None) -> None:
        self.url = url
        self.level = level
        self.name = name or url
        self.reason = reason or ("探测到可用接口 %s" % url)
        self.timeout = timeout
        self.method = method
        self.expected_status = tuple(expected_status or (200, 201, 204, 301, 302, 400, 401, 403, 405))

    def probe(self) -> ProbeResult:
        try:
            import httpx

            resp = httpx.request(self.method, self.url, timeout=self.timeout, follow_redirects=True)
            reachable = resp.status_code in self.expected_status or resp.status_code < 500
            return ProbeResult(
                reachable=bool(reachable),
                status=resp.status_code,
                detail="HTTP %s %s" % (resp.status_code, self.url),
            )
        except Exception as exc:
            return ProbeResult(False, "探测失败：%s: %s" % (type(exc).__name__, exc))


class CommandProbe(CapabilityProbe):
    """命令探测：可执行文件是否存在于 PATH（L0 CLI / L2 系统命令）。"""

    def __init__(self, command: str, level: str = ROUTE_L0, name: Optional[str] = None,
                 reason: str = "") -> None:
        self.command = command
        self.level = level
        self.name = name or command
        self.reason = reason or ("探测到可用命令 %s" % command)

    def probe(self) -> ProbeResult:
        import shutil

        found = shutil.which(self.command)
        return ProbeResult(bool(found), "found at %s" % found if found else "not found in PATH")


class StaticProbe(CapabilityProbe):
    """固定结果探针，用于测试与人工声明。"""

    def __init__(self, reachable: bool, level: str = ROUTE_L0, name: str = "static",
                 reason: str = "", detail: str = "") -> None:
        self._reachable = bool(reachable)
        self.level = level
        self.name = name
        self.reason = reason or ("人工声明可用" if reachable else "人工声明不可用")
        self._detail = detail

    def probe(self) -> ProbeResult:
        return ProbeResult(self._reachable, self._detail or self.reason)


@dataclass
class RouteDecision:
    level: str
    reason: str
    probe_result: Optional[Dict[str, Any]] = None
    candidates: List[Dict[str, Any]] = field(default_factory=list)

    def to_dict(self) -> Dict[str, Any]:
        return {
            "level": self.level,
            "reason": self.reason,
            "probe_result": self.probe_result,
            "candidates": self.candidates,
        }


class Router:
    """按 L0 -> L1 -> L2 -> L3 顺序命中第一个可用级别。"""

    def __init__(self, probes: Optional[Sequence[CapabilityProbe]] = None,
                 cfg: Optional[Config] = None, logger: Optional[Any] = None) -> None:
        self.cfg = cfg or Config()
        self.probes: List[CapabilityProbe] = list(probes or [])
        self.logger = logger

    def register(self, probe: CapabilityProbe) -> None:
        self.probes.append(probe)

    def route(self, task: str = "") -> RouteDecision:
        candidates: List[Dict[str, Any]] = []
        for level in ROUTE_LEVELS:
            if level == ROUTE_L3:
                break
            level_probes = [p for p in self.probes if p.level == level]
            for probe in level_probes:
                result = probe.probe()
                candidates.append({
                    "level": level,
                    "probe": probe.name,
                    "result": result.to_dict(),
                    "reason": probe.reason,
                })
                if result.reachable:
                    decision = RouteDecision(level, probe.reason, result.to_dict(), candidates)
                    self._log(task, decision)
                    return decision
        # 没有任何 API 级别可用 -> GUI 视觉操控兜底
        if not self.cfg.enable_gui_fallback:
            raise CuaError(E_NEED_HUMAN, "没有可用的 API 级别，且 GUI 兜底被关闭",
                           hint="请打开 enable_gui_fallback 或提供可用接口")
        decision = RouteDecision(
            ROUTE_L3,
            "L0~L2 均无可用能力（已探测 %d 个探针），按协议第 6 节落到 GUI 视觉操控兜底"
            % len(self.probes),
            None,
            candidates,
        )
        self._log(task, decision)
        return decision

    def _log(self, task: str, decision: RouteDecision) -> None:
        if self.logger is not None:
            self.logger.event("route", task=task, **decision.to_dict())


def default_router(cfg: Optional[Config] = None, logger: Optional[Any] = None) -> Router:
    """默认无任何探针 -> 直接落到 L3 GUI；探针可由配置 capability_probes 提供。"""
    cfg = cfg or Config()
    probes: List[CapabilityProbe] = []
    for item in cfg.capability_probes or []:
        kind = item.get("kind", "http")
        if kind == "http":
            probes.append(HttpProbe(item["url"], level=item.get("level", ROUTE_L0),
                                    name=item.get("name"), reason=item.get("reason", "")))
        elif kind == "command":
            probes.append(CommandProbe(item["command"], level=item.get("level", ROUTE_L0),
                                       name=item.get("name"), reason=item.get("reason", "")))
        elif kind == "static":
            probes.append(StaticProbe(bool(item.get("reachable")), level=item.get("level", ROUTE_L0),
                                      name=item.get("name", "static"), reason=item.get("reason", "")))
    return Router(probes, cfg=cfg, logger=logger)

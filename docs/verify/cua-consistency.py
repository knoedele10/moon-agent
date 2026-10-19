# -*- coding: utf-8 -*-
"""doc <-> code 一致性核对（verifier 独立编写，task-9）。

做三件事：
  1. 从 docs/cua/{01,02,03} 与 action.schema.json 里**解析出**契约事实；
  2. 从 cua/ 代码里**取出**同一事实（常量 / 派发表 / 运行时行为）；
  3. 逐条比对，打印 PASS / MISMATCH，并以退出码表达结论（0=全部一致）。

运行：python -X utf8 docs/verify/cua-consistency.py
"""
import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(ROOT / "cua" / "tests"))

import ct_fake_pyautogui  # noqa: E402  安全网：不动真实鼠标键盘
ct_fake_pyautogui.install()

import ct_oracle as O  # noqa: E402  task-7 的独立参考实现（期望值从文档冻结）

BT1 = chr(96)
FENCE = chr(96) * 3

DOC3 = (ROOT / "docs" / "cua" / "03-action-protocol.md").read_text(encoding="utf-8")
DOC2 = (ROOT / "docs" / "cua" / "02-agent-prompt.md").read_text(encoding="utf-8")
DOC1 = (ROOT / "docs" / "cua" / "01-architecture.md").read_text(encoding="utf-8")
SCHEMA = json.loads((ROOT / "docs" / "cua" / "action.schema.json").read_text(encoding="utf-8"))

RESULTS = []


def check(item, ok, evidence):
    RESULTS.append((item, bool(ok), evidence))
    print("[%s] %-36s %s" % ("PASS" if ok else "MISMATCH", item, evidence))


def section(title):
    print("\n=== %s ===" % title)


# ---------------- A. 动作类型 ----------------
section("A. 动作类型（应为 9 种，穷举）")
doc_actions = re.findall(r"^### 3\.(\d+) ([a-z_]+)", DOC3, re.M)
doc_names = [n for _, n in doc_actions]
check("doc03 3.1~3.9 动作数", len(doc_names) == 9, "解析到 %s" % doc_names)

schema_strs = []
def walk(node):
    if isinstance(node, dict):
        if isinstance(node.get("enum"), list) and all(isinstance(x, str) for x in node["enum"]):
            schema_strs.extend(node["enum"])
        if isinstance(node.get("const"), str):
            schema_strs.append(node["const"])
        for v in node.values():
            walk(v)
    elif isinstance(node, list):
        for v in node:
            walk(v)
walk(SCHEMA)
schema_set = {a for a in schema_strs if a in set(doc_names)}
check("action.schema.json 动作集合", schema_set == set(doc_names), "schema=%s" % sorted(schema_set))

import cua.executor as ex  # noqa: E402
src_exec = (ROOT / "cua" / "executor.py").read_text(encoding="utf-8")
dispatch = re.findall(r'^\s+"([a-z_]+)": self\._do_', src_exec, re.M)
check("executor.py 派发表动作集合", set(dispatch) == set(doc_names), "code=%s" % sorted(set(dispatch)))

m = re.search(FENCE + r"text\r?\n(# 角色.*?)" + FENCE, DOC2, re.S)
pdoc = m.group(1) if m else ""
sect_actions = pdoc.split("# 动作清单")[1].split("# 默认值")[0] if "# 动作清单" in pdoc else ""
pdoc_actions = [n for _, n in re.findall(r"^(\d)\. ([a-z_]+) ——", sect_actions, re.M)]
check("doc02 提示词列出的动作", pdoc_actions == doc_names, "doc02=%s" % pdoc_actions)

# ---------------- B. 错误码 ----------------
section("B. 错误码（应为 20 个）")
rows = re.findall(r"^\| " + BT1 + r"(E_[A-Z_]+)" + BT1 + r" \|(.*)\|(.*)\|(.*)\|\s*$", DOC3, re.M)
doc_codes = [c for c, _, _, _ in rows]
check("doc03 第 5 节错误码数", len(doc_codes) == 20, "共 %d 个" % len(doc_codes))

import cua.errors as er  # noqa: E402
check("ERROR_TABLE 与文档键集合一致", set(er.ERROR_TABLE) == set(doc_codes),
      "仅文档=%s 仅代码=%s" % (sorted(set(doc_codes) - set(er.ERROR_TABLE)),
                              sorted(set(er.ERROR_TABLE) - set(doc_codes))))
src_err = (ROOT / "cua" / "errors.py").read_text(encoding="utf-8")
const_codes = re.findall(r'^(E_[A-Z_]+) = "', src_err, re.M)
check("errors.py 常量与文档一致", set(const_codes) == set(doc_codes), "常量 %d 个" % len(const_codes))

bad = []
for code, _cond, retry_cell, _disp in rows:
    cell = retry_cell.strip()
    pol = er.ERROR_TABLE.get(code)
    if pol is None:
        bad.append((code, "缺失"))
        continue
    if code == "E_MODEL_HTTP":
        if not (er.is_retryable(code, 500) is True and er.is_retryable(code, 404) is False):
            bad.append((code, "5xx/4xx 语境规则不符"))
        continue
    doc_retry = cell.startswith("是")
    mm = re.search(r"最多 (\d+) 次", cell)
    doc_max = int(mm.group(1)) if mm else (0 if not doc_retry else -1)
    if bool(pol.retryable) != doc_retry:
        bad.append((code, "retryable 文档=%s 代码=%s" % (doc_retry, pol.retryable)))
    elif doc_max >= 0 and int(pol.max_retries) != doc_max:
        bad.append((code, "max_retries 文档=%d 代码=%d" % (doc_max, pol.max_retries)))
check("每码 retryable/max_retries 一致", not bad, "不一致=%s" % (bad or "无"))
check("E_STALL/E_LOOP 必须重规划", er.is_replan("E_STALL") and er.is_replan("E_LOOP")
      and not er.is_retryable("E_STALL") and not er.is_retryable("E_LOOP"), "is_replan 二者为真")

# ---------------- C. 键名白名单 ----------------
section("C. 键名白名单")
import cua.config as cfg_mod  # noqa: E402
doc_keys = set(O.KEY_WHITELIST)
code_keys = set(cfg_mod.KEY_WHITELIST)
doc_l = {k.lower() for k in doc_keys}
code_l = {k.lower() for k in code_keys}
check("白名单集合一致（大小写不敏感）", doc_l == code_l,
      "文档 %d / 代码 %d；仅文档=%s；仅代码=%s；大小写约定：文档写 F1-F12，代码存 f1..f12"
      % (len(doc_keys), len(code_keys), sorted(doc_l - code_l), sorted(code_l - doc_l)))
check("doc02 提示词写明同一套键名",
      all(k in pdoc for k in ("pageup", "pagedown", "backspace", "insert", "F1-F12")),
      "提示词含键名清单与 F1-F12")

# ---------------- D. 破坏性词表 ----------------
section("D. 破坏性词表")
doc_words = set(O.DESTRUCTIVE_WORDS)
code_words = set(cfg_mod.DESTRUCTIVE_WORDS)
check("词表一致（大小写不敏感）", {w.lower() for w in doc_words} == {w.lower() for w in code_words},
      "文档 %d / 代码 %d；对称差=%s" % (len(doc_words), len(code_words),
                                      sorted({w.lower() for w in doc_words} ^ {w.lower() for w in code_words})))
check("中文 13 + 英文 12 = 25", len(doc_words) == 25, "共 %d 条" % len(doc_words))
check("doc02 提示词逐字写出词表",
      all(w in pdoc for w in ("删除", "卸载", "格式化", "Delete", "Uninstall", "Format", "Erase",
                              "Reset", "Overwrite", "Purchase", "Grant", "Agree")),
      "提示词含中英文硬约束词")

# ---------------- E. 坐标公式 ----------------
section("E. 坐标公式与越界")
ok_doc = ("screen_x = origin.x + round(x / scale)" in DOC3
          and "screen_y = origin.y + round(y / scale)" in DOC3)
check("doc03 1.1 写明换算式", ok_doc, "文档含 round(x / scale) + origin")
import cua.screenshot as ss  # noqa: E402
from PIL import Image, ImageDraw  # noqa: E402
src_ss = (ROOT / "cua" / "screenshot.py").read_text(encoding="utf-8")
check("screenshot.py 含同一算式", re.search(r"frame\.origin\[0\] \+ round", src_ss) is not None,
      "源码 frame.origin[0] + round_half_up(x/scale)（文档写 round，实现用 round_half_up，见 docstring）")
W, H, SCALE, OX, OY = 1472, 828, 0.7667, 100, 40
frame = ss.CaptureFrame(image=Image.new("RGB", (W, H)), origin=(OX, OY), scale=SCALE,
                        screen_size=(2560, 1600))
bad = []
for x, y in [(0, 0), (W - 1, H - 1), (640, 210), (1, 1), (1435, 800), (700, 400)]:
    want = O.image_to_screen(x, y, (OX, OY), SCALE)
    got = tuple(ss.image_to_screen(x, y, frame))
    if got != want:
        bad.append(((x, y), want, got))
check("换算数值与文档公式一致（6 点）", not bad, "不一致=%s" % (bad or "无"))
oob_ok = True
for x, y in [(W, 10), (10, H), (-1, 5), (99999, 5)]:
    try:
        ss.check_image_bounds(x, y, frame, "verify")
        oob_ok = False
    except Exception as exc:
        if (getattr(exc, "code", None) or getattr(exc, "error_code", None)) != "E_OUT_OF_BOUNDS":
            oob_ok = False
check("越界拒绝且码为 E_OUT_OF_BOUNDS（4 点）", oob_ok, "x==width 也拒绝；未见 clamp")

# ---------------- F. 四级路由 ----------------
section("F. 四级能力路由")
route_levels = list(getattr(cfg_mod, "ROUTE_LEVELS", []))
raw_consts = {n: getattr(cfg_mod, n, None) for n in ("ROUTE_L0", "ROUTE_L1", "ROUTE_L2", "ROUTE_L3")}
check("代码定义 L0~L3 四级", sorted(route_levels) == ["L0", "L1", "L2", "L3"],
      "ROUTE_LEVELS=%s ；ROUTE_L0..L3=%s" % (route_levels, raw_consts))
doc_levels = re.findall(r"^\| \*\*(L\d)\*\* \|", DOC3, re.M)
check("doc03 第 6 节列出四级", doc_levels == ["L0", "L1", "L2", "L3"], "文档=%s" % doc_levels)
from cua.router import default_router  # noqa: E402
d = default_router(cfg_mod.Config()).route("验证默认兜底")
check("无探针时兜底到 L3（规则 4）", d.level == "L3" and "第 6 节" in (d.reason or ""),
      "level=%s reason=%s" % (d.level, (d.reason or "")[:46]))
check("路由记录含 level/reason/probe_result",
      {"level", "reason", "probe_result"} <= set(d.to_dict()),
      "实际=%s（实现另带 candidates 诊断字段，属超集）" % sorted(d.to_dict()))

# ---------------- G. 弹窗策略 ----------------
section("G. 弹窗检测与处置")
c = cfg_mod.Config()
check("检测阈值 3% / 80% / 0.25",
      abs(c.popup_area_min_ratio - 0.03) < 1e-9 and abs(c.popup_area_max_ratio - 0.80) < 1e-9
      and abs(c.popup_fill_min_ratio - 0.25) < 1e-9,
      "min=%.2f max=%.2f fill=%.2f" % (c.popup_area_min_ratio, c.popup_area_max_ratio, c.popup_fill_min_ratio))


def modal_sample(w=800, h=600, area=0.3):
    a = Image.new("RGB", (w, h), (18, 18, 24))
    ImageDraw.Draw(a).rectangle([0, 0, w - 1, h - 1], fill=(30, 30, 40))
    bw, bh = int(w * area ** 0.5), int(h * area ** 0.5)
    x0, y0 = (w - bw) // 2, (h - bh) // 2
    ImageDraw.Draw(a).rectangle([x0, y0, x0 + bw, y0 + bh], fill=(245, 245, 245))
    b = a.copy()
    ImageDraw.Draw(b).rectangle([x0 + 10, y0 + 10, x0 + bw - 10, y0 + bh - 10], fill=(200, 60, 60))
    return a, b


a, b = modal_sample()
diff = ss.diff_frames(a, b, c)
check("模态框样张判为疑似弹窗", bool(diff.popup_suspected),
      "bbox面积比=%.3f 填充率=%.2f" % (diff.bbox_area_ratio, diff.fill_ratio))
a2, b2 = modal_sample(area=0.90)
d2 = ss.diff_frames(a2, b2, c)
check("贴满全屏的突变不判弹窗", not bool(d2.popup_suspected), "面积比=%.3f" % d2.bbox_area_ratio)
a3 = Image.new("RGB", (800, 600), (18, 18, 24))
b3 = a3.copy()
ImageDraw.Draw(b3).rectangle([5, 5, 5, 5], fill=(255, 255, 255))
d3 = ss.diff_frames(a3, b3, c)
check("单像素变化不判弹窗", not bool(d3.popup_suspected), "面积比=%.6f" % d3.bbox_area_ratio)
check("esc 上限 2 次后转人工", int(c.popup_max_esc) == 2 and bool(c.popup_auto_crop),
      "popup_max_esc=%d popup_auto_crop=%s" % (c.popup_max_esc, c.popup_auto_crop))

# ---------------- H. 预算 ----------------
section("H. 预算（40/300 只对云端档）")
check("默认档 cloud：40 步 / 300s",
      c.step_budget == 40 and abs(c.wall_budget_s - 300.0) < 1e-9 and c.budget_profile == "cloud",
      "steps=%d wall=%.0f profile=%s" % (c.step_budget, c.wall_budget_s, c.budget_profile))
local = cfg_mod.BUDGET_PROFILES.get("local", {})
check("local 档墙钟 >= 1800s", float(local.get("wall_budget_s", 0)) >= 1800.0,
      "local wall=%s" % local.get("wall_budget_s"))
check("按 base_url 自动分档",
      cfg_mod.resolve_budget_profile("auto", "http://127.0.0.1:11434") == "local"
      and cfg_mod.resolve_budget_profile("auto", "https://api.openai.com/v1") == "cloud",
      "本地->local，远端->cloud")
check("文档 8.4 已写明分档", "只对云端 API 后端成立" in DOC3 and "1800" in DOC3, "文档含裁定文字")

# ---------------- I. 安全不变量 ----------------
section("I. 安全不变量（第 8 节七条）")
check("8.1 FAILSAFE 永久开启",
      "pyautogui.FAILSAFE = True" in src_exec and not re.search(r"FAILSAFE\s*=\s*False", src_exec),
      "显式置 True，无关闭路径")
raw = tuple(ss.image_to_screen(99999, -5, frame))
formula = (OX + O.round_half_up(99999 / SCALE), OY + O.round_half_up(-5 / SCALE))
check("8.2 越界不 clamp（行为）", raw == formula,
      "越界点仍按公式算出 %s（无取模/翻转），拒绝由 check_image_bounds 抛 E_OUT_OF_BOUNDS" % (raw,))
check("8.3 步间 >=150ms", "150" in src_exec, "executor 内含 150ms 步间隔")
check("8.5 危险组合键默认关闭",
      any(k in src_exec.lower() for k in ("allowlist", "dangerous", "window_hotkeys")),
      "存在危险组合键开关")
check("8.6 risk=high 需放行", "risk" in src_exec and "high" in src_exec, "executor 内出现 risk/high")
check("8.7 终止释放修饰键", "def release_all_modifiers" in src_exec, "release_all_modifiers 存在")
src_loop = (ROOT / "cua" / "loop.py").read_text(encoding="utf-8")
loop_finally = re.search(r"finally:\s*\n\s+self\.executor\.release_all_modifiers\(\)", src_loop) is not None
exec_finally = re.search(r"finally:[\s\S]{0,400}?self\.release_all_modifiers\(\)", src_exec) is not None
check("8.7 任何终止路径都释放修饰键", loop_finally and exec_finally,
      "loop.run 与 executor.execute 的 finally 都调用释放（覆盖正常/异常/Ctrl+C）")

# ---------------- J. 提示词同源 ----------------
section("J. 提示词是否与文档同源")
import cua.prompts as pr  # noqa: E402
active = None
for name in ("system_prompt", "get_system_prompt", "load_system_prompt"):
    fn = getattr(pr, name, None)
    if callable(fn):
        try:
            active = fn()
            break
        except Exception as exc:  # noqa: BLE001
            active = ("ERR:%s" % exc, "ERR")
if active is None:
    check("能取到运行期系统提示词", False, "未找到公开入口")
else:
    text, origin = active if isinstance(active, tuple) else (active, "?")
    check("运行期提示词取自文档而非占位符",
          "02-agent-prompt.md" in str(origin) and "GUI 操作 agent" in text and "占位" not in text[:200],
          "来源=%s 长度=%d" % (origin, len(text)))
    check("提示词含 9 种动作与 cua/1",
          "cua/1" in text and all(t in text for t in doc_names), "含全部动作名与协议版本")
    check("提示词含白名单与破坏性词硬约束",
          "E_KEY_NOT_ALLOWED" in text and "need_human" in text and "删除" in text and "Delete" in text,
          "含 E_KEY_NOT_ALLOWED / need_human / 中英文词表")

failed = [r for r in RESULTS if not r[1]]
print("\n---- 汇总 ----")
print("检查项 %d ；一致 %d ；不一致 %d" % (len(RESULTS), len(RESULTS) - len(failed), len(failed)))
for item, _ok, ev in failed:
    print("  MISMATCH %s :: %s" % (item, ev))
sys.exit(1 if failed else 0)

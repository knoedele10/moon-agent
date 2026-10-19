# -*- coding: utf-8 -*-
"""task-9 第 3 项要求的六条黑盒探测（verifier 自写）。

只走契约里定义的入口与"行为"，断言按 docs/cua/03-action-protocol.md 冻结；
用 task-7 的适配层解析真实入口、用记录式假 pyautogui 保证不会碰真实键鼠。
运行：python -X utf8 docs/verify/cua-blackbox.py
"""
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(ROOT / "cua" / "tests"))

import ct_fake_pyautogui as F  # noqa: E402
F.install()
import ct_adapter as A  # noqa: E402

F3 = chr(96) * 3
R = []


def code_of(x):
    """把各种形态的实现返回值归一成"错误码字符串"。"""
    if x is None:
        return ""
    if isinstance(x, str):
        return x
    c = getattr(x, "code", None) or getattr(x, "error_code", None)
    if c:
        return str(c)
    try:
        return str(A.verdict(x))
    except Exception:  # noqa: BLE001
        return str(x)


def rec(name, ok, evidence):
    R.append((name, bool(ok)))
    print("[%s] %-32s %s" % ("PASS" if ok else "FAIL", name, evidence))


IMG_W, IMG_H = 1472, 828

print("== 1) dry-run 下零真实键鼠调用 ==")
cap = A.dry_run_executor()
F.get().reset()
res = cap({"type": "click", "x": 100, "y": 100})
calls = F.get().input_calls()
rec("dry_run_zero_input", not calls and not A.rejected(res),
    "入口 %s；真实输入调用=%s；结果=%s" % (cap.where, calls, A.verdict(res)))

print("\n== 2) 越界被拒且未被 clamp ==")
try:
    res_oob = A.bounds_enforced_convert()(4000, 20, {"x": 0, "y": 0}, 1.0, IMG_W, IMG_H)
    if A.rejected(res_oob):
        got = A.rejection_code(res_oob) or "REJECTED_NO_CODE"
    else:
        got = "未被拒绝（返回 %r）" % (A.unwrap(res_oob),)
except Exception as exc:  # noqa: BLE001
    got = str(getattr(exc, "code", None) or getattr(exc, "error_code", None) or exc)
raw = A.unwrap(A.image_to_screen()(IMG_W + 500, 20, {"x": 0, "y": 0}, 1.0))
rec("oob_rejected_not_clamped", "E_OUT_OF_BOUNDS" in str(got),
    "x=1972 于 %dx%d -> %s；同一坐标按公式算是 %s（拒绝而不是收敛到边界）"
    % (IMG_W, IMG_H, got, raw))

print("\n== 3) 围栏 + 前后自然语言仍可解析 ==")
payload = json.dumps({"protocol": "cua/1", "thought": "点按钮",
                      "action": {"type": "click", "x": 640, "y": 210}, "done": False}, ensure_ascii=False)
raw_text = "好的，我看清楚界面了。\n" + F3 + "json\n" + payload + "\n" + F3 + "\n以上是这一步的决策。"
ext = A.json_extractor()(raw_text)
val = A.unwrap(ext)
action = (val or {}).get("action") if isinstance(val, dict) else None
rec("fenced_json_parsed", isinstance(action, dict) and action.get("type") == "click",
    "解析入口 %s；得到 action=%s" % (getattr(ext, "where", "?"), json.dumps(action, ensure_ascii=False)))

print("\n== 4) 未知 action.type -> E_UNKNOWN_ACTION ==")
bad = {"protocol": "cua/1", "thought": "执行命令", "action": {"type": "exec", "cmd": "rm -rf /"}, "done": False}
rr = A.reject_step(bad)
rec("unknown_action_code", "E_UNKNOWN_ACTION" in code_of(rr),
    "拒绝面 %s -> %s" % (getattr(rr, "where", "step 校验/执行面"), code_of(rr)))
rr2 = A.reject_step({"protocol": "cua/1", "thought": "x", "action": {"type": "shell", "cmd": "dir"}, "done": False})
rec("no_shell_escape_hatch", "E_UNKNOWN_ACTION" in code_of(rr2), "shell 同样被拒 -> %s" % code_of(rr2))

print("\n== 5) 失败终止后修饰键被释放 ==")
ex = A.action_executor()
F.get().reset()
F.get().fail_next = True
res5 = ex({"type": "hotkey", "keys": ["ctrl", "c"]})
pressed = dict(F.get().pressed)
unpaired = F.get().unpaired_keydowns()
rec("modifiers_released_on_failure", not pressed and not unpaired,
    "注入 FailSafe 后 ctrl+c -> %s；仍按下=%s；未配对 keyDown=%s" % (code_of(res5), pressed, unpaired))

print("\n== 6) 弹窗命中破坏性词 -> 不点击、转人工 ==")
guard = A.click_guard()
step6 = {"protocol": "cua/1", "thought": "弹窗按钮是「永久删除」",
         "action": {"type": "click", "x": 400, "y": 300}, "done": False}
F.get().reset()
hits = A.destructive_predicate()("确认 删除 该文件？")
v = A.rejection_code(guard(step6["thought"], step6["action"])) or code_of(guard(step6["thought"], step6["action"]))
rec("destructive_needs_human", bool(hits) and "E_NEED_HUMAN" in str(v),
    "词表命中 %s；护栏判定 %s（未执行点击；真实输入调用=%s）" % (hits, v, F.get().input_calls()))
safe = {"protocol": "cua/1", "thought": "点取消关闭弹窗",
        "action": {"type": "click", "x": 400, "y": 300}, "done": False}
rec("safe_button_not_forced_human",
    not A.unwrap(A.destructive_predicate()("取消"))
    and "E_NEED_HUMAN" not in str(A.rejection_code(guard(safe["thought"], safe["action"]))),
    "「取消」不命中词表，护栏判定 %s" % (A.rejection_code(guard(safe["thought"], safe["action"])) or "放行"))

failed = [n for n, ok in R if not ok]
print("\n---- 汇总：探测 %d 项，通过 %d，失败 %d ----" % (len(R), len(R) - len(failed), len(failed)))
for n in failed:
    print("  FAIL %s" % n)
sys.exit(1 if failed else 0)

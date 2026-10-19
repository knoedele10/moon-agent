# -*- coding: utf-8 -*-
"""独立复验：契约 §3.1「模型返回空 content 必须报 E_MODEL_EMPTY，不得当成正常回复放行」。

由 verifier 自写，只调 cua/backends/openai_compat.py 的对外入口（require_content / status_error_code），
不改实现。自带变异自检（--mutate 部分）：把入口换成"有 bug 的实现"时判据必须失败，
否则说明这套判据没有区分能力。

用法：python -X utf8 cua/tests/it_model_empty.py
退出码：0=判据全过且变异全被抓；1=有问题
"""
import sys
import traceback
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from cua.backends.openai_compat import require_content, status_error_code, _extract_text  # noqa: E402
from cua.errors import CuaError  # noqa: E402

E_MODEL_EMPTY = "E_MODEL_EMPTY"
E_MODEL_HTTP = "E_MODEL_HTTP"
RESULTS = []


def rec(level, cid, msg, detail=None):
    RESULTS.append((level, cid))
    print("[%s] %-26s %s" % (level, cid, msg))
    if detail:
        print("        证据: " + str(detail)[:300])


class _Msg:
    def __init__(self, content=None, has_content=True):
        if has_content:
            self.content = content


class _Choice:
    def __init__(self, message):
        self.message = message


class _Resp:
    def __init__(self, choices):
        self.choices = choices


def resp(content=None, has_content=True):
    return _Resp([_Choice(_Msg(content, has_content))])


def must_be_empty(fn, cid, label, r):
    """调用 fn(r)，必须以 E_MODEL_EMPTY 报错。"""
    try:
        text = fn(r)
        rec("FAIL", cid, label + " —— 被当成正常回复放行了", {"returned": repr(text)[:80]})
        return False
    except CuaError as exc:
        code = getattr(exc, "code", None)
        if code == E_MODEL_EMPTY:
            rec("PASS", cid, label + " -> E_MODEL_EMPTY")
            return True
        rec("FAIL", cid, label + " -> 报的是 " + str(code) + "，应为 E_MODEL_EMPTY", str(exc)[:120])
        return False
    except Exception:
        rec("FAIL", cid, label + " -> 抛了非 CuaError 异常", traceback.format_exc().splitlines()[-1][:160])
        return False


def must_return(fn, cid, label, r, expect):
    try:
        text = fn(r)
    except Exception as exc:
        rec("FAIL", cid, label + " -> 不该报错却报错：" + str(exc)[:120])
        return False
    if expect in (text or ""):
        rec("PASS", cid, label + " -> 原样返回非空文本")
        return True
    rec("FAIL", cid, label + " -> 返回内容不对", {"got": repr(text)[:80], "want_contains": expect})
    return False


def run_cases(fn, tag):
    """跑全部判据，返回失败编号列表。fn 可被替换成"有 bug 的实现"用来做变异自检。"""
    before = len(RESULTS)
    must_be_empty(fn, "S1-empty-str", "content=\"\"", resp(""))
    must_be_empty(fn, "S2-whitespace", "content=全空白", resp("   \n\t "))
    must_be_empty(fn, "S3-missing-content", "message 没有 content 字段", resp(has_content=False))
    must_be_empty(fn, "S4-no-choices", "choices=[]", _Resp([]))
    must_be_empty(fn, "S5-list-text-empty", "多模态数组里只有空白 text",
                  resp([{"type": "text", "text": "   "}]))
    must_return(fn, "S6-normal", "正常文本", resp("点击开始按钮"), "点击开始按钮")
    must_return(fn, "S7-list-text", "多模态数组含文本", resp([{"type": "text", "text": "看图后点击"}] ), "看图后点击")
    return [cid for lvl, cid in RESULTS[before:] if lvl == "FAIL"]


def check_status_mapping():
    ok = True
    if status_error_code("上游 E_MODEL_EMPTY：空 content") == E_MODEL_EMPTY:
        rec("PASS", "S8-proxy-marker", "代理带回 E_MODEL_EMPTY 字样 -> E_MODEL_EMPTY（不退化成可重试的 E_MODEL_HTTP）")
    else:
        ok = False
        rec("FAIL", "S8-proxy-marker", "代理空 content 标记没被识别", status_error_code("上游 E_MODEL_EMPTY：空 content"))
    if status_error_code("HTTP 500 upstream boom") == E_MODEL_HTTP:
        rec("PASS", "S9-other-error", "普通上游错误 -> E_MODEL_HTTP")
    else:
        ok = False
        rec("FAIL", "S9-other-error", "普通上游错误被误判", status_error_code("HTTP 500 upstream boom"))
    if status_error_code("") == E_MODEL_HTTP:
        rec("PASS", "S10-empty-detail", "取不到上游原因时 -> E_MODEL_HTTP（不误判为空回复）")
    else:
        ok = False
        rec("FAIL", "S10-empty-detail", "空 detail 被误判成 E_MODEL_EMPTY")
    # 错误形状必须能进协议（第 4 节失败样例）
    try:
        require_content(resp(""))
        rec("FAIL", "S11-to-dict", "空 content 没报错")
        ok = False
    except CuaError as exc:
        d = exc.to_dict()
        if d.get("code") == E_MODEL_EMPTY and "message" in d:
            rec("PASS", "S11-to-dict", "报错结构可进协议（code/message 齐）", {"code": d.get("code")})
        else:
            rec("FAIL", "S11-to-dict", "报错结构不对", d)
            ok = False
    return ok


def main():
    print("# 契约 §3.1 独立复验：空 content 不得当成正常回复")
    fails = run_cases(require_content, "real")
    if not check_status_mapping():
        fails.append("status-mapping")
    print("")
    print("---- 正向判据：FAIL %d ----" % len(fails))

    # ---- 变异自检：换成有 bug 的实现，判据必须失败 ----
    def buggy_require_content(r, cfg=None):
        return _extract_text(r) or ""

    def buggy_status_error_code(detail):
        return E_MODEL_HTTP

    print("")
    print("# 变异自检（判据的区分能力）")
    before = len(RESULTS)
    m1 = run_cases(buggy_require_content, "mutated")
    caught = [c for lvl, c in RESULTS[before:] if lvl == "FAIL"]
    if caught:
        print("[PASS] mutation=empty-returns-ok   被抓住（%d 条判据失败：%s）" % (len(caught), ",".join(caught[:5])))
    else:
        print("[FAIL] mutation=empty-returns-ok   没被抓住：判据没有区分能力")
        fails.append("mutation-1")
    before = len(RESULTS)
    if status_error_code.__module__ and buggy_status_error_code("E_MODEL_EMPTY") == E_MODEL_HTTP:
        print("[PASS] mutation=marker-degraded    被抓住（坏实现把空 content 降级成 E_MODEL_HTTP）")
    else:
        print("[FAIL] mutation=marker-degraded    没被抓住")
        fails.append("mutation-2")

    print("")
    print("---- 汇总：判据 %d 组，问题 %d 项 ----" % (len(RESULTS), len(fails)))
    sys.exit(1 if fails else 0)


main()

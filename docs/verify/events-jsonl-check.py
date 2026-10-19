# -*- coding: utf-8 -*-
"""I1 事件流 + C14 编码保真探测（verifier 自写）。

对齐：docs/cua/06-integration-contract.md §1，以及 Lead 给出的 C14 三条验收判据。

判据：
  E2-jsonl        stdout 每行都是可解析 JSON（人类可读打印必须全关）
  E2-kinds        每行 kind 落在契约集合内（11 种 + 可选 frame_image）
  E2-span         首行 run_start、末行 run_end
  E2-frame-image  frame_image 跟在 frame 之后、image/jpeg、长边 640、base64 可解且 JPEG magic 对
  E8a-utf8-strict stdout 原始字节可被严格 UTF-8 解码（errors='strict' 不抛）
  E8b-ufffd       全量文本里 U+FFFD 计数必须为 0
  E8c-task-fid    run_start 回显的 task 与提交值逐码点相同
  E8d-gbk-diag    诊断（INFO）：原始字节按 GBK 解码是否含提交的 task；True = GBK 落字节的铁证
  E8e-lossless    每行"解码后再编码"必须逐字节等于原文（单行乱码也藏不住）
  E6/E7           --events-images 必须与 --events 同用；不带 --events 时既有行为不变

为什么不能只看 json.loads：Go 的 encoding/json 对非法 UTF-8 不报错，而是静默替换成 U+FFFD，
所以"能解析"完全不代表字节是对的，必须看原始字节。

用法：
  python -X utf8 docs/verify/events-jsonl-check.py
  python -X utf8 docs/verify/events-jsonl-check.py --task "新建一个名为测试的文件夹" --extra-args "--max-steps 1" --no-events-images
  python -X utf8 docs/verify/events-jsonl-check.py --producer "python -X utf8 docs/verify/events-fake-producer.py"
退出码：0=全 PASS；1=有 FAIL；2=前置缺失（--events 尚未落地）
"""
import argparse
import base64
import json
import os
import shlex
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]

KINDS = ["run_start", "frame", "observation", "agent", "step", "action_result",
         "replan", "expect_timeout", "executor_retry", "guardrail", "run_end"]
EXTRA_KINDS = ["frame_image"]
ALL_KINDS = set(KINDS) | set(EXTRA_KINDS)
DEFAULT_TASK = "新建一个名为测试的文件夹"

RESULTS = []


def rec(level, cid, msg, detail=None):
    RESULTS.append((level, cid))
    print("[%s] %-20s %s" % (level, cid, msg))
    if detail is not None:
        text = detail if isinstance(detail, str) else json.dumps(detail, ensure_ascii=False)
        print("        证据: " + text[:400])


def run_producer(cmd, timeout):
    tag = str(os.getpid())
    out_path = Path(tempfile.gettempdir()) / ("it_events_out_" + tag + ".bin")
    err_path = Path(tempfile.gettempdir()) / ("it_events_err_" + tag + ".bin")
    with open(out_path, "wb") as fo, open(err_path, "wb") as fe:
        try:
            proc = subprocess.run(cmd, stdout=fo, stderr=fe, timeout=timeout, cwd=str(ROOT))
            code = proc.returncode
        except subprocess.TimeoutExpired:
            code = -1
        except FileNotFoundError as exc:
            return None, b"", str(exc).encode("utf-8")
    raw = out_path.read_bytes()
    err = err_path.read_bytes()
    for p in (out_path, err_path):
        try:
            p.unlink()
        except OSError:
            pass
    return code, raw, err


def check_encoding(raw, task):
    text = ""
    try:
        text = raw.decode("utf-8")
        rec("PASS", "E8a-utf8-strict", "stdout 原始字节可严格 UTF-8 解码（%d 字节）" % len(raw))
    except UnicodeDecodeError as exc:
        text = raw.decode("utf-8", errors="replace")
        bad = raw[exc.start] if exc.start < len(raw) else None
        rec("FAIL", "E8a-utf8-strict", "stdout 原始字节不是合法 UTF-8（管道下 stdout.encoding 很可能退化成 gbk）",
            {"字节数": len(raw), "第一个坏字节位置": exc.start,
             "坏字节": (hex(bad) if bad is not None else None), "错误": str(exc)[:140]})
    n_fffd = text.count("\ufffd")
    if n_fffd == 0:
        rec("PASS", "E8b-ufffd", "全量文本 U+FFFD 计数 = 0（共 %d 个字符）" % len(text))
    else:
        rec("FAIL", "E8b-ufffd", "全量文本出现 %d 个 U+FFFD（中文被替换成乱码）" % n_fffd, {"总字符": len(text)})

    got = None
    for line in text.splitlines():
        if not line.strip():
            continue
        try:
            obj = json.loads(line)
        except Exception:
            continue
        if isinstance(obj, dict) and obj.get("kind") == "run_start":
            got = obj.get("task")
            break
    if got == task:
        rec("PASS", "E8c-task-fid", "run_start 回显 task 与提交值逐码点相同（%d 个码点）" % len(list(task)))
    else:
        rec("FAIL", "E8c-task-fid", "run_start 回显的 task 与提交值不一致（中文在子进程往返里失真）",
            {"提交": task, "回显": got,
             "回显码点前 16": [hex(ord(c)) for c in list(got or "")[:16]]})

    try:
        gbk_hit = task in raw.decode("gbk", errors="ignore")
    except Exception:
        gbk_hit = False
    rec("INFO", "E8d-gbk-diag",
        ("原始字节按 GBK 解码后包含提交的 task（GBK 落字节的铁证）" if gbk_hit
         else "原始字节按 GBK 解码后不包含提交的 task（不是 GBK 路径）"))

    bad_lines = []
    for i, line in enumerate(raw.split(b"\n"), 1):
        if not line.strip():
            continue
        try:
            s = line.decode("utf-8")
        except UnicodeDecodeError:
            bad_lines.append((i, "decode-fail"))
            continue
        if s.encode("utf-8") != line:
            bad_lines.append((i, "reencode-differs"))
    if not bad_lines:
        rec("PASS", "E8e-lossless", "每一行解码后再编码都逐字节等于原文（单行乱码也藏不住）")
    else:
        rec("FAIL", "E8e-lossless", "有 %d 行不是无损 UTF-8" % len(bad_lines), bad_lines[:4])
    return text


def check_shape(text, prefix):
    lines = [ln for ln in text.splitlines() if ln.strip()]
    if not lines:
        rec("FAIL", prefix + "-output", "stdout 没有任何输出")
        return
    objs = []
    bad = []
    for i, ln in enumerate(lines, 1):
        try:
            objs.append((i, json.loads(ln)))
        except Exception:
            bad.append((i, ln[:60]))
    if not bad:
        rec("PASS", prefix + "-jsonl", "stdout %d 行全部是可解析 JSON" % len(lines))
    else:
        rec("FAIL", prefix + "-jsonl", "有 %d 行不是 JSON" % len(bad), bad[:3])
    kinds = []
    bad_kind = []
    for i, obj in objs:
        k = obj.get("kind") if isinstance(obj, dict) else None
        kinds.append(k)
        if k not in ALL_KINDS:
            bad_kind.append((i, k))
    if not bad_kind:
        rec("PASS", prefix + "-kinds", "每行都有合法 kind（%d 行）" % len(objs))
    else:
        rec("FAIL", prefix + "-kinds", "出现契约外/缺失 kind", bad_kind[:4])
    if kinds and kinds[0] == "run_start" and kinds[-1] == "run_end":
        rec("PASS", prefix + "-span", "首行 run_start、末行 run_end")
    else:
        rec("FAIL", prefix + "-span", "应为 run_start ... run_end",
            {"first": kinds[0] if kinds else None, "last": kinds[-1] if kinds else None})
    frames = [i for i, k in enumerate(kinds) if k == "frame"]
    imgs = [(i, o) for i, o in objs if isinstance(o, dict) and o.get("kind") == "frame_image"]
    if not imgs:
        rec("SKIP", prefix + "-frame-image", "本次没有 frame_image 事件")
        return
    shape_bad = []
    for i, obj in imgs:
        if not isinstance(obj.get("step"), int):
            shape_bad.append((i, "step"))
        if obj.get("media_type") != "image/jpeg":
            shape_bad.append((i, "media_type"))
        w, h = obj.get("width"), obj.get("height")
        if not (isinstance(w, int) and isinstance(h, int)):
            shape_bad.append((i, "width/height"))
        elif max(w, h) != 640:
            shape_bad.append((i, "长边=%d" % max(w, h)))
        uri = obj.get("data_uri")
        if not (isinstance(uri, str) and uri.startswith("data:image/jpeg;base64,")):
            shape_bad.append((i, "data_uri 前缀"))
            continue
        try:
            raw_img = base64.b64decode(uri.split(",", 1)[1], validate=False)
        except Exception:
            shape_bad.append((i, "base64"))
            continue
        if raw_img[:3] != b"\xff\xd8\xff":
            shape_bad.append((i, "JPEG magic"))
    if shape_bad:
        rec("FAIL", prefix + "-frame-image", "frame_image 字段不符契约", shape_bad[:4])
    elif not (frames and imgs[0][0] - 1 >= frames[0]):
        rec("FAIL", prefix + "-frame-image", "frame_image 没有出现在 frame 之后")
    else:
        rec("PASS", prefix + "-frame-image", "frame_image %d 条：在 frame 之后、image/jpeg、长边 640、base64 可解" % len(imgs))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--producer", default=None, help="自定义被探测命令（需接受 --task <text>）")
    ap.add_argument("--task", default=os.environ.get("IT_TASK") or DEFAULT_TASK, help="提交给 CUA 的任务（默认含中文；可用 IT_TASK 环境变量给 emoji 等向量）")
    ap.add_argument("--extra-args", default=None, help="追加给 producer 的额外参数（空格分隔）")
    ap.add_argument("--no-events-images", action="store_true", help="不加 --events-images（对齐 Lead 复现命令）")
    ap.add_argument("--timeout", type=int, default=180)
    args = ap.parse_args()

    extra = shlex.split(args.extra_args, posix=(os.name != "nt")) if args.extra_args else []
    images = [] if args.no_events_images else ["--events-images"]
    if args.producer:
        cmd = shlex.split(args.producer, posix=(os.name != "nt")) + ["--task", args.task] + images + extra
    else:
        cmd = [sys.executable, "-X", "utf8", "-m", "cua.main", "--task", args.task,
               "--mock", "--dry-run", "--no-log", "--events", "stdout"] + images + extra
    print("# I1/C14 事件流探测  producer=" + " ".join(cmd))
    # 先自证向量本身完好（emoji 走宽字符 API，若在传递环节被破坏这里就会暴露），
    # 与 cua-py 用 ",".join(hex(ord(c))) 转储的做法一致。
    rec("INFO", "E0-vector", "本次任务向量 %d 个码点" % len(list(args.task)), ",".join(hex(ord(c)) for c in args.task) if len(args.task) <= 40 else "(过长)")
    code, raw, err = run_producer(cmd, args.timeout)
    if code is None:
        rec("INFO", "Z-producer", "producer 起不来", err.decode("utf-8", "replace")[:200])
        print("PREREQ_MISSING: producer 无法启动")
        sys.exit(2)
    err_text = err.decode("utf-8", "replace")
    if args.producer is None and code != 0 and ("unrecognized arguments" in err_text or "--events" in err_text):
        rec("INFO", "Z-prereq", "--events 尚未落地（CLI 不认这个开关）", err_text.strip().splitlines()[-1:])
        print("PREREQ_MISSING: cua.main 还没有 --events stdout")
        sys.exit(2)
    rec("INFO", "E1-producer-exit", "producer 退出码 %s，stdout %d 字节" % (code, len(raw)))
    text = check_encoding(raw, args.task)
    check_shape(text, "E2")

    if args.producer is not None:
        rec("SKIP", "E6-events-images-requires-events", "自定义 producer 不适用")
        rec("SKIP", "E7-default-behavior-unchanged", "自定义 producer 不适用")
    else:
        base = [sys.executable, "-X", "utf8", "-m", "cua.main", "--task", args.task, "--mock", "--dry-run", "--no-log"]
        c2, o2, e2 = run_producer(base + ["--events-images"], 120)
        if c2 not in (0, None):
            rec("PASS", "E6-events-images-requires-events", "--events-images 单独用被拒（exit=%s）" % c2,
                e2.decode("utf-8", "replace").strip().splitlines()[-1:] or None)
        else:
            rec("FAIL", "E6-events-images-requires-events", "--events-images 未与 --events 同用时应报错，实际 exit=%s" % c2)
        c3, o3, e3 = run_producer(base, 120)
        first = [ln for ln in o3.decode("utf-8", "replace").splitlines() if ln.strip()][:1]
        is_json = False
        if first:
            try:
                json.loads(first[0])
                is_json = True
            except Exception:
                is_json = False
        if c3 == 0 and not is_json:
            rec("PASS", "E7-default-behavior-unchanged", "不带 --events 时 stdout 仍是人类可读（既有行为不变）", first)
        else:
            rec("FAIL", "E7-default-behavior-unchanged", "不带 --events 时不该输出 JSONL", {"exit": c3, "first": first})

    fails = [r for r in RESULTS if r[0] == "FAIL"]
    print("")
    print("---- 汇总：检查 %d 项，FAIL %d ----" % (len(RESULTS), len(fails)))
    for f in fails:
        print("  FAIL " + f[1])
    sys.exit(1 if fails else 0)


main()

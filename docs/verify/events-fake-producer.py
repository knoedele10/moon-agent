# -*- coding: utf-8 -*-
"""负向对照用的假事件生产者：按环境变量 IT_MUT 制造各种违约（含 C14 编码违约）。

必须接受 --task <text> 并把收到的 task 原样回显进 run_start（模拟真实 CLI 的契约行为），
这样 C14 的 --task 回显保真判据（E8c）在负向对照里才有意义。
"""
import argparse
import base64
import json
import os
import sys

MUT = os.environ.get("IT_MUT", "")
ap = argparse.ArgumentParser()
ap.add_argument("--task", default="(fake)")
ap.add_argument("--events-images", action="store_true")
args, _unknown = ap.parse_known_args()
TASK = args.task
JPEG = base64.b64encode(b"\xff\xd8\xff" + b"\x00" * 200).decode()


def emit(obj):
    print(json.dumps(obj, ensure_ascii=False), flush=True)


run_start = {"kind": "run_start", "protocol": "cua/1", "task": TASK}
frame = {"kind": "frame", "step": 0, "image": {"width": 640, "height": 360}}
run_end = {"kind": "run_end", "ok": True}
image = {"kind": "frame_image", "step": 0, "media_type": "image/jpeg",
         "width": 640, "height": 360, "data_uri": "data:image/jpeg;base64," + JPEG}

if MUT == "human_text":
    print("正在思考，请稍等……")
if MUT == "unknown_kind":
    run_end = {"kind": "mystery_event"}
if MUT == "task_mojibake":
    run_start["task"] = "".join(("\ufffd" if ord(c) > 127 else c) for c in TASK)

order = [run_start, frame, image, run_end]
if MUT == "no_run_start":
    order = [frame, image, run_end]
elif MUT == "no_run_end":
    order = [run_start, frame, image]
elif MUT == "image_before_frame":
    order = [run_start, image, frame, run_end]
elif MUT == "image_no_data_uri":
    image.pop("data_uri")
elif MUT == "image_big":
    image["width"], image["height"] = 1200, 800
elif MUT == "image_png":
    image["data_uri"] = "data:image/png;base64," + JPEG
elif MUT == "image_bad_base64":
    image["data_uri"] = "data:image/jpeg;base64,!!!not-base64!!!"

if MUT == "gbk_strict_crash":
    # emoji 在 GBK 里根本不存在：strict 编码直接抛 UnicodeEncodeError，进程崩、stdout 只剩一条 run_end。
    # 这是"比乱码更严重"的那一类故障（Lead 2026-10-11 实测：修复前 emoji 场景 exit 1、只剩 run_end）。
    sys.stdout.buffer.write((json.dumps({"kind": "run_end", "ok": False}, ensure_ascii=False) + "\n").encode("gbk"))
    sys.stdout.buffer.flush()
    sys.stdout.buffer.write((json.dumps(run_start, ensure_ascii=False) + "\n").encode("gbk"))
    sys.stdout.buffer.flush()
elif MUT == "gbk_output":
    # 复现真 bug：管道下 stdout 退化成 gbk，中文按 GBK 落字节，Go 按 UTF-8 读 -> U+FFFD
    for obj in order:
        sys.stdout.buffer.write((json.dumps(obj, ensure_ascii=False) + "\n").encode("gbk", errors="replace"))
    sys.stdout.buffer.flush()
else:
    for obj in order:
        emit(obj)

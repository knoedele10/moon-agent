# -*- coding: utf-8 -*-
"""I1 事件流探测的负向对照：干净假生产者必须全过，每个违约必须被抓住。"""
import os
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
FAKE = ROOT / "docs" / "verify" / "events-fake-producer.py"
CHECKER = ROOT / "docs" / "verify" / "events-jsonl-check.py"

CASES = [
    ("", None),
    ("gbk_output", "E8a-utf8-strict"),
    ("task_mojibake", "E8c-task-fid"),
    ("human_text", "E2-jsonl"),
    ("unknown_kind", "E2-kinds"),
    ("no_run_start", "E2-span"),
    ("no_run_end", "E2-span"),
    ("image_before_frame", "E2-frame-image"),
    ("image_no_data_uri", "E2-frame-image"),
    ("image_big", "E2-frame-image"),
    ("image_png", "E2-frame-image"),
    ("image_bad_base64", "E2-frame-image"),
]


def run_case(mut):
    env = dict(os.environ)
    env["IT_MUT"] = mut
    proc = subprocess.run(
        [sys.executable, "-X", "utf8", str(CHECKER), "--producer", "python -X utf8 " + FAKE.as_posix()],
        cwd=str(ROOT), capture_output=True, text=True, encoding="utf-8", env=env, timeout=180)
    fails = [ln.split("] ", 1)[1].split(" ", 1)[0] for ln in proc.stdout.splitlines() if ln.startswith("[FAIL]")]
    return proc.returncode, fails, proc.stdout


def main():
    FAKE.parent.mkdir(parents=True, exist_ok=True)
    print("# I1 事件流探测的负向对照（fake producer: %s）" % FAKE.name)
    bad = 0
    code, fails, out = run_case("")
    if code == 0 and not fails:
        print("[PASS] clean-producer        干净假生产者全过（无 FAIL）")
    else:
        bad += 1
        print("[FAIL] clean-producer        干净假生产者出现误报 exit=%s FAIL=%s" % (code, fails))
        print(out[-600:])
    for mut, expect in CASES[1:]:
        code, fails, out = run_case(mut)
        if expect in fails:
            print("[PASS] mutation=%-22s 被抓住（FAIL=%s）" % (mut, fails))
        else:
            bad += 1
            print("[FAIL] mutation=%-22s 没被抓到：期望 %s，实际 %s（exit=%s）" % (mut, expect, fails, code))
            print(out[-500:])
    print("")
    print("---- 汇总：变异 %d 项，未被抓/误报 %d 项 ----" % (len(CASES) - 1, bad))
    sys.exit(1 if bad else 0)


main()

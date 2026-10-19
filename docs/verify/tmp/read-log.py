# 读日志并按需解码（PowerShell 的 > 重定向在 5.1 下写 UTF-16LE，PyInstaller 侧也可能带 BOM）。
import json
import sys

path = sys.argv[1]
raw = open(path, "rb").read()
if raw[:2] in (b"\xff\xfe", b"\xfe\xff"):
    text = raw.decode("utf-16")
else:
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError:
        text = raw.decode("gbk", errors="replace")
print("bytes=%d encoding=%s" % (len(raw), "utf-16" if raw[:2] in (b"\xff\xfe", b"\xfe\xff") else "utf-8"))
lines = [l for l in text.splitlines() if l.strip()]
events = []
for l in lines:
    s = l.strip()
    if s.startswith("{"):
        try:
            events.append(json.loads(s))
        except Exception:
            pass
print("事件行数=%d" % len(events))
if events:
    print("kinds=" + json.dumps(sorted({e.get("kind", "?") for e in events})))
    tp = []
    for e in events:
        meta = (e.get("result") or {}).get("meta") or {}
        if meta.get("type_path"):
            tp.append((e.get("kind"), e.get("step"), meta.get("type_path"), meta.get("type_skipped_chars")))
    print("type_path 记录=" + json.dumps(tp, ensure_ascii=False))
    last = events[-1]
    print("最后一条=" + json.dumps({k: last.get(k) for k in ("kind", "code", "steps", "ok")}, ensure_ascii=False))
else:
    print("前 3 行：" )
    for l in lines[:3]:
        print("  " + l[:120])

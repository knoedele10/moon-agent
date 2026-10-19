import sys
raw = open(sys.argv[1], "rb").read()
enc = "utf-16" if raw[:2] in (b"\xff\xfe", b"\xfe\xff") else "utf-8"
try:
    text = raw.decode(enc)
except UnicodeDecodeError:
    text = raw.decode("gbk", errors="replace")
print("encoding=" + enc + " bytes=%d" % len(raw))
for line in text.splitlines():
    s = line.strip()
    if any(k in s for k in ("通过", "失败", "协议 cua", "Python", "模式")):
        print("  " + s)

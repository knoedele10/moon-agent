import re, sys
path = sys.argv[1]
pat = re.compile(sys.argv[2]) if len(sys.argv) > 2 else None
raw = open(path, "rb").read()
enc = "utf-16" if raw[:2] in (b"\xff\xfe", b"\xfe\xff") else "utf-8"
try:
    text = raw.decode(enc)
except UnicodeDecodeError:
    text = raw.decode("utf-8", errors="replace")
for line in text.splitlines():
    if pat is None or pat.search(line):
        print(line.rstrip()[:200])

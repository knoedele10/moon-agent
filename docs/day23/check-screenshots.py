# Day 23 验收辅助：确认截图不是空白页/纯色页。
# 用法（仓库根目录）：python docs/day23/check-screenshots.py
# 说明：这是"机器能判的那部分"——画面有明显的灰度层次（std 大、灰阶多）说明页面渲染出了内容；
#       但"看起来对不对"仍需人眼看截图，本脚本不能替代视觉评审。
import glob
import os
import sys
from PIL import Image, ImageStat

files = sorted(glob.glob(os.path.join('docs', 'day23', 'screenshots', '*.png')))
if not files:
    print('no screenshots found under docs/day23/screenshots')
    sys.exit(1)

rows = []
for f in files:
    im = Image.open(f).convert('L')
    st = ImageStat.Stat(im)
    colors = im.getcolors(maxcolors=1_000_000) or []
    rows.append((os.path.basename(f), im.size[0], im.size[1], st.mean[0], st.stddev[0], len(colors)))

print('%-28s %11s %7s %7s %7s' % ('file', 'size', 'mean', 'std', 'levels'))
for r in rows:
    print('%-28s %5dx%-5d %7.1f %7.1f %7d' % (r[0], r[1], r[2], r[3], r[4], r[5]))

flat = [r[0] for r in rows if r[4] < 3.0]
print('FLAT(possibly blank): ' + (', '.join(flat) if flat else 'none'))
print('RESULT: ' + ('FAIL' if flat else 'PASS') + ' (threshold: grayscale stddev >= 3.0)')
sys.exit(1 if flat else 0)
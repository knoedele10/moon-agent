# -*- mode: python ; coding: utf-8 -*-
"""PyInstaller 单文件打包：bin/cua-sidecar.exe（集成契约 §5）。

构建（在仓库根目录执行，CWD 必须是仓库根 —— 自检会读 docs/cua/03-action-protocol.md）：
    python -m PyInstaller --noconfirm --distpath bin --workpath build/cua-sidecar packaging/cua.spec

产物：bin/cua-sidecar.exe（单文件）。Go 侧优先用它，找不到才回落 python -m cua.main。

两个必须一起打进去的东西：
1) 契约文件 docs/cua/action.schema.json、02-agent-prompt.md、03-action-protocol.md。
   cua 包按 Path(__file__).resolve().parent.parent / "docs/cua/..." 找它们；打包后
   __file__ 落在 _MEIPASS 里，所以 datas 的目标目录也必须是 docs/cua。
2) 入口 packaging/cua_sidecar_entry.py：它会吃掉 selfcheck 子进程里的 "-m cua.main"
   前缀（单文件 exe 不认 -m），否则打包产物的 --selfcheck 会因 UTF-8 往返子进程
   exit 2 而失败。
"""

import os

# SPECPATH 由 PyInstaller 注入，等于本 spec 所在目录（<repo>/packaging）。
ROOT = os.path.abspath(os.path.join(SPECPATH, ".."))

datas = [
    (os.path.join(ROOT, "docs", "cua", "action.schema.json"), "docs/cua"),
    (os.path.join(ROOT, "docs", "cua", "02-agent-prompt.md"), "docs/cua"),
    (os.path.join(ROOT, "docs", "cua", "03-action-protocol.md"), "docs/cua"),
    (os.path.join(ROOT, "cua", "mock", "demo.json"), "cua/mock"),
]

hiddenimports = [
    # pyautogui 的依赖链里有运行时才 import 的模块，静态分析容易漏。
    "pyscreeze",
    "pygetwindow",
    "pymsgbox",
    "pytweening",
    "mouseinfo",
    "pyperclip",
]

a = Analysis(
    [os.path.join(SPECPATH, "cua_sidecar_entry.py")],
    pathex=[ROOT],
    binaries=[],
    datas=datas,
    hiddenimports=hiddenimports,
    hookspath=[],
    hooksconfig={},
    runtime_hooks=[],
    excludes=[
        # 实测：import cua.main 不会引入下面任何一个（HEAVY=[]），
        # 但 PyInstaller 会顺着依赖里的条件 import 把它们全收进来，
        # 单文件产物因此从 ~60MB 涨到 340MB。这里显式排除。
        "torch",
        "torchvision",
        "torchaudio",
        "cv2",
        "pandas",
        "scipy",
        "sklearn",
        "onnxruntime",
        "grpc",
        "opentelemetry",
        "sqlalchemy",
        "matplotlib",
        "IPython",
        "notebook",
        "pytest",
    ],
    noarchive=False,
    optimize=0,
)

pyz = PYZ(a.pure)

exe = EXE(
    pyz,
    a.scripts,
    a.binaries,
    a.datas,
    [],
    name="cua-sidecar",
    debug=False,
    bootloader_ignore_signals=False,
    strip=False,
    upx=False,
    runtime_tmpdir=None,
    console=True,
    disable_windowed_traceback=False,
    argv_emulation=False,
    target_arch=None,
    codesign_identity=None,
    entitlements_file=None,
)

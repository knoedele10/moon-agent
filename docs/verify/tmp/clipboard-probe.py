# task-25 独立复验（verifier）：type 是否真的以剪贴板为主路径、剪贴板不可用能否退化、clear_first/submit 语义是否未变。
# 用自己的假设备 + 自己的断言，不引用实现者的自检结论。
import json
import os
import sys

sys.path.insert(0, os.getcwd())

from cua.config import Config          # noqa: E402
from cua.executor import Executor, RecordingDevice  # noqa: E402
from cua.screenshot import MockScreenshot           # noqa: E402

FAILS = []
CHECKS = []


def chk(name, cond, detail=""):
    CHECKS.append(name)
    print(("[PASS] " if cond else "[FAIL] ") + name + ("  " + str(detail) if detail else ""))
    if not cond:
        FAILS.append(name)


def mock_cfg(**kw):
    return Config(use_mock_model=True, use_mock_screen=True, dry_run=True, log_path=None).with_(**kw)


def build(cfg, device):
    provider = MockScreenshot(cfg)
    ex = Executor(cfg, provider=provider, device=device, dry_run=True, sleep_fn=lambda s: None)
    return ex, provider.capture()


def step(action):
    return {"protocol": "cua/1", "thought": "探针", "action": action, "risk": "low", "done": False}


class NoClipboard(RecordingDevice):
    """剪贴板完全不可用。"""

    def set_clipboard(self, text):
        return None

    def get_clipboard(self):
        return None


class HalfClipboard(RecordingDevice):
    """写入返回 None（拿不到旧内容），但读回来确实是新文本 —— 这条分支实现者的自检没覆盖。"""

    def set_clipboard(self, text):
        self.clipboard = text
        return None


def main():
    cfg = mock_cfg()

    # 1) 剪贴板可用：ASCII 也必须走剪贴板（task-25 的核心主张）
    dev = RecordingDevice()
    ex, frame = build(cfg, dev)
    dev.clear()
    out = ex.execute(step({"type": "type", "text": "hello from CUA"}), frame)
    ops = [o["op"] for o in dev.ops]
    chk("1-clipboard-main-path", out.result["meta"].get("type_path") == "clipboard",
        "type_path=" + str(out.result["meta"].get("type_path")) + " ops=" + json.dumps(ops))
    chk("1-no-char-by-char", "write" not in ops, "不应残留逐字符 write：%s" % ops)
    chk("1-paste-order", ops == ["set_clipboard", "key_down", "press", "key_up"], json.dumps(ops))
    chk("1-paste-key", any(o["op"] == "press" and o.get("key") == "v" for o in dev.ops), "")
    chk("1-clipboard-text-exact",
        any(o["op"] == "set_clipboard" and o.get("text") == "hello from CUA" for o in dev.ops), "")

    # 2) 中文同样走剪贴板
    dev2 = RecordingDevice()
    ex2, frame2 = build(cfg, dev2)
    out2 = ex2.execute(step({"type": "type", "text": "中文输入🙂"}), frame2)
    chk("2-unicode-clipboard", out2.result["meta"].get("type_path") == "clipboard",
        "type_path=" + str(out2.result["meta"].get("type_path")))

    # 3) 半可用剪贴板（写成功但拿不到旧值）也应走剪贴板
    dev3 = HalfClipboard()
    ex3, frame3 = build(cfg, dev3)
    out3 = ex3.execute(step({"type": "type", "text": "half"}), frame3)
    chk("3-half-clipboard", out3.result["meta"].get("type_path") == "clipboard",
        "type_path=" + str(out3.result["meta"].get("type_path")))

    # 4) 剪贴板不可用 + ASCII -> 退化到整段 write（旧行为）
    dev4 = NoClipboard()
    ex4, frame4 = build(cfg, dev4)
    dev4.clear()
    out4 = ex4.execute(step({"type": "type", "text": "hello from CUA"}), frame4)
    chk("4-fallback-write", out4.result["meta"].get("type_path") == "write",
        "type_path=" + str(out4.result["meta"].get("type_path")) + " ops=" + json.dumps([o["op"] for o in dev4.ops]))
    chk("4-fallback-write-text", [o["op"] for o in dev4.ops] == ["write"],
        json.dumps(dev4.ops))

    # 5) 剪贴板不可用 + 非 ASCII -> 逐字符并记录被跳过的字符
    dev5 = NoClipboard()
    ex5, frame5 = build(cfg, dev5)
    out5 = ex5.execute(step({"type": "type", "text": "中文"}), frame5)
    meta5 = out5.result["meta"]
    chk("5-fallback-char-by-char", meta5.get("type_path") == "char_by_char",
        "type_path=" + str(meta5.get("type_path")))
    chk("5-skipped-recorded", meta5.get("type_skipped_chars") == 2,
        "type_skipped_chars=" + str(meta5.get("type_skipped_chars")))

    # 6) clear_first 语义未变：先 Ctrl+A，再输入；submit 必须最后回车
    dev6 = RecordingDevice()
    ex6, frame6 = build(cfg, dev6)
    dev6.clear()
    out6 = ex6.execute(step({"type": "type", "text": "abc", "clear_first": True, "submit": True}), frame6)
    seq = [(o["op"], o.get("key") or o.get("text")) for o in dev6.ops]
    chk("6-clear-first-order", seq[:3] == [("key_down", "ctrl"), ("press", "a"), ("key_up", "ctrl")],
        json.dumps(seq, ensure_ascii=False))
    chk("6-type-after-clear", any(o["op"] == "set_clipboard" for o in dev6.ops[3:]),
        json.dumps(seq, ensure_ascii=False))
    chk("6-submit-last", dev6.ops[-1]["op"] == "press" and dev6.ops[-1].get("key") == "enter",
        "最后一步=" + json.dumps(dev6.ops[-1], ensure_ascii=False))
    chk("6-type-path-still-clipboard", out6.result["meta"].get("type_path") == "clipboard",
        "type_path=" + str(out6.result["meta"].get("type_path")))

    # 7) 显式关掉剪贴板开关 -> 回到旧路径（文档承诺的配置行为）
    cfg_off = mock_cfg(type_use_clipboard_for_unicode=False)
    dev7 = RecordingDevice()
    ex7, frame7 = build(cfg_off, dev7)
    out7 = ex7.execute(step({"type": "type", "text": "ascii"}), frame7)
    chk("7-config-off-ascii-write", out7.result["meta"].get("type_path") == "write",
        "type_path=" + str(out7.result["meta"].get("type_path")))
    dev7b = RecordingDevice()
    ex7b, frame7b = build(cfg_off, dev7b)
    out7b = ex7b.execute(step({"type": "type", "text": "中文"}), frame7b)
    chk("7-config-off-unicode-char-by-char", out7b.result["meta"].get("type_path") == "char_by_char",
        "type_path=" + str(out7b.result["meta"].get("type_path")))

    # 8) 空文本：不得碰输入通道
    dev8 = RecordingDevice()
    ex8, frame8 = build(cfg, dev8)
    dev8.clear()
    out8 = ex8.execute(step({"type": "type", "text": ""}), frame8)
    chk("8-empty-no-ops", out8.result["meta"].get("type_path") == "empty" and dev8.ops == [],
        "type_path=" + str(out8.result["meta"].get("type_path")) + " ops=" + json.dumps([o["op"] for o in dev8.ops]))

    print("---- 汇总：判据 %d 条，FAIL %d ----" % (len(CHECKS), len(FAILS)))
    if FAILS:
        print("FAIL 明细：" + ", ".join(FAILS))
    return 1 if FAILS else 0


if __name__ == "__main__":
    sys.exit(main())

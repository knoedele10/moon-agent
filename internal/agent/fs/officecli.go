package fs

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// OfficeCLI 集成（2026-10-12）。
//
// 分工很明确：
//   - OfficeCLI 负责**它比我们强的那部分**：生成带母版/版式/主题的合法 OOXML 骨架，
//     以及 OpenXML 模式校验（validate）；
//   - 我们负责**内容注入**（往骨架里塞段落/单元格/幻灯片）。
//
// 为什么不让 officecli 包办全部：实测它这个版本的 `add slide` 会报成功但不落盘
// （zip 里没有 slide1.xml），所以内容由我们自己写；而 `validate` 是可信的，用它兜底。
//
// 安全边界：只执行这一个已知程序，参数以数组传递（不经 shell），带超时。

// officeCLIProbeTimeout 是查找与执行 officecli 的超时。
const officeCLITimeout = 60 * time.Second

// OfficeCLIPath 返回可用的 officecli 可执行文件路径；找不到返回空串。
//
// 查找顺序：环境变量 OFFICECLI_BIN → PATH → Windows 默认安装位置。
// 找到后**真跑一次 --version** 确认可用（照搬技能脚本那条教训：which 命中不等于能跑）。
func OfficeCLIPath() string {
	if p := strings.TrimSpace(os.Getenv("OFFICECLI_BIN")); p != "" {
		if fileExists(p) {
			return p
		}
	}
	if p, err := exec.LookPath("officecli"); err == nil {
		return p
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		cand := filepath.Join(local, "OfficeCLI", "officecli.exe")
		if fileExists(cand) {
			return cand
		}
	}
	return ""
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// runOfficeCLI 执行一次 officecli。返回 stdout+stderr 合并输出。
func runOfficeCLI(ctx context.Context, args ...string) (string, error) {
	bin := OfficeCLIPath()
	if bin == "" {
		return "", errOfficeCLIMissing
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, officeCLITimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, bin, args...)
	// 驻留模式默认延迟落盘：设为 each 保证命令返回时文件已经写好，
	// 否则下一条命令（或我们自己的 zip 读取）会看到旧内容。
	cmd.Env = append(os.Environ(), "OFFICECLI_RESIDENT_FLUSH=each")

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

var errOfficeCLIMissing = errString("本机没有 OfficeCLI（officecli），已退回内置实现")

type errString string

func (e errString) Error() string { return string(e) }

// OfficeCLICreate 用 officecli 生成一个空文档骨架（按扩展名决定 docx/xlsx/pptx）。
func OfficeCLICreate(ctx context.Context, path string) error {
	// officecli 在目标目录就地创建；先确保父目录存在。
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if _, err := runOfficeCLI(ctx, "create", path); err != nil {
		return err
	}
	if !fileExists(path) {
		return errString("officecli create 没有产出文件")
	}
	// 用完就让它释放驻留：否则文件会被 officecli 的进程占着（Windows 上连删临时目录都会失败）。
	OfficeCLIRelease(ctx, path)
	return nil
}

// OfficeCLIValidate 校验一个 Office 文件是否是合法 OpenXML。
// OfficeCLIRelease 让 officecli 关闭对该文件的驻留（释放文件句柄）。
//
// 为什么必须调：officecli 会把文档留在驻留进程里（方便连续编辑），代价是**文件被占用**——
// 实测表现为拿到验证结果后文件仍被锁住，连临时目录都删不掉。用完就关，是它的正常用法。
func OfficeCLIRelease(ctx context.Context, path string) {
	if OfficeCLIPath() == "" {
		return
	}
	_, _ = runOfficeCLI(ctx, "close", path) // 释放失败不报错：只是句柄多留一会儿
}

// 找不到 officecli 时返回 nil（跳过校验）——校验是加分项，不该让基础功能不可用。
func OfficeCLIValidate(ctx context.Context, path string) (string, error) {
	if OfficeCLIPath() == "" {
		return "", nil
	}
	out, err := runOfficeCLI(ctx, "validate", path)
	// 校验完顺便释放句柄，免得后续读/写/删被 officecli 占着。
	OfficeCLIRelease(ctx, path)
	if err != nil {
		return out, err
	}
	return out, nil
}

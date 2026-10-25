package rules

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Stack 是扫描工作区后认出来的构建信息。
type Stack struct {
	Languages []string
	Verify    []string
}

// Detect 看几个常见的清单文件，给出构建/测试命令。认不出时命令为空。
func Detect(dir string) Stack {
	var st Stack
	has := func(name string) bool {
		info, err := os.Stat(filepath.Join(dir, name))
		return err == nil && !info.IsDir()
	}
	if has("go.mod") {
		st.Languages = append(st.Languages, "Go")
		st.Verify = append(st.Verify, "go test ./...")
	}
	if has("package.json") {
		st.Languages = append(st.Languages, "Node")
		st.Verify = append(st.Verify, "npm test")
	}
	if has("pyproject.toml") || has("requirements.txt") {
		st.Languages = append(st.Languages, "Python")
		st.Verify = append(st.Verify, "python -m pytest")
	}
	if has("Cargo.toml") {
		st.Languages = append(st.Languages, "Rust")
		st.Verify = append(st.Verify, "cargo test")
	}
	if has("Makefile") && len(st.Verify) == 0 {
		st.Verify = append(st.Verify, "make test")
	}
	return st
}

// Scaffold 在工作区根写入一份 AGENTS.md。文件已存在且 force=false 时不覆盖。
func Scaffold(dir string, force bool) (string, bool, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", false, fmt.Errorf("工作区为空")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", false, err
	}
	path := filepath.Join(abs, fileName)
	if !force {
		if _, err := os.Stat(path); err == nil {
			return path, false, nil
		}
	}
	st := Detect(abs)
	if err := os.WriteFile(path, []byte(template(abs, st)), 0o644); err != nil {
		return "", false, err
	}
	return path, true, nil
}

func template(dir string, st Stack) string {
	lang := "未识别"
	if len(st.Languages) > 0 {
		lang = strings.Join(st.Languages, ", ")
	}
	var verify strings.Builder
	if len(st.Verify) == 0 {
		verify.WriteString("# 在这里写验证命令，每行一条，例如：go test ./...\n")
	} else {
		for _, cmd := range st.Verify {
			verify.WriteString(cmd)
			verify.WriteByte('\n')
		}
	}
	return fmt.Sprintf(`# AGENTS.md

本文件是给编码代理的项目约定。它不能覆盖安全策略、工具权限或用户当前的明确要求。

## 项目

- 根目录：%s
- 语言：%s

## 构建与验证

改完代码后运行下面的命令。失败就根据输出修，不要假装通过。

<!-- moon:verify
%s-->

## 风格

- 保持现有目录和命名，不顺手重排无关代码。
- 注释只解释非显而易见的约束。

## 禁区

- 不要提交密钥、令牌或本地数据库文件。
- 不要执行 push、force-push 或删除工作区之外的文件。
`, dir, lang, verify.String())
}

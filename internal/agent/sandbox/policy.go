// Package sandbox 是命令与出网的策略门。
//
// 它不是 Windows MXC / Linux Landlock 的替代品：那些 OS 隔离在这台构建里没有可移植实现。
// 打开 AGENT_SANDBOX 之后，run_command 用这份策略代替命令白名单：拒绝已知的交互式 shell，
// 工作目录必须落在可写根里，出网域名必须命中允许列表。审计写到 JSONL。
package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Enabled 在 AGENT_SANDBOX=1/true/on/yes 时为真。缺省关闭，run_command 仍走白名单。
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AGENT_SANDBOX"))) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}

// Policy 描述一次命令或出网检查。
type Policy struct {
	WriteRoots   []string
	AllowExec    []string // 空 = 除拒绝名单外都允许
	AllowDomains []string // 空 = 不额外限制域名（私网仍由调用方拦）
	AuditPath    string
}

type Decision struct {
	Allow  bool   `json:"allow"`
	Action string `json:"action"`
	Target string `json:"target"`
	Reason string `json:"reason,omitempty"`
	At     string `json:"at"`
}

var alwaysDeny = map[string]bool{
	"cmd": true, "cmd.exe": true,
	"powershell": true, "powershell.exe": true,
	"pwsh": true, "pwsh.exe": true,
	"wscript": true, "wscript.exe": true,
	"cscript": true, "cscript.exe": true,
}

// FromWorkspace 用工作区根和 AGENT_SANDBOX_ALLOW / AGENT_SANDBOX_DOMAINS 组一份策略。
func FromWorkspace(root string, extra []string) Policy {
	roots := []string{}
	if strings.TrimSpace(root) != "" {
		roots = append(roots, root)
	}
	for _, r := range extra {
		if strings.TrimSpace(r) != "" {
			roots = append(roots, r)
		}
	}
	return Policy{
		WriteRoots:   roots,
		AllowExec:    splitList(os.Getenv("AGENT_SANDBOX_ALLOW")),
		AllowDomains: splitList(os.Getenv("AGENT_SANDBOX_DOMAINS")),
		AuditPath:    strings.TrimSpace(os.Getenv("AGENT_SANDBOX_AUDIT")),
	}
}

func splitList(raw string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t'
	}) {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (p Policy) CheckExec(program string) error {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(program)))
	if base == "" || base == "." {
		p.audit("exec", program, "empty program")
		return fmt.Errorf("拒绝执行空命令")
	}
	if alwaysDeny[base] {
		p.audit("exec", base, "denied shell")
		return fmt.Errorf("沙箱拒绝执行 %s", base)
	}
	if len(p.AllowExec) > 0 && !containsFold(p.AllowExec, base) && !containsFold(p.AllowExec, program) {
		p.audit("exec", base, "not in allow list")
		return fmt.Errorf("沙箱允许的命令是 %s，不能执行 %s", strings.Join(p.AllowExec, ", "), base)
	}
	p.audit("exec", base, "")
	return nil
}

func (p Policy) CheckWrite(path string) error {
	if len(p.WriteRoots) == 0 {
		p.audit("write", path, "no write root")
		return fmt.Errorf("沙箱没有可写目录")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for _, root := range p.WriteRoots {
		rootAbs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rootAbs, abs)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			p.audit("write", abs, "")
			return nil
		}
	}
	p.audit("write", abs, "outside write root")
	return fmt.Errorf("沙箱拒绝写入工作区之外的路径: %s", abs)
}

func (p Policy) CheckDomain(host string) error {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		p.audit("net", host, "empty host")
		return fmt.Errorf("拒绝空的主机名")
	}
	if len(p.AllowDomains) == 0 {
		return nil
	}
	for _, allow := range p.AllowDomains {
		allow = strings.ToLower(strings.TrimSpace(allow))
		if host == allow || strings.HasSuffix(host, "."+allow) {
			p.audit("net", host, "")
			return nil
		}
	}
	p.audit("net", host, "domain not allowed")
	return fmt.Errorf("沙箱拒绝访问域名 %s", host)
}

var auditMu sync.Mutex

func (p Policy) audit(action, target, reason string) {
	if strings.TrimSpace(p.AuditPath) == "" {
		return
	}
	rec := Decision{Allow: reason == "", Action: action, Target: target, Reason: reason, At: time.Now().UTC().Format(time.RFC3339)}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	auditMu.Lock()
	defer auditMu.Unlock()
	f, err := os.OpenFile(p.AuditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}

func containsFold(list []string, want string) bool {
	for _, item := range list {
		if strings.EqualFold(item, want) {
			return true
		}
	}
	return false
}

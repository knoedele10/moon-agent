// Command skillimport 把技能库目录下的 SKILL.md 批量导入数据库。
//
// 为什么不走 HTTP 接口：批量导入是一次性运维动作（一次几百个文件），
// 不该要求先起服务、先登录。导入逻辑与 HTTP 接口共用 model.ImportSkillDir，
// 所以两条路径的去重规则、幂等语义完全一致——不会出现“命令行导的和界面导的不一样”。
//
// 用法（在仓库根目录执行，会自动读同目录的 .env）：
//
//	go run ./cmd/skillimport -user 2399493728 -dir "C:\Users\me\.agents\skills"
//	go run ./cmd/skillimport -all-users -dir ... -dir ...
//	go run ./cmd/skillimport -user alice -dir ... -dry-run
//	go run ./cmd/skillimport -list
package main

import (
	"flag"
	"fmt"
	"log"
	"strings"

	"github.com/joho/godotenv"

	"blog-my/internal/agent/skill"
	"blog-my/internal/model"
)

// stringList 让 -dir 可以重复出现（一次导多个技能库目录）。
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	var dirs stringList
	user := flag.String("user", "", "用户名或用户 ID（技能是按用户存的）")
	allUsers := flag.Bool("all-users", false, "给 user 表里每个账号都导一份")
	disable := flag.Bool("disable", false, "导入后一律停用（默认：新建的启用，已有的保留原开关）")
	includeHidden := flag.Bool("include-hidden", false, "连隐藏目录（.gbrain/.openclaw 这类同步副本）一起扫")
	dryRun := flag.Bool("dry-run", false, "只扫描并打印，不写库")
	enable := flag.Bool("enable", false, "导入后一律启用（会打开手工关掉的技能）")
	list := flag.Bool("list", false, "只列出每个账号当前的技能条数")
	flag.Var(&dirs, "dir", "技能库目录，可重复给多个")
	flag.Parse()

	// 与 main.go 一样先加载 .env：数据库连接参数都在里面。
	if err := godotenv.Load(); err != nil {
		log.Printf("未加载 .env（%v），将只使用系统环境变量", err)
	}
	if err := model.InitDB(); err != nil {
		log.Fatal(err)
	}

	if *list {
		printStats()
		return
	}
	if len(dirs) == 0 {
		log.Fatal("请至少给一个 -dir（技能库目录）")
	}

	targets := resolveUsers(*user, *allUsers)
	// 三态：默认 nil（保留已有技能自己的开关），-enable/-disable 才强制覆盖。
	var enabled *bool
	switch {
	case *enable:
		t := true
		enabled = &t
	case *disable:
		f := false
		enabled = &f
	}
	opt := skill.ScanOptions{IncludeHidden: *includeHidden}

	for _, u := range targets {
		for _, dir := range dirs {
			if *dryRun {
				dryRunOne(u, dir, opt)
				continue
			}
			stats, err := model.ImportSkillDir(u.Id, dir, enabled, opt)
			if err != nil {
				log.Printf("[%s] 导入失败 %s: %v", u.Username, dir, err)
				continue
			}
			printStatsFor(u, dir, stats, enabled)
		}
	}

	if !*dryRun {
		fmt.Println()
		printStats()
	}
}

// resolveUsers 把 -user / -all-users 解析成具体账号。
func resolveUsers(user string, all bool) []model.User {
	if all {
		var users []model.User
		if err := model.DB.Order("created_at asc").Find(&users).Error; err != nil {
			log.Fatalf("读取用户失败: %v", err)
		}
		if len(users) == 0 {
			log.Fatal("user 表里没有任何账号")
		}
		return users
	}
	key := strings.TrimSpace(user)
	if key == "" {
		log.Fatal("请用 -user 指定账号（用户名或 ID），或用 -all-users；-list 可以看现有账号")
	}
	var u model.User
	if err := model.DB.Where("id = ? OR username = ?", key, key).First(&u).Error; err != nil {
		log.Fatalf("找不到账号 %q：%v（用 -list 看现有账号）", key, err)
	}
	return []model.User{u}
}

// dryRunOne 只扫描不写库：先看清“会导进来什么”再决定要不要落库。
func dryRunOne(u model.User, dir string, opt skill.ScanOptions) {
	rep, err := skill.ScanDir(dir, opt)
	if err != nil {
		log.Printf("[%s] 扫描失败 %s: %v", u.Username, dir, err)
		return
	}
	fmt.Printf("[dry-run][%s] %s\n", u.Username, rep.Root)
	fmt.Printf("  扫描到 %d 个 SKILL.md，去重后 %d 条技能，跳过隐藏目录 %d 个，同名副本 %d 个\n",
		rep.Files, len(rep.Items), rep.SkippedHidden, len(rep.Duplicates))
	for i, it := range rep.Items {
		if i >= 10 {
			fmt.Printf("  … 其余 %d 条略\n", len(rep.Items)-i)
			break
		}
		fmt.Printf("  - %s（正文 %d 字）\n", it.Name, len([]rune(it.Body)))
	}
}

// printStatsFor 打一次导入的结果。
func printStatsFor(u model.User, dir string, s model.SkillImportStats, enabled *bool) {
	fmt.Printf("[%s] %s\n", u.Username, s.Root)
	fmt.Printf("  扫描到 %d 个 SKILL.md，去重后 %d 条技能（新建 %d，更新 %d）\n",
		s.Files, s.Imported, s.Created, s.Updated)
	mode := "新建的启用，已有的保留原开关"
	if enabled != nil {
		mode = map[bool]string{true: "一律启用", false: "一律停用"}[*enabled]
	}
	fmt.Printf("  同名副本 %d 个，错误 %d 条，导入后状态：%s\n",
		len(s.Duplicates), len(s.Errors), mode)
	for i, d := range s.Duplicates {
		if i >= 3 {
			fmt.Printf("  … 其余同名副本 %d 个略\n", len(s.Duplicates)-i)
			break
		}
		fmt.Printf("  同名副本（保留层级最浅的那份）：%s\n", d)
	}
	for i, e := range s.Errors {
		if i >= 5 {
			fmt.Printf("  … 其余错误 %d 条略\n", len(s.Errors)-i)
			break
		}
		fmt.Printf("  错误：%s\n", e)
	}
}

// printStats 列出每个账号当前有多少条技能（含启用数），导入前后各跑一次就能看出效果。
func printStats() {
	var users []model.User
	if err := model.DB.Order("created_at asc").Find(&users).Error; err != nil {
		log.Fatalf("读取用户失败: %v", err)
	}
	fmt.Println("账号技能统计：")
	for _, u := range users {
		var total, enabled int64
		model.DB.Model(&model.AgentSkill{}).Where("user_id = ?", u.Id).Count(&total)
		if total == 0 {
			continue
		}
		model.DB.Model(&model.AgentSkill{}).Where("user_id = ? AND enabled = ?", u.Id, true).Count(&enabled)
		fmt.Printf("  %s（%s）：%d 条，其中已启用 %d 条\n", u.Username, u.Id, total, enabled)
	}
}

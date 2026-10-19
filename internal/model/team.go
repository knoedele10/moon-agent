package model

import (
	"log"
	"time"
)

// 团队任务板（契约 docs/agent/07-team.md §1.2）。
//
// 一条任务 = 一行，对外 id 是 task-<TaskNo>：TaskNo 在团队会话内单调递增、永不复用
// （删除留 tombstone，正是为了保住这个 id 的稳定性）。Revision 是 CAS 用的版本号，
// 每次变更 +1——两个成员同时拿着旧快照提交时，后到的那个会被拒，而不是静默覆盖。
//
// BlockedBy / WriteScopes 都是 JSON 数组文本：前者是依赖（写入时校验必须指向存在且
// 未删除的任务），后者只是**提示**（哪些路径可能被这个任务动到），不加锁、不拦人。

// 任务状态机取值（冻结）。
const (
	TeamTaskPending    = "pending"
	TeamTaskInProgress = "in_progress"
	TeamTaskCompleted  = "completed"
	TeamTaskDeleted    = "deleted"
)

type AgentTeamTask struct {
	Id     uint   `gorm:"column:id;primaryKey;autoIncrement"`
	UserID string `gorm:"column:user_id;index;size:36;not null"`
	// TeamConversationID 是团队会话（root 会话）的 id：隐式团队，TeamId 就是它。
	TeamConversationID uint `gorm:"column:team_conversation_id;index;not null"`
	// TaskNo 在团队会话内递增；对外呈现为 task-<TaskNo>。
	TaskNo      int    `gorm:"column:task_no;not null"`
	Subject     string `gorm:"column:subject;size:255"`
	Description string `gorm:"column:description;type:longtext"`
	Status      string `gorm:"column:status;size:16;index"`
	// Owner 是成员名（lead 或某个 teammate）；空串 = 无人认领。
	Owner string `gorm:"column:owner;size:64"`
	// BlockedBy / WriteScopes 存 JSON 数组文本，见文件头注释。
	BlockedBy   string    `gorm:"column:blocked_by;type:text"`
	WriteScopes string    `gorm:"column:write_scopes;type:text"`
	Revision    int       `gorm:"column:revision;not null"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

func (AgentTeamTask) TableName() string { return "agent_team_task" }

// migrateTeamSchema 做 v2 的索引对账（AutoMigrate 只建不删）。
//
// v1 把 agent_subagent_run.child_conversation_id 建成了**唯一**索引；v2 的
// 「给已结束成员发消息 = 同一个子会话上再起一轮续跑（新 run 行）」要求同一子会话
// 可以有多行 run，所以那一列必须是普通索引。新库里 model/subagent.go 的 tag 已经
// 是 index，AutoMigrate 直接建对；老库里唯一索引同名存在，AutoMigrate 会跳过，
// 必须在这里显式降级，否则续跑插入必撞唯一键。
//
// 索引名与 GORM 的默认命名（idx_<表>_<列>）保持一致：改列 tag 时不要换名字，
// 否则这里对不上账。失败只记日志——服务能照常启动，问题会在续跑时以明确错误暴露。
func migrateTeamSchema() {
	if DB == nil {
		return
	}
	const (
		table = "agent_subagent_run"
		index = "idx_agent_subagent_run_child_conversation_id"
	)
	var rows []struct {
		NonUnique int `gorm:"column:non_unique"`
	}
	if err := DB.Raw("SELECT non_unique FROM information_schema.statistics "+
		"WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ? LIMIT 1",
		table, index).Scan(&rows).Error; err != nil {
		log.Printf("[team] 检查历史唯一索引失败（跳过清理）：%v", err)
		return
	}
	if len(rows) == 0 || rows[0].NonUnique == 1 {
		return // 不存在（新库还没建出来）或已经是普通索引
	}
	if err := DB.Exec("DROP INDEX " + index + " ON " + table).Error; err != nil {
		log.Printf("[team] 删除 agent_subagent_run 的历史唯一索引失败（续跑可能不可用）：%v", err)
		return
	}
	if err := DB.Exec("CREATE INDEX " + index + " ON " + table + " (child_conversation_id)").Error; err != nil {
		log.Printf("[team] 重建普通索引失败（查询会变慢，功能不受影响）：%v", err)
		return
	}
	log.Printf("[team] 已把 agent_subagent_run.child_conversation_id 从唯一索引降级为普通索引（v2 续跑需要同一子会话多行 run）")
}

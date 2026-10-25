package web

// 团队服务（契约 docs/agent/07-team.md §1.2）：agent.TeamService 的 web 侧实现。
//
// 分层与 v1 的子 agent 一致：agent 包只认接口（agent/team.go 的工具层），
// 真正的落库、会话解析、续跑都在这里。四条主线：
//
//  1. 成员 = 具名子会话。spawn 复用 v1 的 subagentRunner.Delegate，不另起一套运行器；
//     续跑（给已结束成员发消息）复用 executeChild/settle，只是不新建子会话、不加深 depth。
//  2. 消息只落「目标会话」的 message 表（role=user，带【来自 x】前缀）：成功 = 已收下，
//     不表示对方已处理。目标在跑就排队（看守轮次结束后自动起续跑），不在跑就直接续跑。
//  3. 等待只报「有没有变化」：500ms 一次拉父会话下所有 run 的 id+status 指纹。
//  4. 任务板：revision CAS + 依赖 DAG 校验 + 删除留 tombstone；write_scopes 只存不拦。
//
// 单测只覆盖纯函数（名字校验、状态机、CAS、依赖校验、文案），不连 MySQL。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"blog-my/internal/agent"
	"blog-my/internal/model"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	// teamLeadName 是根会话在名册里的固定名字。成员名永不复用、也不允许叫 lead。
	teamLeadName = "lead"
	// teamNamePattern 是冻结的名字规则（docs/agent/07-team.md §1.2）。
	teamNamePattern = `^[a-z][a-z0-9-]{1,31}$`
	// teamMessagePrefix 是成员间消息的正文前缀（契约 §1.2）。
	teamMessagePrefix = "【来自 %s】\n%s"
	// teamContinuationPoll 是续跑看守与 wait_agent 的轮询间隔（契约 §1.2：500ms）。
	teamContinuationPoll = 500 * time.Millisecond
	// teamContinuationWaitMax 是排队看守的最长等待：超时就放弃（消息仍在会话里，
	// 用户下次说话或再发一条消息时自然会被读到），避免看守 goroutine 无限挂着。
	teamContinuationWaitMax = 30 * time.Minute
)

var teamNameRe = regexp.MustCompile(teamNamePattern)

// teamService 是 agent.TeamService 的实现。刻意不带内存状态：roster 与任务板都是
// 数据库的投影，进程重启后行为不变（排队看守是唯一的进程内状态，见文件末尾）。
type teamService struct{}

func newTeamService() *teamService { return &teamService{} }

// ---------------------------------------------------------------------------
// 会话与成员解析
// ---------------------------------------------------------------------------

// teamActor 是「谁在调用团队服务」：team 会话 + 调用者身份。
type teamActor struct {
	SessionID uint   // 调用者自己的会话 id（lead = ConvID；成员 = 它自己的子会话）
	ConvID    uint   // 团队会话（root 会话）id
	UserID    string // 权威用户身份，取自会话行
	Name      string // lead 或成员名（匿名 v1 子会话为空串）
	IsLead    bool
}

// resolveTeamActor 把 Exec.SessionID 解析成「团队会话 + 成员名」。
//
// 根会话调用 → 它就是团队会话，身份 lead；成员调用 → 它自己是一条子会话，
// 团队会话取它那一行 run 的父会话。这样工具层可以无脑传 Exec.SessionID。
func resolveTeamActor(sessionID string) (teamActor, error) {
	if model.DB == nil {
		return teamActor{}, errors.New("数据库未初始化")
	}
	convID, err := parseSubagentConvID(sessionID)
	if err != nil {
		return teamActor{}, err
	}
	var conv model.Conversation
	if err := model.DB.Where("id = ?", convID).First(&conv).Error; err != nil {
		return teamActor{}, fmt.Errorf("会话 %s 不存在", sessionID)
	}
	var run model.AgentSubagentRun
	err = model.DB.Where("child_conversation_id = ?", convID).Order("id desc").First(&run).Error
	if err == nil && run.ParentConversationID != 0 {
		return teamActor{
			SessionID: convID,
			ConvID:    run.ParentConversationID,
			UserID:    conv.UserID,
			Name:      strings.TrimSpace(run.Name),
			IsLead:    false,
		}, nil
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return teamActor{}, fmt.Errorf("读取会话归属失败：%w", err)
	}
	return teamActor{SessionID: convID, ConvID: convID, UserID: conv.UserID, Name: teamLeadName, IsLead: true}, nil
}

// teamActorName 解析一次调用里的 actor 参数（工具层传的是会话 id）。
// 与调用方会话一致时直接用已解析出的名字，避免同一件事查两遍库。
func teamActorName(a teamActor, actorSessionID string) (string, error) {
	raw := strings.TrimSpace(actorSessionID)
	if raw == "" || parseTeamChildIDOrZero(raw) == a.SessionID {
		if a.Name == "" {
			return "", errors.New("当前会话不是团队成员（它是匿名子任务），动不了团队功能")
		}
		return a.Name, nil
	}
	id, err := parseSubagentConvID(raw)
	if err != nil {
		return "", err
	}
	if id == a.ConvID {
		return teamLeadName, nil
	}
	var run model.AgentSubagentRun
	if err := model.DB.Where("parent_conversation_id = ? AND child_conversation_id = ?", a.ConvID, id).
		Order("id desc").First(&run).Error; err != nil {
		return "", fmt.Errorf("会话 %s 不是这个团队的成员", raw)
	}
	if name := strings.TrimSpace(run.Name); name != "" {
		return name, nil
	}
	return "", fmt.Errorf("会话 %s 不是团队成员（它是匿名子任务）", raw)
}

// teamRunsFor 列出团队会话下所有子 run（含续跑的多行），按 id 升序 = 时间顺序。
func teamRunsFor(convID uint) ([]model.AgentSubagentRun, error) {
	return subagentRunsFor(convID)
}

// latestTeamRunForChild 取某个子会话最新的一行 run（成员当前状态以它为准）。
func latestTeamRunForChild(childID uint) (model.AgentSubagentRun, error) {
	if model.DB == nil {
		return model.AgentSubagentRun{}, errors.New("数据库未初始化")
	}
	var run model.AgentSubagentRun
	err := model.DB.Where("child_conversation_id = ?", childID).Order("id desc").First(&run).Error
	return run, err
}

// latestTeamRunByName 按成员名找最新一行 run；找不到 = 目标不是本团队成员。
func latestTeamRunByName(convID uint, name string) (model.AgentSubagentRun, error) {
	if model.DB == nil {
		return model.AgentSubagentRun{}, errors.New("数据库未初始化")
	}
	var run model.AgentSubagentRun
	err := model.DB.Where("parent_conversation_id = ? AND name = ?", convID, name).
		Order("id desc").First(&run).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return run, fmt.Errorf("成员 %q 不存在（用 list_agents 看当前名册）", name)
		}
		return run, fmt.Errorf("查成员失败：%w", err)
	}
	return run, nil
}

// teamMemberExists 判断名字是不是本团队的成员（lead 永远算）。
func teamMemberExists(convID uint, name string) bool {
	if name == teamLeadName {
		return true
	}
	if model.DB == nil || name == "" {
		return false
	}
	var n int64
	if err := model.DB.Model(&model.AgentSubagentRun{}).
		Where("parent_conversation_id = ? AND name = ?", convID, name).Count(&n).Error; err != nil {
		return false
	}
	return n > 0
}

// ---------------------------------------------------------------------------
// 名字与文案（纯函数，单测钉住）
// ---------------------------------------------------------------------------

// validateTeamName 校验成员名：小写短名、父会话内唯一、永不复用（契约 §1.2）。
// lead 是保留名——它是根会话在名册里的身份，不能被成员顶掉。
func validateTeamName(name string) error {
	n := strings.TrimSpace(name)
	if n == "" {
		return errors.New("成员名不能为空：请给小写短名，形如 researcher（2-32 位，a-z/0-9/-，首字符必须是字母）")
	}
	if !teamNameRe.MatchString(n) {
		return fmt.Errorf("成员名 %q 不合法：只允许 2-32 位小写字母/数字/连字符，首字符必须是字母（形如 researcher）", name)
	}
	if n == teamLeadName {
		return fmt.Errorf("成员名 %q 是保留名（根会话的身份），换一个", name)
	}
	return nil
}

// teamMessageContent 拼一条成员间消息的正文（前缀冻结：`【来自 <from>】`）。
func teamMessageContent(from, message string) string {
	name := strings.TrimSpace(from)
	if name == "" {
		name = "未知成员"
	}
	return fmt.Sprintf(teamMessagePrefix, name, strings.TrimSpace(message))
}

// teamMemberStatus 把 run 的状态翻成名册状态（running | inactive | failed）。
// done 与 aborted 都归 inactive：名册回答的是「这个成员现在能不能马上接活」。
func teamMemberStatus(runStatus string) string {
	switch runStatus {
	case model.SubagentRunning:
		return "running"
	case model.SubagentFailed, model.SubagentAborted:
		return "failed"
	default:
		return "inactive"
	}
}

// ---------------------------------------------------------------------------
// 任务板：解析与校验（纯函数）
// ---------------------------------------------------------------------------

// parseTeamTaskID 解析对外任务 id（task-3；宽松接受裸数字 3）。
func parseTeamTaskID(raw string) (int, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "task-")
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("任务 id %q 不合法（形如 task-1）", raw)
	}
	return n, nil
}

// normalizeTeamTaskIDs 把依赖列表归一化成 task-N 形式：去重、保序、逐个校验格式。
func normalizeTeamTaskIDs(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	seen := map[int]bool{}
	for _, item := range raw {
		if strings.TrimSpace(item) == "" {
			continue
		}
		no, err := parseTeamTaskID(item)
		if err != nil {
			return nil, err
		}
		if seen[no] {
			continue
		}
		seen[no] = true
		out = append(out, fmt.Sprintf("task-%d", no))
	}
	return out, nil
}

// validateTeamBlockedBy 校验依赖：每个 id 必须指向本团队里**存在且未删除**的任务。
// lookup 由调用方从当前任务板构造；抽成函数是为了单测不连库也能钉住这条红线。
func validateTeamBlockedBy(ids []string, lookup func(taskNo int) (string, bool)) error {
	for _, id := range ids {
		no, err := parseTeamTaskID(id)
		if err != nil {
			return err
		}
		status, ok := lookup(no)
		if !ok {
			return fmt.Errorf("依赖 %s 不存在：blocked_by 只能指向本团队已存在的任务", id)
		}
		if status == model.TeamTaskDeleted {
			return fmt.Errorf("依赖 %s 已被删除，不能作为 blocked_by（被删的任务永远不算完成）", id)
		}
	}
	return nil
}

// normalizeTeamWriteScopes 只做去重与去空：write_scopes 是提示性的，
// **不做任何路径校验、不加锁、不拦重叠**（契约 §二.5）。
func normalizeTeamWriteScopes(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, s := range raw {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func encodeTeamStrings(v []string) string {
	if len(v) == 0 {
		return "[]"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func decodeTeamStrings(raw string) []string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

// teamTaskDepsReady 判断依赖是否全部完成。依赖指向不存在的任务（含被删的 tombstone）
// 一律算「未就绪」：tombstone 保住 id 的稳定，但不会把依赖偷偷判成完成。
func teamTaskDepsReady(blockedBy string, lookup func(taskNo int) (string, bool)) bool {
	// 依赖字段坏了（不是 JSON 数组）时按「未就绪」处理：宁可卡住，也不能误判成可以开工。
	if t := strings.TrimSpace(blockedBy); t != "" && !strings.HasPrefix(t, "[") {
		return false
	}
	for _, id := range decodeTeamStrings(blockedBy) {
		no, err := parseTeamTaskID(id)
		if err != nil {
			return false
		}
		status, ok := lookup(no)
		if !ok || status != model.TeamTaskCompleted {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 任务板：状态机（纯函数）
// ---------------------------------------------------------------------------

// teamBoardFacts 是状态机需要的外部事实。抽成参数让状态机保持纯函数：
// 单测不连库也能钉住每一条前置条件（依赖就绪、成员是否存在）。
type teamBoardFacts struct {
	DepsReady    bool
	MemberExists func(name string) bool
}

// planTeamTaskUpdate 校验一次任务变更并算出要写的列。
//
// 权限模型（契约 §二.6：任务板对全员开放，但破坏性动作要 owner 或 lead）：
//   - claim：pending + 依赖全完成 + 无主（或本来就是自己）；
//   - complete：必须是 owner，且任务在进行中；
//   - release / reopen：owner 或 lead；
//   - edit / set_dependencies / delete：lead，或任务无主，或 actor 是 owner；
//   - reassign：lead 随便改；成员只能把自己拥有的任务交出去。
func planTeamTaskUpdate(
	t model.AgentTeamTask,
	actor teamActor,
	expectedRevision int,
	action string,
	patch agent.TeamTaskInput,
	facts teamBoardFacts,
) (map[string]any, error) {
	if expectedRevision <= 0 {
		return nil, errors.New("缺少 expected_revision：请先 team_task_get 拿到当前 revision 再提交")
	}
	if expectedRevision != t.Revision {
		return nil, fmt.Errorf("TEAM_TASK_STALE_REVISION：任务 task-%d 已经变了（当前 revision=%d，你给的是 %d）；"+
			"请重新 team_task_get 看最新内容再提交", t.TaskNo, t.Revision, expectedRevision)
	}
	if t.Status == model.TeamTaskDeleted {
		return nil, fmt.Errorf("任务 task-%d 已被删除（tombstone 保留 id，但不能再改）", t.TaskNo)
	}
	if actor.Name == "" {
		return nil, errors.New("当前会话不是团队成员，动不了任务板")
	}

	updates := map[string]any{}
	switch action {
	case "claim":
		if t.Status != model.TeamTaskPending {
			return nil, fmt.Errorf("只能认领 pending 的任务（task-%d 当前是 %s）", t.TaskNo, t.Status)
		}
		if !facts.DepsReady {
			return nil, fmt.Errorf("task-%d 的依赖还没全部完成，不能认领；等依赖完成或先 set_dependencies 改依赖", t.TaskNo)
		}
		if t.Owner != "" && t.Owner != actor.Name {
			return nil, fmt.Errorf("task-%d 已经被 %s 认领；要换人请让 lead 用 reassign", t.TaskNo, t.Owner)
		}
		updates["status"] = model.TeamTaskInProgress
		updates["owner"] = actor.Name

	case "release":
		if t.Owner != actor.Name && !actor.IsLead {
			return nil, fmt.Errorf("只有 owner（%s）或 lead 能释放 task-%d", teamOwnerText(t.Owner), t.TaskNo)
		}
		if t.Status != model.TeamTaskPending && t.Status != model.TeamTaskInProgress {
			return nil, fmt.Errorf("只有 pending / in_progress 的任务能释放（task-%d 当前是 %s）", t.TaskNo, t.Status)
		}
		updates["status"] = model.TeamTaskPending
		updates["owner"] = ""

	case "complete":
		if t.Owner != actor.Name {
			return nil, fmt.Errorf("只有 owner（%s）能完成 task-%d；要替别人收尾请让 lead 用 reassign 换给你", teamOwnerText(t.Owner), t.TaskNo)
		}
		if t.Status != model.TeamTaskInProgress {
			return nil, fmt.Errorf("只有 in_progress 的任务能完成（task-%d 当前是 %s）：先 claim", t.TaskNo, t.Status)
		}
		updates["status"] = model.TeamTaskCompleted

	case "reopen":
		if t.Owner != actor.Name && !actor.IsLead {
			return nil, fmt.Errorf("只有 owner（%s）或 lead 能重开 task-%d", teamOwnerText(t.Owner), t.TaskNo)
		}
		if t.Status != model.TeamTaskCompleted {
			return nil, fmt.Errorf("只有 completed 的任务能重开（task-%d 当前是 %s）", t.TaskNo, t.Status)
		}
		updates["status"] = model.TeamTaskPending // owner 保留：原 owner 可以再 claim

	case "edit":
		if !teamCanMutate(t, actor) {
			return nil, fmt.Errorf("task-%d 归 %s，只有 owner 或 lead 能改", t.TaskNo, teamOwnerText(t.Owner))
		}
		if s := strings.TrimSpace(patch.Subject); s != "" {
			updates["subject"] = s
		}
		if s := strings.TrimSpace(patch.Description); s != "" {
			updates["description"] = s
		}
		if patch.WriteScopes != nil {
			updates["write_scopes"] = encodeTeamStrings(normalizeTeamWriteScopes(patch.WriteScopes))
		}
		if len(updates) == 0 {
			return nil, errors.New("edit 没带任何可改字段：至少给 subject / description / write_scopes 之一")
		}

	case "set_dependencies":
		if !teamCanMutate(t, actor) {
			return nil, fmt.Errorf("task-%d 归 %s，只有 owner 或 lead 能改依赖", t.TaskNo, teamOwnerText(t.Owner))
		}
		if patch.BlockedBy == nil {
			return nil, errors.New("set_dependencies 需要 blocked_by（传空数组 [] 表示清空依赖）")
		}

	case "reassign":
		if patch.Owner != "" && facts.MemberExists != nil && !facts.MemberExists(patch.Owner) {
			return nil, fmt.Errorf("owner %q 不是本团队成员：先用 list_agents 看名册", patch.Owner)
		}
		if !actor.IsLead && t.Owner != actor.Name {
			return nil, fmt.Errorf("只有 lead 能改别人的任务归属（task-%d 归 %s）", t.TaskNo, teamOwnerText(t.Owner))
		}
		updates["owner"] = strings.TrimSpace(patch.Owner)

	case "delete":
		if !teamCanMutate(t, actor) {
			return nil, fmt.Errorf("task-%d 归 %s，只有 owner 或 lead 能删", t.TaskNo, teamOwnerText(t.Owner))
		}
		updates["status"] = model.TeamTaskDeleted // tombstone：行不删，id 不复用

	default:
		return nil, fmt.Errorf("未知的 action %q：可用 claim / release / complete / reopen / edit / set_dependencies / reassign / delete", action)
	}
	return updates, nil
}

// teamCanMutate 判断 actor 能不能动这条任务：lead 随便；成员只能动无主的或自己的。
func teamCanMutate(t model.AgentTeamTask, actor teamActor) bool {
	if actor.IsLead {
		return true
	}
	return t.Owner == "" || t.Owner == actor.Name
}

func teamOwnerText(owner string) string {
	if strings.TrimSpace(owner) == "" {
		return "无人认领"
	}
	return owner
}

// ---------------------------------------------------------------------------
// 视图映射
// ---------------------------------------------------------------------------

func teamTaskView(t model.AgentTeamTask, ready bool) agent.TeamTask {
	return agent.TeamTask{
		ID:          fmt.Sprintf("task-%d", t.TaskNo),
		Revision:    t.Revision,
		Subject:     t.Subject,
		Description: t.Description,
		Status:      t.Status,
		Owner:       t.Owner,
		BlockedBy:   decodeTeamStrings(t.BlockedBy),
		WriteScopes: decodeTeamStrings(t.WriteScopes),
		Ready:       ready,
	}
}

// teamTasksFor 读出团队的全部任务（含 tombstone），task_no 升序。
func teamTasksFor(convID uint) ([]model.AgentTeamTask, error) {
	if model.DB == nil {
		return nil, errors.New("数据库未初始化")
	}
	var rows []model.AgentTeamTask
	if err := model.DB.Where("team_conversation_id = ?", convID).Order("task_no asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// teamTaskStatusLookup 把任务行折成「task_no → status」的查询函数。
func teamTaskStatusLookup(rows []model.AgentTeamTask) func(int) (string, bool) {
	m := make(map[int]string, len(rows))
	for _, r := range rows {
		m[r.TaskNo] = r.Status
	}
	return func(no int) (string, bool) {
		st, ok := m[no]
		return st, ok
	}
}

// ---------------------------------------------------------------------------
// TeamService：成员
// ---------------------------------------------------------------------------

// SpawnTeammate 建一个具名成员：校验名字 → 复用 v1 的 Delegate → 返回名册行。
func (s *teamService) SpawnTeammate(ctx context.Context, req agent.TeammateRequest) (agent.TeamMember, error) {
	a, err := resolveTeamActor(req.ParentSessionID)
	if err != nil {
		return agent.TeamMember{}, err
	}
	if !a.IsLead {
		return agent.TeamMember{}, errors.New("只有 lead（根会话）能创建团队成员")
	}
	name := strings.TrimSpace(req.Name)
	if err := validateTeamName(name); err != nil {
		return agent.TeamMember{}, err
	}
	description := strings.TrimSpace(req.Description)
	if description == "" {
		return agent.TeamMember{}, errors.New("缺少 description：给这个成员一句人话职责说明")
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return agent.TeamMember{}, errors.New("缺少 prompt：把交给它的完整任务写清楚（它默认看不到你和用户的对话）")
	}
	// 名字永不复用：查的是这个父会话下的**所有** run，包含已结束的。
	var existing int64
	if err := model.DB.Model(&model.AgentSubagentRun{}).
		Where("parent_conversation_id = ? AND name = ?", a.ConvID, name).Count(&existing).Error; err != nil {
		return agent.TeamMember{}, fmt.Errorf("检查重名失败：%w", err)
	}
	if existing > 0 {
		return agent.TeamMember{}, fmt.Errorf("TEAM_NAME_TAKEN：成员名 %q 已经被用过（包括已结束的成员）；名字永不复用，换一个", name)
	}

	handle, err := newSubagentRunner().Delegate(ctx, agent.SubagentRequest{
		ParentSessionID: strconv.FormatUint(uint64(a.ConvID), 10),
		RootSessionID:   strings.TrimSpace(req.RootSessionID),
		Depth:           req.Depth,
		Name:            name,
		Label:           clipRunes(description, 120),
		Prompt:          prompt,
		Context:         req.Context,
		History:         req.History,
		Model:           req.Model,
		Kind:            req.Kind,
		Root:            req.Root,
		Roots:           append([]string(nil), req.Roots...),
		Mode:            req.Mode,
	})
	if err != nil {
		return agent.TeamMember{}, err
	}
	run, err := latestTeamRunForChild(parseTeamChildIDOrZero(handle.ChildID))
	if err != nil {
		// Delegate 已经建好成员；读不回行时用回执兜一个名册行，别把成功的委派报成失败。
		return agent.TeamMember{
			Name:        name,
			Description: description,
			Role:        "teammate",
			Status:      "running",
			Depth:       handle.Depth,
			ChildID:     handle.ChildID,
			Model:       req.Model,
		}, nil
	}
	return agent.TeamMember{
		Name:        run.Name,
		Description: run.Label,
		Role:        "teammate",
		Status:      teamMemberStatus(run.Status),
		Depth:       run.Depth,
		ChildID:     strconv.FormatUint(uint64(run.ChildConversationID), 10),
		Model:       run.Model,
	}, nil
}

// Roster 返回名册：lead（父会话本身）+ 该父会话下所有具名子会话（每个只取最新一行 run）。
func (s *teamService) Roster(ctx context.Context, parentSessionID string) ([]agent.TeamMember, error) {
	a, err := resolveTeamActor(parentSessionID)
	if err != nil {
		return nil, err
	}
	var conv model.Conversation
	if err := model.DB.Where("id = ?", a.ConvID).First(&conv).Error; err != nil {
		return nil, fmt.Errorf("团队会话不存在：%w", err)
	}
	leadStatus := "inactive"
	if conversationBusy(conv.Id) {
		leadStatus = "running"
	}
	out := []agent.TeamMember{{
		Name:        teamLeadName,
		Description: "团队负责人（根会话）",
		Role:        "lead",
		Status:      leadStatus,
		Depth:       0,
		ChildID:     "",
		Model:       conv.Model,
	}}

	runs, err := teamRunsFor(a.ConvID)
	if err != nil {
		return nil, err
	}
	// 续跑会在同一个子会话上留下多行 run：按出现顺序取每个子会话的最新一行。
	latest := map[uint]model.AgentSubagentRun{}
	order := make([]uint, 0, len(runs))
	for _, run := range runs {
		if strings.TrimSpace(run.Name) == "" {
			continue // v1 的匿名委派不进团队名册（它们只在子任务卡片里出现）
		}
		if _, seen := latest[run.ChildConversationID]; !seen {
			order = append(order, run.ChildConversationID)
		}
		latest[run.ChildConversationID] = run
	}
	for _, childID := range order {
		run := latest[childID]
		out = append(out, agent.TeamMember{
			Name:        run.Name,
			Description: run.Label,
			Role:        "teammate",
			Status:      teamMemberStatus(run.Status),
			Depth:       run.Depth,
			ChildID:     strconv.FormatUint(uint64(childID), 10),
			Model:       run.Model,
			Activity:    run.Activity,
			Progress:    run.Progress,
		})
	}
	return out, nil
}

// teamTreeMembers 把整棵委派树投影给右上角面板。名册工具只管理具名直属成员，
// 面板则需要同时展示匿名子任务和更深层的子 agent。
func teamTreeMembers(root model.Conversation, runs []model.AgentSubagentRun, leadBusy bool) []gin.H {
	leadStatus := "inactive"
	if leadBusy {
		leadStatus = "running"
	}
	out := []gin.H{{
		"name": teamLeadName, "description": "团队负责人（根会话）", "role": "lead",
		"status": leadStatus, "depth": 0, "child_id": "", "parent_id": nil,
		"model": root.Model,
	}}
	latest := make(map[uint]model.AgentSubagentRun, len(runs))
	order := make([]uint, 0, len(runs))
	for _, run := range runs {
		if run.ChildConversationID == 0 || run.ChildConversationID == root.Id {
			continue
		}
		if _, exists := latest[run.ChildConversationID]; !exists {
			order = append(order, run.ChildConversationID)
		}
		latest[run.ChildConversationID] = run
	}
	children := make(map[uint][]uint)
	for _, childID := range order {
		run := latest[childID]
		children[run.ParentConversationID] = append(children[run.ParentConversationID], childID)
	}
	seen := make(map[uint]bool, len(order))
	var appendBranch func(uint)
	appendBranch = func(parentID uint) {
		for _, childID := range children[parentID] {
			if seen[childID] {
				continue
			}
			seen[childID] = true
			run := latest[childID]
			name, role := strings.TrimSpace(run.Name), "teammate"
			if name == "" {
				name, role = strings.TrimSpace(run.Label), "subagent"
			}
			if name == "" {
				name = fmt.Sprintf("子 agent %d", childID)
			}
			out = append(out, gin.H{
				"name": name, "description": run.Label, "role": role,
				"status": teamMemberStatus(run.Status), "depth": run.Depth,
				"child_id":  strconv.FormatUint(uint64(childID), 10),
				"parent_id": run.ParentConversationID, "model": run.Model,
				"activity": run.Activity, "progress": clipRunes(run.Progress, subagentReplyMaxRunes),
			})
			appendBranch(childID)
		}
	}
	appendBranch(root.Id)
	// 历史坏数据可能缺父节点；仍让用户能从面板打开这些会话。
	for _, childID := range order {
		if !seen[childID] {
			children[root.Id] = append(children[root.Id], childID)
		}
	}
	appendBranch(root.Id)
	return out
}

// ---------------------------------------------------------------------------
// TeamService：消息与等待
// ---------------------------------------------------------------------------

// SendMessage 把消息落进目标会话（role=user，带【来自 x】前缀）。
// 成功只表示「已收下」；目标在跑就排队，等这一轮结束自动起续跑。
func (s *teamService) SendMessage(ctx context.Context, parentSessionID, from, target, message string) error {
	a, err := resolveTeamActor(parentSessionID)
	if err != nil {
		return err
	}
	body := strings.TrimSpace(message)
	if body == "" {
		return errors.New("消息内容不能为空")
	}
	fromName, err := teamActorName(a, from)
	if err != nil {
		return err
	}
	targetName := strings.TrimSpace(target)
	if targetName == "" {
		return errors.New("缺少 target：要发给哪个成员（用 list_agents 看名册）")
	}
	content := teamMessageContent(fromName, body)

	// 发给 lead = 落进团队会话本身；它不在跑就直接唤醒一轮。
	if targetName == teamLeadName {
		if _, err := saveTeamMessage(a.ConvID, content); err != nil {
			return err
		}
		if !conversationBusy(a.ConvID) {
			if registry == nil {
				log.Printf("[team] 模型注册表未就绪，先不唤醒 lead（消息已落库）")
				return nil
			}
			wakeParentAfterSubagents(a.ConvID)
		} else {
			scheduleTeamWake(a.ConvID)
		}
		return nil
	}

	run, err := latestTeamRunByName(a.ConvID, targetName)
	if err != nil {
		return err
	}
	msgID, err := saveTeamMessage(run.ChildConversationID, content)
	if err != nil {
		return err
	}
	if run.Status == model.SubagentRunning {
		queueTeamContinuation(run, msgID)
		return nil
	}
	return s.startContinuation(ctx, run, msgID)
}

// WaitForChange 等下一次变化：每 500ms 拉一次父会话下所有 run 的 id+status 指纹。
// 没有活跃成员时立即返回 changed=false（避免模型空等）。
func (s *teamService) WaitForChange(ctx context.Context, parentSessionID string, timeoutMS int) (bool, error) {
	a, err := resolveTeamActor(parentSessionID)
	if err != nil {
		return false, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	base, err := teamRunFingerprint(a.ConvID)
	if err != nil {
		return false, err
	}
	if !strings.Contains(base, ":"+model.SubagentRunning+"|") {
		return false, nil // 没有活跃成员：再等也不会有人动
	}
	timer := time.NewTimer(teamTimeoutDuration(timeoutMS))
	defer timer.Stop()
	ticker := time.NewTicker(teamContinuationPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
			return false, nil
		case <-ticker.C:
			cur, err := teamRunFingerprint(a.ConvID)
			if err != nil {
				return false, err
			}
			if cur != base {
				return true, nil
			}
		}
	}
}

// teamTimeoutDuration 归一化 wait_agent 的超时：缺省 30s，上限 1 小时（契约 §1.1）。
func teamTimeoutDuration(timeoutMS int) time.Duration {
	if timeoutMS <= 0 {
		timeoutMS = 30000
	}
	if timeoutMS > 3600000 {
		timeoutMS = 3600000
	}
	return time.Duration(timeoutMS) * time.Millisecond
}

// teamRunFingerprint 把「父会话下所有 run 的 id+status」拼成一个指纹。
func teamRunFingerprint(convID uint) (string, error) {
	runs, err := teamRunsFor(convID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, run := range runs {
		fmt.Fprintf(&b, "%d:%s|", run.Id, run.Status)
	}
	return b.String(), nil
}

// saveTeamMessage 落一条 role=user 的消息并返回 id（续跑要用它把这条从历史里摘出来）。
func saveTeamMessage(convID uint, content string) (uint, error) {
	if model.DB == nil {
		return 0, errors.New("数据库未初始化")
	}
	msg := model.Message{ConversationID: convID, Role: "user", Content: content}
	if err := model.DB.Create(&msg).Error; err != nil {
		return 0, fmt.Errorf("落消息失败：%w", err)
	}
	touchConversation(convID)
	return msg.Id, nil
}

// ---------------------------------------------------------------------------
// TeamService：任务板
// ---------------------------------------------------------------------------

// CreateTask 建一条 pending 任务；任务号在团队会话内递增，依赖必须指向已存在且未删除的任务。
func (s *teamService) CreateTask(ctx context.Context, parentSessionID string, in agent.TeamTaskInput) (agent.TeamTask, error) {
	a, err := resolveTeamActor(parentSessionID)
	if err != nil {
		return agent.TeamTask{}, err
	}
	subject := strings.TrimSpace(in.Subject)
	if subject == "" {
		return agent.TeamTask{}, errors.New("缺少 subject：任务标题一句话说清要做什么")
	}
	deps, err := normalizeTeamTaskIDs(in.BlockedBy)
	if err != nil {
		return agent.TeamTask{}, err
	}
	owner := strings.TrimSpace(in.Owner)
	if owner != "" && !teamMemberExists(a.ConvID, owner) {
		return agent.TeamTask{}, fmt.Errorf("owner %q 不是本团队成员：先用 list_agents 看名册", owner)
	}

	var created model.AgentTeamTask
	err = model.DB.Transaction(func(tx *gorm.DB) error {
		// 锁住团队会话这一行，串行化任务号分配：两个成员同时建任务不会拿到同一个 task_no。
		var conv model.Conversation
		if err := model.LockUpdate(tx).
			Where("id = ? AND user_id = ?", a.ConvID, a.UserID).First(&conv).Error; err != nil {
			return errors.New("团队会话不存在")
		}
		var rows []model.AgentTeamTask
		if err := tx.Where("team_conversation_id = ?", a.ConvID).Order("task_no asc").Find(&rows).Error; err != nil {
			return err
		}
		if err := validateTeamBlockedBy(deps, teamTaskStatusLookup(rows)); err != nil {
			return err
		}
		maxNo := 0
		for _, r := range rows {
			if r.TaskNo > maxNo {
				maxNo = r.TaskNo
			}
		}
		created = model.AgentTeamTask{
			UserID:             a.UserID,
			TeamConversationID: a.ConvID,
			TaskNo:             maxNo + 1,
			Subject:            subject,
			Description:        strings.TrimSpace(in.Description),
			Status:             model.TeamTaskPending,
			Owner:              owner,
			BlockedBy:          encodeTeamStrings(deps),
			WriteScopes:        encodeTeamStrings(normalizeTeamWriteScopes(in.WriteScopes)),
			Revision:           1,
		}
		return tx.Create(&created).Error
	})
	if err != nil {
		return agent.TeamTask{}, fmt.Errorf("建任务失败：%w", err)
	}
	return teamTaskView(created, teamTaskDepsReady(created.BlockedBy, teamTaskStatusLookup(nil))), nil
}

// ListTasks 列任务：默认不含 tombstone；ready 是**算出来**的（依赖是否全完成）。
func (s *teamService) ListTasks(ctx context.Context, parentSessionID string, filter agent.TaskFilter) ([]agent.TeamTask, error) {
	a, err := resolveTeamActor(parentSessionID)
	if err != nil {
		return nil, err
	}
	status := strings.TrimSpace(filter.Status)
	switch status {
	case "", model.TeamTaskPending, model.TeamTaskInProgress, model.TeamTaskCompleted, model.TeamTaskDeleted:
	default:
		return nil, fmt.Errorf("未知的 status %q：可用 pending / in_progress / completed（不传 = 全部未删除）", filter.Status)
	}
	owner := strings.TrimSpace(filter.Owner)
	rows, err := teamTasksFor(a.ConvID)
	if err != nil {
		return nil, err
	}
	lookup := teamTaskStatusLookup(rows)
	out := make([]agent.TeamTask, 0, len(rows))
	for _, row := range rows {
		if status == "" {
			if row.Status == model.TeamTaskDeleted {
				continue
			}
		} else if row.Status != status {
			continue
		}
		if owner != "" && row.Owner != owner {
			continue
		}
		ready := teamTaskDepsReady(row.BlockedBy, lookup)
		if filter.Ready != nil && *filter.Ready != ready {
			continue
		}
		out = append(out, teamTaskView(row, ready))
	}
	return out, nil
}

// GetTask 读一条任务（含 tombstone：id 稳定，删除后仍能查到它为什么没了）。
func (s *teamService) GetTask(ctx context.Context, parentSessionID, id string) (agent.TeamTask, error) {
	a, err := resolveTeamActor(parentSessionID)
	if err != nil {
		return agent.TeamTask{}, err
	}
	no, err := parseTeamTaskID(id)
	if err != nil {
		return agent.TeamTask{}, err
	}
	var row model.AgentTeamTask
	if err := model.DB.Where("team_conversation_id = ? AND task_no = ?", a.ConvID, no).First(&row).Error; err != nil {
		return agent.TeamTask{}, fmt.Errorf("任务 %s 不存在", id)
	}
	rows, err := teamTasksFor(a.ConvID)
	if err != nil {
		return agent.TeamTask{}, err
	}
	return teamTaskView(row, teamTaskDepsReady(row.BlockedBy, teamTaskStatusLookup(rows))), nil
}

// UpdateTask 是任务板唯一的写入口：CAS + 状态机 + 依赖校验，全部按契约。
func (s *teamService) UpdateTask(ctx context.Context, parentSessionID, id, actor string, expectedRevision int, action string, patch agent.TeamTaskInput) (agent.TeamTask, error) {
	a, err := resolveTeamActor(parentSessionID)
	if err != nil {
		return agent.TeamTask{}, err
	}
	no, err := parseTeamTaskID(id)
	if err != nil {
		return agent.TeamTask{}, err
	}
	actorName, err := teamActorName(a, actor)
	if err != nil {
		return agent.TeamTask{}, err
	}
	var row model.AgentTeamTask
	if err := model.DB.Where("team_conversation_id = ? AND task_no = ?", a.ConvID, no).First(&row).Error; err != nil {
		return agent.TeamTask{}, fmt.Errorf("任务 %s 不存在", id)
	}
	rows, err := teamTasksFor(a.ConvID)
	if err != nil {
		return agent.TeamTask{}, err
	}
	lookup := teamTaskStatusLookup(rows)
	facts := teamBoardFacts{
		DepsReady:    teamTaskDepsReady(row.BlockedBy, lookup),
		MemberExists: func(name string) bool { return teamMemberExists(a.ConvID, name) },
	}
	updates, err := planTeamTaskUpdate(row, teamActor{Name: actorName, IsLead: actorName == teamLeadName}, expectedRevision, action, patch, facts)
	if err != nil {
		return agent.TeamTask{}, err
	}
	// set_dependencies 的依赖列表要先归一化、再校验存在性（含「不能依赖自己」）。
	if action == "set_dependencies" {
		deps, err := normalizeTeamTaskIDs(patch.BlockedBy)
		if err != nil {
			return agent.TeamTask{}, err
		}
		for _, dep := range deps {
			if depNo, _ := parseTeamTaskID(dep); depNo == row.TaskNo {
				return agent.TeamTask{}, fmt.Errorf("task-%d 不能依赖自己", row.TaskNo)
			}
		}
		if err := validateTeamBlockedBy(deps, lookup); err != nil {
			return agent.TeamTask{}, err
		}
		updates["blocked_by"] = encodeTeamStrings(deps)
	}
	updates["revision"] = expectedRevision + 1
	updates["updated_at"] = time.Now()

	// CAS 落到 SQL 上：WHERE revision = expected。RowsAffected=0 说明等我们写的时候
	// 已经有别人改过（纯函数那次检查只是提前给出好错误，SQL 这次才是权威）。
	res := model.DB.Model(&model.AgentTeamTask{}).
		Where("id = ? AND revision = ?", row.Id, expectedRevision).
		Updates(updates)
	if res.Error != nil {
		return agent.TeamTask{}, fmt.Errorf("更新任务失败：%w", res.Error)
	}
	if res.RowsAffected == 0 {
		return agent.TeamTask{}, fmt.Errorf("TEAM_TASK_STALE_REVISION：任务 task-%d 刚刚被别人改了；请重新 team_task_get 再提交", row.TaskNo)
	}
	if err := model.DB.Where("id = ?", row.Id).First(&row).Error; err != nil {
		return agent.TeamTask{}, fmt.Errorf("读取更新后的任务失败：%w", err)
	}
	rows, err = teamTasksFor(a.ConvID)
	if err != nil {
		return agent.TeamTask{}, err
	}
	return teamTaskView(row, teamTaskDepsReady(row.BlockedBy, teamTaskStatusLookup(rows))), nil
}

// ---------------------------------------------------------------------------
// 续跑：把「给已结束成员发消息」变成同一子会话上的新一轮
// ---------------------------------------------------------------------------

// startContinuation 在同一个子会话上起一轮续跑：新的 run 行、depth 不变、Name 继承。
// 消息已经由 SendMessage 落库；这里把它从历史里摘出来当 Prompt，避免喂两遍。
func (s *teamService) startContinuation(ctx context.Context, prev model.AgentSubagentRun, messageID uint) error {
	if model.DB == nil {
		return errors.New("数据库未初始化")
	}
	// 同一成员的续跑只允许一个在起：两个并发 send_message 不能各起一轮。
	teamStartMu.Lock()
	defer teamStartMu.Unlock()
	if latest, err := latestTeamRunForChild(prev.ChildConversationID); err == nil {
		if latest.Status == model.SubagentRunning {
			// 已经有新一轮在跑：它可能没读到我们这条消息（它的历史是在我们落库前拍的），
			// 再排一个看守，保证这条消息一定有人读。
			queueTeamContinuation(latest, messageID)
			return nil
		}
	}

	var msg model.Message
	if err := model.DB.Where("id = ? AND conversation_id = ?", messageID, prev.ChildConversationID).
		First(&msg).Error; err != nil {
		return fmt.Errorf("待续跑的消息不存在：%w", err)
	}
	var rows []model.Message
	if err := model.DB.Where("conversation_id = ?", prev.ChildConversationID).
		Order("id asc").Find(&rows).Error; err != nil {
		return fmt.Errorf("读取成员会话失败：%w", err)
	}
	history := make([]model.Message, 0, len(rows))
	for _, m := range rows {
		if m.Id == msg.Id {
			continue // 这条由 req.Prompt 带进去
		}
		history = append(history, m)
	}

	run := model.AgentSubagentRun{
		UserID:               prev.UserID,
		ParentConversationID: prev.ParentConversationID,
		ChildConversationID:  prev.ChildConversationID,
		RootConversationID:   prev.RootConversationID,
		Depth:                prev.Depth, // 续跑不加深
		Label:                prev.Label,
		Name:                 prev.Name, // 名字继承
		Prompt:               msg.Content,
		ContextMode:          "fresh",
		Status:               model.SubagentRunning,
		Model:                prev.Model,
		Kind:                 prev.Kind,
	}
	if err := model.DB.Create(&run).Error; err != nil {
		return fmt.Errorf("起续跑失败：%w", err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	bg := context.WithoutCancel(ctx)
	markSubagentBatch(run.ParentConversationID)
	teamRoots := teamWorkspaceRootsFor(run.UserID, run.Kind)
	req := agent.SubagentRequest{
		ParentSessionID: strconv.FormatUint(uint64(run.ParentConversationID), 10),
		RootSessionID:   strconv.FormatUint(uint64(run.RootConversationID), 10),
		Depth:           run.Depth,
		Label:           run.Label,
		Prompt:          msg.Content,
		Context:         "fresh",
		History:         rebuildLLMHistory(history),
		Model:           run.Model,
		Kind:            run.Kind,
		Root:            firstWorkspaceRoot(teamRoots),
		Roots:           additionalWorkspaceRoots(teamRoots),
		Mode:            agentModeFor(run.UserID),
	}
	go runTeamContinuation(bg, run, req, run.Depth)
	return nil
}

// runTeamContinuation 跑一轮续跑：复用 v1 的 executeChild/settle，不另起一套。
func runTeamContinuation(ctx context.Context, run model.AgentSubagentRun, req agent.SubagentRequest, depth int) {
	runner := newSubagentRunner()
	started := time.Now()
	status, stopReason, reply, errText := runner.executeChild(ctx, req, depth, run.UserID, run.Id, run.ChildConversationID, run.ParentConversationID)
	persist := run
	// 消息已经由 SendMessage 落进子会话（role=user）；settle 里那次 run.Prompt 落库要跳过，
	// 否则同一句话会在成员会话里出现两遍。
	persist.Prompt = ""
	runner.settle(persist, run.ParentConversationID, run.ChildConversationID, started, status, stopReason, reply, errText)
}

// teamRootFor 给续跑挑工作区：与会话一轮的规则一致（模式专属目录优先，其次通用工作区）。
// run 行没存 spawn 时的工作区/档位快照，续跑用用户当前设置——这是已知的小缺口。
func teamRootFor(userID, kind string) string {
	return firstWorkspaceRoot(teamWorkspaceRootsFor(userID, kind))
}

func teamWorkspaceRootsFor(userID, kind string) []string {
	return kindWorkspaceRootsFor(userID, kind, agentFSRootsFor(userID))
}

func firstWorkspaceRoot(roots []string) string {
	if len(roots) == 0 {
		return ""
	}
	return roots[0]
}

// 排队看守：给运行中的成员发消息 = 消息先落库，等这一轮跑完自动起续跑。
// 每个目标同时只有一个看守（map 去重），看守最长等 teamContinuationWaitMax。
var (
	// teamStartMu 串行化「检查最新 run + 插入续跑 run 行」这一段：并发的 send_message
	// 不能给同一个成员各起一轮续跑。
	teamStartMu      sync.Mutex
	teamQueueMu      sync.Mutex
	teamQueueWaiting = map[string]bool{}
)

func teamQueueClaim(key string) bool {
	teamQueueMu.Lock()
	defer teamQueueMu.Unlock()
	if teamQueueWaiting[key] {
		return false
	}
	teamQueueWaiting[key] = true
	return true
}

func teamQueueDone(key string) {
	teamQueueMu.Lock()
	delete(teamQueueWaiting, key)
	teamQueueMu.Unlock()
}

// queueTeamContinuation 给「正在跑的成员」排一条续跑：看守轮询该成员的最新 run，
// 一旦不在跑就起新一轮（把刚落的这条消息读进来）。
func queueTeamContinuation(prev model.AgentSubagentRun, messageID uint) {
	key := "member:" + strconv.FormatUint(uint64(prev.ChildConversationID), 10)
	if !teamQueueClaim(key) {
		return // 已经有人守着这个成员了，消息会进历史
	}
	go func() {
		defer teamQueueDone(key)
		deadline := time.Now().Add(teamContinuationWaitMax)
		for time.Now().Before(deadline) {
			time.Sleep(teamContinuationPoll)
			if model.DB == nil {
				return
			}
			latest, err := latestTeamRunForChild(prev.ChildConversationID)
			if err != nil {
				return
			}
			if latest.Status == model.SubagentRunning {
				continue
			}
			if err := newTeamService().startContinuation(context.Background(), latest, messageID); err != nil {
				log.Printf("[team] 成员 %s 续跑启动失败（消息仍在会话里）：%v", latest.Name, err)
			}
			return
		}
		log.Printf("[team] 成员 %s 的排队消息等太久（%v），放弃自动续跑；消息仍在会话里", prev.Name, teamContinuationWaitMax)
	}()
}

// scheduleTeamWake 给「正在跑的 lead」排一次唤醒：等它这一轮结束再走 v1 的唤醒路径。
func scheduleTeamWake(convID uint) {
	key := "lead:" + strconv.FormatUint(uint64(convID), 10)
	if !teamQueueClaim(key) {
		return
	}
	go func() {
		defer teamQueueDone(key)
		deadline := time.Now().Add(teamContinuationWaitMax)
		for time.Now().Before(deadline) {
			time.Sleep(teamContinuationPoll)
			if !conversationBusy(convID) {
				wakeParentAfterSubagents(convID)
				return
			}
		}
	}()
}

// parseTeamChildIDOrZero 解析 Delegate 回执里的子会话 id；解析不了返回 0（调用方兜底）。
func parseTeamChildIDOrZero(raw string) uint {
	n, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0
	}
	return uint(n)
}

// ---------------------------------------------------------------------------
// HTTP 接口（root 在 web/web.go 里挂路由：api.GET("/conversations/:id/team", getConversationTeam)）
// ---------------------------------------------------------------------------

// getConversationTeam 返回团队面板要的两份投影：名册 + 未删除任务。
// 形状冻结见 docs/agent/07-team.md §1.3。
func getConversationTeam(c *gin.Context) {
	if model.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库未初始化"})
		return
	}
	userID := currentUserID(c)
	var conv model.Conversation
	if err := model.DB.Where("id = ? AND user_id = ?", c.Param("id"), userID).First(&conv).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "会话不存在"})
		return
	}
	svc := newTeamService()
	_, rootSessionID := conversationDelegation(&conv)
	rootID, err := parseSubagentConvID(rootSessionID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取团队根会话失败"})
		return
	}
	var root model.Conversation
	if err := model.DB.Where("id = ? AND user_id = ?", rootID, userID).First(&root).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "团队根会话不存在"})
		return
	}
	var runs []model.AgentSubagentRun
	if err := model.DB.Where("root_conversation_id = ? OR parent_conversation_id = ?", rootID, rootID).
		Order("id asc").Find(&runs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取团队成员失败"})
		return
	}
	tasks, err := svc.ListTasks(c.Request.Context(), rootSessionID, agent.TaskFilter{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取任务板失败"})
		return
	}
	memberItems := teamTreeMembers(root, runs, conversationBusy(rootID))
	taskItems := make([]gin.H, 0, len(tasks))
	for _, t := range tasks {
		taskItems = append(taskItems, gin.H{
			"id":           t.ID,
			"revision":     t.Revision,
			"subject":      t.Subject,
			"status":       t.Status,
			"owner":        t.Owner,
			"blocked_by":   t.BlockedBy,
			"write_scopes": t.WriteScopes,
			"ready":        t.Ready,
		})
	}
	c.JSON(http.StatusOK, gin.H{"members": memberItems, "tasks": taskItems, "root_id": rootID})
}

package toolkit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"blog-my/internal/agent"
)

// ask_user 的选项会被前端画成按钮，所以这里钉住三件事：
// 1) 合法参数必须把 options / question 原样带到 Display；
// 2) 数量、空串、超长等非法参数必须回 is_error（而不是 panic 或让本轮失败）；
// 3) 错误文案必须告诉模型「必须给 2-4 个选项」，它才会改对参数重试。

func runAskUser(t *testing.T, args map[string]any) agent.Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("构造参数失败：%v", err)
	}
	res, err := AskUserTool{}.Execute(context.Background(), raw, nil)
	if err != nil {
		t.Fatalf("Execute 返回了 Go error（应折成 is_error 结果）：%v", err)
	}
	return res
}

func displayOptions(t *testing.T, res agent.Result) []string {
	t.Helper()
	opts, ok := res.Display["options"].([]string)
	if !ok {
		t.Fatalf("Display.options 应是 []string，实际 %#v", res.Display["options"])
	}
	return opts
}

func TestAskUserCarriesOptionsToDisplay(t *testing.T) {
	res := runAskUser(t, map[string]any{
		"question": "用哪个名字？",
		"options":  []string{"小月", "小羊"},
	})
	if res.IsError {
		t.Fatalf("合法提问不该是错误：%s", res.Content)
	}
	if got := res.Display["question"]; got != "用哪个名字？" {
		t.Fatalf("Display.question 丢了：%#v", got)
	}
	opts := displayOptions(t, res)
	if len(opts) != 2 || opts[0] != "小月" || opts[1] != "小羊" {
		t.Fatalf("选项解析不对：%+v", opts)
	}
	if res.AwaitUser == "" {
		t.Fatal("AwaitUser 必须非空，否则循环会继续跑下去")
	}
	if !strings.Contains(res.AwaitUser, "小月") || !strings.Contains(res.AwaitUser, "小羊") {
		t.Fatalf("落库文本里应包含候选项：%q", res.AwaitUser)
	}
}

// 不传 options 是老用法，现在必须报错：模型要看到明确文案后补上选项。
func TestAskUserRequiresOptions(t *testing.T) {
	res := runAskUser(t, map[string]any{"question": "你希望我怎么称呼你？"})
	if !res.IsError {
		t.Fatal("缺少 options 时应当返回 is_error")
	}
	if !strings.Contains(res.Content, "2-4 个选项") {
		t.Fatalf("错误文案应告诉模型必须给 2-4 个选项：%q", res.Content)
	}
}

func TestAskUserRejectsTooFewOptions(t *testing.T) {
	res := runAskUser(t, map[string]any{
		"question": "选哪个？",
		"options":  []string{"只有一个"},
	})
	if !res.IsError {
		t.Fatal("只有 1 个候选项时应当返回 is_error")
	}
	if !strings.Contains(res.Content, "2-4 个选项") {
		t.Fatalf("错误文案应告诉模型必须给 2-4 个选项：%q", res.Content)
	}
}

func TestAskUserRejectsTooManyOptions(t *testing.T) {
	res := runAskUser(t, map[string]any{
		"question": "选哪个？",
		"options":  []string{"一", "二", "三", "四", "五"},
	})
	if !res.IsError {
		t.Fatal("5 个候选项时应当返回 is_error")
	}
	if !strings.Contains(res.Content, "2-4 个选项") {
		t.Fatalf("错误文案应告诉模型必须给 2-4 个选项：%q", res.Content)
	}
}

func TestAskUserRejectsBlankOption(t *testing.T) {
	res := runAskUser(t, map[string]any{
		"question": "选哪个？",
		"options":  []string{"甲", "   "},
	})
	if !res.IsError {
		t.Fatal("空字符串选项应当返回 is_error")
	}
	if !strings.Contains(res.Content, "2-4 个选项") {
		t.Fatalf("错误文案应告诉模型必须给 2-4 个选项：%q", res.Content)
	}
}

func TestAskUserRejectsOverlongOption(t *testing.T) {
	long := strings.Repeat("长", askUserMaxOptionLen+1)
	res := runAskUser(t, map[string]any{
		"question": "选哪个？",
		"options":  []string{"甲", long},
	})
	if !res.IsError {
		t.Fatal("超过 40 个字的选项应当返回 is_error")
	}
	if !strings.Contains(res.Content, "40") {
		t.Fatalf("错误文案应说明每个选项不超过 40 个字：%q", res.Content)
	}
}

// 40 个汉字正好在限制内，不能被按字节数误伤。
func TestAskUserAllowsFortyRuneOption(t *testing.T) {
	exact := strings.Repeat("字", askUserMaxOptionLen)
	res := runAskUser(t, map[string]any{
		"question": "选哪个？",
		"options":  []string{"甲", exact},
	})
	if res.IsError {
		t.Fatalf("40 个字的选项应当合法：%s", res.Content)
	}
	if got := displayOptions(t, res); len(got) != 2 || got[1] != exact {
		t.Fatalf("选项内容不对：%+v", got)
	}
}

// 旧的 {label, description} 写法不再是合法参数：参数契约已冻结成 string[]。
// 这时要给模型一条可执行的错误说明，让它改写成字符串数组后重试。
func TestAskUserRejectsObjectOptions(t *testing.T) {
	raw := json.RawMessage(`{"question":"选哪个？","options":[{"label":"甲"},{"label":"乙"}]}`)
	res, err := AskUserTool{}.Execute(context.Background(), raw, nil)
	if err != nil {
		t.Fatalf("Execute 不该返回 Go error：%v", err)
	}
	if !res.IsError {
		t.Fatal("对象写法应当返回 is_error，提示模型改用字符串数组")
	}
	if !strings.Contains(res.Content, "2-4 个选项") {
		t.Fatalf("错误文案应告诉模型必须给 2-4 个选项：%q", res.Content)
	}
}

// 问题本身为空仍然是参数错误（与选项无关），保持原有的 error 返回。
func TestAskUserRejectsBlankQuestion(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"question": "   ", "options": []string{"甲", "乙"}})
	if _, err := (AskUserTool{}).Execute(context.Background(), raw, nil); err == nil {
		t.Fatal("空问题应当报错")
	}
}

// 合法提问的 Parameters 里，options 必须是 required，且数量/长度约束要露给模型。
func TestAskUserParametersDeclareRequiredOptions(t *testing.T) {
	p := AskUserTool{}.Parameters()
	req, _ := p["required"].([]string)
	found := false
	for _, name := range req {
		if name == "options" {
			found = true
		}
	}
	if !found {
		t.Fatalf("options 必须是 required：%#v", p["required"])
	}
	props, _ := p["properties"].(map[string]any)
	opts, _ := props["options"].(map[string]any)
	if opts["minItems"] != askUserMinOptions || opts["maxItems"] != askUserMaxOptions {
		t.Fatalf("options 的 minItems/maxItems 不对：%#v", opts)
	}
	items, _ := opts["items"].(map[string]any)
	if items["maxLength"] != askUserMaxOptionLen {
		t.Fatalf("options.items.maxLength 不对：%#v", items)
	}
}

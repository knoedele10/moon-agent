package web

import "testing"

// 这道闸门的目标是“宁漏不错”：闲聊不该触发技能检索（省一次模型调用 + 不塞无关技能），
// 真正的任务请求要能进去。
func TestNeedSkills(t *testing.T) {
	no := []string{
		"", "  ", "你好", "您好", "在吗", "好的", "好滴", "谢谢", "多谢", "嗯", "哦",
		"ok", "OK", "收到", "明白", "知道了", "哈哈", "早上好", "晚安", "再见",
		"吃了吗", "今天天气不错", "几点了",
	}
	for _, s := range no {
		if needSkills(s) {
			t.Errorf("%q 是闲聊/太短，不该触发技能检索", s)
		}
	}

	yes := []string{
		"帮我做一个 PPT",
		"帮我写一份周报",
		"把这份文档整理成表格",
		"生成一份季度报告",
		"设计一张海报",
		"帮我调研一下竞品情况",
		"create a slide deck about memory",
		"generate a report from this data",
		"把 excel 里的数据做个图表",
		"这段代码帮我重构一下",
		// 没有信号词但足够长（≥30 字）：算有信息量的请求
		"我想让你先看看这段材料然后告诉我可以从哪几个角度切入会更好一些",
	}
	for _, s := range yes {
		if !needSkills(s) {
			t.Errorf("%q 是任务请求，应当触发技能检索", s)
		}
	}
}

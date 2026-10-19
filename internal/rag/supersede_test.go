package rag

import "testing"

// “新事实取代旧事实”的判定：宁可漏掉一条旧记忆，也不能把不同的事误判成互相取代。
func TestShouldSupersede(t *testing.T) {
	// 同一件事改了口径：应当取代。
	if !shouldSupersede("用户的部署端口是 9000", "用户的部署端口是 8300") {
		t.Fatal("同一主题的新旧事实应当判为取代")
	}
	if !shouldSupersede("用户的项目代号是蓝鲸二号", "用户的项目代号是蓝鲸") {
		t.Fatal("同主题的细化应当判为取代")
	}

	// 不同的事：绝不能互相取代（“喜欢喝咖啡” vs “喜欢喝喝茶”）。
	if shouldSupersede("用户喜欢喝咖啡", "用户喜欢喝茶") {
		t.Fatal("不同偏好不该被判为取代")
	}
	if shouldSupersede("用户住在上海", "用户的项目代号是蓝鲸") {
		t.Fatal("无关事实不该被判为取代")
	}

	// 边界：空串与完全相同。
	if shouldSupersede("", "用户喜欢喝咖啡") || shouldSupersede("用户喜欢喝咖啡", "") {
		t.Fatal("空串不该判为取代")
	}
	if shouldSupersede("用户喜欢喝咖啡", "用户喜欢喝咖啡") {
		t.Fatal("完全相同不该判为取代（那是重复，不是更新）")
	}
}

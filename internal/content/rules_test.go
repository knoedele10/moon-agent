package content

import (
	"errors"
	"testing"

	"blog-my/internal/model"
)

// 稿件状态机：允许的路走通，跳级与乱填被拒。
func TestDraftStatusMachine(t *testing.T) {
	ok := [][2]string{
		{DraftIdea, DraftDraft},
		{DraftDraft, DraftReview},
		{DraftReview, DraftDraft}, // 待审打回
		{DraftReview, DraftFinal},
		{DraftFinal, DraftReview}, // 定稿打回
		{DraftFinal, DraftScheduled},
		{DraftScheduled, DraftPublished},
		{DraftScheduled, DraftFinal}, // 取消排期
		{DraftPublished, DraftArchived},
		{DraftArchived, DraftDraft}, // 重开
		{"", DraftDraft},            // 导入历史数据：新建即草稿
		{DraftDraft, DraftDraft},    // 同状态幂等
	}
	for _, c := range ok {
		if err := CanTransitionDraft(c[0], c[1]); err != nil {
			t.Fatalf("%s → %s 应允许，实际 %v", c[0], c[1], err)
		}
	}

	bad := [][2]string{
		{DraftIdea, DraftPublished}, // 不许跳级
		{DraftIdea, DraftFinal},
		{DraftPublished, DraftDraft}, // 已发布只能归档
		{DraftArchived, DraftPublished},
		{"", DraftPublished},
		{DraftDraft, "publishedd"}, // 拼错
		{DraftDraft, ""},
	}
	for _, c := range bad {
		err := CanTransitionDraft(c[0], c[1])
		if err == nil {
			t.Fatalf("%s → %s 应被拒", c[0], c[1])
		}
		if c[1] == "" || !ValidDraftStatus(c[1]) {
			if !errors.Is(err, ErrInvalidStatus) {
				t.Fatalf("%s → %s 应是 ErrInvalidStatus，实际 %v", c[0], c[1], err)
			}
			continue
		}
		if !errors.Is(err, ErrBadTransition) {
			t.Fatalf("%s → %s 应是 ErrBadTransition，实际 %v", c[0], c[1], err)
		}
	}
}

// 发布台账状态机：失败可重排、已发可撤回。
func TestPubStatusMachine(t *testing.T) {
	for _, c := range [][2]string{
		{PubPlanned, PubPublished},
		{PubPlanned, PubFailed},
		{PubFailed, PubPlanned}, // 重试
		{PubFailed, PubPublished},
		{PubPublished, PubWithdrawn},
		{PubWithdrawn, PubPlanned},
		{PubPlanned, PubPlanned},
	} {
		if err := CanTransitionPub(c[0], c[1]); err != nil {
			t.Fatalf("%s → %s 应允许，实际 %v", c[0], c[1], err)
		}
	}
	for _, c := range [][2]string{
		{PubPublished, PubPlanned},
		{PubPublished, PubFailed},
		{PubWithdrawn, PubPublished},
		{PubPlanned, "publish"},
	} {
		if err := CanTransitionPub(c[0], c[1]); err == nil {
			t.Fatalf("%s → %s 应被拒", c[0], c[1])
		}
	}
}

// 标签：去重、去空白、坏数据不 panic。
func TestTagsRoundTrip(t *testing.T) {
	raw := EncodeTags([]string{" 干货 ", "", "干货", "AI"})
	if raw != `["干货","AI"]` {
		t.Fatalf("编码结果 = %q", raw)
	}
	got := DecodeTags(raw)
	if len(got) != 2 || got[0] != "干货" || got[1] != "AI" {
		t.Fatalf("解码结果 = %v", got)
	}
	if EncodeTags(nil) != "" {
		t.Fatal("空标签应编成空串")
	}
	for _, bad := range []string{"", "  ", "not-json", `{"a":1}`, `["未闭合`} {
		if got := DecodeTags(bad); got != nil {
			t.Fatalf("坏数据 %q 应返回 nil，实际 %v", bad, got)
		}
	}
}

// 标题归一：折叠空白、裁到 120 字符。
func TestCleanTitle(t *testing.T) {
	if got := CleanTitle("  你好   世界  "); got != "你好 世界" {
		t.Fatalf("得到 %q", got)
	}
	long := make([]rune, 200)
	for i := range long {
		long[i] = '字'
	}
	if got := CleanTitle(string(long)); len([]rune(got)) != 120 {
		t.Fatalf("超长标题应裁到 120 字符，实际 %d", len([]rune(got)))
	}
}

// 输入校验：素材必须有来源之一，稿件必须有标题与正文。
func TestInputValidation(t *testing.T) {
	if err := CheckAssetInput("image", "封面", "uploads/a.png", ""); err != nil {
		t.Fatalf("合法素材被拒：%v", err)
	}
	if err := CheckAssetInput("image", "封面", "", ""); err == nil {
		t.Fatal("没有 file_key/url 应被拒")
	}
	if err := CheckAssetInput("gif", "封面", "a", ""); !errors.Is(err, ErrInvalidKind) {
		t.Fatalf("非法类型应是 ErrInvalidKind，实际 %v", err)
	}
	if err := CheckAssetInput("image", "  ", "a", ""); err == nil {
		t.Fatal("空标题应被拒")
	}
	if err := CheckDraftInput("", "正文"); err == nil {
		t.Fatal("空标题应被拒")
	}
	if err := CheckDraftInput("标题", "   "); err == nil {
		t.Fatal("空正文应被拒")
	}
	if err := CheckDraftInput("标题", "正文"); err != nil {
		t.Fatalf("合法稿件被拒：%v", err)
	}
}

// 素材用途：空串归一成 reference，非法值拒绝。
func TestRoleNormalize(t *testing.T) {
	if NormalizeRole("") != RoleRef {
		t.Fatal("空用途应归一成 reference")
	}
	if NormalizeRole(" image ") != RoleImage {
		t.Fatal("应去掉空白")
	}
	if !ValidRole("") || !ValidRole(RoleQuote) || ValidRole("poster") {
		t.Fatal("用途白名单不对")
	}
}

// 分页：0/负数用默认 50，超上限压到 200。
func TestPageLimit(t *testing.T) {
	if pageLimit(0) != 50 || pageLimit(-3) != 50 {
		t.Fatal("默认分页应为 50")
	}
	if pageLimit(1000) != 200 {
		t.Fatal("上限应为 200")
	}
	if pageLimit(30) != 30 {
		t.Fatal("正常值应原样返回")
	}
}

// 未绑数据库时所有入口都应返回明确错误，而不是 nil 指针 panic。
func TestStoreWithoutDB(t *testing.T) {
	s := NewStore(nil)
	if _, _, err := s.SaveAsset("u1", model.ContentAsset{Kind: AssetImage, Title: "封面", FileKey: "uploads/a.png"}); err == nil {
		t.Fatal("没有数据库时登记素材应报错")
	}
	if _, err := s.GetDraft("u1", 1); err == nil {
		t.Fatal("没有数据库时取稿件应报错")
	}
	if err := s.LinkAsset("u1", 1, 1, RoleImage, ""); err == nil {
		t.Fatal("没有数据库时关联素材应报错")
	}
	if _, err := s.ExportDraft("u1", 1, "wechat"); err == nil {
		t.Fatal("没有数据库时导出应报错")
	}
}

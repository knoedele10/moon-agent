package content

import (
	"errors"
	"os"
	"testing"
	"time"

	"blog-my/internal/model"
)

// 内容库需要真 MySQL（表结构是 AutoMigrate 建的）。默认跳过，设 CONTENT_IT=1 才跑：
//
//	$env:CONTENT_IT='1'; go test ./internal/content -run Store -v
//
// 与其他 IT 用例共用同一个库，所以这里只操作自己造的用户 id，并在结束时清干净。
func openTestStore(t *testing.T) *Store {
	t.Helper()
	if os.Getenv("CONTENT_IT") != "1" {
		t.Skip("需要真 MySQL：设 CONTENT_IT=1 才跑")
	}
	if err := model.InitDB(); err != nil {
		t.Skipf("初始化数据库失败，跳过：%v", err)
	}
	return NewStore(model.DB)
}

func testUser(t *testing.T) string {
	t.Helper()
	// 用固定前缀 + 纳秒后缀，避免与真人数据或别的用例撞车。
	return "content-it-" + time.Now().Format("150405.000000000")
}

func cleanupUser(t *testing.T, s *Store, userID string) {
	t.Helper()
	db := s.DB
	db.Where("user_id = ?", userID).Delete(&model.ContentDraftAsset{})
	db.Where("user_id = ?", userID).Delete(&model.ContentPublication{})
	db.Where("user_id = ?", userID).Delete(&model.ContentDraftVersion{})
	db.Where("user_id = ?", userID).Delete(&model.ContentDraft{})
	db.Where("user_id = ?", userID).Delete(&model.ContentAsset{})
}

// 素材：同 sha256 二次登记复用同一行；跨用户各自独立。
func TestStoreAssetDedupeAndIsolation(t *testing.T) {
	s := openTestStore(t)
	u1, u2 := testUser(t)+"-a", testUser(t)+"-b"
	defer cleanupUser(t, s, u1)
	defer cleanupUser(t, s, u2)

	a1, reused, err := s.SaveAsset(u1, model.ContentAsset{Kind: AssetImage, Title: "封面", FileKey: "uploads/a.png", Sha256: "ABC123"})
	if err != nil || reused {
		t.Fatalf("首次登记失败：err=%v reused=%v", err, reused)
	}
	a2, reused, err := s.SaveAsset(u1, model.ContentAsset{Kind: AssetImage, Title: "同一张图", FileKey: "uploads/b.png", Sha256: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if !reused || a2.Id != a1.Id {
		t.Fatalf("同 sha 应复用同一条：reused=%v id=%d/%d", reused, a1.Id, a2.Id)
	}
	other, reused, err := s.SaveAsset(u2, model.ContentAsset{Kind: AssetImage, Title: "别人同一张", FileKey: "uploads/a.png", Sha256: "abc123"})
	if err != nil || reused {
		t.Fatalf("跨用户不应复用：err=%v reused=%v", err, reused)
	}
	if other.Id == a1.Id {
		t.Fatal("跨用户不应命中同一行")
	}
	// 跨用户不可见
	if _, err := s.GetAsset(u2, a1.Id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨用户取素材应是 ErrNotFound，实际 %v", err)
	}
}

// 稿件：版本链连续、正文同步、回滚也追加版本。
func TestStoreDraftVersionsAndRollback(t *testing.T) {
	s := openTestStore(t)
	u := testUser(t)
	defer cleanupUser(t, s, u)

	d, err := s.CreateDraft(u, "第一篇稿子", "RAG", "初稿正文", []string{"AI", "测试"})
	if err != nil {
		t.Fatal(err)
	}
	if d.CurrentVersion != 1 {
		t.Fatalf("新建后版本应为 1，实际 %d", d.CurrentVersion)
	}
	d, err = s.SaveDraftBody(u, d.Id, "第二版正文", "补充案例", u)
	if err != nil {
		t.Fatal(err)
	}
	if d.CurrentVersion != 2 || d.BodyMd != "第二版正文" {
		t.Fatalf("保存后应为 v2 且正文更新，实际 v%d %q", d.CurrentVersion, d.BodyMd)
	}
	// 非法首次跳转
	if _, err := s.SetDraftStatus(u, d.Id, DraftPublished); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("草稿直接已发布应被拒，实际 %v", err)
	}
	for _, st := range []string{DraftReview, DraftFinal} {
		if _, err := s.SetDraftStatus(u, d.Id, st); err != nil {
			t.Fatalf("跳到 %s 失败：%v", st, err)
		}
	}
	// 回滚到 v1：正文回到初稿，但版本号继续增长
	rolled, err := s.RollbackDraft(u, d.Id, 1, u)
	if err != nil {
		t.Fatal(err)
	}
	if rolled.BodyMd != "初稿正文" || rolled.CurrentVersion != 3 {
		t.Fatalf("回滚后应回到初稿且版本 3，实际 %q v%d", rolled.BodyMd, rolled.CurrentVersion)
	}
	vers, err := s.ListVersions(u, d.Id)
	if err != nil {
		t.Fatal(err)
	}
	if len(vers) != 3 {
		t.Fatalf("版本数应为 3，实际 %d", len(vers))
	}
	if vers[0].Version != 3 || vers[0].BodyMd != "初稿正文" {
		t.Fatalf("最新版本应是回滚产生的那条：%+v", vers[0])
	}
}

// 关联与删除保护：被引用就删不掉，且错误里带引用稿件 id。
func TestStoreAssetDeleteProtection(t *testing.T) {
	s := openTestStore(t)
	u := testUser(t)
	defer cleanupUser(t, s, u)

	a, _, err := s.SaveAsset(u, model.ContentAsset{Kind: AssetImage, Title: "配图", FileKey: "uploads/x.png"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDraft(u, "用到配图的稿子", "", "正文", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LinkAsset(u, d.Id, a.Id, RoleImage, "第 2 段"); err != nil {
		t.Fatal(err)
	}
	err = s.DeleteAsset(u, a.Id)
	var inUse *AssetInUseError
	if !errors.As(err, &inUse) || len(inUse.Drafts) != 1 || inUse.Drafts[0] != d.Id {
		t.Fatalf("被引用素材应拒绝删除并列出稿件，实际 %v", err)
	}
	// 只列未使用素材时，这条不应出现
	list, err := s.ListAssets(u, AssetFilter{Unused: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range list {
		if it.Id == a.Id {
			t.Fatal("Unused 过滤把被引用的素材也列出来了")
		}
	}
	if err := s.UnlinkAsset(u, d.Id, a.Id, RoleImage); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAsset(u, a.Id); err != nil {
		t.Fatalf("解除引用后应能删除：%v", err)
	}
}

// 发布台账：状态机、失败重试计数、导出用平台定制版。
func TestStorePublicationLifecycle(t *testing.T) {
	s := openTestStore(t)
	u := testUser(t)
	defer cleanupUser(t, s, u)

	d, err := s.CreateDraft(u, "待发布的稿子", "", "主正文", nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := time.Now().Add(2 * time.Hour)
	p, err := s.SavePublication(u, model.ContentPublication{
		DraftID: d.Id, Channel: "WeChat", Status: PubPlanned, PlannedAt: &plan,
		VariantTitle: "公众号标题", VariantBody: "公众号正文",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Channel != "wechat" {
		t.Fatalf("渠道应归一成小写，实际 %q", p.Channel)
	}
	p, err = s.UpdatePublication(u, p.Id, PubFailed, "", "接口 502", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != PubFailed || p.Retries != 1 || p.Error != "接口 502" {
		t.Fatalf("失败记录不对：%+v", p)
	}
	p, err = s.UpdatePublication(u, p.Id, PubPublished, "https://mp.weixin.qq.com/s/xxx", "", `{"read":10}`)
	if err != nil {
		t.Fatal(err)
	}
	if p.PublishedAt == nil || p.Url == "" {
		t.Fatalf("发布成功应记录时间与链接：%+v", p)
	}
	exp, err := s.ExportDraft(u, d.Id, "wechat")
	if err != nil {
		t.Fatal(err)
	}
	if exp.Title != "公众号标题" || exp.BodyMd != "公众号正文" {
		t.Fatalf("导出应使用平台定制版，实际 %q / %q", exp.Title, exp.BodyMd)
	}
	// 没有定制版的渠道回落到主正文
	exp2, err := s.ExportDraft(u, d.Id, "zhihu")
	if err != nil {
		t.Fatal(err)
	}
	if exp2.Title != d.Title || exp2.BodyMd != d.BodyMd {
		t.Fatalf("无定制版应回落主正文，实际 %q / %q", exp2.Title, exp2.BodyMd)
	}
}

package rag

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"blog-my/internal/model"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
)

// This opt-in check exercises the actual MySQL queries with fixed vectors. It
// does not require Ollama or claim to validate an embedding model.
func TestStoreMySQLRetrieval(t *testing.T) {
	if os.Getenv("KB_STORE_IT") != "1" {
		t.Skip("set KB_STORE_IT=1 and DB_NAME to a dedicated test database")
	}
	if os.Getenv("DB_NAME") == "" {
		t.Fatal("explicit DB_NAME for a dedicated test database is required")
	}
	_ = godotenv.Load(filepath.Join("..", "..", ".env"))
	previousDB := model.DB
	if err := model.InitDB(); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := model.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sqlDB.Close()
		model.DB = previousDB
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := NewStore(model.DB)
	userID, otherID := uuid.NewString(), uuid.NewString()
	marker := "store-check-" + uuid.NewString()
	now := time.Now().Truncate(time.Millisecond)
	makeDoc := func(owner string, stale bool, chunks int) model.KBDocument {
		t.Helper()
		doc := model.KBDocument{UserID: owner, Kind: "text", Title: marker, Source: "integration",
			Sha256: uuid.NewString(), Content: []byte("中文记忆内容"), Scope: ScopeWork,
			Meta: fmt.Sprintf(`{"entities":[%q]}`, marker), Importance: .7, Activity: .8, Emotion: .3,
			ReinforcedAt: &now, Stale: stale}
		var inputs []ChunkInput
		for i := 0; i < chunks; i++ {
			inputs = append(inputs, ChunkInput{Seq: i, Kind: "text", Text: marker + " 中文记忆", Vec: []float32{1, 2}})
		}
		if err := store.CreateDocument(ctx, &doc, inputs); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if ok, err := store.DeleteDocument(cleanupCtx, doc.Id, owner); err != nil || !ok {
				t.Errorf("clean up fixture %d: deleted=%v err=%v", doc.Id, ok, err)
			}
		})
		return doc
	}
	active := makeDoc(userID, false, 2)
	stale := makeDoc(userID, true, 1)
	other := makeDoc(otherID, false, 1)
	for _, semantic := range []bool{false, true} {
		t.Run(fmt.Sprintf("semantic=%v", semantic), func(t *testing.T) {
			search := func() []Hit {
				t.Helper()
				var hits []Hit
				var err error
				if semantic {
					hits, err = store.SearchScoped(ctx, []float32{1, 2}, marker, 100, userID, ScopeWork)
				} else {
					hits, err = store.SearchLexicalScoped(ctx, marker, 100, userID, ScopeWork)
				}
				if err != nil {
					t.Fatal(err)
				}
				return hits
			}
			seen, owned := map[uint]bool{}, 0
			for _, h := range search() {
				if seen[h.ChunkID] {
					t.Fatalf("duplicate chunk: %+v", h)
				}
				seen[h.ChunkID] = true
				if h.DocumentID == stale.Id || h.DocumentID == other.Id {
					t.Fatalf("stale or another user's document was recalled: %+v", h)
				}
				if h.DocumentID == active.Id {
					owned++
					if h.Scope != ScopeWork || h.Importance != .7 || h.Activity != .8 || h.Emotion != .3 ||
						h.ReinforcedAt == nil || !h.ReinforcedAt.Equal(now) || len(h.Entities) != 1 || h.Entities[0] != marker {
						t.Fatalf("incomplete SQL candidate metadata: %+v", h)
					}
				}
			}
			if owned != 2 {
				t.Fatalf("expected two different active chunks, got %d", owned)
			}
		})
	}
	if err := model.DB.Model(&model.KBDocument{}).Where("id = ?", active.Id).Update("scope", ScopeLife).Error; err != nil {
		t.Fatal(err)
	}
	hits, err := store.SearchLexicalScoped(ctx, marker, 100, userID, ScopeLife)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.DocumentID == active.Id && h.Scope != ScopeLife {
			t.Fatal("a later query reused stale document metadata")
		}
	}
}

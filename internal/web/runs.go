package web

import (
	"log"
	"os"
	"path/filepath"
	"sync"

	"blog-my/internal/agent/runstore"
)

var (
	runStoreOnce sync.Once
	runStore     *runstore.Store
)

func agentRunStore() *runstore.Store {
	runStoreOnce.Do(func() {
		dir := os.Getenv("AGENT_RUN_DIR")
		if dir == "" {
			base, err := os.UserCacheDir()
			if err != nil || base == "" {
				base = os.TempDir()
			}
			dir = filepath.Join(base, "blog-my", "runs")
		}
		st, err := runstore.New(dir)
		if err != nil {
			log.Printf("runstore: %v", err)
			return
		}
		runStore = st
	})
	return runStore
}

// ReconcileRuns 在启动时把上次没写完的 journal 标成 recovered 或 needs_confirm。
// 不重放工具，避免把写文件再做一遍。
func ReconcileRuns() {
	st := agentRunStore()
	if st == nil {
		return
	}
	rows, err := st.Reconcile()
	if err != nil {
		log.Printf("runstore reconcile: %v", err)
		return
	}
	if len(rows) > 0 {
		log.Printf("runstore: reconciled %d unfinished run(s) without replaying tools", len(rows))
	}
}

func startRunJournal(runID, sessionID, model string) func(string, any) {
	st := agentRunStore()
	if st == nil || runID == "" {
		return nil
	}
	if err := st.Start(runID, sessionID, map[string]any{"model": model}); err != nil {
		log.Printf("runstore start: %v", err)
		return nil
	}
	return func(kind string, payload any) {
		_ = st.Append(runID, sessionID, kind, runstore.StatusRunning, payload)
	}
}

func finishRunJournal(runID, sessionID string, runErr error) {
	st := agentRunStore()
	if st == nil || runID == "" {
		return
	}
	status := runstore.StatusDone
	payload := map[string]any{}
	if runErr != nil {
		status = runstore.StatusFailed
		payload["error"] = runErr.Error()
	}
	if err := st.Finish(runID, sessionID, status, payload); err != nil {
		log.Printf("runstore finish: %v", err)
	}
}

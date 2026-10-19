package fs

import "sync"

// ObsKind 是观察策略的三态（契约 §4.2）。**三态而非两态**：
// 「确认不存在」必须与「没见过」区分开——前者 edit 报 FS_NOT_FOUND，后者报 FS_NOT_OBSERVED。
type ObsKind uint8

const (
	// ObsUnknown 没见过：模型没读过这个目标。
	ObsUnknown ObsKind = iota
	// ObsPresent 见过且存在，Version 是当时的版本令牌。
	ObsPresent
	// ObsAbsent 确认不存在（read 失败也算，照 DSH）。
	ObsAbsent
)

// Observation 是一个目标的观察结果。
type Observation struct {
	Kind    ObsKind
	Version string // 仅 ObsPresent 有意义
}

// observationStore 是每会话一张观察表：map[SessionID]map[targetKey]Observation。
//
// 用包级表而不是工具实例字段：工具是无状态值，Exec 每次运行都是新的，
// 观察结果必须跨调用活下来（否则「读过」这件事下一轮就丢了）。
type observationStore struct {
	mu       sync.Mutex
	sessions map[string]map[string]Observation
}

var observations = &observationStore{sessions: make(map[string]map[string]Observation)}

func (s *observationStore) get(session, target string) Observation {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.sessions[session]; ok {
		return m[target]
	}
	return Observation{} // ObsUnknown
}

func (s *observationStore) set(session, target string, o Observation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.sessions[session]
	if !ok {
		m = make(map[string]Observation)
		s.sessions[session] = m
	}
	m[target] = o
}

// ForgetSession 清掉一个会话的观察表（请求收尾与测试用）。
func ForgetSession(session string) {
	observations.mu.Lock()
	defer observations.mu.Unlock()
	delete(observations.sessions, session)
}

// targetLocks 给每个目标一把锁，使 edit 的「探版本 → 校验 → 读 → 匹配 → 重写」
// 成为一个临界区（契约 §4.6）。并发两个 edit 同一文件时，后者必然看到新版本，
// 于是拿到 FS_STALE_VERSION 而不是互相覆盖。
type targetLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

var locks = &targetLocks{m: make(map[string]*sync.Mutex)}

func lockTarget(session, target string) *sync.Mutex {
	key := session + "\x00" + target
	locks.mu.Lock()
	defer locks.mu.Unlock()
	l, ok := locks.m[key]
	if !ok {
		l = &sync.Mutex{}
		locks.m[key] = l
	}
	return l
}

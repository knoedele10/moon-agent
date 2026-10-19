// Package rag 把长文切成大小合适、彼此有重叠的片段，供向量化与检索使用。
//
// 切分的动机有两层：模型上下文窗口有限，长文必须先切小；
// 但切得太生硬会把一句话拦腰截断，两边都读不懂，所以相邻块之间留一段重叠。
package rag

import (
	"strings"
	"unicode"
)

// ChunkOptions 控制切块粒度。
type ChunkOptions struct {
	MaxRunes     int // 每块最大字符数（按 rune 计，这样中文不会被切碎成乱码）
	OverlapRunes int // 相邻块之间重叠的字符数，避免一句话被切成两半后两边都读不懂
}

// DefaultChunkOptions 返回一组适合中文长文的默认值。
func DefaultChunkOptions() ChunkOptions {
	return ChunkOptions{MaxRunes: 600, OverlapRunes: 80}
}

// normalizeOptions 把越界参数收敛成能跑通的值。
// 传 0、负数或"重叠比块还长"时不报错也不 panic，而是退回默认值——
// 切块失败不该拖垮整条入库流程，能继续处理比严格校验更重要。
func normalizeOptions(opt ChunkOptions) ChunkOptions {
	def := DefaultChunkOptions()
	if opt.MaxRunes <= 0 {
		opt.MaxRunes = def.MaxRunes
	}
	if opt.OverlapRunes < 0 {
		opt.OverlapRunes = def.OverlapRunes
	}
	// 重叠必须严格小于块长，否则下一块起点落回上一块内部，切块原地打转推不进。
	if opt.OverlapRunes >= opt.MaxRunes {
		opt.OverlapRunes = def.OverlapRunes
	}
	if opt.OverlapRunes >= opt.MaxRunes {
		opt.OverlapRunes = opt.MaxRunes - 1
	}
	if opt.OverlapRunes < 0 {
		opt.OverlapRunes = 0
	}
	return opt
}

// ChunkText 把一段长文本切成若干块；输入为空或全是空白时返回 nil。
//
// 切分优先级：段落边界（空行）> 句末标点/换行 > 硬切到 MaxRunes。
// 全程按 rune 计算长度，避免把中文按字节切碎。
func ChunkText(text string, opt ChunkOptions) []string {
	opt = normalizeOptions(opt)

	runes := []rune(strings.TrimSpace(text))
	n := len(runes)
	if n == 0 {
		return nil
	}

	paraCut, sentCut := cutPositions(runes)

	var chunks []string
	pos := 0
	for pos < n {
		end := pos + opt.MaxRunes
		if end >= n {
			if c := strings.TrimSpace(string(runes[pos:])); c != "" {
				chunks = append(chunks, c)
			}
			break
		}

		cut := lastCut(paraCut, pos, end)
		if cut < 0 {
			cut = lastCut(sentCut, pos, end)
		}
		if cut < 0 {
			cut = end // 找不到任何标点就硬切；单个超长"句子"也只在这里被截断
		}

		start, stop := trimBounds(runes, pos, cut)
		if start >= stop {
			pos = cut // 整块都是空白，跳过但继续推进，别死循环
			continue
		}
		chunks = append(chunks, string(runes[start:stop]))

		// 下一块从本块真实结尾往前退 OverlapRunes 个字符，拼接处正好重复一段。
		next := stop - opt.OverlapRunes
		if next <= start {
			next = start + 1 // 块本身比重叠还短，退不满，至少保证前进
		}
		// 重叠起点若落在空白上就往前挪，免得下一块被 TrimSpace 削掉前缀。
		for next < stop && unicode.IsSpace(runes[next]) {
			next++
		}
		pos = next
	}
	return chunks
}

// cutPositions 预先标出每个下标处能否断开：
//
//	paraCut[i] 为真表示 i 之前刚结束一个空行（\n\s*\n），适合当段落边界；
//	sentCut[i] 为真表示 i-1 是句末标点或换行，适合当句子边界。
//
// 先扫一遍而不是每次切块都回头找，是因为长文上反复扫描很浪费。
func cutPositions(runes []rune) (paraCut, sentCut []bool) {
	n := len(runes)
	paraCut = make([]bool, n+1)
	sentCut = make([]bool, n+1)
	pendingNL := false // 上一个非空白字符是不是换行
	for i, r := range runes {
		switch {
		case r == '\n':
			if pendingNL {
				paraCut[i+1] = true // 空行到这里结束，可在此断开
			}
			pendingNL = true
		case unicode.IsSpace(r):
			// 单个空白不改变 pendingNL，等下一个换行确认是否真的构成空行
		default:
			pendingNL = false
		}
		if isSentenceEnd(r) {
			sentCut[i+1] = true
		}
	}
	return paraCut, sentCut
}

// isSentenceEnd 判断一个字符能否当作句子收尾。
func isSentenceEnd(r rune) bool {
	switch r {
	case '。', '！', '？', '；', '!', '?', ';', '.', '\n':
		return true
	}
	return false
}

// lastCut 在 (from, to] 区间里找最靠右的切点，找不到返回 -1，由调用方回退到硬切。
func lastCut(cuts []bool, from, to int) int {
	for i := to; i > from; i-- {
		if cuts[i] {
			return i
		}
	}
	return -1
}

// trimBounds 收窄 [from, to) 两端的空白，返回真正的正文范围。
func trimBounds(runes []rune, from, to int) (int, int) {
	for from < to && unicode.IsSpace(runes[from]) {
		from++
	}
	for to > from && unicode.IsSpace(runes[to-1]) {
		to--
	}
	return from, to
}

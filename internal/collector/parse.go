package collector

import (
	"fmt"

	"github.com/miekg/dns"

	"zonestation/internal/zone"
)

// Format 是传送流的实际格式。
type Format int

const (
	// FormatFull 全量格式：SOA …记录… SOA。
	FormatFull Format = iota
	// FormatDelta 增量格式：新 SOA，随后每段为 旧 SOA、删除项、新 SOA、增加项。
	FormatDelta
	// FormatUpToDate 仅一条 SOA：区已是最新。
	FormatUpToDate
)

// Delta 是一个解析出的增量修订段。
type Delta struct {
	OldSOA *dns.SOA
	NewSOA *dns.SOA
	Del    []dns.RR
	Add    []dns.RR
}

// Parsed 是传送流的解析结果。
type Parsed struct {
	Format    Format
	NewSerial uint32
	Segments  []SegmentInfo
	Deltas    []Delta
	Full      []dns.RR // FormatFull 时的全部区内记录（含首条 SOA，不含末尾 SOA）
}

// ParseStream 校验并解析一条完整的传送流。
//
// 校验内容：首条必须是本区 SOA（区归属）；全量格式必须以相同序列号的
// SOA 收尾且中间不得出现 SOA；增量格式必须能划分为若干
// “旧 SOA、删除项、新 SOA、增加项”段，段间序列号衔接且按 RFC 1982
// 递增，末段新 SOA 必须回到响应首条 SOA 宣告的序列号。
func ParseStream(origin string, mode Mode, rrs []dns.RR) (*Parsed, error) {
	if len(rrs) == 0 {
		return nil, fmt.Errorf("服务器未返回任何记录")
	}
	first, ok := rrs[0].(*dns.SOA)
	if !ok {
		return nil, fmt.Errorf("首条记录不是 SOA（区归属校验失败）：%s", rrs[0].String())
	}
	if zone.Canonical(first.Header().Name) != origin {
		return nil, fmt.Errorf("首条 SOA 属于 %s，与请求的区 %s 不符（区归属校验失败）",
			first.Header().Name, origin)
	}

	if len(rrs) == 1 {
		if mode == ModeIXFR {
			return &Parsed{Format: FormatUpToDate, NewSerial: first.Serial}, nil
		}
		return nil, fmt.Errorf("全量传送只有一条 SOA，缺少区内容与结束 SOA")
	}

	if mode == ModeIXFR {
		if _, isSOA := rrs[1].(*dns.SOA); isSOA {
			p, complete, err := scanDelta(origin, rrs)
			if err != nil {
				return nil, err
			}
			if !complete {
				return nil, fmt.Errorf("增量流不完整：修订段未到达响应宣告的序列号（缺失结束段）")
			}
			return p, nil
		}
		// 第二条不是 SOA：服务器只肯给全量，按全量格式解析。
	}
	return parseFull(origin, rrs)
}

// parseFull 解析全量格式（AXFR，或 IXFR 的全量回退）。
func parseFull(origin string, rrs []dns.RR) (*Parsed, error) {
	first := rrs[0].(*dns.SOA)
	last, ok := rrs[len(rrs)-1].(*dns.SOA)
	if !ok {
		return nil, fmt.Errorf("全量传送缺少结束 SOA（最后一条是 %s）", rrs[len(rrs)-1].Header().String())
	}
	if zone.Canonical(last.Header().Name) != origin {
		return nil, fmt.Errorf("结束 SOA 属于 %s，与本区 %s 不符", last.Header().Name, origin)
	}
	if last.Serial != first.Serial {
		return nil, fmt.Errorf("结束 SOA 序列号 %d 与起始 SOA 序列号 %d 不一致", last.Serial, first.Serial)
	}
	for i := 1; i < len(rrs)-1; i++ {
		if _, isSOA := rrs[i].(*dns.SOA); isSOA {
			return nil, fmt.Errorf("全量传送中间出现多余 SOA（第 %d 条）", i+1)
		}
	}
	return &Parsed{
		Format:    FormatFull,
		NewSerial: first.Serial,
		Full:      rrs[:len(rrs)-1],
	}, nil
}

// scanDelta 扫描增量格式：新 SOA 在前，随后每段为 旧 SOA、删除项、新 SOA、增加项。
//
// 返回值的语义：
//   - err != nil：流存在确定的结构问题（段界错位、区归属不符、序列号
//     非递增或不确定等），接收方应立即停止并按错误处理；
//   - complete == false（且 err == nil）：流可能只是还没收完，
//     接收方应继续读取；
//   - complete == true：修订段链已到达响应首条 SOA 宣告的序列号。
func scanDelta(origin string, rrs []dns.RR) (*Parsed, bool, error) {
	announced := rrs[0].(*dns.SOA).Serial
	p := &Parsed{Format: FormatDelta, NewSerial: announced}

	i := 1
	for i < len(rrs) {
		oldSOA, ok := rrs[i].(*dns.SOA)
		if !ok {
			return nil, false, fmt.Errorf("增量段 %d 应以旧 SOA 开始，实际收到 %s", len(p.Deltas)+1, rrs[i].Header().String())
		}
		if zone.Canonical(oldSOA.Header().Name) != origin {
			return nil, false, fmt.Errorf("增量段 %d 的旧 SOA 属于 %s，与本区不符", len(p.Deltas)+1, oldSOA.Header().Name)
		}
		i++
		var del []dns.RR
		for i < len(rrs) {
			if _, isSOA := rrs[i].(*dns.SOA); isSOA {
				break
			}
			del = append(del, rrs[i])
			i++
		}
		if i >= len(rrs) {
			// 删除项之后还没有新 SOA：可能只是还没收完。
			return p, false, nil
		}
		newSOA := rrs[i].(*dns.SOA)
		if zone.Canonical(newSOA.Header().Name) != origin {
			return nil, false, fmt.Errorf("增量段 %d 的新 SOA 属于 %s，与本区不符", len(p.Deltas)+1, newSOA.Header().Name)
		}
		i++
		var add []dns.RR
		for i < len(rrs) {
			if _, isSOA := rrs[i].(*dns.SOA); isSOA {
				break
			}
			add = append(add, rrs[i])
			i++
		}

		// 段内序列号必须按 RFC 1982 严格递增；不确定时停止并说明。
		rel := zone.CompareSerial(oldSOA.Serial, newSOA.Serial)
		switch rel {
		case zone.SerialUncertain:
			return nil, false, fmt.Errorf("增量段 %d 的序列号 %d → %d 差值恰为 2^31，按 RFC 1982 无法确定新旧，已停止切换",
				len(p.Deltas)+1, oldSOA.Serial, newSOA.Serial)
		case zone.SerialEqual, zone.SerialGreater:
			return nil, false, fmt.Errorf("增量段 %d 的序列号 %d → %d 不是严格递增（按 RFC 1982 比较）",
				len(p.Deltas)+1, oldSOA.Serial, newSOA.Serial)
		}
		// 段间必须衔接。
		if n := len(p.Deltas); n > 0 && p.Deltas[n-1].NewSOA.Serial != oldSOA.Serial {
			return nil, false, fmt.Errorf("增量段 %d 的起点序列号 %d 与上一段终点 %d 不衔接",
				len(p.Deltas)+1, oldSOA.Serial, p.Deltas[n-1].NewSOA.Serial)
		}

		p.Deltas = append(p.Deltas, Delta{OldSOA: oldSOA, NewSOA: newSOA, Del: del, Add: add})
		p.Segments = append(p.Segments, SegmentInfo{
			Index:     len(p.Deltas),
			OldSerial: oldSOA.Serial,
			NewSerial: newSOA.Serial,
			Deleted:   len(del),
			Added:     len(add),
		})

		// 末段新 SOA 回到宣告的序列号：链已完整。
		if newSOA.Serial == announced {
			return p, true, nil
		}
	}

	if len(p.Deltas) == 0 {
		return nil, false, fmt.Errorf("增量响应中没有任何修订段")
	}
	// 记录读完了但链还没到达宣告的序列号：可能还没收完。
	return p, false, nil
}

// announcedSerial 从收到的记录里取出首条 SOA 宣告的序列号。
func announcedSerial(rrs []dns.RR) (uint32, bool) {
	if len(rrs) == 0 {
		return 0, false
	}
	if soa, ok := rrs[0].(*dns.SOA); ok {
		return soa.Serial, true
	}
	return 0, false
}

// streamLooksComplete 在接收过程中判断流是否已完整。
// 测试服务器把每次传送放在单个报文中发送，因此按内容即可判定。
func streamLooksComplete(origin string, mode Mode, rrs []dns.RR) bool {
	if len(rrs) == 0 {
		return false
	}
	first, ok := rrs[0].(*dns.SOA)
	if !ok || zone.Canonical(first.Header().Name) != origin {
		return true // 明显非法，交给解析器报错
	}
	if len(rrs) == 1 {
		return mode == ModeIXFR // 单条 SOA：区已是最新
	}
	if mode == ModeIXFR {
		if _, isSOA := rrs[1].(*dns.SOA); isSOA {
			return deltaComplete(origin, rrs)
		}
	}
	// 全量格式：最后一条是与首条序列号相同的本区 SOA。
	last, ok := rrs[len(rrs)-1].(*dns.SOA)
	return ok && zone.Canonical(last.Header().Name) == origin && last.Serial == first.Serial
}

// deltaComplete 判断增量流是否可以停止接收：
// 修订段链到达宣告序列号（完整），或已能确定存在结构错误（交解析器报错）。
func deltaComplete(origin string, rrs []dns.RR) bool {
	_, complete, err := scanDelta(origin, rrs)
	return complete || err != nil
}

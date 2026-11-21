// Package collector 是区数据采集引擎：向权威服务器发起全量（AXFR）
// 或增量（IXFR）传送，接收过程中校验首尾 SOA、区归属与记录集一致性，
// 按 RFC 1982 的 32 位规则比较序列号，并把增量流理解为
// “旧 SOA、删除项、新 SOA、增加项”的若干修订段。
package collector

import (
	"fmt"
	"time"

	"github.com/miekg/dns"

	"zonestation/internal/zone"
)

// Mode 是传送方式。
type Mode string

const (
	// ModeAXFR 全量传送。
	ModeAXFR Mode = "axfr"
	// ModeIXFR 增量传送。
	ModeIXFR Mode = "ixfr"
)

// SegmentInfo 描述一个解析出的增量修订段。
type SegmentInfo struct {
	Index     int    `json:"index"`
	OldSerial uint32 `json:"oldSerial"`
	NewSerial uint32 `json:"newSerial"`
	Deleted   int    `json:"deleted"`
	Added     int    `json:"added"`
}

// Result 是一次传送尝试的完整记录（成功或失败都会保存）。
type Result struct {
	ID         int64     `json:"id"`
	Zone       string    `json:"zone"`
	Mode       Mode      `json:"mode"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`

	Success        bool `json:"success"`
	UpToDate       bool `json:"upToDate"`       // 服务器告知区已是最新
	FellBackToFull bool `json:"fellBackToFull"` // 请求增量但服务器只肯给全量
	HadLocalCopy   bool `json:"hadLocalCopy"`

	OldSerial      uint32 `json:"oldSerial"`
	NewSerial      uint32 `json:"newSerial"`
	SerialRelation string `json:"serialRelation"` // less/equal/greater/uncertain/unknown

	Segments   []SegmentInfo `json:"segments"`
	Rejections []string      `json:"rejections"` // 拒收原因
	Warnings   []string      `json:"warnings"`

	ReceivedCount int      `json:"receivedCount"`
	Raw           []string `json:"raw"` // 实际收到的记录文本，供失败时检查

	Diff *zone.Diff `json:"-"` // 传送前后对照（成功且有旧副本时）
}

func (r *Result) reject(format string, args ...any) {
	r.Rejections = append(r.Rejections, fmt.Sprintf(format, args...))
}

func (r *Result) warn(format string, args ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

// Service 执行传送。它无状态，结果由调用方（采集站存储层）保存。
type Service struct {
	Addr    string        // 权威服务器地址 host:port
	Timeout time.Duration // 单次读超时；为零时取 5 秒
}

// Execute 执行一次传送。
//
// local 为本地当前区副本（可为 nil）。返回的新区快照仅在成功且
// 确有更新时非 nil；任何校验失败都不会产生新快照，旧区保持不变。
func (s *Service) Execute(id int64, zoneName string, mode Mode, local *zone.Zone) (*zone.Zone, *Result) {
	origin := zone.Canonical(zoneName)
	res := &Result{
		ID:           id,
		Zone:         origin,
		Mode:         mode,
		StartedAt:    time.Now(),
		HadLocalCopy: local != nil,
	}
	if local != nil {
		res.OldSerial = local.Serial
	}
	defer func() { res.FinishedAt = time.Now() }()

	if mode == ModeIXFR && local == nil {
		res.reject("本地没有该区副本，无法发起增量传送，请先做一次全量传送")
		return nil, res
	}

	var localSerial uint32
	if local != nil {
		localSerial = local.Serial
	}
	rrs, fatal := s.receive(origin, mode, localSerial)
	res.ReceivedCount = len(rrs)
	for _, rr := range rrs {
		res.Raw = append(res.Raw, rr.String())
	}
	if fatal != "" {
		res.reject("%s", fatal)
		return nil, res
	}

	// 先记录响应宣告的序列号与本地序列号的关系（即使后续解析失败，
	// 网页也能看到“不确定”等关键信息）。
	if local != nil {
		if announced, ok := announcedSerial(rrs); ok {
			res.NewSerial = announced
			res.SerialRelation = zone.CompareSerial(local.Serial, announced).String()
		}
	}

	parsed, perr := ParseStream(origin, mode, rrs)
	if perr != nil {
		res.reject("%s", perr.Error())
		return nil, res
	}
	res.NewSerial = parsed.NewSerial
	res.Segments = parsed.Segments
	if parsed.Format == FormatUpToDate {
		res.UpToDate = true
		res.Success = true
		res.SerialRelation = zone.SerialEqual.String()
		res.NewSerial = localSerial
		return nil, res
	}
	if mode == ModeIXFR && parsed.Format == FormatFull {
		// 服务器只肯给全量：识别出来并按全量处理。
		res.FellBackToFull = true
		res.warn("请求的是增量传送，但服务器只提供全量（AXFR 回退），已按全量处理")
	}

	// 序列号闸门：按 RFC 1982 的 32 位规则比较，不确定时停止切换。
	if local != nil {
		rel := zone.CompareSerial(local.Serial, parsed.NewSerial)
		res.SerialRelation = rel.String()
		switch rel {
		case zone.SerialUncertain:
			res.reject("序列号校验未通过：本地 %d 与新区 %d 的差值恰为 2^31，按 RFC 1982 无法确定新旧，已停止切换（旧区保持不变）",
				local.Serial, parsed.NewSerial)
			return nil, res
		case zone.SerialGreater:
			if mode == ModeIXFR {
				res.reject("序列号校验未通过：服务器给出的序列号 %d 按 RFC 1982 比本地 %d 更旧，拒绝增量切换",
					parsed.NewSerial, local.Serial)
				return nil, res
			}
			res.warn("新区序列号 %d 按 RFC 1982 比本地 %d 更旧（可能是管理员重置），仍按全量替换", parsed.NewSerial, local.Serial)
		case zone.SerialEqual:
			res.warn("新区序列号与本地相同（%d），内容按全量结果替换", parsed.NewSerial)
		}
	} else {
		res.SerialRelation = "unknown"
	}

	// 依据传送格式构造新区。
	var newZone *zone.Zone
	var errs []string
	if parsed.Format == FormatFull {
		newZone, errs = BuildFromFull(origin, parsed.Full, id)
	} else {
		if local == nil {
			res.reject("服务器给出了增量段，但本地没有旧区副本可应用")
			return nil, res
		}
		if len(parsed.Segments) > 0 && parsed.Segments[0].OldSerial != local.Serial {
			res.reject("增量段起点序列号 %d 与本地副本 %d 不一致，无法衔接（请改用全量传送）",
				parsed.Segments[0].OldSerial, local.Serial)
			return nil, res
		}
		var warns []string
		newZone, warns, errs = ApplyDelta(local, parsed.Deltas, id)
		res.Warnings = append(res.Warnings, warns...)
	}
	for _, e := range errs {
		res.reject("%s", e)
	}
	if len(res.Rejections) > 0 {
		return nil, res
	}

	// 接收后整体校验：首尾 SOA、区归属、记录集一致性。
	vErrs, vWarns := ValidateZone(newZone, parsed.NewSerial)
	res.Warnings = append(res.Warnings, vWarns...)
	for _, e := range vErrs {
		res.reject("%s", e)
	}
	if len(res.Rejections) > 0 {
		return nil, res
	}

	res.Success = true
	if local != nil {
		res.Diff = zone.DiffZones(local, newZone)
	}
	return newZone, res
}

// receive 发起传送并读取全部记录，直到流完整或连接中断。
// 返回已收到的记录（即使不完整，也供失败检查）与致命错误描述。
func (s *Service) receive(origin string, mode Mode, localSerial uint32) ([]dns.RR, string) {
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	conn, err := dns.DialTimeout("tcp", s.Addr, timeout)
	if err != nil {
		return nil, fmt.Sprintf("无法连接权威服务器 %s：%v", s.Addr, err)
	}
	defer conn.Close()

	q := new(dns.Msg)
	if mode == ModeIXFR {
		q.SetIxfr(origin, localSerial, "ns."+origin, "admin."+origin)
	} else {
		q.SetAxfr(origin)
	}
	if err := conn.WriteMsg(q); err != nil {
		return nil, fmt.Sprintf("发送传送请求失败：%v", err)
	}

	var rrs []dns.RR
	for {
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
		in, err := conn.ReadMsg()
		if err != nil {
			if len(rrs) > 0 {
				return rrs, fmt.Sprintf("传送未完整结束：收到 %d 条记录后连接中断（缺少结束段，未收到收尾 SOA）：%v", len(rrs), err)
			}
			return rrs, fmt.Sprintf("读取传送数据失败：%v", err)
		}
		if in.Id != q.Id {
			return rrs, fmt.Sprintf("响应报文 ID 与请求不匹配")
		}
		if in.Rcode != dns.RcodeSuccess {
			return rrs, fmt.Sprintf("服务器拒绝传送：RCODE=%s", dns.RcodeToString[in.Rcode])
		}
		rrs = append(rrs, in.Answer...)
		if streamLooksComplete(origin, mode, rrs) {
			return rrs, ""
		}
	}
}

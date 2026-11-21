// Package testdns 实现随项目启动的本地权威测试 DNS 服务。
//
// 它提供若干多版本测试区，支持 AXFR/IXFR（含跨多次修订的增量段），
// 并可通过行为开关制造各种异常：序列号回绕、缺失结束段、
// 重复记录、拒绝传送，以及差值恰为 2^31 的临界序列号。
// 普通查询始终用当前版本应答（旧区在采集站一侧也一直可查询）。
package testdns

import (
	"fmt"
	"net"
	"sync"

	"github.com/miekg/dns"

	"zonestation/internal/zone"
)

// Behaviors 是测试服务器的行为开关。
type Behaviors struct {
	// WrapSerial：wrap.test. 的序列号按 32 位回绕递增。
	WrapSerial bool `json:"wrapSerial"`
	// MissingEnd：传送响应缺少结束段（全量少末尾 SOA，增量少末段新 SOA 及增加项），随后直接断开连接。
	MissingEnd bool `json:"missingEnd"`
	// DuplicateRecords：传送响应中夹带重复记录。
	DuplicateRecords bool `json:"duplicateRecords"`
	// RefuseTransfer：对 AXFR/IXFR 请求直接回 REFUSED。
	RefuseTransfer bool `json:"refuseTransfer"`
	// AmbiguousSerial：下一次推进版本时序列号跳变恰好 2^31（一次性），触发“不确定”比较。
	AmbiguousSerial bool `json:"ambiguousSerial"`
}

// Version 是区的一个版本。
type Version struct {
	Serial uint32
	RRs    []dns.RR // SOA 在首位
}

// Zone 是服务器持有的一个测试区。
type Zone struct {
	Name        string
	KeepHistory bool
	Versions    []*Version
	Cur         int
}

// ZoneInfo 是区的对外状态视图。
type ZoneInfo struct {
	Name        string `json:"name"`
	Serial      uint32 `json:"serial"`
	Cur         int    `json:"cur"`
	Count       int    `json:"count"`
	KeepHistory bool   `json:"keepHistory"`
	CanBump     bool   `json:"canBump"`
}

// Server 是本地权威测试 DNS 服务器。
type Server struct {
	mu    sync.Mutex
	beh   Behaviors
	zones []*Zone
	zmap  map[string]*Zone
	addr  string
	tcp   *dns.Server
	udp   *dns.Server
}

// New 创建并启动测试服务器，同时监听 TCP 与 UDP。
// addr 形如 "127.0.0.1:5353"；端口为 0 时自动选择空闲端口。
func New(addr string) (*Server, error) {
	s := &Server{zmap: map[string]*Zone{}}
	for _, def := range zoneDefs {
		z := &Zone{Name: dns.CanonicalName(def.name), KeepHistory: def.keepHistory}
		for _, text := range def.versions {
			rrs, err := parseVersion(def.name, text)
			if err != nil {
				return nil, err
			}
			z.Versions = append(z.Versions, &Version{RRs: rrs})
		}
		s.zones = append(s.zones, z)
		s.zmap[z.Name] = z
	}
	s.assignSerials()

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if host == "" {
		host = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("DNS TCP 监听失败: %w", err)
	}
	realPort := ln.Addr().(*net.TCPAddr).Port
	pc, err := net.ListenPacket("udp", net.JoinHostPort(host, fmt.Sprint(realPort)))
	if err != nil {
		ln.Close()
		return nil, fmt.Errorf("DNS UDP 监听失败: %w", err)
	}
	s.addr = ln.Addr().String()
	s.tcp = &dns.Server{Listener: ln, Handler: s}
	s.udp = &dns.Server{PacketConn: pc, Handler: s}
	go func() { _ = s.tcp.ActivateAndServe() }()
	go func() { _ = s.udp.ActivateAndServe() }()
	return s, nil
}

// Addr 返回服务器实际监听地址。
func (s *Server) Addr() string { return s.addr }

// Close 停止服务器。
func (s *Server) Close() {
	if s.tcp != nil {
		_ = s.tcp.Shutdown()
	}
	if s.udp != nil {
		_ = s.udp.Shutdown()
	}
}

// assignSerials 按当前行为开关为每个版本分配序列号（调用时需持锁或处于初始化阶段）。
func (s *Server) assignSerials() {
	for _, z := range s.zones {
		base, step := baseSerial(z.Name), uint32(1)
		if z.Name == "wrap.test." && s.beh.WrapSerial {
			base, step = wrapSerialBase, wrapSerialStep
		}
		for i, v := range z.Versions {
			v.Serial = base + uint32(i)*step // 无符号加法天然按 2^32 回绕
			if soa, ok := v.RRs[0].(*dns.SOA); ok {
				soa.Serial = v.Serial
			}
		}
	}
}

func baseSerial(zoneName string) uint32 {
	for _, def := range zoneDefs {
		if dns.CanonicalName(def.name) == zoneName {
			return def.normalBase
		}
	}
	return 1
}

// Behaviors 返回当前行为开关。
func (s *Server) Behaviors() Behaviors {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.beh
}

// SetBehaviors 更新行为开关。切换“序列号回绕”会重置 wrap.test. 到初始版本。
func (s *Server) SetBehaviors(b Behaviors) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.WrapSerial != s.beh.WrapSerial {
		for _, z := range s.zones {
			if z.Name == "wrap.test." {
				z.Cur = 0
			}
		}
	}
	s.beh = b
	s.assignSerials()
}

// Bump 把指定区推进到下一个版本，返回新区序列号与版本进度。
// 若开启了 AmbiguousSerial，本次推进的序列号跳变恰好 2^31（一次性）。
func (s *Server) Bump(zoneName string) (ZoneInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	z := s.zmap[dns.CanonicalName(zoneName)]
	if z == nil {
		return ZoneInfo{}, fmt.Errorf("未知测试区 %s", zoneName)
	}
	if z.Cur >= len(z.Versions)-1 {
		return s.infoLocked(z), fmt.Errorf("区 %s 已是最新版本", zoneName)
	}
	z.Cur++
	if s.beh.AmbiguousSerial {
		v := z.Versions[z.Cur]
		v.Serial = z.Versions[z.Cur-1].Serial + zone.SerialHalf // 恰为 2^31 的差值
		if soa, ok := v.RRs[0].(*dns.SOA); ok {
			soa.Serial = v.Serial
		}
		s.beh.AmbiguousSerial = false
	}
	return s.infoLocked(z), nil
}

// ZoneInfos 返回所有测试区的状态。
func (s *Server) ZoneInfos() []ZoneInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ZoneInfo, 0, len(s.zones))
	for _, z := range s.zones {
		out = append(out, s.infoLocked(z))
	}
	return out
}

func (s *Server) infoLocked(z *Zone) ZoneInfo {
	return ZoneInfo{
		Name:        z.Name,
		Serial:      z.Versions[z.Cur].Serial,
		Cur:         z.Cur,
		Count:       len(z.Versions),
		KeepHistory: z.KeepHistory,
		CanBump:     z.Cur < len(z.Versions)-1,
	}
}

// ServeDNS 实现 dns.Handler。
func (s *Server) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	if len(r.Question) == 0 {
		m := new(dns.Msg)
		m.SetRcode(r, dns.RcodeFormatError)
		_ = w.WriteMsg(m)
		return
	}
	q := r.Question[0]
	switch q.Qtype {
	case dns.TypeAXFR, dns.TypeIXFR:
		s.handleTransfer(w, r, q)
	default:
		s.handleQuery(w, r, q)
	}
}

// handleTransfer 处理 AXFR/IXFR 请求，按行为开关可能拒绝、截断或夹带重复记录。
func (s *Server) handleTransfer(w dns.ResponseWriter, r *dns.Msg, q dns.Question) {
	s.mu.Lock()
	beh := s.beh
	z := s.zmap[dns.CanonicalName(q.Name)]
	s.mu.Unlock()

	reply := func(rcode int) {
		m := new(dns.Msg)
		m.SetRcode(r, rcode)
		m.Authoritative = true
		_ = w.WriteMsg(m)
	}

	if z == nil {
		reply(dns.RcodeNotAuth)
		return
	}
	if beh.RefuseTransfer {
		reply(dns.RcodeRefused)
		return
	}
	if w.RemoteAddr() != nil && w.RemoteAddr().Network() == "udp" {
		// 区传送必须走 TCP。
		reply(dns.RcodeRefused)
		return
	}

	var stream []dns.RR
	deltaMode := false
	if q.Qtype == dns.TypeIXFR {
		var clientSerial uint32
		ok := false
		if len(r.Ns) > 0 {
			if soa, isSOA := r.Ns[0].(*dns.SOA); isSOA {
				clientSerial, ok = soa.Serial, true
			}
		}
		if !ok {
			reply(dns.RcodeFormatError)
			return
		}
		cur := z.Versions[z.Cur]
		switch {
		case clientSerial == cur.Serial:
			// 已是最新：单条 SOA 响应。
			stream = []dns.RR{cur.RRs[0]}
		case !z.KeepHistory:
			// 服务器不保留历史：只肯给全量。
			stream = fullStream(cur)
		default:
			fromIdx := -1
			for i, v := range z.Versions {
				if v.Serial == clientSerial {
					fromIdx = i
					break
				}
			}
			if fromIdx < 0 || fromIdx >= z.Cur {
				stream = fullStream(cur)
			} else {
				stream = deltaStream(z, fromIdx)
				deltaMode = true
			}
		}
	} else {
		stream = fullStream(z.Versions[z.Cur])
	}

	// 夹带重复记录。
	if beh.DuplicateRecords {
		if at := dupInjectionPoint(stream, deltaMode); at >= 0 {
			stream = append(stream[:at:at], append([]dns.RR{stream[at]}, stream[at:]...)...)
		}
	}

	// 缺失结束段：截断流，随后直接断开连接。
	truncated := false
	if beh.MissingEnd {
		if deltaMode {
			// 去掉末段的新 SOA 与增加项：流停在最末删除项之后。
			cut := len(stream)
			soaSeen := 0
			for i := len(stream) - 1; i >= 0; i-- {
				if _, ok := stream[i].(*dns.SOA); ok {
					soaSeen++
					if soaSeen == 2 { // 末段新 SOA 之前的位置
						cut = i + 1
						break
					}
				}
			}
			if cut < len(stream) {
				stream = stream[:cut]
				truncated = true
			}
		} else if len(stream) > 1 {
			// 全量：去掉末尾 SOA。
			stream = stream[:len(stream)-1]
			truncated = true
		}
	}

	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	m.Answer = stream
	_ = w.WriteMsg(m)
	if truncated {
		// 模拟服务器异常：不传完就断开 TCP 连接。
		_ = w.Close()
	}
}

// dupInjectionPoint 选择重复记录的注入位置：
// 全量格式取中间一条普通记录；增量格式取末段增加项中的最后一条
// （避免复制 SOA 段界，否则会被解析成结构错误而非重复记录）。
func dupInjectionPoint(stream []dns.RR, deltaMode bool) int {
	if deltaMode {
		for i := len(stream) - 1; i > 0; i-- {
			if _, isSOA := stream[i].(*dns.SOA); !isSOA {
				return i
			}
		}
		return -1
	}
	if len(stream) > 2 {
		return len(stream) / 2
	}
	return -1
}

// fullStream 构造全量传送流：SOA …记录… SOA。
func fullStream(v *Version) []dns.RR {
	out := make([]dns.RR, 0, len(v.RRs)+1)
	out = append(out, v.RRs...)
	out = append(out, v.RRs[0]) // 末尾 SOA
	return out
}

// deltaStream 构造从 fromIdx 版本到当前版本的增量传送流：
// 新 SOA 在前，随后每段为 旧 SOA、删除项、新 SOA、增加项，可跨多次修订。
func deltaStream(z *Zone, fromIdx int) []dns.RR {
	cur := z.Versions[z.Cur]
	out := []dns.RR{cur.RRs[0]}
	for k := fromIdx; k < z.Cur; k++ {
		oldV, newV := z.Versions[k], z.Versions[k+1]
		del, add := computeDelta(oldV.RRs, newV.RRs)
		out = append(out, oldV.RRs[0])
		out = append(out, del...)
		out = append(out, newV.RRs[0])
		out = append(out, add...)
	}
	return out
}

// computeDelta 计算两个版本间的记录级差异（SOA 除外，它作为段界）。
// 以整条记录（含 TTL）比较，因此 TTL 改动自然表现为 删旧+增新。
func computeDelta(oldRRs, newRRs []dns.RR) (del, add []dns.RR) {
	id := func(rr dns.RR) string { return rr.String() }
	oldSet := map[string]bool{}
	for _, rr := range oldRRs {
		if _, ok := rr.(*dns.SOA); ok {
			continue
		}
		oldSet[id(rr)] = true
	}
	newSet := map[string]bool{}
	for _, rr := range newRRs {
		if _, ok := rr.(*dns.SOA); ok {
			continue
		}
		newSet[id(rr)] = true
	}
	for _, rr := range oldRRs {
		if _, ok := rr.(*dns.SOA); ok {
			continue
		}
		if !newSet[id(rr)] {
			del = append(del, rr)
		}
	}
	for _, rr := range newRRs {
		if _, ok := rr.(*dns.SOA); ok {
			continue
		}
		if !oldSet[id(rr)] {
			add = append(add, rr)
		}
	}
	return del, add
}

// handleQuery 处理普通查询，始终用当前版本应答（不受行为开关影响）。
func (s *Server) handleQuery(w dns.ResponseWriter, r *dns.Msg, q dns.Question) {
	s.mu.Lock()
	var z *Zone
	for _, cand := range s.zones {
		if dns.IsSubDomain(cand.Name, q.Name) && (z == nil || len(cand.Name) > len(z.Name)) {
			z = cand
		}
	}
	s.mu.Unlock()

	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	m.RecursionAvailable = false

	if z == nil {
		m.Rcode = dns.RcodeRefused
		_ = w.WriteMsg(m)
		return
	}

	cur := z.Versions[z.Cur]
	qname := dns.CanonicalName(q.Name)
	var matched []dns.RR
	nameExists := false
	for _, rr := range cur.RRs {
		if dns.CanonicalName(rr.Header().Name) == qname {
			nameExists = true
			if q.Qtype == dns.TypeANY || rr.Header().Rrtype == q.Qtype {
				matched = append(matched, rr)
			}
		}
	}
	switch {
	case len(matched) > 0:
		m.Answer = matched
	case nameExists:
		// NODATA：名称存在但没有该类型。
		m.Ns = []dns.RR{cur.RRs[0]}
	default:
		m.Rcode = dns.RcodeNameError
		m.Ns = []dns.RR{cur.RRs[0]}
	}
	_ = w.WriteMsg(m)
}

// ZoneNames 返回全部测试区名称（便于日志与自检）。
func (s *Server) ZoneNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.zones))
	for _, z := range s.zones {
		out = append(out, z.Name)
	}
	return out
}

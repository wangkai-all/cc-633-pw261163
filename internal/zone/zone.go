// Package zone 定义区数据模型：记录、记录集、区快照，
// 以及记录集识别、规范化排序等基础操作。
//
// 关键约定：记录集（RRset）以 （名称， 类别， 类型） 三元组识别，
// TTL 与 RDATA 属于记录本身。因此单条记录的 TTL 改动只是
// 记录集内部的变化，绝不能被理解为“删除整个记录集”。
package zone

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/miekg/dns"
)

// Source 记录一条资源记录的来源（哪一次传送把它带进了当前区）。
type Source struct {
	TransferID int64  `json:"transferId"`
	Kind       string `json:"kind"` // "full"（全量）或 "delta"（增量段）
	Segment    int    `json:"segment,omitempty"`
}

// Label 返回来源的中文短描述。
func (s Source) Label() string {
	if s.TransferID == 0 {
		return "未知来源"
	}
	if s.Kind == "delta" && s.Segment > 0 {
		return fmt.Sprintf("传送 #%d · 增量段 %d", s.TransferID, s.Segment)
	}
	return fmt.Sprintf("传送 #%d · 全量", s.TransferID)
}

// Record 是一条资源记录（RR）。记录集身份不含 TTL 与 RDATA。
type Record struct {
	Name   string `json:"name"` // 所有者名称，FQDN 规范小写形式
	Class  uint16 `json:"-"`
	Type   uint16 `json:"-"`
	TTL    uint32 `json:"ttl"`
	RData  string `json:"rdata"` // 展示形式的 RDATA（已压缩空白）
	Source Source `json:"source"`
}

// SetKey 返回记录集键：（名称， 类别， 类型）。刻意不含 TTL/RDATA。
func (r *Record) SetKey() string {
	return SetKey(r.Name, r.Class, r.Type)
}

// SetKey 由三元组构造记录集键。
func SetKey(name string, class, typ uint16) string {
	return fmt.Sprintf("%s|%d|%d", Canonical(name), class, typ)
}

// Identity 返回整条记录的唯一标识（含 TTL 与 RDATA），用于重复检测。
func (r *Record) Identity() string {
	return fmt.Sprintf("%s|%d|%s", r.SetKey(), r.TTL, normalizeRData(r.RData))
}

// DataIdentity 返回不含 TTL 的记录标识，用于“TTL 改动”配对。
func (r *Record) DataIdentity() string {
	return fmt.Sprintf("%s|%s", r.SetKey(), normalizeRData(r.RData))
}

// SerialOf 解析 SOA 记录 RDATA 中的序列号字段（第 3 个字段）。
func (r *Record) SerialOf() (uint32, bool) {
	if r.Type != dns.TypeSOA {
		return 0, false
	}
	fields := strings.Fields(r.RData)
	if len(fields) < 3 {
		return 0, false
	}
	n, err := strconv.ParseUint(fields[2], 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
}

// Canonical 把名称规范化为小写 FQDN。
func Canonical(name string) string {
	return dns.CanonicalName(name)
}

// CompareName 按 RFC 4034 第 6.1 节的规范顺序比较两个域名：
// 从右侧标签开始逐段比较（不区分大小写），返回 -1/0/1。
func CompareName(a, b string) int {
	la := dns.SplitDomainName(Canonical(a))
	lb := dns.SplitDomainName(Canonical(b))
	for i, j := len(la)-1, len(lb)-1; i >= 0 && j >= 0; i, j = i-1, j-1 {
		if la[i] != lb[j] {
			if la[i] < lb[j] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(la) < len(lb):
		return -1
	case len(la) > len(lb):
		return 1
	default:
		return 0
	}
}

// FromRR 把一条 DNS 记录转换为内部模型。
func FromRR(rr dns.RR, src Source) *Record {
	h := rr.Header()
	return &Record{
		Name:   Canonical(h.Name),
		Class:  h.Class,
		Type:   h.Rrtype,
		TTL:    h.Ttl,
		RData:  RDataOf(rr),
		Source: src,
	}
}

// RDataOf 提取记录的 RDATA 展示文本（去掉所有者/TTL/类别/类型头部）。
func RDataOf(rr dns.RR) string {
	s := rr.String()
	hdr := rr.Header().String()
	if strings.HasPrefix(s, hdr) {
		return normalizeRData(s[len(hdr):])
	}
	return normalizeRData(s)
}

func normalizeRData(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Zone 是区内容的一份不可变快照。
type Zone struct {
	Origin  string    `json:"origin"` // 区顶点，FQDN 规范形式
	Serial  uint32    `json:"serial"`
	Records []*Record `json:"records"`
}

// SOA 返回区的 SOA 记录；没有则返回 nil。
func (z *Zone) SOA() *Record {
	for _, r := range z.Records {
		if r.Type == dns.TypeSOA {
			return r
		}
	}
	return nil
}

// RRsets 把记录按 （名称， 类别， 类型） 分组为记录集。
// 返回的 map 键为 SetKey。
func (z *Zone) RRsets() map[string][]*Record {
	sets := make(map[string][]*Record)
	for _, r := range z.Records {
		k := r.SetKey()
		sets[k] = append(sets[k], r)
	}
	return sets
}

// Clone 返回区的浅层深拷贝：记录指针共享（记录视为不可变），切片独立。
func (z *Zone) Clone() *Zone {
	out := &Zone{Origin: z.Origin, Serial: z.Serial, Records: make([]*Record, len(z.Records))}
	copy(out.Records, z.Records)
	return out
}

// HasIdentity 判断区中是否存在完全相同的记录。
func (z *Zone) HasIdentity(id string) bool {
	for _, r := range z.Records {
		if r.Identity() == id {
			return true
		}
	}
	return false
}

// Remove 删除满足条件的唯一一条记录，返回是否删除成功。
func (z *Zone) Remove(match func(*Record) bool) bool {
	for i, r := range z.Records {
		if match(r) {
			z.Records = append(z.Records[:i], z.Records[i+1:]...)
			return true
		}
	}
	return false
}

// SortedRecords 返回按规范顺序（RFC 4034 第 6.1 节名称顺序，
// 再按类别、类型、RDATA）排序的记录副本，用于规范化输出。
func (z *Zone) SortedRecords() []*Record {
	out := make([]*Record, len(z.Records))
	copy(out, z.Records)
	SortRecords(out)
	return out
}

// SortRecords 原地按规范顺序排序。
func SortRecords(recs []*Record) {
	sort.SliceStable(recs, func(i, j int) bool {
		a, b := recs[i], recs[j]
		if c := CompareName(a.Name, b.Name); c != 0 {
			return c < 0
		}
		if a.Class != b.Class {
			return a.Class < b.Class
		}
		if a.Type != b.Type {
			// SOA 始终排在最前，便于阅读。
			if a.Type == dns.TypeSOA {
				return true
			}
			if b.Type == dns.TypeSOA {
				return false
			}
			return a.Type < b.Type
		}
		return strings.ToLower(a.RData) < strings.ToLower(b.RData)
	})
}

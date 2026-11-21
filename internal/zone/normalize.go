package zone

import (
	"fmt"
	"strings"

	"github.com/miekg/dns"
)

// MasterFile 把区快照输出为规范化的主文件格式文本：
//   - 记录按 RFC 4034 第 6.1 节的规范名称顺序排序；
//   - 同一记录集内按 RDATA 排序；
//   - 顶点名称写作 @，其余名称写为相对形式；
//   - 附带 $ORIGIN 与 $TTL 指令，便于直接下载存档或载入比对工具。
func (z *Zone) MasterFile() string {
	var b strings.Builder
	origin := z.Origin
	fmt.Fprintf(&b, "$ORIGIN %s\n", origin)

	// 默认 TTL 取 SOA 的 minimum 字段；没有 SOA 时退化为 3600。
	defaultTTL := uint32(3600)
	if soa := z.SOA(); soa != nil {
		if rr, err := dns.NewRR(fmt.Sprintf("%s 300 IN SOA %s", soa.Name, soa.RData)); err == nil {
			if s, ok := rr.(*dns.SOA); ok {
				defaultTTL = s.Minttl
			}
		}
	}
	fmt.Fprintf(&b, "$TTL %d\n", defaultTTL)

	for _, r := range z.SortedRecords() {
		fmt.Fprintf(&b, "%s\t%d\t%s\t%s\t%s\n",
			ownerName(r.Name, origin), r.TTL, ClassString(r.Class), TypeString(r.Type), r.RData)
	}
	return b.String()
}

// ownerName 把 FQDN 转换为相对顶点的展示名；顶点本身写作 @。
func ownerName(name, origin string) string {
	name = Canonical(name)
	origin = Canonical(origin)
	if name == origin {
		return "@"
	}
	if dns.IsSubDomain(origin, name) {
		return strings.TrimSuffix(name, "."+origin)
	}
	return name
}

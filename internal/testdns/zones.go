package testdns

import (
	"fmt"
	"strings"

	"github.com/miekg/dns"
)

// zoneDef 描述一个测试区：名称、是否保留用于 IXFR 的历史版本、
// 常规序列号起点，以及每个版本的主文件文本（SOA 序列号占位为 0，
// 实际序列号由服务器按当前行为开关统一分配）。
type zoneDef struct {
	name        string
	keepHistory bool
	normalBase  uint32
	versions    []string
}

// zoneDefs 是随项目启动的权威测试数据。
//
// example.com. 的四个版本覆盖了：记录集内单条记录 TTL 改动、
// 多条记录记录集的增删、整个记录集的新增与删除，
// 相邻版本之间都可以做增量传送，跨版本则一次传送多个修订段。
var zoneDefs = []zoneDef{
	{
		name:        "example.com.",
		keepHistory: true,
		normalBase:  2024010101,
		versions: []string{
			// v1
			`example.com.        300  IN SOA   ns1.example.com. admin.example.com. 0 7200 3600 1209600 300
			 example.com.        300  IN NS    ns1.example.com.
			 example.com.        300  IN NS    ns2.example.com.
			 example.com.        3600 IN MX    10 mail.example.com.
			 example.com.        300  IN TXT   "v=spf1 -all"
			 ns1.example.com.    300  IN A     192.0.2.53
			 ns2.example.com.    300  IN A     192.0.2.54
			 www.example.com.    3600 IN A     192.0.2.80
			 www.example.com.    3600 IN A     192.0.2.81
			 mail.example.com.   3600 IN A     192.0.2.25`,
			// v2：MX 的 TTL 改动（3600→600）；www 记录集增加一条 A
			`example.com.        300  IN SOA   ns1.example.com. admin.example.com. 0 7200 3600 1209600 300
			 example.com.        300  IN NS    ns1.example.com.
			 example.com.        300  IN NS    ns2.example.com.
			 example.com.        600  IN MX    10 mail.example.com.
			 example.com.        300  IN TXT   "v=spf1 -all"
			 ns1.example.com.    300  IN A     192.0.2.53
			 ns2.example.com.    300  IN A     192.0.2.54
			 www.example.com.    3600 IN A     192.0.2.80
			 www.example.com.    3600 IN A     192.0.2.81
			 www.example.com.    3600 IN A     192.0.2.82
			 mail.example.com.   3600 IN A     192.0.2.25`,
			// v3：www 记录集中一条记录的 TTL 改动（3600→600，记录集不能被视为删除）；
			//     TXT 记录集增加一条记录
			`example.com.        300  IN SOA   ns1.example.com. admin.example.com. 0 7200 3600 1209600 300
			 example.com.        300  IN NS    ns1.example.com.
			 example.com.        300  IN NS    ns2.example.com.
			 example.com.        600  IN MX    10 mail.example.com.
			 example.com.        300  IN TXT   "v=spf1 -all"
			 example.com.        300  IN TXT   "google-site-verification=abc123"
			 ns1.example.com.    300  IN A     192.0.2.53
			 ns2.example.com.    300  IN A     192.0.2.54
			 www.example.com.    600  IN A     192.0.2.80
			 www.example.com.    3600 IN A     192.0.2.81
			 www.example.com.    3600 IN A     192.0.2.82
			 mail.example.com.   3600 IN A     192.0.2.25`,
			// v4：www 记录集删一条记录（记录集保留）；TXT 删一条（记录集保留）；
			//     新增 api 记录集；整体删除 ns2 的 A 记录集与一条 NS 记录
			`example.com.        300  IN SOA   ns1.example.com. admin.example.com. 0 7200 3600 1209600 300
			 example.com.        300  IN NS    ns1.example.com.
			 example.com.        600  IN MX    10 mail.example.com.
			 example.com.        300  IN TXT   "google-site-verification=abc123"
			 ns1.example.com.    300  IN A     192.0.2.53
			 www.example.com.    600  IN A     192.0.2.80
			 www.example.com.    3600 IN A     192.0.2.82
			 mail.example.com.   3600 IN A     192.0.2.25
			 api.example.com.    300  IN A     192.0.2.90`,
		},
	},
	{
		// 不保留历史版本的区：对 IXFR 查询只肯给全量（AXFR 回退），
		// 采集站必须能识别这种情况。
		name:        "nohist.test.",
		keepHistory: false,
		normalBase:  100,
		versions: []string{
			`nohist.test.         300  IN SOA   ns.nohist.test. admin.nohist.test. 0 7200 3600 1209600 300
			 nohist.test.         300  IN NS    ns.nohist.test.
			 ns.nohist.test.      300  IN A     10.0.0.53
			 host.nohist.test.    300  IN A     10.0.0.1
			 nohist.test.         300  IN TXT   "one"`,
			`nohist.test.         300  IN SOA   ns.nohist.test. admin.nohist.test. 0 7200 3600 1209600 300
			 nohist.test.         300  IN NS    ns.nohist.test.
			 ns.nohist.test.      300  IN A     10.0.0.53
			 host.nohist.test.    300  IN A     10.0.0.2
			 nohist.test.         300  IN TXT   "one"
			 nohist.test.         300  IN TXT   "two"`,
			`nohist.test.         300  IN SOA   ns.nohist.test. admin.nohist.test. 0 7200 3600 1209600 300
			 nohist.test.         300  IN NS    ns.nohist.test.
			 ns.nohist.test.      300  IN A     10.0.0.53
			 host.nohist.test.    300  IN A     10.0.0.2
			 host2.nohist.test.   300  IN A     10.0.0.3
			 nohist.test.         300  IN TXT   "one"
			 nohist.test.         300  IN TXT   "two"`,
		},
	},
	{
		// 序列号回绕演示区：开启“序列号回绕”后，序列号序列按 32 位
		// 回绕递增（4294967290 → 4294967294 → 2 → 6），
		// 采集站必须按 RFC 1982 规则而不是普通大小比较来判断新旧。
		name:        "wrap.test.",
		keepHistory: true,
		normalBase:  1,
		versions: []string{
			`wrap.test.           300  IN SOA   ns.wrap.test. admin.wrap.test. 0 7200 3600 1209600 300
			 wrap.test.           300  IN NS    ns.wrap.test.
			 ns.wrap.test.        300  IN A     203.0.113.53
			 node.wrap.test.      300  IN A     203.0.113.1
			 wrap.test.           300  IN TXT   "wrap-1"`,
			`wrap.test.           300  IN SOA   ns.wrap.test. admin.wrap.test. 0 7200 3600 1209600 300
			 wrap.test.           300  IN NS    ns.wrap.test.
			 ns.wrap.test.        300  IN A     203.0.113.53
			 node.wrap.test.      300  IN A     203.0.113.1
			 node.wrap.test.      300  IN A     203.0.113.2
			 wrap.test.           300  IN TXT   "wrap-2"`,
			`wrap.test.           300  IN SOA   ns.wrap.test. admin.wrap.test. 0 7200 3600 1209600 300
			 wrap.test.           300  IN NS    ns.wrap.test.
			 ns.wrap.test.        300  IN A     203.0.113.53
			 node.wrap.test.      900  IN A     203.0.113.1
			 node.wrap.test.      300  IN A     203.0.113.2
			 wrap.test.           300  IN TXT   "wrap-2"`,
			`wrap.test.           300  IN SOA   ns.wrap.test. admin.wrap.test. 0 7200 3600 1209600 300
			 wrap.test.           300  IN NS    ns.wrap.test.
			 ns.wrap.test.        300  IN A     203.0.113.53
			 node.wrap.test.      900  IN A     203.0.113.1
			 peer.wrap.test.      300  IN A     203.0.113.9
			 wrap.test.           300  IN TXT   "wrap-2"`,
		},
	},
}

// wrapSerialBase/wrapSerialStep 是开启“序列号回绕”后 wrap.test. 使用的
// 序列号起点与步长：序列贴着 2^32 边界，几步之内就会回绕。
const (
	wrapSerialBase = uint32(4294967290) // 2^32 - 6
	wrapSerialStep = uint32(4)
)

// parseVersion 把一个版本的主文件文本解析为记录列表（SOA 在首位）。
func parseVersion(name, text string) ([]dns.RR, error) {
	var rrs []dns.RR
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ";") {
			continue
		}
		rr, err := dns.NewRR(line)
		if err != nil {
			return nil, fmt.Errorf("区 %s 第 %d 行解析失败: %w", name, i+1, err)
		}
		rrs = append(rrs, rr)
	}
	if len(rrs) == 0 {
		return nil, fmt.Errorf("区 %s 的版本为空", name)
	}
	if _, ok := rrs[0].(*dns.SOA); !ok {
		return nil, fmt.Errorf("区 %s 的版本首条记录不是 SOA", name)
	}
	return rrs, nil
}

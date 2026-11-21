# DNS 区数据采集站

一个自包含的 DNS 区数据采集演示系统：**单个进程**内同时运行

1. **本地权威测试 DNS 服务**（随项目启动，提供多版本测试区，支持 AXFR/IXFR 与各类异常开关）；
2. **采集引擎**（发起全量/增量传送，接收校验，按 RFC 1982 比较序列号，装配新区）；
3. **网页界面**（Vue 3，选择测试区、发起传送、展开记录集来源、对照传送前后、查看拒收原因、下载规范化结果）。

后端为 Go + [miekg/dns](https://github.com/miekg/dns)，前端为 Vue 3（生产构建已内嵌，无需 Node 构建步骤）。

## 快速开始

```bash
./run.sh            # 构建并启动：网页 http://localhost:8080，测试 DNS 127.0.0.1:5353
./run.sh -http :9090 -dns 127.0.0.1:15353   # 自定义端口
```

`run.sh` 依次在 PATH、`./.tools/go`、常见安装位置寻找 Go 1.22+。首次构建会通过
`GOPROXY`（默认 `https://goproxy.cn,direct`，可覆盖）拉取唯一依赖 miekg/dns。

启动后可以：

- 打开 <http://localhost:8080> 使用网页；
- 用 `dig @127.0.0.1 -p 5353 example.com. SOA` 直接查询测试 DNS（普通查询始终可用）；
- `curl http://localhost:8080/api/overview` 查看 API。

## 使用流程

1. 在左侧选择测试区（如 `example.com.`），点 **全量传送** 建立本地副本；
2. 点 **推进版本** 让服务器上的区前进一个版本（可连点多次，制造跨多次修订的差距）；
3. 点 **增量传送**，采集站会收到若干「旧 SOA、删除项、新 SOA、增加项」修订段并依次应用；
4. 在 **区内容** 页展开任意记录集，查看每条记录的 TTL、RDATA 与来源（哪次传送、哪个增量段）；
5. 在 **传送记录** 页点任意一行，查看传送前后对照（含 TTL 改动配对）与修订段明细；
6. 打开顶部 **行为开关** 制造异常，再到 **失败检查** 页查看拒收原因与服务器实际发来的数据；
7. 用 **下载规范化结果** 把任意历史版本导出为规范排序的区文件。

## 测试区

| 区 | 特点 |
|---|---|
| `example.com.` | 4 个版本，保留 IXFR 历史；变更覆盖：记录集内单条记录 TTL 改动、多条记录记录集的增删、整个记录集的新增与删除 |
| `nohist.test.` | 不保留历史：对 IXFR 请求只肯给全量（AXFR 回退），采集站会识别并说明 |
| `wrap.test.` | 配合「序列号回绕」开关，序列号按 32 位回绕递增（4294967290 → 4294967294 → 2 → 6） |

## 行为开关（测试服务器）

| 开关 | 效果 |
|---|---|
| 序列号回绕 | `wrap.test.` 序列号贴着 2^32 边界递增并回绕（切换后该区重置到初始版本） |
| 缺失结束段 | 传送到一半就断开 TCP（全量少末尾 SOA，增量少末段新 SOA 及增加项） |
| 重复记录 | 传送响应中夹带重复记录 |
| 拒绝传送 | 对 AXFR/IXFR 直接回 `REFUSED`（普通查询不受影响） |
| 临界序列号 | 下一次推进版本时序列号跳变恰好 2^31（一次性），触发「不确定」比较 |

## 关键设计

- **记录集身份**：记录集以 （名称， 类别， 类型） 三元组识别，TTL/RDATA 属于记录本身。
  因此一条记录的 TTL 改动在传送前后对照中体现为「记录集内 TTL 变化」，
  绝不会被当成删除整个记录集（见 `internal/zone/diff.go` 与 `zone_test.go`）。
- **增量段边界**：IXFR 流解析为 新 SOA + 若干「旧 SOA、删除项、新 SOA、增加项」段，
  段间序列号必须衔接，末段新 SOA 必须回到响应首条 SOA 宣告的序列号；
  第二条记录不是 SOA 时识别为「服务器只肯给全量」并按全量处理
  （见 `internal/collector/parse.go`）。
- **序列号比较**：严格按 RFC 1982 的 32 位算术（`internal/zone/serial.go`）。
  回绕场景（如 4294967294 → 2）正确判定为递增；差值恰为 2^31 时关系不确定，
  采集站**停止切换**并在拒收原因中说明，旧区保持不变。
- **接收校验**：首条必须是本区 SOA（区归属）；全量必须以同序列号 SOA 收尾；
  所有记录必须归属本区且类别为 IN；记录集内不得有重复记录；
  TTL 不一致按 RFC 2181 视为警告而非拒收（见 `internal/collector/apply.go`）。
- **失败隔离**：只有全部校验通过才切换当前区；失败只追加一条传送记录
  （含收到的原始数据与拒收原因），旧区快照一直可查询、可下载
  （见 `internal/station/store.go`）。

## API 一览

```
GET  /api/overview                        服务器与采集站总览
PUT  /api/server/behaviors                设置行为开关
POST /api/server/zones/{zone}/bump        推进服务器上的区版本
POST /api/zones/{zone}/transfer           发起传送 {"mode":"axfr"|"ixfr"}
GET  /api/zones/{zone}/snapshot           当前区内容（记录集 + 每条记录来源）
GET  /api/zones/{zone}/snapshots          历史快照列表（旧区一直可查询）
GET  /api/zones/{zone}/snapshots/{serial} 指定序列号的旧区内容
GET  /api/zones/{zone}/download[?serial=] 下载规范化区文件
DELETE /api/zones/{zone}                  清除本地副本
GET  /api/transfers[?zone=&failed=1]      传送记录（可只列失败）
GET  /api/transfers/{id}                  传送详情（段、对照、拒收原因、原始数据）
```

## 测试

```bash
go test ./...
```

覆盖：RFC 1982 边界（含 2^31 不确定）、跨多次修订的增量、TTL 改动不误判为记录集删除、
全量回退识别、缺失结束段、重复记录、拒绝传送、序列号回绕、旧区可查询、失败单独可查、
规范化输出排序等（`internal/zone/*_test.go`、`internal/collector/*_test.go`）。

## 目录结构

```
run.sh                     启动入口（构建并运行）
cmd/zonestation/           主程序：单进程启动全部服务
internal/zone/             区模型、RFC 1982 序列号、对照、规范化
internal/testdns/          本地权威测试 DNS 服务（多版本区 + 行为开关）
internal/collector/        采集引擎：接收、解析、校验、应用
internal/station/          状态存储：快照历史与传送记录
internal/httpapi/          REST API 与网页静态服务（内嵌 web/）
internal/httpapi/web/      Vue 3 前端（免构建，Vue 已内置于 vendor/）
```

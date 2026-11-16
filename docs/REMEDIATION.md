# quack-proxy 整改台账（REMEDIATION）

> 版本 v0.2.0-r13 | 2026-11-13 ~ 11-16 | 依据：2026-11-13 六领域专家评审
> r4 = r3 + R-21（停止时进程组回收缺陷，由最后一次真实引擎 E2E 复跑发现并修复）。
> r5 = r4 + R-07（reload 字段级 diff）+ R-22（daemon 崩溃后重启产生假阳性健康状态，由 `scripts/daemon-crash-drill.sh` 实测发现并修复）+ daemon crash 演练与 CI 回归门槛。
> r6 = r5 + 真实 `haproxy -c -f` / `systemd-analyze verify` 验收（**解除 r5 的「环境阻断」结论**）+ `scripts/verify-deploy.sh` 与必需 CI job `deploy-verify` + R-23（生成的 HAProxy 配置缺 root 加固，文档化缓解）。
> r7 = r6 + **真实 systemd 255 PID 1 非 root 安装演练**（再次更正 r6 的「环境无 systemd」误判）+ R-23 可验证加固路径（`proxy.hardened` opt-in）+ govulncheck 基线清零并转必需 + 发布工具链固定 go1.27.4。
> r8 = r7 + **LAN 明文风险真实抓包实证**（`scripts/verify-plaintext.sh`，必需 CI job `plaintext-audit`）+ `quack-e2e` 外部依赖重试加固与 advisory 范围收紧到仅 PR。注意：**审计不等于修复**，R-05 的 `disable_ssl` 硬编码仍需设计决策。
> r9 = r8 + **R-18 收口**（真实 Windows 实证 → 修 `lookupBin` 跨平台可执行判定 → 启动显式拒绝 → release 撤下 Windows 产物 → 必需 `windows-runtime-guard` job）+ 真实引擎 E2E 用离线 pinned CLI / 签名扩展完整复跑通过。r8 把「Windows 产物去留」写成待产品决策，实为 PRD §8 既有 non-goal 未执行到位。
> r10 = r9 + **`quack-e2e` 在 PR 上转必需**：新增 `scripts/prefetch-quack-e2e.sh` 把 pinned CLI + 签名 `quack` + 签名 `httpfs` 做成 SHA256 校验的 CI 离线缓存，E2E 测试进程完全不联网（`QUACK_E2E_OFFLINE=1`），`--network none` 容器实证 `ALL_QUACK_E2E_CHECKS_PASSED`。**同时更正 r9 的「离线」口径**：r9 的容器其实仍联网，DuckDB 靠 autoload 下载了 `httpfs`；严格离线时磁盘库会报 `read-only crypto module loaded`。
> r11 = r10 + **HAProxy + VIP 真实联调**（`scripts/verify-vip-failover.sh` + 必需 CI job `vip-failover`）：Docker bridge 上两容器漂移真实 `/32` 浮动 IP、真实 HAProxy 2.8 转发、GARP 收敛，解除「需真实浮动 IP / 双机网络」这一误判。**但联调不是产品 HA**：VIP 由脚本模拟，切换后后端身份变化（`SESSION_DISCONTINUITY=1`），会话与数据不连续；R-04 / R-05 均未被修复。
> r12 = r11 + **引擎原生 TLS 能力实证**（`scripts/verify-engine-tls.sh` + 必需 CI job `engine-tls-capability`）：用同一套 pinned v2.0.0 引擎与签名扩展证明 Quack 原生支持 TLS（`disable_ssl=false` → `https://`、TLSv1.3 握手、证书指纹校验），产品 `disable_ssl=true` 是**主动关闭**而非能力受限。**更正 R-05 口径**：「内建 TLS 属需设计决策」原读起来像能力限制，实际引擎能力已具备，剩余是产品接线的设计决策（见 §2 R-05）。**这是引擎审计，不是产品修复**：R-05 仍开放，明文与 token 落盘风险未变。
> r13 = r12 + **CI 门禁契约机器可验证**（`internal/cicontract`，随已必需的 `go test ./...` 跑）：把「9 个门禁必需 / Actions 全 SHA pin / 共享缓存 key 与 pin 一致 / `engine-tls-capability` 依赖 `quack-e2e` / 离线开关已设置」从注释与散文变成断言，并配 9 个单点篡改负例证明每个检查非空转。同时在单个 `--network none` 容器按 CI 真实顺序复跑三段链路（`OFFLINE_CI_CHAIN_PASSED`，3/3 RC=0）。**这是配置契约检查，不是产品修复也不是新增门禁**：必需门禁仍为 9 个，R-05 仍开放。
> 下表中标注 r5 ~ r13 的验证行是**当时时点的历史快照**（其中 r12 / r13 行是本轮收尾复跑后的结果），不是跨轮恒定状态；当前状态以 §1 Gate 行、§3 各 R 条目与 §5 末尾「仍未做的验收」为准。
> 证据行号以**原始提交 eadef51** 为准；补丁后行号会偏移，请按问题描述定位。
> 状态：`已修` / `部分缓解` / `待办` / `需设计决策` / `超范围`。
> 「已修」= 代码 + 单测 +（凡可做者）实机验证，见 §5。

## 1. 分道与通过条件

| Gate | 主题 | 通过条件 | 当前 |
|------|------|----------|------|
| 0 | 止血，不动架构 | 回归用例 + `go test -race ./...` 全绿；真实引擎 E2E；照 README 装 systemd 能启动 | **代码、单测、竞态、真实引擎 E2E 全部通过**；systemd 非 root 实机安装演练 **r7 已在真实 systemd 255 PID 1 补做并通过**（3/3，含负例，见 §4）→ **过门** |
| 1 | 收安全面与边界 | 注入用例、文件权限用例、明文检查；Actions SHA pin + 产物签名；撤跨库 roundrobin | **全部通过**；LAN 明文抓包 r8 已用真实 `AF_PACKET` 实证并落成必需门禁（结论是「弱点确实存在」，不是「已修」，见 §3 R-05） → **过门** |
| 2 | 验证与诚实化 | 基准可复现；覆盖率门槛；文档-代码一致性检查表逐条勾完 | **主体完成**；govulncheck 已于 r7 清零基线并转**必需** `vuln-scan` job；**r10 起 `quack-e2e` 已无 advisory**，包括 `pull_request` 在内的所有事件均为**必需**，且测试进程由 `--network none` 实证为严格离线（pinned CLI + `quack` + `httpfs` 缓存）；**r11 补齐 HAProxy + VIP 联调并转必需 `vip-failover`**（如实标注它只验证入口漂移、不证明产品 HA）；**r12 新增必需 `engine-tls-capability`**，用同一缓存实证引擎原生 TLS 并把「能力受限」口径更正为「产品接线设计决策」（引擎审计，R-05 仍开放）；端到端基准已可复现并给出实测 → **实质过门** |

**执行顺序**：先写会失败的测试，再改代码。原 35 个测试全绿却漏掉全部 P0 —— supervisor 用 `duckdb_bin: "true"` stub，并靠 `ManualSetShard` 直接注入内部状态。

---

## 2. P0 阻断项

### R-01 daemon 是 SPOF，"HA 集群"名不副实
- 证据：`README.md:9,11`；`deploy/systemd/quack-proxy.service:45` `Restart=no`，该文件 `17-24` 行自承 daemon 死后子进程占端口
- 修复：`SCOPE.md` 降级定位 + README 撤下该措辞；长期需 control plane 冗余 + 孤儿进程接管
- 验收：文档边界检查（已完成）；**daemon crash 演练已完成**（`scripts/daemon-crash-drill.sh`，见 §5）：`SIGKILL` daemon 后孤儿分片存活、端口仍占用、pid/status 残留，status 停在 `starting`；直接重启会报假 `healthy` —— 实测坐实了 systemd 单元注释里的旧断言，并催生 R-22 的启动前端口预检。孤儿自动接管仍属真 HA，超范围。
- 状态：**边界已定并写入文档**；冗余 **超范围**（新产品立项，非本次整改）

### R-02 状态机终态不可逆、重启预算不清零
- 证据：`supervisor.go:472-476` healthy 分支只重置 `backoff`/`lastUnhealthy`；`ApplyConfig:195-196` 把同名分片当 existing 直接 continue
- 修复：healthy 清零 `Restarts`；`ApplyConfig` 对仍在配置中的 stopped 分片重启并重置预算
- 验收：`TestGate0HealthyResetsRestartBudget`、`TestGate0StoppedShardRecoversOnReload`
- 状态：**已修**

### R-03 锁内阻塞可致守护进程整体死锁
- 证据：`supervisor.go:456-458` 持 `s.mu` 串行 `health.Check`（2s×N）并在锁内调 `stopShardLocked`；SIGKILL 后 `<-done` 无超时
- 修复（本轮完整重构）：`checkAll` 四段式 —— ① RLock 快照探测目标 ② 无锁并发探测（上限 8）③ Lock 内只做纯内存决策与「认领」 ④ 锁外 stop/restart；`ShardProcess.opMu` 串行化单分片生命周期；`WriteStatusFile` 快照后锁外做文件 I/O；`ApplyConfig` / `StartAll` 的 fork 与 stop 全部移出 `s.mu`
- 验收：`go test -race ./...`（Linux 容器全绿）、`TestGate2ProbeDoesNotBlockStatusReads`
- 状态：**已修**

### R-04 HAProxy 把分片当副本轮询
- 证据：`haproxy.go:50,61,64-67` 不同 `.db` 塞进同一 `mode tcp` + `roundrobin` backend；每分片独立 token
- 修复：Gate 1 用 `replicaGroup()` 拒绝跨库；**本轮实机验证后发现该修复仍不充分** —— 同库同 token 的「副本组」在单机上会触发 DuckDB 文件锁冲突（`IO Error: Could not set lock on file`），第二个分片根本起不来。最终改为 `singleBackendOnly()`：**只允许单分片端口转发，多分片一律拒绝**
- 验收：`TestGenerateHAProxyRejectsCrossDatabaseShards`、`TestGenerateHAProxyRejectsSameDatabaseShards`、`TestGenerateHAProxySingleShard`、容器实测锁冲突
- 状态：**已修（含补强）**

### R-05 默认全网卡明文 + 凭据明文落盘 + TLS 死配置
- 证据：`config.go:105-106` 默认 `0.0.0.0`；`supervisor.go:28` 硬编码 `disable_ssl=true, allow_other_hostname=true`；token 0644 写 status.json；`main.go:183` 无鉴权输出 token；`SSLConfig` 从未被读取
- 修复：默认 `127.0.0.1`；`allow_other_hostname` 默认 false 且可配；status/pid `0600` + 原子写；`status --json` 默认脱敏、需 `--show-tokens`；`proxy.ssl.enabled=true` 加载期报错；新增 `health_expect` 身份校验
- 验收：`TestDefaults`、`TestGate1BootSQLDefaultsToStrictHostname`、`TestBuildStatusRedactsTokens`、`TestWritePIDIsPrivate`、`TestGate0StatusFileIsPrivateAndComplete`、`TestCheckWithExpectRejectsWrongBody`、SSL/注入用例
- 状态：**部分缓解（r8 已实证，r12 已厘清能力边界，风险未修）**。r8 用 `scripts/verify-plaintext.sh` 在真实 daemon + 独立嗅探容器下证实：`bind_host: 0.0.0.0` 时 token / `Authorization: Bearer` / SQL 正文全部出现在原始 TCP payload 中（`PLAINTEXT_TOKEN_VISIBLE=1`），同网段 TLS 控制通道的 canary 不可见（负例 `TLS_CONTROL_HIDDEN=1`），真实 CLI 拒绝 `proxy.ssl.enabled=true`（`PRODUCT_TLS_UNAVAILABLE=1`），`status.json` 0600 但 token 明文落盘（`STATUS_TOKEN_AT_REST=1`）。**这是已知弱点的证据，不是修复**；3/3 + 跨镜像 1/1 通过，已落为必需 CI job `plaintext-audit`
- **r12 口径更正（重要，影响甲方决策与工作量估算）**：此前多轮把「内建 TLS」笼统归入「需设计决策」，读起来像引擎不支持 TLS、需要自行实现。实测**否**。用项目自己 pin 的 DuckDB v2.0.0 CLI（`c6507206…`）+ 签名 core `quack` / `httpfs` 扩展，`scripts/verify-engine-tls.sh` 证明：① `quack_serve` 原生带 `disable_ssl` / `ssl_cert_file` / `ssl_key_file`，`quack_query` 带 `ssl_fingerprint`；② `quack_generate_keys()` 生成自签证书与 SHA256 指纹；③ `disable_ssl := true` 返回 `http://`、`false` 返回 `https://`；④ 独立 `openssl s_client` 完成 **TLSv1.3 / TLS_AES_256_GCM_SHA384** 握手，明文 HTTP 打 TLS 端口被拒、HTTPS GET 返回 200；⑤ 客户端用正确 `ssl_fingerprint` 经 TLS 查得 `SELECT 42 → 42`，错误指纹 / 缺失指纹 / 错误 token 全部被拒（负例），明文对照端点仍能应答（证明负例不是「服务已死」造成的假通过）。**结论：引擎原生支持 TLS，`supervisor.go` 硬编码 `disable_ssl = true` 是产品主动关闭，不是能力受限。**
- **但审计 ≠ 修复**：R-05 仍**开放**。该脚本直接驱动引擎、不经过 quack-proxy daemon，产品当前依旧明文 HTTP、token 仍落 status.json。把引擎能力变成可用功能，是**产品接线与运维设计决策**，需甲方拍板的具体项：① 证书生命周期（自签 vs CA、生成/轮换/吊销、私钥权限与存放）；② 向客户端分发并轮换 64 位 SHA256 指纹的通道（当前 `status.json` / `AttachSQL` 契约里没有这个字段）；③ 健康探测改为 HTTPS 且禁用自签证书校验或固定指纹（`health.go` 当前是明文 `GET /` 判 2xx）；④ `status.json` 的 `attach_sql` / token 契约；⑤ `gen-proxy` 生成的 HAProxy 前端与 `proxy.ssl` 死配置的处置（二者语义不同：前者是 LB 前端终止，后者从未被读取）。**这些不是「实现 TLS」，而是「把已有 TLS 接进产品」**，工作量与风险边界据此重估。脚本内含诚实性断言 `PRODUCT_DISABLES_ENGINE_TLS=1`：一旦产品真的启用 TLS，此门禁会（正确地）失败，须同步更新本文档、`verify-plaintext.sh` 与 `SECURITY.md`，不得删除。必需 CI job `engine-tls-capability`（复用 `quack-e2e` 的 pinned 缓存、`needs` 保证已填充、零新增下载）

### R-06 照文档装 systemd 根本起不来
- 证据：`quack-proxy.service:33` 非 root `User=`；`config.go:99-103` 默认 PID/status 在 `/var/run/quack-proxy/`；`main.go:248-252` `MkdirAll` 必失败
- 修复：unit 补 `RuntimeDirectory=quack-proxy` + `RuntimeDirectoryMode=0750` + `UMask=0077`；`writePID` 目录权限 0700
- 验收：代码与静态一致性检查（已完成）；**r7 补做实机演练** —— `scripts/systemd-install-drill.sh` 在真实 systemd 255 PID 1 安装 shipped unit 并启停，断言非 root MainPID、`RuntimeDirectory` 0750、pid/status 0600、healthy、token 脱敏、停止后无孤儿，且带「无 RuntimeDirectory 必失败」负例，3/3 `SYSTEMD_INSTALL_DRILL_PASSED`，已落为必需 CI job `systemd-drill`。r6 记的「本机/容器无 systemd」是容器启动参数误判（详见 §4）
- 状态：**已修并实机复验**

### R-07 reload 只增删不更新字段，且不联动 HAProxy
- 证据：`supervisor.go:194-197`；`main.go:110-118` SIGHUP 不触发 `gen-proxy`
- 修复（r4 部分）：reload 后重生成 HAProxy 配置并告警需手动 reload HAProxy；`cfg` 指针同步更新；复活 stopped 分片
- 修复（**r5 完成，原「需设计决策」已消解**）：`ApplyConfig` 现在做真正的字段级 diff，并明确连接中断语义——
  - **分片字段变化**（`port` / `database` / `token` / `init_sql` / `init_sql_file` 路径）→ 只重启该分片；返回值新增 `restarted []string`，daemon 日志记录数量。
  - **影响进程的全局参数变化**（`duckdb_bin` / `bind_host` / `allow_other_hostname` / `allow_unsigned_extensions`）→ 重启全部分片，因为运行中的 DuckDB 无法采纳。
  - **健康类参数**（`health_path` / `health_interval` / `health_expect`）→ 不重启，健康循环每次从 `s.cfg` 读取。
  - `init_sql_file` **内容**变化（路径不变）也触发重启：启动时记 sha256 摘要，reload 时重算。
  - 关键陷阱：`startShard` 会把省略的 token 解析成随机值并回填 `Config.Token`，直接比 struct 会让每次 reload 都误重启所有分片；`shardSpecChanged()` 在新旧均省略 token 时先归一化再比较。
  - 删除操作先于重启/新增执行，避免同一 reload 内的端口自撞；健康循环已认领（`restarting`）的分片不并发二次启停。
- 验收：8 个 `gate5_reload_test.go` 单元用例（含「生成 token 不误重启」「幂等 reload」「策略文件内容变化」）+ `TestIntegrationReloadSwitchesRealPort`（真实进程组换端口，`-race` 0.22s PASS）
- 状态：**已修**。剩余语义限制：把 token 从「显式」改为「省略」不会轮换现有 token（省略被理解为「沿用生成的凭据」）；HAProxy 进程本身仍须运维手动 reload

### R-08 性能数字归属错误
- 证据：`docs/benchmark.md:34` 自承命中空库 `GET /` greeting、直打分片端口、代理不在数据路径；README 把 83K/169K QPS 当产品性能
- 修复：README 章节改名「Historical benchmark」并加醒目 caveat（含 c=50 掉到 33K、P99 14.5ms、nightly 引擎）；新增 `scripts/quack-e2e.sh --benchmark N` 走真实 `ATTACH/SELECT`；实机给出 50 次往返 221ms (~226 qps) 与 r4 复跑 312ms (~160 qps)，均为单客户端、含 ATTACH 的顺序往返延迟，不是吞吐
- 状态：**已修**（口径诚实化 + 可复现端到端基准）

---

## 3. P1 高危

| ID | 问题 | 证据 | Gate | 状态 |
|----|------|------|------|------|
| R-09 | 健康检查只判 2xx、跟随重定向、`health_path` 可注入成 SSRF、每次新建 client | `health.go:12-25` | 1 | **部分缓解**：结构化 URL + 禁重定向 + 连接复用 + 限流读取 + config 白名单 + 可选 `health_expect` body 子串（实测 greeting）。真正的 Quack 协议握手/token 验证 → **待办** |
| R-10 | `AttachSQL` 用原始 BindHost，`0.0.0.0` 时生成不可连接地址 | `supervisor.go:237-238,548-549` | 0 | **已修**（`connectHost`） |
| R-11 | status.json 非原子写、错误被吞；pid 文件 0644、目录 0755 | `supervisor.go:242-243`；`main.go:247-252` | 0/1 | **已修**（两处均 0600 + CreateTemp/Rename，且文件 I/O 已移出锁）；`O_NOFOLLOW` 未做 |
| R-12 | 停机无 `CHECKPOINT`、串行 10s/片，N≥4 超 `TimeoutStopSec=30` | `supervisor.go:382-411` | 2 | **并行停机已修**（`StopAll` 并发 + 每分片 `opMu`）。`CHECKPOINT` 判定**架构不可行**：DuckDB CLI 阻塞在 `quack_serve`，stdin 无 admin 通道，注入 SQL 不会执行；数据完整性依赖 WAL → 写入 `SCOPE.md §7` 决策记录，需独立 admin 连接或引擎侧 shutdown hook |
| R-13 | `name` 只禁引号/换行，`/..` 路径穿越、空格污染 SQL/HAProxy 行 | `config.go:137-144,180-182` | 1 | **已修**（标识符正则 + `bind_host`/`health_path` 白名单，含 4 个注入用例） |
| R-14 | 供应链：`-unsigned` + 远程 INSTALL、`curl\|bash`、Actions 未 SHA pin、产物无签名 | `supervisor.go:308`；`README.md:34`；`release.yml` | 1/2 | **已修（r7 收口）**：`-unsigned` 显式开关、README 去 `curl\|bash`、**全部 Actions 按真实 commit SHA pin**、`govulncheck@v1.9.0` 版本 pin、release 顶层最小权限 + `id-token: write` + **cosign keyless 签名 `SHA256SUMS`**（附 `.sig`/`.pem`）、DuckDB CLI SHA256 pin + 签名 core 扩展。**r7**：用 `go1.27.4` 把 govulncheck 基线从 11 项清零（实测 `No vulnerabilities found` / SCAN_RC=0），新增必需 `vuln-scan` job，release 固定 `go-version: '1.27.4'`，确保发布产物用同一套补丁 stdlib |
| R-15 | 测试假象：原 35 个测试用 stub 与状态注入；CI 无 `-race`/覆盖率/E2E | `supervisor_test.go`；`ci.yml:39` | 2 | **已修**：**74** 个测试函数（r5 +12；r6 加 R-23 提示用例；r7 加 `TestGenerateHAProxyHardenedEmitsDirectives`；r9 加 `TestRuntimeSupported` / `TestWindowsUnsupportedMessageIsActionable`；r13 加 `internal/cicontract` 3 个契约用例）；CI 加 `-race -cover` + 50% 门槛及必需的 `crash-drill` / `deploy-verify` / `systemd-drill` / `vuln-scan` / `plaintext-audit` / **`windows-runtime-guard`** job；脚本增至 12 个。r13 Linux 容器实测总覆盖率 **72.2%**（新增 `cicontract` 80.5%；r9 的 70.9% 曾因新增 Windows 运行时分支在 Linux 不可达而较 r7 的 71.1% 略降）；真实引擎 E2E 于 r9 用离线通道完整复跑、r10 收紧为严格离线、r13 按 CI 顺序串联复跑通过 |
| R-16 | 文档矛盾：DuckDB 三套版本口径、Windows 发布但跑不起来、tag/源码版本不一致 | `README.md`；`PRD.md`；`main.go:25` | 2 | **已修**：`go.mod` 对齐 1.25.0、`version=dev`、**DuckDB 单一 pin v2.0.0 + SHA256**、三套历史口径标注出处、Windows 不可用说明、PRD 状态表 |
| R-17 | 退避在默认配置下永不触发：每次重启刷新 30s grace，backoff 上限也是 30s | `supervisor.go:347,482-491,511` | 2 | **已修**：`performRestart` 保留原始 `lastStart`，30s grace 只作用于冷启动；`TestGate2RestartDoesNotRefreshStartupGrace` 验证第二次检查落在 backoff 窗口而非新 grace |
| R-18 | Windows 分支无进程组语义、硬编码 `bash`/`tail`；CI 仅交叉编译；release 仍发布不可运行的签名产物 | `attr_windows.go`；`supervisor.go:lookupBin`；`release.yml`；`ci.yml` | 2 | **已收口（r9）**：真实 Windows 上 `version` 正常但 `start` 报误导性 `duckdb_bin ... not executable`，根因是 `lookupBin` 用 POSIX `mode&0o111`（Windows 的 `os.Stat` 对普通文件返回 0666/0444，**任何绝对路径引擎都被误判**）。修为平台分流 `executableFile()`（Windows 按 `PATHEXT`，裁剪环境仍认 `.exe/.com/.cmd/.bat`）、移除 gate5 的 Windows skip、`GOOS=windows go test -c ./internal/supervisor` 通过；`runStart` 在 config.Load **之前**显式拒绝并给出 Linux/macOS 或 WSL2 路径 + 2 个不依赖宿主平台的单测；**release 撤下 windows/amd64 产物及 `.zip` 打包分支**；新增 `scripts/verify-windows.sh` 与必需 job `windows-runtime-guard`（真实 windows-latest：断言 `version` 可用、`start` 非 0 且含 R-18/WSL2，负例断言旧误导错误、`failed to load config`、`all shards started` 均不出现、不留 pid 文件）。本机 MINGW64_NT 实跑 `WINDOWS_RUNTIME_GUARD_PASSED`。**Windows 支持本身仍为 non-goal**，此处是「不再分发假产物 + 明确拒绝」，不是「已支持」 |
| R-19 | 文档指引 `INSTALL quack FROM community` 在 v2.0.0 返回 HTTP 404；且 README 宣称「自动 install + load」，代码 boot SQL 只有 `LOAD quack` | `config.go` 注释 / README Architecture / 容器实测 | 2 | **已修**：确认 quack 是**签名 core 扩展**；改为「运维预装 `INSTALL quack;`」并写入 README / example / SCOPE / SECURITY；E2E 脚本演示预装与 SHA256 校验 |
| R-20 | 单机「同库同 token 副本组」假设不成立 | 容器实测：两个 DuckDB 进程打开同一 `.db` → `IO Error: Could not set lock on file` | 2 | **已修**：`gen-proxy` 仅允许单分片（见 R-04） |
| R-21 | **停止后整组未回收**：daemon 已打印 `shard stopped`，端口却仍被占用（孤儿分片） | 最后一轮真实引擎 E2E 复跑：`FAIL: port 19491 still open after stop`；daemon 日志同时有 `level=INFO msg="shutting down"` 与 `msg="shard stopped" name=shard1`。**根因**：`cmd.Wait()` 只回收进程组组长（bash wrapper），DuckDB / `tail -f /dev/null` 后代仍在退出或忽略 SIGTERM，Quack 监听端口未释放；前两次通过纯属时序运气 | 2 | **已修**：`groupAlive()` 用 `kill(-pid, 0)` 探测整组；`stopShard` 在 `Wait()` 后调 `waitGroupGone()` 确认，超时则对整组补发 SIGKILL 并二次确认、失败记 ERROR；`TestStopAllReapsLingeringDescendants` 用忽略 SIGTERM 的真实子进程组复现，`-race` 下 2.13s PASS；E2E 脚本增加 6s drain 观察窗，修复后真实 DuckDB 引擎连续 3 轮（端口 19491 / 19512 / 19513）全部 `ALL_QUACK_E2E_CHECKS_PASSED` 且端口稳定释放 |
| R-22 | **daemon 崩溃后重启产生假阳性健康状态**：旧分片孤儿仍占端口，新 daemon 的健康探测由孤儿应答，status 却报 `healthy` | `scripts/daemon-crash-drill.sh` 实测（两代 stub 用不同响应体区分监听者）：`SIGKILL` daemon 后 `ORPHAN_SHARD_ALIVE=1`、`ORPHAN_PORT_OPEN=1`、`STALE_PID_FILE=1`、`STALE_STATUS_FILE=1`、`STALE_STATUS_VALUE=starting`；重启后 `RESTART_REPORTED_STATUS=healthy` 但 `HTTP_BODY_AFTER_RESTART=drill-OLD`（旧孤儿），杀掉孤儿组后端口立即关闭。修复前 `FALSE_HEALTHY=1` | 2 | **已修**：新增 `portInUse()`（bind 探测；Go 的 SO_REUSEADDR 使 TIME_WAIT 不误报）+ `startShard` 启动前预检，端口占用时 fail loud 并提示清理遗留进程组。修复后演练 `RESTART_DAEMON_ALIVE=0`、`RESTART_REFUSED_ON_ORPHAN_PORT=1`、`FALSE_HEALTHY=0`，日志给出明确原因。单测 2 个 + 真实进程组集成 1 个在 `-race` 下通过；演练脚本带 R-22 断言并作为**必需** CI job（`crash-drill`）。孤儿自动接管属真 HA，超范围 |
| R-23 | **生成的 HAProxy 配置缺 root 加固**：以 root 启动、无 `chroot` / `user`，HAProxy ≥ 3.x 运行时告警 | r6 首次用真实 HAProxy 3.4.7 跑 `haproxy -db -f`：`[WARNING] HAProxy was started as root without any 'chroot' directive` 与 `does not make use of 'user' nor 'uid'`。**`haproxy -c -f` 仍返回 0** —— 语法正确不等于安全 | 2 | **r7 已提供可验证的加固路径（默认仍 opt-in）**：新增 `proxy.hardened: true`，生成器输出生效的 `chroot`/`user`/`group`；`scripts/verify-hardening.sh` 预置 `haproxy` 账号与 root:root 0755 `/var/lib/haproxy`，并用真实 HAProxy 断言：未预置账号时 `haproxy -c -f` **拒绝**（`cannot find user id`）、未加固配置在 3.x 启动时确实告警、加固配置 real uid = `haproxy` 且无 root 告警、前端 19620 → 后端 19621 真实转发。HAProxy 3.4.7 3/3、2.8.31 1/1 通过；`TestGenerateHAProxyHardenedEmitsDirectives` 守生成文本。**默认值仍为 false**：负例证明注入到未预置主机会让 HAProxy 直接起不来。CI `deploy-verify` job 增加必需步骤 |

---

## 4. 落地清单

### Gate 0（第一轮）
| 文件 | 变更 |
|------|------|
| `docs/SCOPE.md` / `docs/REMEDIATION.md` / `docs/PRD.md` | 边界、台账、PRD 状态表 |
| `internal/supervisor/supervisor.go` | R-02 / R-05 / R-10 / R-11；注入 `checkFn`/`startFn` |
| `internal/supervisor/gate0_regression_test.go` | 4 个回归用例 |
| `deploy/systemd/quack-proxy.service` | `RuntimeDirectory` / `UMask` |

### Gate 1（第二轮）
| 文件 | 变更 |
|------|------|
| `internal/config/config.go` | 默认环回、白名单校验、`SSLConfig` fail loud、`allow_unsigned_extensions` / `allow_other_hostname` / `health_expect` |
| `internal/health/health.go` | 结构化 URL、禁重定向、连接复用、限流读取、`CheckWithExpect` |
| `internal/proxy/haproxy.go` | 拒绝跨库；本轮收紧为仅单分片 |
| `internal/supervisor/supervisor.go` | `buildShardCommand()` 抽出、`-unsigned` 显式开关、hostname 可配 |
| `cmd/quack-proxy/main.go` | `buildStatus()` token 脱敏 + `--show-tokens`、`writePrivateFile()`、SIGHUP 重生成 proxy 配置、`version=dev` |
| `*.go` / `.gitattributes` / `go.mod` | 统一 LF、对齐 1.25.0 |
| CI / release | 最小权限、`-race -cover` + 50% 门槛、govulncheck、SHA pin、cosign |

### 本轮（第三轮，收尾）
| 文件 | 变更 |
|------|------|
| `internal/supervisor/supervisor.go` | 四段式 `checkAll`、`startShard`/`launch`/`stopShard` 拆分、`opMu`、`restarting` 认领、并行 `StopAll`、锁外 status I/O、R-17 `lastStart` 语义 |
| `internal/config/config.go` / `internal/health/health.go` | `health_expect` 配置与 body 子串校验 |
| `internal/proxy/haproxy.go` | `singleBackendOnly()`，多分片一律拒绝 |
| `internal/supervisor/gate2_regression_test.go` | 3 个并发/退避/停机回归用例 |
| `internal/supervisor/integration_test.go` | **真实进程组 E2E**（起→kill→自愈→停，无孤儿） |
| `scripts/quack-e2e.sh` | **真实 DuckDB v2.0.0 + Quack E2E**：SHA256 pin、签名 core 扩展、token 正/负认证、GET / 2xx、healthy 状态、可选基准、清理校验 |
| `.github/workflows/ci.yml` / `release.yml` | 全 SHA pin、govulncheck pin、`quack-e2e` job、cosign keyless 签名 |
| `README.md` / `quack-proxy.example.yaml` / `docs/*.md` / `CHANGELOG.md` | 版本 pin、扩展预装、单机无副本、health_expect、E2E、供应链、状态收敛 |

### 第四轮（R-21 收尾，由最终复跑触发）

| 文件 | 变更 |
|------|------|
| `internal/supervisor/attr_unix.go` | 新增 `groupAlive(pid)`：`syscall.Kill(-pid, 0)` 探测整组是否仍有成员 |
| `internal/supervisor/attr_windows.go` | 新增 no-op `groupAlive()`；Windows 无进程组语义，属已记录的非目标（R-18） |
| `internal/supervisor/supervisor.go` | `stopShard` 在 `cmd.Wait()` 后 `waitGroupGone()` 确认整组退出，超时对整组补发 SIGKILL，仍存活则记 ERROR；新增 `waitGroupGone()` 轮询 helper |
| `internal/supervisor/integration_test.go` | `QUACK_STUB_IGNORE_TERM` 让真实子进程忽略 SIGTERM；新增 `TestStopAllReapsLingeringDescendants`：`StopAll` 返回后**立即**断言端口释放、进程组消亡、状态为 stopped（不做轮询） |
| `scripts/quack-e2e.sh` | 停止后端口检查改 30 × 0.2s drain 观察窗，避免把「正在退出」误判为通过；仍以最终释放为硬失败。另改为优先 `LOAD quack` 复用本地预装扩展、缺失才联网 `INSTALL`（验证时扩展 CDN 实测 HTTP 502），使 E2E 可离线运行 |

**为什么必须补这一轮**：R-15 的真实进程组集成测试里，stub 是 Go 测试二进制，父进程退出时子进程不会滞后；而真实分片是 `bash → duckdb + tail` 三层。只有真实引擎 E2E 能暴露「组长已回收、后代仍持端口」的差距——这正是「用 stub 全绿却漏掉 P0」的同一类陷阱在收尾阶段的重演。

### 第五轮（r5：R-07 / R-22 / 崩溃演练与 CI 门槛）

| 文件 | 变更 |
|------|------|
| `internal/supervisor/supervisor.go` | `processSpec` / `processSpecOf` / `initSQLFileDigest` / `shardSpecChanged`；`ShardProcess.initSQLDigest`；`ApplyConfig` 字段级 diff + 三返回值；`portInUse` + `startShard` 启动前预检 |
| `cmd/quack-proxy/main.go` | SIGHUP 日志增加 `restarted` 计数 |
| `internal/supervisor/gate5_reload_test.go` | 8 个 reload diff 回归用例（含生成 token 不误重启、幂等、策略文件内容变化） |
| `internal/supervisor/gate5_orphan_test.go` | 2 个端口占用回归用例 |
| `internal/supervisor/integration_test.go` | 真实进程组换端口 reload、第二 daemon 拒绝孤儿端口 |
| `scripts/daemon-crash-drill.sh` | **新增**：daemon SIGKILL 演练，两代 stub 用不同响应体判定假阳性，并断言 R-22 修复生效 |
| `.github/workflows/ci.yml` | 新增**必需** `crash-drill` job（无外部网络依赖） |
| `docs/*.md` / `README.md` / `CHANGELOG.md` | r5 状态收敛 |
| `scripts/verify-deploy.sh` | **r6 新增**：HAProxy / systemd 真实工具校验、正负例、真实转发 |
| `internal/proxy/haproxy.go` / `haproxy_test.go` | R-23 加固提示与回归用例 |
| `deploy/systemd/quack-proxy.service` | 定位文案修正、`Restart=` 注释补 R-22 |
| `.github/workflows/ci.yml` | 新增**必需** `deploy-verify` job |
| `docs/*.md` / `README.md` / `CHANGELOG.md` | r6 状态收敛（解除 r5 的「环境阻断」结论） |

### 第七轮（r7：systemd 实机演练 / R-23 加固 / 漏洞基线清零）

| 文件 | 变更 |
|------|------|
| `scripts/drill-stub.go` | **新增**（`//go:build ignore`，不进 `go build ./...` / 覆盖率）：读取 daemon 通过管道喂入的 boot SQL，解析 `quack_serve` 端口并起一个返回固定 greeting 的 HTTP 服务，供 systemd 演练使用 |
| `scripts/systemd-install-drill.sh` | **新增**：在真实 systemd PID 1 上安装 shipped unit，验证非 root 账户、`RuntimeDirectory` 属主/权限、0600 pid/status、healthy、token 脱敏、`systemctl stop` 全量清理；带「无 RuntimeDirectory 必失败」负例 |
| `internal/config/config.go` / `internal/proxy/haproxy.go` | 新增 `proxy.hardened` opt-in；`true` 时在 `global` 段输出生效的 `chroot` / `user` / `group`，默认仍只写注释 |
| `internal/proxy/haproxy_test.go` | 新增 `TestGenerateHAProxyHardenedEmitsDirectives`（69 个测试函数） |
| `scripts/verify-hardening.sh` | **新增**：预置账号/chroot + 真实 HAProxy 降权与转发验证，两个负例（未预置账号被拒、≥3.x 未加固告警）；按主版本显式跳过不适用的告警负例 |
| `.github/workflows/ci.yml` | 新增**必需** `systemd-drill` 与 `vuln-scan` job；`deploy-verify` 增加 R-23 必需步骤；`go` 矩阵加 1.27；移除原 advisory `govulncheck` 步骤 |
| `.github/workflows/release.yml` | 固定 `go-version: '1.27.4'`，发布产物与漏洞扫描使用同一套补丁 stdlib |
| `quack-proxy.example.yaml` / `docs/*.md` / `README.md` / `CHANGELOG.md` | r7 状态收敛与用法说明 |

**本轮再次更正上一轮的「环境限制」结论**：r6 写「环境无 systemd，非 root 安装演练未做」。实际是容器启动参数不对 —— Docker Desktop / WSL2 下用 `--privileged --cgroupns=host` 并把 `/sys/fs/cgroup` 以 **rw** 挂成 cgroup2，`systemd 255` 即 `is-system-running=running`；用 `--cgroupns=private` 会以 `Failed to create /init.scope control group: No such file or directory` 退出。r5 把 apt 被拦当成装不上工具、r6 把 cgroup 命名空间不对当成没有 systemd，**是同一类误判的连续两次重演**，已写入 SCOPE §8 决策记录。

### 第八轮（r8：LAN 明文实证 / E2E 依赖加固）

| 文件 | 变更 |
|------|------|
| `scripts/verify-plaintext.sh` | **新增**：一条 Docker bridge 隔离网段 + 真实 daemon（`bind_host: 0.0.0.0`）+ 独立客户端容器 + **独立嗅探容器**（`--network container:<server>` 共享 netns，`AF_PACKET` 解析原始以太网帧）。断言明文端口的 TCP payload 含 token / `Authorization: Bearer` / SQL 正文；TLS 控制端口 canary 不可见（负例）；真实 CLI 拒绝 `proxy.ssl.enabled=true`；`status.json` 0600 且 token 明文落盘 |
| `scripts/drill-stub.go` | 新增 `DRILL_STUB_BIND` 环境变量（默认仍 `127.0.0.1`），供明文审计把 stand-in 端点暴露在容器接口上；systemd 演练行为不变 |
| `.github/workflows/ci.yml` | 新增**必需** `plaintext-audit` job；`quack-e2e` 的 `continue-on-error` 改为 `${{ github.event_name == 'pull_request' }}`，push main / 手动触发为必需 |
| `scripts/quack-e2e.sh` | 下载与 `INSTALL quack` 各 3 次重试，失败时输出明确诊断；**不回退** `-unsigned` / community 源，保住签名扩展的供应链约束 |
| `docs/*.md` / `README.md` / `CHANGELOG.md` | r8 状态收敛 |

**本轮的教训与前两轮不同**：r5~r7 把「LAN 明文抓包」列在「仍未做」里，并没有断言环境不允许——只是没做。本轮证明两台容器 + 一个 `AF_PACKET` 原始套接字即可实证，不需要第二台物理机。**「未做」被清单化久了会默认变成「做不到」**；清单应逐条重问「现在真的做不到吗」。同时明确区分：**审计通过 ≠ 风险已修**，`plaintext-audit` 断言的是当前已知弱点，若将来加了 TLS 终止，该门禁会（正确地）失败，必须同步改断言与文档，而不是删掉它。

---

## 5. 验证记录（2026-11-13 ~ 11-16 实机）

本机无 gcc 且 WSL 被安全策略禁用，改用 **Docker `golang:1.25`（Linux，go1.25.14，gcc）** 跑竞态检测与真实引擎；Windows 侧用便携 Go 1.25.0。

| 命令 / 检查 | 结果 |
|------|------|
| `gofmt -l .` | 空 |
| `go vet ./...` | 通过 |
| `go test ./... -count=1`（Windows） | 5 包全绿，POSIX 用例显式 SKIP |
| `go test ./... -race -count=1`（Linux 容器，r5 最终重跑） | **5 包全绿**；含 R-21 孤儿回收、R-07 真实换端口 reload、R-22 第二 daemon 拒绝孤儿端口 |
| `go test ./internal/supervisor -race -run '^TestStopAllReapsLingeringDescendants$' -v` | **PASS 2.13s**；忽略 SIGTERM 的真实后代被整组回收，证明 SIGKILL 兜底路径被执行（R-21） |
| `go test ./... -cover`（r6 重跑） | cmd 23.0% · config 93.8% · health 90.9% · proxy **97.8%**（r6 新增 R-23 用例）· supervisor **60.0%**；68 个测试函数；CI 有 50% 总门槛 |
| 交叉构建 | linux / darwin / windows amd64 通过 |
| YAML 语法 | ci.yml / release.yml / example.yaml 经 `yaml.v3` 实解析通过 |
| CLI E2E | `version`=dev；跨库 `gen-proxy` 拒绝且不落文件；单分片生成；`status --json` 不含 token、`--show-tokens` 才输出 |
| 真实进程组 E2E | 起 → HTTP 健康 → 外部 `SIGKILL` 整组 → 自愈（新 PID、Restarts=1）→ `StopAll` 后端口释放、无孤儿 |
| **真实 DuckDB + Quack E2E** | `ALL_QUACK_E2E_CHECKS_PASSED`：pinned v2.0.0 + SHA256 OK、签名 core quack、`GET /` HTTP 200 greeting、health loop 报 healthy、正确 token `ATTACH/SELECT` 返回 42、错误 token 被拒、50 次往返 221ms (~226 qps)、stop 后端口释放 |
| **R-21 修复后 3 轮真实引擎复跑** | 容器内离线预装签名扩展后连续 3 轮（19491 / 19512 / 19513），每轮：正确/错误 token 认证、`GET /` 200、healthy、停止后 `port released, no orphan listeners`；**3/3 `ALL_QUACK_E2E_CHECKS_PASSED`**，其中第 1 轮 50 次往返 312ms (~160 qps)。此前偶发的 `FAIL: port ... still open after stop` 不再出现 |
| `bash -n scripts/quack-e2e.sh` | 通过（离线预装改动后重验） |
| `go test ./internal/supervisor -race -run '^TestIntegrationReloadSwitchesRealPort$' -v` | **PASS 0.22s**：真实子进程组释放旧端口并绑定新端口，证明 R-07 不只是内存里的 diff |
| R-22 三项 `-race -v` | 3/3 PASS：`TestPortInUseDetectsListener`、`TestStartShardRejectsOccupiedPort`、`TestIntegrationSecondDaemonRefusesOrphanedPort` |
| `bash scripts/daemon-crash-drill.sh`（Linux 容器） | 修复前 `FALSE_HEALTHY=1`（`healthy` 由孤儿应答）；修复后 `RESTART_DAEMON_ALIVE=0`、`RESTART_REFUSED_ON_ORPHAN_PORT=1`、`FALSE_HEALTHY=0`、`DAEMON_CRASH_DRILL_DONE` |
| `bash -n scripts/daemon-crash-drill.sh` / `ci.yml` 新增 job 的 YAML 实解析 | 通过 |
| **r5 真实 DuckDB E2E 复跑** | **5 轮 `ALL_QUACK_E2E_CHECKS_PASSED`**（30 次往返 148 / 155 / 162 / 323 / 148 ms，~92–202 qps）；另有 1 轮冷启动超过脚本原 30s 等待窗口报 `Quack endpoint never bound`，已把窗口加长到 60s 并在失败时输出 boot SQL / status.json / 进程现场，随后 3 次全新复跑 3/3 通过 —— 判定为冷启动时序偶发，非端口预检回归 |
| **r5 自愈 vs 端口预检** | `SIGKILL` 分片整组后 daemon 在 ~31s 自愈（`restarts=1`、端点恢复、停止后端口释放），证明新增的 `portInUse` 不会误伤健康循环的正常重启 |
| 单写者冲突 | 两个进程打开同一 `.db` → `IO Error: Could not set lock on file`（据此收紧 R-04） |
| **r6 真实 HAProxy 3.4.7**（Alpine `apk`） | `bash scripts/verify-deploy.sh --only haproxy --forward` → `HAPROXY_CONFIG_VALID=1 (explicit confirmation)`、`HAPROXY_NEGATIVE_CONTROL_OK=1`、`HAPROXY_FORWARD_OK=1 (frontend 19600 -> backend 19601)`、`DEPLOY_VERIFY_PASSED` |
| **r6 真实 systemd 255**（Ubuntu noble 离线 `.deb`） | `bash scripts/verify-deploy.sh --only systemd` → `SYSTEMD_UNIT_VALID=1`、`SYSTEMD_NEGATIVE_CONTROL_OK=1`、`DEPLOY_VERIFY_PASSED`；负例 `KillMode=TOTALLY_BOGUS` 输出 `Failed to parse kill mode specification`（该工具对部分解析错误**仍返回 0**，故脚本断言文本而非只看退出码） |
| `go test ./... -race -count=1`（r6 最终 Linux 容器重跑） | **5 包全绿**，含 r6 新增 `TestGenerateHAProxyDocumentsRootHardening` |
| `bash -n`（r6 时点 3 个脚本）/ YAML 三份实解析 / `git diff --check` | 全通过（历史快照；r7 已扩展到 7 个脚本） |
| **r7 真实 systemd 255 非 root 安装演练**（`jrei/systemd-ubuntu:24.04` + `--privileged --cgroupns=host` + cgroup2 rw） | `SYSTEMD_DRILL_NEGATIVE_CONTROL_OK=1`（`permission denied` 创建 `/var/run/quack-proxy`）、`SYSTEMD_SERVICE_ACTIVE=1`、`SYSTEMD_SHARD_HTTP_OK=1`、`SYSTEMD_STATUS_HEALTHY=1`、`SYSTEMD_SERVICE_NONROOT=1`（MainPID 属主 `quack-proxy`）、`SYSTEMD_RUNTIME_DIR_OK=1`（`quack-proxy:quack-proxy 0750`，pid/status 0600）、`SYSTEMD_TOKEN_REDACTION_OK=1`、`SYSTEMD_STOP_CLEAN=1` → `SYSTEMD_INSTALL_DRILL_PASSED`，**3/3** |
| **r7 真实 HAProxy 加固验证**（Alpine，HAProxy 3.4.7 / 2.8.31） | `HARDENING_DIRECTIVES_EMITTED=1`、`HARDENING_PREREQUISITES_OK=1`（uid=100、chroot root:root 0755）、`HARDENING_UNPROVISIONED_REJECTED=1`（`cannot find user id`）、`HARDENING_NEGATIVE_CONTROL_OK=1`（3.4.7 未加固告警）、`HARDENING_DROPPED_ROOT=1`（real uid=100、无 root 告警）、`HARDENING_FORWARD_OK=1`（19620 → 19621 `quack-hardening-ok`）→ `HARDENING_VERIFY_PASSED`。3.4.7 **3/3**；2.8.31 **1/1** 且 `HARDENING_ROOT_WARNING_CONTROL_SKIPPED=1` 按主版本显式跳过 |
| **r7 govulncheck 基线清零** | `govulncheck@v1.9.0` + `GOTOOLCHAIN=go1.27.4` → `No vulnerabilities found` / **SCAN_RC=0**（对照 r5 的 `go1.26.11` 11 项 stdlib 告警）。新增必需 `vuln-scan` job；release 固定 `go1.27.4` |
| `go test ./... -race -count=1` / `-cover`（r7，Linux 容器） | 5 包全绿；**69** 个测试函数；cmd 27.0% · config 93.8% · health 90.9% · proxy **98.3%** · supervisor **80.1%**；`go tool cover` 总计 **71.1%**；gofmt / `go vet` 全绿 |
| `bash -n`（7 个 shell 脚本）/ YAML 三份 `yaml.v3` 实解析 / 三平台交叉构建 / `git diff --check`（r7） | 全通过；补齐 `.gitattributes` 的 LF 覆盖并统一工作区历史 CRLF 后，`scripts/quack-start.sh` 等 7 个脚本全部通过 |
| **r8 LAN 明文抓包实证**（`scripts/verify-plaintext.sh`） | 默认镜像 `python:3.11-slim` **3/3** `PLAINTEXT_AUDIT_PASSED`，换 `jrei/systemd-ubuntu:24.04` **1/1**；明文端口捕获 366~367 B 含 `qp-lan-cleartext-token` / `Authorization: Bearer` / `SELECT * FROM secrets`（`PLAINTEXT_TOKEN_VISIBLE=1`）；TLS 控制端口捕获 5009 B 但两枚 canary 均不可见（`TLS_CONTROL_HIDDEN=1`）；真实 CLI 报 `proxy.ssl.enabled is not implemented`（`PRODUCT_TLS_UNAVAILABLE=1`）；`status.json` mode 600、token 明文落盘（`STATUS_TOKEN_AT_REST=1`）。**负例**：TLS 通道、以及「嗅探器失灵 → 捕获文件为空」都会让脚本失败 |
| **r8 `quack-e2e` 重试加固** | 下载 / `INSTALL quack` 各 3 次重试；CI advisory 仅保留在 `pull_request` |
| `bash -n`（8 个 shell 脚本）/ YAML 三份 `yaml.v3` 实解析 / 三平台交叉构建 / `git diff --check`（r8） | 全通过；含新增 `scripts/verify-plaintext.sh` 与改动后的 `scripts/quack-e2e.sh` |
| **r9 真实 Windows 实证与 R-18 收口** | 本机 MINGW64_NT-10.0-19045 两组 PATH（纯净 Windows / Git Bash）均复现旧产物 `version` 正常、`start` 报 `duckdb_bin ... not executable`；根因 `lookupBin` 的 POSIX mode 位。修复后 `scripts/verify-windows.sh` → `WINDOWS_RUNTIME_GUARD_PASSED`（`version` 可用、`start` 含 R-18/WSL2、4 个负例全过、无 pid 残留）。`release.yml` 撤下 windows 产物，CI 新增必需 `windows-runtime-guard` |
| **r9 真实引擎 E2E 完整复跑** | 主机 HTTPS 取 pinned CLI（SHA256 与 pin 一致）+ 官方签名 core 扩展离线喂入容器 → `ALL_QUACK_E2E_CHECKS_PASSED`：DuckDB v2.0.0、正确/错误 token、健康循环、50 次 SELECT 207 ms（~241 qps）、停止后端口释放无孤儿 |
| `go test ./... -race -count=1` / `-cover`（r9，Linux 容器） | 5 包全绿；**71** 个测试函数；总计 **70.9%**（cmd 27.0% / config 93.8% / health 90.9% / proxy 98.3% / supervisor 80.1%）；较 r7 的 71.1% 略降因新增 Windows 运行时分支在 Linux 不可达 |
| **r9 两条真实门禁复跑** | `SYSTEMD_INSTALL_DRILL_PASSED`（含负例，真实 systemd 255）与 `PLAINTEXT_AUDIT_PASSED`（4 项断言全 1）均 RC=0 |
| `bash -n`（9 个 shell 脚本）/ `GOOS=windows go test -c ./internal/supervisor` / YAML 三份 `yaml.v3` 实解析 / 三平台交叉构建 / `git diff --check`（r9） | 全通过 |
| **r10 口径更正：严格离线实证** | 首次用 `--network none` 复跑 r9 的「离线」路径，磁盘库分片报 `DuckDB currently has a read-only crypto module loaded. Please ensure httpfs is loaded` → 证明 r9 容器仍联网、由 DuckDB autoload 下载了 `httpfs`。判别实验：同一无网络容器里 `:memory:` 数据库正常，只有磁盘库触发该错误。修法是把签名 `httpfs` 一并 pin 进缓存放进隔离 HOME（文件存在即可被 autoload 命中，**不改 boot SQL、不用 `-unsigned`**）。同时新增生产运维约束：离线部署必须预装 `quack` + `httpfs` |
| **r10 无网络容器全链路**（`golang:1.25`，`--network none`） | `prefetch-quack-e2e.sh` 的 cache-miss / cache-hit / `QUACK_PREFETCH_OFFLINE=1` 三条路径全通（三个 SHA256 校验 `OK`、`OFFLINE_CACHE_USED=1`）；`QUACK_E2E_OFFLINE=1 quack-e2e.sh --cache-dir … --benchmark 50` → `ALL_QUACK_E2E_CHECKS_PASSED`：DuckDB v2.0.0、`LOAD quack` 验签、正确/错误 token、HTTP 200 + healthy、50 次 SELECT 230 ms（~217 qps）、停止后端口释放无孤儿 |
| **r10 四个负例**（同无网络容器） | 空缓存 + `OFFLINE=1`、无 `--cache-dir` + `OFFLINE=1`、篡改 `httpfs` 条目、prefetch 严格离线断言遇不完整缓存 —— **4/4 非零退出**，且输出中不含 `ALL_QUACK_E2E_CHECKS_PASSED` / `all shards started` / `PREFETCH_CACHE_HIT`（即绝不静默回退联网）。另有不预装 httpfs 时磁盘库写失败的判别负例 |
| `bash -n`（10 个 shell 脚本）/ YAML 三份 `yaml.v3` 实解析 / `git diff --check` / Go 全量回归（r10） | 全通过；Go 代码本轮未改动，`gofmt` / `go vet` / `go test ./... -race -count=1` 复跑 5 包全绿、总计 **70.9%** |
| **r11 HAProxy + VIP 真实联调**（`scripts/verify-vip-failover.sh`） | 两个 `haproxy:2.8-alpine` 节点（`--user root` + `NET_ADMIN`）+ 独立客户端，Docker bridge 上真实 `/32` VIP 由 A 漂移到 B（GARP，客户端不做 ARP flush）。真实 CLI 生成 1004 B 配置，`haproxy -c -f` 接受；`HAPROXY_CONFIG_VALID=1` / `GEN_PROXY_MULTI_SHARD_REJECTED=1` / `VIP_ABSENT_CONTROL_OK=1` / `VIP_PRIMARY_NODE=NODE_A` → `VIP_FAILOVER_NODE=NODE_B` / **`SESSION_DISCONTINUITY=1`** / `VIP_REMOVED_CONTROL_OK=1` → `VIP_FAILOVER_DRILL_PASSED`。两个不同网段（`172.29.83.0/24`、`172.29.91.0/24`）各 RC=0，退出后无容器/网络残留。**额外变体负例**：两节点返回同一标识时脚本 `FAIL: VIP did not fail over to node B`、rc=1、无 PASS 标记，证明身份断言非空转。CI 新增必需 job `vip-failover` |
| `bash -n`（11 个 shell 脚本）/ YAML 三份 `yaml.v3` 实解析 / 三平台交叉构建 / Go 全量回归（r11） | 全通过；`gofmt` / `go vet` / `go test ./... -race -count=1` 5 包全绿、总计 **70.9%**（cmd 27.0% / config 93.8% / health 90.9% / proxy 98.3% / supervisor 80.1%）；`git diff --check` 干净 |
| **r12 引擎原生 TLS 能力实证**（`scripts/verify-engine-tls.sh`，`--network none` 容器） | pinned v2.0.0 CLI / `quack` / `httpfs` 三 SHA256 `OK`、`OFFLINE_CACHE_USED=1`；`ENGINE_TLS_PARAMS_PRESENT=1`、`CLIENT_FINGERPRINT_PARAM_PRESENT=1`、`SELF_SIGNED_KEYS_GENERATED=1`、`GENERATED_KEY_VALID=1`；`TLS_LISTEN_URL=https://127.0.0.1:19911` vs `PLAIN_LISTEN_URL=http://127.0.0.1:19912`（`ENGINE_URL_SCHEME_SWITCH=1`）；`TLS_HANDSHAKE_OK=1 (TLSv1.3)`、`PLAINTEXT_REJECTED_ON_TLS_PORT=1`、`HTTPS_GET_OK=1 (HTTP 200)`；`FINGERPRINT_PINNED_QUERY_OK=1 (SELECT 42 -> 42 over TLS)`、`WRONG_FINGERPRINT_REJECTED=1`、`MISSING_FINGERPRINT_REJECTED=1`、`WRONG_TOKEN_REJECTED_OVER_TLS=1`、`PLAINTEXT_CONTROL_ANSWERS=1` → **`ENGINE_TLS_CAPABILITY_PASSED`**，RC=0。产物仍硬编码 `disable_ssl=true`（`PRODUCT_DISABLES_ENGINE_TLS=1`）。CI 新增必需 job `engine-tls-capability` |
| **r12 负例**（同严格离线脚本） | 4/4 非零退出且无 `ENGINE_TLS_CAPABILITY_PASSED`：`--control-variant`（TLS 端点被强制成明文）→ `FAIL: TLS endpoint did not report an https:// URL (got 'http://…')`、rc=1；`QUACK_TLS_OFFLINE=1` 无 `--cache-dir` → rc=1；篡改 `httpfs` 缓存 → `FAIL: offline cache missing/corrupt`、rc=1；空缓存 → rc=1。变体负例证明 URL 判别断言非空转 |
| `bash -n`（12 个 shell 脚本）/ YAML 三份 `yaml.v3` 实解析 / 三平台交叉构建 / Go 全量回归（r12） | 全通过；本轮 Go 源码未改动，`gofmt` / `go vet` / `go test ./... -race -count=1` 5 包全绿、总计 **70.9%**；`git diff --check` 干净 |
| **r13 CI 三段链路严格离线复跑**（单个 `--network none` 容器） | 按 `.github/workflows/ci.yml` 的真实顺序：① `QUACK_PREFETCH_OFFLINE=1 prefetch-quack-e2e.sh` → `PREFETCH_CACHE_HIT`（三 SHA256 `OK`）、RC=0；② `QUACK_E2E_OFFLINE=1 quack-e2e.sh --benchmark 50` → `ALL_QUACK_E2E_CHECKS_PASSED`（DuckDB v2.0.0、验签 `LOAD quack`、正确/错误 token、HTTP 200 + healthy、50 次 SELECT 203 ms / ~246 qps、停止后端口释放无孤儿）、RC=0；③ `QUACK_TLS_OFFLINE=1 verify-engine-tls.sh` → `ENGINE_TLS_CAPABILITY_PASSED`、RC=0 → `OFFLINE_CI_CHAIN_PASSED`。证明 `needs: quack-e2e` + 同一 cache key 的**串联**成立，而非两个脚本各自孤立能跑 |
| **r13 CI 契约测试**（`internal/cicontract`，随必需 `go test ./...`） | 真实 `ci.yml` / `release.yml` 解析通过：9 个必需门禁齐全、无 `continue-on-error` / 无 job 级 `if`、两工作流共 28 处 `uses:`（`ci.yml` 23 + `release.yml` 5）全为 40 位 hex SHA、两个 `actions/cache` 步骤 `path`/`key` 完全一致且 key 含三个 pin 前缀、`needs` 链正确、离线开关齐全 → `TestCIWorkflowContract` PASS。**9 个单点篡改负例全部按预期失败且只触发对应的那一个检查**：改 `needs`、只改下游 key、key 去掉 pin、关 `QUACK_TLS_OFFLINE`、加回 `continue-on-error`、加 `if: false`、删 `vip-failover` job、action 改 tag pin、release action 改 tag pin |
| `bash -n`（12 个 shell 脚本）/ YAML 三份 `yaml.v3` 实解析 / 四组合交叉构建 / Go 全量回归（r13） | 全通过；`gofmt` / `go vet` / `go test ./... -race -count=1` **6 包全绿**、**74** 个测试函数、`go tool cover` 总计 **72.2%**（cmd 27.0% / cicontract 80.5% / config 93.8% / health 90.9% / proxy 98.3% / supervisor 80.1%）；linux·darwin(amd64/arm64)·windows 交叉构建通过；`git diff --check` 干净 |

**仍未做的验收（不得当作已完成）**：R-05 的 `disable_ssl` 硬编码 / token 不落盘 / **产品接入 TLS** 属**需设计决策**（r8 的抓包门禁只证明弱点、r11 的 VIP 联调与 r12 的引擎能力审计同样不修风险）。**r12 已解除其中一项长期被误读的前提**：「内建 TLS」不再是「引擎能否支持 TLS」的问题——引擎原生支持已实证；剩余决策被收窄为「证书/指纹分发、健康探测、status.json 契约、HAProxy 配置」的产品接线设计，详见 §2 R-05。真 HA（control plane 冗余 / 跨机发现 / Coordinator 常驻）属新产品立项。另有 r10 如实记录的残余风险：CI 首次 cache miss 仍需访问上游一次（runner 侧、带重试），但**不再有 advisory 逃生口**，且缓存命中后测试进程零网络。**r13 已解除**：「共享缓存 key / pin / `needs` 是否一致」从注释变成机器可验证契约，且三段链路已在 `--network none` 容器按 CI 顺序复跑；但**这不消除 r10 的首次 cache miss 依赖**，两者不是同一件事。**r11 已解除**：HAProxy + VIP 联调（注意口径：证明的是**生成配置在浮动 IP 入口下可用**，不是 quack-proxy 具备 HA；脚本实证 `SESSION_DISCONTINUITY=1`，R-04 仍成立）。**r10 已解除**：`quack-e2e` 在 PR 上的 advisory。**r9 已解除**：Windows 产物去留（撤下发布产物 + 必需门禁）、真实引擎 E2E 完整复跑（r10 进一步收紧为严格离线）。**r8 已解除**：LAN 明文抓包实证。**r7 已解除**：非 root systemd 安装演练、R-23 生产加固、govulncheck 基线清零转必需。

**r6 更正 r5 的「本环境受阻」结论**：r5 三次尝试只走 apt（Debian trixie-security / Ubuntu noble / HTTPS 源），全部 502 或 TLS 握手失败，据此判定「无法安装工具」是**把通道受限误当成能力受限**。r6 改走本地已有 `alpine:latest` 的 `apk`（HTTPS CDN）装 HAProxy 3.4.7，以及主机 HTTPS 下载 Ubuntu noble `systemd` / `libsystemd-shared` / `libapparmor1` / `libkmod2` 的 `.deb`、容器内离线 `dpkg-deb -x` 取得 systemd 255，两项真实校验全部跑通。结论：**上一轮的未完成项不构成环境限制**。

**r9 的同类更正**：① 「Windows 产物去留」连标四轮「属产品决策」，实际 PRD §8 从 v0.1 起就是 non-goal —— **决策早有，缺的是执行**；本机即 Windows，一次 `version` + `start` 实测就发现失败原因还是错的（POSIX mode 位误判），比单纯「不支持」更误导。② 容器内 `INSTALL quack` 卡住被记成「外部 CDN 不稳定」，本轮主机通道 48s / 5s 下完 pinned CLI 与签名扩展，E2E 完整通过 —— **下载慢 ≠ 通道被拦**，重试与 advisory 范围据此重新界定。

**r10 的同类更正，但指向自己**：r9 写「离线喂入 pinned CLI + 签名扩展后完整跑通」，把「产物离线」当成了「运行离线」。r10 用 `--network none` 才暴露 DuckDB 对 `httpfs` 的隐式 autoload 依赖 —— **上一轮的结论不是错在结果，而是错在用词比证据更强**。这是 r5/r6「通道受限误记为能力受限」、r8「未做被清单化成做不到」之后的第三种形态：**「验证通过」被写成了比实际验证条件更宽的口径**。修法不是删掉 r9 的结论，而是补上缺失的 pin 并把条件收紧到可复现。另外，本机 CDN 对 `httpfs` 对象反复在 ~8 MB 处断流（完整 8.66 MB），最终用精确 `Range: bytes=8244838-` 拿到 412631 字节尾段拼接、gzip 与解压双 hash 校验通过；这再次说明「下载慢/断流」不等于「拿不到」。

**r11 回到第一种形态，但把理由写具体**：「HAProxy + VIP 联调」连续多轮被写成「需真实浮动 IP / 双机网络」，读起来像环境限制，实际只是没试。一个 Docker bridge + `NET_ADMIN` + 显式 `--user root` 就能让真实 `/32` 地址在两容器间漂移、由 GARP 收敛。两个真实踩坑都是参数问题而非能力问题：`haproxy:2.8-alpine` 默认 uid 99 使 capability 失效（`RTNETLINK: Operation not permitted`）；Git Bash 的 MSYS 把容器内 `-w /work` 改写成宿主路径。**同时要防反向误读**：联调通过绝不等于「项目现在有 HA 了」——VIP 是脚本搬的，切换后后端身份变了；脚本把这个「不连续」做成断言（`SESSION_DISCONTINUITY=1`）而不是藏起来。变体负例（两节点同标识 → 脚本失败）保证这条断言确实在检查。

**下一阶段（若继续，均需甲方拍板的设计决策，非本轮整改可执行项）**：① **R-05 产品接线**：r12 已证明引擎原生 TLS 可用，待决策的是证书生命周期与私钥存放、向客户端分发/轮换 SHA256 指纹的通道、健康探测改 HTTPS 后的证书校验策略、`status.json` 的 `attach_sql`/token 契约、以及 `gen-proxy` 生成的 HAProxy 前端与 `proxy.ssl` 死配置的处置；同时决定是否实现 PRD §4.2 的「token 不落盘 + 显式开关」（当前 `status.json` 是运维取得自动生成 token 的唯一通道，改它必须同时给出替代分发方式）。② 真 HA 属新产品立项，需重做含验收标准的 PRD + 架构 RFC。③ 若将来升级 DuckDB，需同步刷新 CLI / `quack` / `httpfs` 三组 pin 与 CI cache key。**r7 ~ r13 已完成的不再列出**：非 root systemd 实机演练、R-23 加固、govulncheck 清零转必需、LAN 明文抓包实证、Windows 产物收口、`quack-e2e` PR 转必需、HAProxy + VIP 联调、引擎原生 TLS 能力实证、CI 门禁契约测试与离线链路复跑。

**r12 是本轮方法论的第四次更正，但方向相反**：r5/r6 把「通道受限」误记为「能力受限」，r8 把「未做」误当成「做不到」，r9 发现「决策早有、缺的是执行」，r10 指出「结论口径比证据更宽」。r12 则是把**「产品选择」误写成了「能力受限」**——「内建 TLS 属需设计决策」连标多轮，从没查过项目自己 pin 的引擎到底支不支持 TLS。一次 `--network none` 容器 + 三组已缓存的 pinned 产物就证明引擎原生支持，且 `disable_ssl = true` 是产品主动关掉的。**教训：清单里的「需设计决策」也要逐条拆开——其中哪些是「能力不具备」，哪些是「能力具备但要定契约」；两者对甲方的工作量估算完全不同。** 同时防反向膨胀：实证引擎支持 TLS 绝不意味着 quack-proxy 已具备 TLS，脚本用 `PRODUCT_DISABLES_ENGINE_TLS=1` 把这条边界钉死。

**r13 是第五种形态：把「写在注释里的契约」误当成「已保证」**。r12 新增的 `engine-tls-capability` 靠 `needs: quack-e2e` 与**同一个** cache key 串起来，这件事只有 CI 注释、`README` 和本台账在说；没有任何门禁会在「改了 pin 却没换 key」「给 job 加个 `if`」「某个 action 改回 `@v4`」时当场拒绝。这和 r5 的 `continue-on-error` 吞掉失败是同一类问题：**配置层面的退化不会让测试变红，只会让保证悄悄失效**。修法不是再加一个 CI job（那会多一次下载与一轮 doc 收敛），而是把它做进**已经必需**的 `go test ./...`：解析真实工作流文件、断言契约、并用 9 个单点篡改负例证明每个断言真的在检查。**教训：「文档声明 X」和「X 被机器验证」不是一回事；凡是两个组件共享一个不变量（此处是缓存 key/pin），就该有一条检查盯住这个不变量。** 同时防反向误读：契约通过**不消除** r10 的「首次 cache miss 仍需上游一次」，也**不改变** R-05 的明文与 token 落盘风险；它只保证「缓存一旦存在，三段链路严格离线且共享同一份已校验产物」。

**r5 同时修掉一个被 advisory 掩盖的 CI 配置错误**：`govulncheck@v1.9.0` 要求 Go ≥ 1.26，但步骤固定在 `matrix.go == '1.25'`，`go install` 每次失败又被 `continue-on-error` 吞掉，等于从未运行。当时移到 1.26 job 后工具才能正常运行并产出 11 项真实告警供人工分诊；**r7 已把该步骤整体移除**，改为固定 `go1.27.4` 的必需 `vuln-scan` job（零告警基线），并在 `release.yml` 固定同一工具链。

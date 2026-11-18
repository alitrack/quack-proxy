# quack-proxy PRD（实现状态跟踪版）

> Product Requirements Document | 原始稿 v0.1 | 2026-05-29
> Author: alitrack
> 状态修订 2026-11-13（依据六领域专家评审）；最近更新 2026-11-18（**r16：收口 r15 结论在正文的残留措辞** —— §3.3 / §7 / §9 的历史 `⚠️` 批注补上时点声明并指向头部状态表，消除 R-17 仍被写成「待设计决策」的自相矛盾；产品状态、门禁、代码均未变。**r15：按 §4.2 与 §8 既有决策验收「外部代理终止 TLS」部署方案，并把「产品接入 TLS 属需甲方拍板」的口径纠正**，见下表「外部 TLS 终止部署验收」与 §4.3；r14：执行 §4.2 既有修订要求 —— token 默认不落盘 + 显式开关；r13：CI 门禁契约机器可验证；r12 引擎原生 TLS 能力实证见「内建 TLS 能力边界」与 §4.1）
>
> **本文不再作为愿景稿使用。** 原始 v0.1 内容保留作背景，但所有功能声明必须对照下表与 `docs/SCOPE.md`。凡标记「未实现 / 已放弃」的条目，README 不得再作为既有能力宣传。边界与成功判据以 `SCOPE.md` 为准，缺陷与整改进度以 `REMEDIATION.md` 为准。

## 实现状态总览

| 能力 | 原始章节 | 状态 | 说明 |
|------|----------|------|------|
| 一份 YAML 启动 N 个 DuckDB+Quack 子进程 | §2 §3.2 | 已实现 | 单机 |
| 健康检查 + 自动重启 + 退避 + 停泊 | §3.3 | **已修** | `Restarts` 清零、stopped 可经 reload 恢复、重启不刷新 grace 使退避真正生效（R-02 / R-17）；探测并发且锁外（R-03） |
| `start` / `stop` / `status` / `version` | §5 | 已实现 | |
| `reload` 增量更新 | §3.3 §5 | **已修（r5）** | 按名称增删 + 复活 stopped + 重生成 HAProxy 配置 + **字段级 diff**：分片字段变化只滚动重启该分片，影响进程的全局参数变化重启全部，健康类参数不重启；`init_sql_file` 内容变化也纳入摘要比对（R-07）。HAProxy 进程本身仍须运维手动 reload |
| `gen-proxy` 生成 HAProxy 配置 | §4.1 §5 | **已修** | 单机不存在副本：不同 `.db` 是分片，相同 `.db` 触发 DuckDB 文件锁冲突。仅允许单分片端口转发，多分片一律拒绝（R-04 / R-20） |
| `status --json` 的 `coordinator_attach_sql` | §5.2 | **已修**（Gate 0） | 使用可连接地址；token 默认脱敏，需 `--show-tokens`（R-10 / R-05） |
| 常驻 Coordinator / 统一查询入口 VIP | §2 §5.2 §6 | **未实现** | 只生成 ATTACH SQL 片段，无进程、无端口、无路由 |
| 跨机器联邦 / 端点发现 | §1 §2 | **未实现** | §8 已列为 non-goal，与 §1 / §2 矛盾 |
| 「高可用集群」 | §2 | **已放弃** | daemon 是 SPOF，见 `SCOPE.md` §1 |
| `proxy.ssl` | §4.1 | **显式拒绝** | Gate 1 起 `enabled: true` 在 config 加载期报错，不允许「看起来开了 TLS」；真正实现仍需外部终止或后续设计 |
| Windows 支持 | §7 §8 §10 | **已放弃并收口（r9）** | 三处章节互相矛盾（R-18）。r9 在真实 Windows 上实证产物不可用后：**release 不再发布 Windows 二进制**、修掉 `lookupBin` 的 POSIX mode 位误判、二进制启动时显式拒绝并给出 WSL2 替代路径，新增必需 CI job `windows-runtime-guard` |
| 性能：83K / 169K QPS | `benchmark.md` | **口径错误，已诚实化** | README 改为 historical benchmark 并注明测量对象；新增可复现的真实 `ATTACH/SELECT` 基准脚本与实测数字（R-08） |
| 真实引擎 / 进程组 E2E | — | **已补齐** | Go 集成测试（真实进程组，`-race` 通过）+ `scripts/quack-e2e.sh`（真实 DuckDB v2.0.0 + Quack + token 认证，实跑通过）（R-15） |
| 供应链：Actions pin / 产物签名 / 版本 pin | — | **已补齐** | 全 SHA pin、cosign keyless 签名、govulncheck 版本 pin、DuckDB v2.0.0 + SHA256、签名 core 扩展（R-14 / R-16 / R-19） |
| 并发安全：锁内阻塞 / 串行停机 | §3.3 | **已修** | `checkAll` 四段式锁外化、并行 `StopAll`（R-03 / R-12） |
| 停止后分片端口释放 / 无孤儿进程 | §3.3 §5 | **已修** | 由最终真实引擎 E2E 复跑发现：`cmd.Wait()` 只回收 bash 组长，DuckDB 后代仍持端口。现按进程组确认退出 + 超时整组 SIGKILL，并有忽略 SIGTERM 的回归用例（R-21） |
| daemon 崩溃后的重启行为 | §3.3 §6 | **已修（检测侧）** | 演练证实孤儿分片会留占端口，直接重启会报假 `healthy`；r5 起 `startShard` 预检端口占用并 fail loud（R-22）。孤儿自动接管属真 HA，**未实现** |
| 部署产物经厂商真实工具校验 | — | **已补齐** | r6：`scripts/verify-deploy.sh` 用 HAProxy 3.4.7 跑 `haproxy -c -f` + 真实转发 + 负例，用 systemd 255 跑 `systemd-analyze verify` + 负例；CI 必需 job `deploy-verify`（r5 曾误判为「环境阻断」） |
| 生成的 HAProxy 配置以 root 运行 | — | **已补齐可验证加固路径（默认 opt-in）** | r6 发现真实 HAProxy 3.4.7 启动告警；r7 新增 `proxy.hardened: true` 输出生效的 `chroot` / `user` / `group`，`scripts/verify-hardening.sh` 预置账号目录并用真实 HAProxy 验证降权 + 转发（3.4.7 3/3、2.8.31 1/1）。默认仍为 false：负例证明注入未预置主机会让 HAProxy 拒绝启动（R-23） |
| 非 root systemd 安装演练 | §3.3 §6 | **已实机通过（r7）** | `scripts/systemd-install-drill.sh` 在真实 systemd 255 PID 1 安装 shipped unit 并启停，验证账户、`RuntimeDirectory`、0600 文件、healthy、脱敏与全量清理，带「无 RuntimeDirectory 必失败」负例；3/3 通过，CI 必需 job `systemd-drill`。r6 的「环境无 systemd」结论作废 |
| 漏洞扫描与发布工具链 | — | **已收口（r7）** | `go1.26.11` 的 11 项 stdlib 告警在 `go1.27.4` 下清零；必需 `vuln-scan` job + release 固定 `go1.27.4`；CI 矩阵 1.25 / 1.26 / 1.27（R-14） |
| LAN 明文传输风险 | §4.1 | **已实证（风险未修）** | r8：`scripts/verify-plaintext.sh` 用真实 daemon + 独立 `AF_PACKET` 嗅探容器，在 `bind_host: 0.0.0.0` 下捕获到 token / `Authorization: Bearer` / SQL 正文；TLS 控制通道负例不泄漏；真实 CLI 拒绝 `proxy.ssl.enabled=true`。3/3 + 跨镜像 1/1 通过，必需 CI job `plaintext-audit`。**审计≠修复**；r14 只修了下方独立的「凭据落盘」项。**r15 补上对照半边**：同一门禁再验证 §4.2 既定的「loopback 分片 + 外部代理 TLS 终止」部署，证明该方案下 LAN 无可读凭据；但**错误部署（分片直连 `0.0.0.0`）的传输风险仍未修** |
| Token 安全模型（§4.2 修订要求） | §4.2 | **已实现（r14）** | 甲方 2026-11-13 修订要求早已给出三条判据：token 不得明文落盘、`status --json` 默认不输出、确需持久化时 0600 + 显式开关。后两条此前已做，**第一条连标多轮「需甲方拍板」实为「决策早有、缺执行」**。r14 新增 `global.persist_tokens`（默认 `false`）：`status.json` 默认不写 `token` 字段（`omitempty` + 条件赋值），0600 原子写保留；默认模式下省略分片 token 由 `config.Load` 硬拒绝并给出两种修复（显式自带 token / 开启开关），避免生成无法分发的凭据；`--show-tokens` 在 stderr 解释空结果。`plaintext-audit` 改为硬断言 `STATUS_TOKEN_AT_REST=0`，`systemd-drill` 显式开开关以保留脱敏验证并新增拒绝负例。**r15 更正**：本行末句「仍需甲方拍板的是产品接入 TLS」作废 —— §4.2 原始稿与 §8 早已决定产品不内建 TLS、由外部代理终止，本轮已验收该方案，无需再拍板 |
| 内建 TLS 能力边界 | §4.1 | **已实证引擎能力（产品仍未启用）** | r12：`scripts/verify-engine-tls.sh` 用 pinned v2.0.0 引擎在 `--network none` 容器内证明 `disable_ssl=false` → `https://`、TLSv1.3 握手、`quack_generate_keys()` 自签证书与客户端 `ssl_fingerprint` 校验全链路可用。`supervisor.go` 硬编码 `disable_ssl=true` 是**产品主动关闭**，不是引擎不支持。更正旧口径「内建 TLS 属需设计决策（隐含能力受限）」。**r15 再更正**：当年写的「剩余是产品接线设计决策、需甲方拍板」也站不住 —— §4.2 原始稿与 §8 non-goal 早已决定「产品不内建 TLS，由外部代理终止」，这是既定范围而非待决事项；本轮已把该部署方案验收（见下表与 §4.3）。证书/指纹分发、HTTPS 健康探测、`attach_sql` 契约等属于「若将来改变范围启用内建 TLS」才需要的新决策。必需 CI job `engine-tls-capability` 含 `PRODUCT_DISABLES_ENGINE_TLS=1` 诚实性断言。**不得据此认为 quack-proxy 已具备 TLS**（R-05） |
| 真实引擎 E2E 在 CI 的门禁范围 | — | **全部必需（r10）** | r8 曾把 `continue-on-error` 收紧为仅 PR advisory；r10 移除该逃生口。新增 `scripts/prefetch-quack-e2e.sh` + SHA-pinned `actions/cache`，把 pinned CLI / `quack` / `httpfs` 做成离线缓存；测试进程 `QUACK_E2E_OFFLINE=1` 严格离线，缓存缺失即硬失败。**PR 也为必需**。如实保留：首次 cache miss 仍需 runner 访问上游一次 |
| CI 门禁契约 | — | **已机器可验证（r13）** | `internal/cicontract` 随已必需的 `go test ./...` 解析真实 `ci.yml` / `release.yml`，断言 9 个门禁齐全、无 `continue-on-error` / 无 job 级 `if`、全部 `uses:` 为 40 位 hex SHA、两个共享缓存步骤 `path`/`key` 一致且 key 含三个 pin 前缀、`engine-tls-capability` 的 `needs` 含 `quack-e2e`、离线开关齐全；9 个单点篡改负例保证断言非空转。三段链路在单个 `--network none` 容器按 CI 顺序 3/3 RC=0。**不是第 10 个门禁**、不改产品行为、**不消除** r10 的首次 cache miss 依赖 |
| 外部 TLS 终止部署验收 | §4.2 §8 | **已验收（r15）** | 甲方从 v0.1 起就指定「Nginx 反向代理做 SSL termination + 固定 token」、§8 把内建 TLS 终止列为 non-goal，并非「待甲方拍板」。r15 不新增产品功能、不新增门禁，在已有 `plaintext-audit` 里起第二个真实 daemon（分片只绑 `127.0.0.1`、独立 greeting）+ 真实 HAProxy 2.8.31 TLS 终止前端（共享 netns，明文腿走 loopback），客户端经 HTTPS 拿到分片自己的 200 应答；`AF_PACKET` 断言 TLS 端口只有握手记录、无可读凭据，后端端口零 payload，LAN 客户端无法直连分片。明文注入负例精确触发 canary 断言（RC=1、无 PASS）。**这是运维方案验收，不是产品 TLS**：`disable_ssl=true` / `proxy.ssl` 死配置 / 明文 `gen-proxy` 输出均未变；错误部署的明文风险仍在，证书生命周期与来源 IP 限制仍属运维责任。详见 §4.3 |
| Windows 发布产物 | §7 §8 §10 | **已撤下（r9）** | 真实 Windows 实证后从 `release.yml` 移除 windows/amd64 与 `.zip` 打包；CI 保留交叉编译，二进制启动即显式拒绝。必需 job `windows-runtime-guard` 守住该行为（R-18） |
| 真实引擎 E2E 完整复跑 | — | **已通过（r9），r10 收紧口径** | r9：主机 HTTPS 取 pinned DuckDB v2.0.0 CLI（SHA256 与脚本 pin 一致）+ 官方签名 core 扩展，喂入容器后 `ALL_QUACK_E2E_CHECKS_PASSED`（50 次 SELECT 207 ms / ~241 qps）。**r10 更正**：r9 的容器仍联网、由 DuckDB autoload 下载了 `httpfs`，严格来说不是「离线」。r10 首次用 `--network none` 复跑，发现磁盘库报 `read-only crypto module loaded`，据此把 `httpfs` 也 pin 进缓存，并在单个无网络容器内再次跑到 `ALL_QUACK_E2E_CHECKS_PASSED`（50 次 SELECT 230 ms / ~217 qps）+ 4 个负例 |
| 完全离线部署的扩展依赖 | — | **r10 新增运维约束** | DuckDB v2.0.0 写磁盘数据库必须能加载签名 core `httpfs` 提供完整 crypto 后端；只预装 `quack` 会在无网络主机上启动失败。离线部署需同时预装 `quack` + `httpfs`，`allow_unsigned_extensions` 保持 false。文件存在即可被 autoload 命中，boot SQL 无需改动 |
| HAProxy + VIP 联调（生成配置的入口兼容性） | §4.1 §6 | **已实证（r11），但非 HA 能力** | `scripts/verify-vip-failover.sh` 在 Docker bridge 上让真实 `/32` 浮动 IP 于两节点间漂移（GARP），真实 HAProxy 2.8 用 `gen-proxy` 生成的配置转发：`VIP_PRIMARY_NODE=NODE_A` → `VIP_FAILOVER_NODE=NODE_B`、`SESSION_DISCONTINUITY=1`、`VIP_FAILOVER_DRILL_PASSED`；多分片配置仍被拒绝（R-04）。必需 CI job `vip-failover`。**VIP 由脚本模拟外部编排，产品不提供也不感知；切换后会话与数据不连续**，上表「常驻 Coordinator / 统一查询入口 VIP」仍为未实现 |

> 状态取值：已实现 / 部分实现 / 未实现 / 已放弃。编号 `R-xx` 对应 `REMEDIATION.md`。

---

## 1. 问题陈述

### 背景

DuckDB 默认是单进程架构，一个 `.duckdb` 文件同时只能有一个进程写入。DuckDB v1.5.2 发布的 Quack 协议原生解决了多客户端并发写入的问题，但 DuckDB 不提供内置的进程管理和服务编排能力。

当前，任何人想要运行一个生产级的"Quack server 集群"（多个 DuckDB 文件各自开 Quack 端口），需要手写 systemd unit、shell 脚本、健康检查、负载均衡配置。没有一个开箱即用的工具。

### 核心痛点

| 痛点 | 现状 | 目标 |
|------|------|------|
| 启动 Quack server | 手写 shell 脚本 `duckdb -c "CALL quack_serve(...)"` | 一个 YAML 配置，一条命令 |
| 管理 N 个 DuckDB 文件 | N 个 systemd unit | 一个 daemon 管理所有进程 |
| 健康检查 | 手写 curl + cron | 内置 HTTP health check + 自动重启 |
| 负载均衡 | 手写 HAProxy/Nginx 配置 | 自动生成配置，reload 信号 |
| 故障恢复 | 手动重启 | 崩溃自动重启 + 指数退避 |
| 跨机器联邦 | 手动 ATTACH 每台机器 | Coordinator DuckDB 一键联邦（**未实现，且已列入 non-goal**） |

### 目标用户

- 数据分析团队：有多台机器上的 DuckDB 分区数据，需要统一查询入口（**当前不满足**）
- 小规模 SaaS：用 DuckDB 做嵌入式 OLAP，需要多客户端并发写入
- duckdb_fdw 用户：需要通过 Quack 解决多个 PG backend 的并发写入问题
- 个人开发者：本地开发时需要 Quack server 进程的便捷管理

---

## 2. 产品愿景

~~**通过一个 YAML 文件和一条命令，把任意数量的 DuckDB 数据库文件变成高可用的 Quack 服务集群。**~~

> **2026-11-13 边界澄清**：上句是 v0.1 愿景，非既成事实。当前定位是**单机多进程 DuckDB + Quack 端点监管器**；daemon 为单点、`Restart=no`、无跨机能力、无内嵌 LB、不在数据路径上。下方架构图中的「Optional: Coordinator DuckDB → 统一查询入口 (VIP)」从未实现。对外表述一律以 `SCOPE.md` §1 为准。

```
quack-proxy start  ← 一条命令
    ↓
┌─────────────────────────────────────────┐
│  quack-proxy daemon                      │
│                                           │
│  ├── Process Supervisor                  │
│  │   ├── DuckDB #1 → Quack :9491        │
│  │   ├── DuckDB #2 → Quack :9492        │
│  │   └── DuckDB #N → Quack :949N        │
│  │                                       │
│  ├── Health Checker                      │
│  │   └── HTTP GET / → 每个端点 5s 一次  │
│  │                                       │
│  ├── Config Generator                    │
│  │   └── → HAProxy/Nginx 配置文件       │
│  │                                       │
│  └── Optional: Coordinator DuckDB        │  ← 未实现
│      └── ATTACH 所有 Quack 端点          │
│          → 统一查询入口 (VIP)             │
└─────────────────────────────────────────┘
```

---

## 3. 架构设计

### 3.1 逻辑架构

```
quack-proxy (Go 二进制)
    │
    ├── quack-proxy start     ← 启动 daemon
    ├── quack-proxy stop      ← 停止 daemon
    ├── quack-proxy status    ← 查看状态
    ├── quack-proxy reload    ← 热重载配置
    └── quack-proxy gen-proxy ← 生成 HAProxy 配置
```

### 3.2 进程模型

```
quack-proxy (主进程, PID 1)
    ├── signal handler (SIGTERM, SIGHUP → 增量 reload)
    ├── status file 写入 (每个健康检查周期写 status.json)
    ├── health check loop (每 N 秒轮询, 指数退避重启, 10 次失败后停泊)
    │
    └── child processes (每个 DuckDB 实例一个 goroutine)
        ├── duckdb -- Quack on :9491 ← /data/shard_1.db
        ├── duckdb -- Quack on :9492 ← /data/shard_2.db
        └── duckdb -- Quack on :9493 ← /data/shard_3.db
```

### 3.3 生命周期

> 下面的状态机与 `⚠️` 批注是 **v0.1 原始稿 + 2026-11-13 评审时点**的实现偏差记录，
> 保留作背景。**它们不是当前未修项清单，也不含待甲方拍板的设计决策**；各 `R-xx`
> 条目的当前状态一律以头部「实现状态总览」表与 `REMEDIATION.md §3` 为准
> （r16 补注）。例如本节的 R-17 已由 Gate 2 修复，而 R-05 的明文传输风险
> 至今**未在产品层修复**（r15 验收的是外部代理终止方案，不是产品 TLS）——
> 两者不可混为一谈。

```
START:
  1. 解析 YAML 配置（含 log_level / init_sql_file 存在性等校验）
  2. 验证 .db 文件的父目录存在
     （原始稿写「验证所有 .duckdb 文件存在」，与实现不符：DuckDB 首次打开会新建库）
  3. 为每个 shard 启动 duckdb 子进程（各自独立进程组）
     - 生成 crypto-random token（如果未指定）
     - boot SQL 写入临时文件，经 (cat file; tail -f /dev/null) 管道逐字送入
       <duckdb_bin> -unsigned <db> 的 stdin（避免 shell 引号破坏多行 init_sql）
  4. 等待所有进程就绪（HTTP health check 通过，15s 宽限期）
  5. 写 status.json，进入运行循环

RUNNING:
  1. 每 N（可配置）秒 HTTP GET 所有端点
  2. 不健康的端点：kill 整个进程组 → 重启 → 指数退避（1s→30s）
     连续失败 10 次后停泊该 shard（status=stopped），不再无限重启
     ⚠️ 评审时点实现偏差（保留作背景，见本节开头时点声明）：
        healthy 后未清零重启预算、stopped 无法经 reload 恢复（R-02，Gate 0 已修）；
        30s startupGrace 曾使 1s→30s 退避在默认配置下永不触发（R-17，Gate 2 已修：
        performRestart 保留原始 lastStart，30s grace 只作用于冷启动，退避现真实生效）
  3. 每个周期刷新 status.json（含 token / 重启计数 / 健康状态）
     ⚠️ Gate 0 前为非原子写且权限 0644（R-05 / R-11）

STOP:
  1. SIGTERM → 每个 shard 的进程组（而非单个 bash 包装进程）
  2. 10s 未退出则对进程组 SIGKILL；随后清理 pid / status 文件
     ⚠️ 停机前无 CHECKPOINT，串行 10s/片在 N≥4 时会超 TimeoutStopSec（R-12）

RELOAD:
  1. SIGHUP → 重新解析配置 → 增量更新子进程
    - 新增 shard → 启动新进程
    - 移除 shard → graceful shutdown
    - 未变化的 shard → 保持运行
    - 字段变化的 shard → 滚动重启（r5 新增）
    ✅ 状态修订：早期实现只按 name 增删（R-07）；r5 已补字段级 diff、全局参数联动与 `init_sql_file` 内容摘要。HAProxy 进程本身仍需运维手动 reload。
```

---

## 4. 配置规范

### 4.1 quack-proxy.yaml

```yaml
# quack-proxy v0.1 config

global:
  log_level: info              # debug | info | warn | error
  pid_file: /var/run/quack-proxy/quack-proxy.pid
  status_file: /var/run/quack-proxy/status.json

listener:
  bind_host: 0.0.0.0            # Quack 端点监听地址（SCOPE 要求改为默认 127.0.0.1）
  port_start: 9491              # 端口起始（递增分配）
  health_path: /                # 健康检查路径 (Quack HTTP)
  health_interval: 5s           # 健康检查间隔

shards:
  - name: orders_2024           # 逻辑名称
    database: /data/orders_2024.db
    port: 9491                  # 可选，自动分配
    token: ""                   # 原始稿：可选，空则自动生成（crypto-random，32 字符）
                                # r14 更正：默认 persist_tokens:false 下此写法会被
                                # config.Load 拒绝，必须显式给值；见 §4.2 修订要求

  - name: orders_2025
    database: /data/orders_2025.db
    # port 自动分配为 9492

  - name: customers
    database: /data/customers.db
    # port 自动分配为 9493

proxy:
  enabled: true                 # 是否启用自动生成 HAProxy 配置
  output: /etc/haproxy/quack-proxy.cfg
  bind_port: 9490               # HAProxy 前端端口
  mode: roundrobin              # roundrobin | leastconn（跨库轮询语义错误，见 R-04）
  ssl:
    enabled: false
    cert: /etc/ssl/quack.pem
```

> **实现状态**：`proxy.ssl` 在 `internal/config/config.go` 中定义，但 `internal/proxy/haproxy.go` 与 supervisor 均未读取；同时 `supervisor.go` 硬编码 `disable_ssl=true`。该配置项当前**完全无效**，不得据此认为启用了 TLS。见 R-05。
>
> **r12 补充（影响甲方决策）**：这里的 `disable_ssl=true` 不是引擎能力限制。pinned DuckDB v2.0.0 的 Quack 扩展原生支持 HTTPS（`disable_ssl=false` + `ssl_cert_file`/`ssl_key_file` + 客户端 `ssl_fingerprint`），已由 `scripts/verify-engine-tls.sh` 在严格离线容器内实证（TLSv1.3 握手 + 指纹校验 + 4 个负例）。因此「是否启用 TLS」不是「能否实现」，而是**范围选择**。**r15 更正**：当年写在这里的「产品接线与运维契约的设计决策」也不成立 —— §4.2 原始稿与 §8 早已选定「产品不内建 TLS、由外部代理终止」，本轮已把该既定方案端到端验收（见 §4.3）。下列契约项（证书生命周期与私钥存放、向客户端分发/轮换 SHA256 指纹的通道、健康探测改 HTTPS、`status.json` 的 `attach_sql` 契约、`gen-proxy` 输出的 HAProxy 前端与 `proxy.ssl` 死配置）**只在甲方改变范围、要求内建 TLS 时才需要重新设计**；`status.json` 的 **token 契约已由 r14 定案**（默认不落盘 + 显式开关），不再需要拍板。**r12 本轮不改产品行为**（r14 才改了 token 落盘行为）。**r15 更正本段「R-05 现只剩产品接入 TLS 开放 / 需甲方拍板」的口径**：§4.2 原始稿与 §8 早已决定产品不内建 TLS、由外部代理终止，本轮已按该既定方案完成部署验收（见 §4.3）；真正剩下的传输风险只有「分片被错误地直接暴露在 LAN 上」。证书/指纹分发等契约项属于「若改变范围启用内建 TLS」才需要的新决策，而非本次整改待办。

### 4.2 Token 安全模型

> 以下前三条是 **v0.1 原始稿**；**r14 起以实现状态为准**，见「当前实现」。

- 未指定 token → 启动时用 `crypto/rand` 生成 32 字符密码学安全 token，写入 `status.json`
- 指定 token → 使用指定值
- 生产环境建议：Nginx 反向代理做 SSL termination + 固定 token
- **2026-11-13 修订要求**：token 不得明文落盘；`status --json` 默认不输出；确需持久化时 0600 + 显式开关。见 `SCOPE.md` §5

**当前实现（r14，修订要求已执行完毕）**：

| 判据 | 状态 | 实现 |
|------|------|------|
| token 不得明文落盘 | **已实现** | `global.persist_tokens` 默认 `false`；`StatusFileRecord.Token` 改 `omitempty`，`buildStatusLocked()` 仅在开关开启时赋值；`status.json` 默认连 `token` 字段都没有 |
| `status --json` 默认不输出 | 已实现（此前） | `buildStatus(sf, false)` 脱敏；需 `--show-tokens` |
| 确需持久化时 0600 + 显式开关 | **已实现** | 仅 `global.persist_tokens: true` 恢复自动生成 + 落盘；`writeStatusAtomically` 仍 0600 原子写 |

- **默认模式的配套约束**：因不落盘时自动生成的凭据无处分发，`config.Load` 会拒绝任何省略 token 的分片，并提示「显式给 token（推荐，运维自管密钥）」或「开启 `persist_tokens: true`」。沿用 `proxy.ssl.enabled` 的 fail-closed 模式。
- `--show-tokens` 在无持久化凭据时由 `tokenPersistenceNotice` 在 **stderr** 解释原因与两种选择，不污染 JSON 输出。
- 示例配置与 README Quick Start 已改为显式 token；照抄旧示例会（正确地）在加载期失败。
- **边界**：本项只修「凭据落盘」，**不修明文传输**（R-05 的 `disable_ssl=true` 与 LAN 抓包结论完全不变）。

### 4.3 外部 TLS 终止的部署验收（r15）

**这不是新增需求，而是执行 §4.2 原始稿与 §8 non-goal 早已给出的决定**：quack-proxy 自身不提供 TLS，分片保持只监听 `127.0.0.1`，对外访问由运维在外部反向代理终止 TLS（原始稿举例 Nginx）；`SECURITY.md` 的运维清单同样要求「在外部代理终止 TLS，并限制来源 IP」。r15 之前，必需门禁只证明了「错误部署会泄漏」，从未验收「按 PRD 部署是否真的消除 LAN 暴露面」。台账把「产品接入 TLS」连标多轮「需甲方拍板」，实际是把「产品不做 X、由运维做 Y」误记成「要不要做 X 尚待决策」——X 没做，Y 也没验收。

**验收方式**（`scripts/verify-plaintext.sh`，仍是必需 job `plaintext-audit`；**不新增门禁、不改产品行为、Go 源码零改动**）：

| 项 | 实现与实测 |
|------|-----------|
| 错误部署对照 | daemon A 的 `bind_host: 0.0.0.0`，客户端直连 → 抓到 387 B 明文，含 token / `Authorization: Bearer` / SQL 正文（`PLAINTEXT_TOKEN_VISIBLE=1`），保留为已知弱点 |
| 正确部署 | daemon B 的分片只绑 `127.0.0.1:19745`，应答独立 greeting `external-tls-backend-ok`；真实 HAProxy 2.8.31（`USE_OPENSSL`，一次性自签证书）与服务端共享 netns，`0.0.0.0:19746 ssl crt` 终止 TLS 后把明文腿转发到 loopback |
| 端到端功能 | 另一容器经 HTTPS 得 `EXTERNAL_TLS_ENDPOINT (200, b'external-tls-backend-ok')`，证明流量真到了分片、不是代理自己应答 |
| 线上不可读 | TLS 端口抓到 5144 B 且为 TLS 握手记录，四类 canary（token / Bearer / SQL / `HTTP/1.1 200`）均不可读；后端端口 **0 B**；LAN 客户端直连分片被拒（`LOOPBACK_DIRECT BLOCKED`） |
| 负例 | 坏 HAProxy 配置被厂商 `haproxy -c -f` 拒绝；`PLAINTEXT_INJECT_CLEARTEXT=1` 注入一条明文凭据请求后，门禁精确报 `cleartext canary ... visible on the LAN`、RC=1 且无 `PLAINTEXT_AUDIT_PASSED`，证明断言非空转 |

**边界**：HAProxy 配置由演练脚本生成，**不是 `gen-proxy` 的输出**；`supervisor.go` 仍硬编码 `disable_ssl = true`，`proxy.ssl` 仍是死配置，`gen-proxy` 仍只生成明文前端，`engine-tls-capability` 的 `PRODUCT_DISABLES_ENGINE_TLS=1` 不变。证书生命周期、CA 与轮换、来源 IP 限制仍是运维责任，运维清单中的待办**不因本验收而被勾掉**；分片端点仍是 drill stub，不覆盖真实 Quack 协议（由 `quack-e2e` 覆盖）。**结论只能写成「既定部署方案有效」，不能写成「quack-proxy 已具备 TLS」**；把分片直接绑 `0.0.0.0` 并直连的风险仍在。

---

## 5. CLI 接口

```
quack-proxy start [-c config.yaml]     # 启动 daemon
quack-proxy stop                        # 停止 daemon
quack-proxy status [--json]            # 查看所有 shard 状态
quack-proxy reload                     # 热重载配置
quack-proxy gen-proxy [-c config.yaml] # 生成 HAProxy/Nginx 配置
quack-proxy version                    # 打印版本
```

### 5.1 status 输出示例

```
$ quack-proxy status

 NAME           PORT   STATUS    UPTIME    RESTARTS  DATABASE
 orders_2024   9491   healthy   2h 15m    0         /data/orders_2024.db
 orders_2025   9492   healthy   2h 15m    0         /data/orders_2025.db
 customers     9493   healthy   2h 14m    1         /data/customers.db

$ quack-proxy status --json
{
  "shards": [
    {
      "name": "orders_2024",
      "port": 9491,
      "status": "healthy",
      "pid": 12345,
      "uptime": "2h15m",
      "restarts": 0,
      "token": "a8f3...b9e1",
      "database": "/data/orders_2024.db",
      "last_health_check": "2026-05-29T12:00:00Z"
    }
  ],
  "coordinator_attach_sql": "ATTACH 'quack:localhost:9491' AS orders_2024;\nATTACH 'quack:localhost:9492' AS orders_2025;\n..."
}
```

### 5.2 coordinator_attach_sql

`status --json` 输出中包含 `coordinator_attach_sql` 字段——一段可以直接复制到 Coordinator DuckDB 执行的 SQL，ATTACH 所有当前健康的 Quack 端点。这是连接 quack-proxy 管理的集群和 Coordinator DuckDB 的桥梁。

> **2026-11-13 澄清**：这里只生成 SQL 片段，**不启动任何 Coordinator 进程、不监听 VIP、不做路由**。§6 示例中「HAProxy VIP + 单个 quack_token」在当前实现下不可用：每个分片 token 独立，且跨库 roundrobin 会打散有状态会话（R-04）。默认 `bind_host: 0.0.0.0` 时生成的 ATTACH 地址不可连接（R-10，Gate 0 已修）。

---

## 6. 与 duckdb_fdw 的集成

quack-proxy 管理者 Quack 端点后，duckdb_fdw 用户可以直接连接：

```sql
-- PG 中配置 duckdb_fdw
CREATE SERVER quack_cluster FOREIGN DATA WRAPPER duckdb_fdw
OPTIONS (quack_host 'localhost:9490');  -- HAProxy VIP（需用户自行部署 HAProxy）

CREATE USER MAPPING FOR current_user SERVER quack_cluster
OPTIONS (quack_token 'a8f3...b9e1');

IMPORT FOREIGN SCHEMA "remote" FROM SERVER quack_cluster INTO public;
```

或者单端点直连：
```sql
CREATE SERVER orders_2024_srv FOREIGN DATA WRAPPER duckdb_fdw
OPTIONS (quack_host '192.168.1.10:9491');

CREATE USER MAPPING FOR current_user SERVER orders_2024_srv
OPTIONS (quack_token 'token_from_status_json');
```

---

## 7. 非功能性需求

> 本节表格是 **v0.1 原始稿目标 + 2026-11-13 评审时点的实现状态快照**，保留作背景，
> 不是当前状态声明，也不含待甲方拍板的设计决策。表内「待核实 / 须 pin / 串行持锁 /
> 不触发」等均为**当时的**批注；各 `R-xx` 条目的当前状态一律以头部「实现状态总览」
> 表与 `REMEDIATION.md §3` 为准（r16 补注，避免把历史快照读成当前未修项）。

| 需求 | 目标 |
|------|------|
| 启动时间 | <5s（含所有 DuckDB 进程启动 + extension 加载）；与 15s 初始宽限 + 30s startupGrace 不一致，待核实 |
| 健康检查间隔 | 可配置，默认 5s |
| 故障恢复 | 崩溃自动重启，指数退避（1s→2s→4s→…→30s max）；**原始实现中退避曾因每次重启刷新 30s grace 而永不触发（R-17，Gate 2 已修：重启不刷新 `lastStart`，当前退避真实生效）** |
| 内存占用 | <50MB（不含 DuckDB 子进程） |
| 二进制大小 | <15MB（Go 静态编译） |
| 平台 | Linux + macOS（Windows 为 v0.1 non-goal，见 §8）。**2026-11-13 核实 → 2026-11-16 收口**：r9 起 release 不再发布 Windows 二进制；真实 Windows 实证发现旧产物 `version` 可用但 `start` 因 `lookupBin` 的 POSIX mode 位误判报「duckdb_bin 不可执行」，现已修判定并改为启动即显式拒绝（R-18） |
| DuckDB 版本 | >= 1.5.2（Quack 可用版本）；**与 README 的 2.0.0 stable、benchmark 的 1.5.2 nightly 三套口径冲突，须 pin 单一版本**（R-16） |
| 并发 shard 数 | 1-1000（未经压测验证，且健康检查为串行持锁，N 大时阻塞明显，见 R-03） |

---

## 8. 不做什么

| 不做 | 理由 |
|------|------|
| **内建 TLS 终止** | **v0.1 起即如此（r15 明确写入本表，与 SCOPE §3 一致）**：§4.2 原始稿指定「Nginx 反向代理做 SSL termination + 固定 token」，对外暴露由运维在外部代理终止；`proxy.ssl.enabled: true` 在加载期显式报错，产品不生成 TLS 前端。r12 证明引擎原生支持 TLS 也不改变本范围；r15 已在必需 `plaintext-audit` 中验收该**既定部署方案**（真实 HAProxy 终止 + loopback 分片，LAN 无可读凭据），但这是运维职责不是产品功能。若将来改变范围启用内建 TLS，须新立项并同步改门禁断言 |
| 分布式事务协调 | 超出范围，DuckDB 单事务只能写一个 ATTACH 数据库 |
| 自动分区 / sharding | 用户自己决定数据分区策略 |
| Web UI / Dashboard | v0.1 只做 CLI |
| Quack 协议实现 | 用 DuckDB 自身的 Quack 客户端（ATTACH 模式） |
| 跨机器 Quack 端点发现 | v0.1 专注单机多文件管理 |
| 持久化 Coordinator | v0.1 的 Coordinator 是无状态的，用户自己启动 DuckDB + ATTACH |
| Windows 支持 | supervisor 依赖 bash 进程组（`Setpgid`），v0.1 专注 Linux/macOS |
| **control plane 冗余 / daemon 自动接管** | **2026-11-13 补充**：当前无此能力，且 `Restart=no`，故不得宣传「高可用」 |
| **跨库负载均衡** | **2026-11-13 补充**：分片不等于副本，见 R-04 |

> 注意：§1 / §2 的「跨机器联邦」「统一查询入口」「高可用集群」与本节的 non-goals 直接矛盾，以本节与 `SCOPE.md` 为准。

---

## 9. 里程碑（2026-11-13 转为状态表）

原始稿的 M1/M2/M3 全部未勾选，但代码已实现大部分功能；尾注仍写「下一步 go mod init」，明显是过期草稿。现改按 `REMEDIATION.md` 的 Gate 跟踪。

### 原始 M1 / M2 / M3 实现状态

> 下表「状态」列是 **2026-11-13 评审时点的历史判定**，不是当前状态声明，也不含
> 待甲方拍板的设计决策；各 `R-xx` 条目的当前状态一律以头部「实现状态总览」表与
> `REMEDIATION.md §3` 为准（r16 补注）。**勿据本表估算剩余工作量**：表内
> 「部分实现」是当时的判定，其中带编号的缺陷已在后续整改中处理，原始稿的
> 非功能性观察项则不在本次 `R-xx` 台账范围内。

| 条目 | 状态 | 备注 |
|------|------|------|
| YAML 配置解析 | 已实现 | `internal/config/config.go` |
| 启动 N 个 DuckDB+Quack 子进程 | 已实现 | |
| `start` / `stop` / `status` | 已实现 | |
| 基础健康检查 + 自动重启 | 部分实现 | 评审时点状态机缺陷见 R-02 / R-17，两项均已修，见头部状态表 |
| `status --json` 输出 `coordinator_attach_sql` | 已实现 | 地址 bug 见 R-10（Gate 0 已修） |
| SIGHUP 热重载（增量更新子进程） | 已实现 | 早期仅增删；r5 补字段级 diff 与滚动重启，见 R-07 |
| `gen-proxy` 生成 HAProxy 配置 | 已实现，语义有误 | 见 R-04 |
| 指数退避重启策略 | 部分实现 | 评审时点「默认配置下不触发」见 R-17；Gate 2 已修（重启不刷新 `lastStart`），当前退避真实生效 |
| PID 文件 + 优雅关闭 | 部分实现 | 非原子写、非 root 部署阻断，见 R-06 / R-11 / R-12 |
| systemd unit 文件模板 | 已实现，部署阻断 | 缺 `RuntimeDirectory`（Gate 0 已修） |
| 日志结构化输出 | 部分实现 | `drainLog` 绕开 slog |
| README + 快速开始指南 | 已实现，含不实宣称 | 见 R-08 / R-16 |
| 二进制发布（GitHub Releases） | 已实现 | 缺签名 / SHA pin，见 R-14 |

### 当前整改里程碑

| Gate | 主题 | 状态 | 通过条件 |
|------|------|------|----------|
| 0 | 止血，不动架构 | 进行中（补丁已落地，待在有 Go 的环境验证） | 回归用例 + `go test -race ./...` 全绿；真实 DuckDB E2E；systemd 可启动 |
| 1 | 收安全面与边界 | 待办 | 注入 / 权限 / 明文检查通过；供应链加固；撤跨库 roundrobin |
| 2 | 验证与诚实化 | 待办 | 基准可复现；覆盖率门槛；文档-代码一致性检查表勾完 |
| — | 真 HA 集群 | **不在本次范围** | 视为新产品立项，须另写 PRD 与架构 RFC |

---

## 10. 技术选型

| 选项 | 决策 | 理由 |
|------|------|------|
| 语言 | Go | goroutine 天然映射子进程管理，net/http 开箱即用，编译快 |
| 配置格式 | YAML | 人类可读，DuckDB 生态通用 |
| 进程管理 | os/exec | 标准库，无需第三方 |
| HTTP 客户端 | net/http | 健康检查够用 |
| 日志 | slog | Go 1.21+ 标准库 |
| CLI 框架 | 无（flag + 手写） | 命令少，不值得引入 cobra |
| 跨平台 | Linux + macOS（Windows 已放弃） | supervisor 依赖 bash 进程组与 `Setpgid`；**原稿此行写「Linux + macOS + Windows」，与 §7 / §8 矛盾（R-16）** |

---

*原始稿 v0.1 | 2026-11-13 转为实现状态跟踪版 | 当前下一步：按 `REMEDIATION.md` 依次过 Gate 0 → Gate 1 → Gate 2*

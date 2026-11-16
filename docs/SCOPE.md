# quack-proxy 范围声明（SCOPE）

> 版本 v0.2.0-r13 | 2026-11-16 | 状态：**已确认；Gate 0 / Gate 1 / Gate 2 实质过门；R-21 停止回收、R-07 reload 字段 diff、R-22 孤儿端口假阳性均已修复并复验；部署产物已用厂商真实二进制校验，systemd 非 root 实机安装演练通过，R-23 有可验证的加固路径，govulncheck 基线清零并转必需；R-05 的 LAN 明文风险已用真实抓包实证并落成必需门禁（风险本身仍在）；R-18 已收口——真实 Windows 实证后撤下发布产物、修复跨平台可执行判定、加显式运行时拒绝与必需门禁；r10 起 `quack-e2e` 在 PR 上也不再 advisory，改用 SHA256 校验的离线缓存并由 `--network none` 实证；同时更正 r9「离线」口径，明确完全离线部署需预装 `quack` + `httpfs`；r11 补齐 HAProxy + VIP 真实联调并转必需门禁，但**这只验证生成配置在浮动 IP 入口下可用，不代表 quack-proxy 具备 HA**（VIP 由外部编排，切换后会话/数据不连续，R-04 仍成立）；**r12 用同一套 pinned 引擎实证 Quack 原生支持 TLS（`disable_ssl=false` → `https://`、TLSv1.3 握手、证书指纹校验），更正「内建 TLS 属需设计决策」隐含的能力受限口径 —— 引擎能力已具备，产品 `disable_ssl=true` 是主动关闭；这是引擎审计、不是产品修复，R-05 仍开放，剩余决策收窄为产品接线与运维契约；r13 把「9 个门禁必需 / Actions 全 SHA pin / 共享缓存 key 与 pin 一致 / `needs` 链 / 离线开关」做成随必需 `go test ./...` 执行的机器可验证契约（`internal/cicontract`，9 个负例），并在单个 `--network none` 容器按 CI 真实顺序复跑三段链路 —— 这是配置契约检查，不新增必需门禁、不改产品行为、不消除 r10 的首次 cache miss 依赖**
> 本文件是本次整改的唯一边界依据。与 README / PRD 冲突时，以本文件为准。

## 1. 一句话定位

quack-proxy 是**单机多进程 DuckDB + Quack 端点监管器**：一份 YAML 启动并守护 N 个 DuckDB 子进程，提供健康检查、有限次数自动重启、状态查询，以及 HAProxy 配置文本的生成。

**它不是高可用集群，不提供控制面冗余，也不在数据路径上。**

## 2. 目标用户与场景

- 本地开发 / CI：一条命令拉起若干 Quack 端点
- 单机小规模 OLAP：多库分端口、需要统一进程管理
- 学习 Quack 协议与 duckdb_fdw 集成

## 3. 明确不做什么（Non-Goals）

| 不做 | 原因 |
|------|------|
| control plane 冗余 / daemon 自动接管 | 状态全在单进程内存；daemon 崩溃后子进程占端口，需重新设计进程归属（R-01，属新产品立项） |
| 跨机器端点发现、跨机集群 | 生成的 HAProxy `server` 行共用同一个 bind_host |
| 内置负载均衡 / VIP | 只生成 HAProxy 配置文本，不启动、不管理 HAProxy |
| 常驻 Coordinator / 统一查询入口 | 只生成 ATTACH SQL 片段，不启动 Coordinator 进程 |
| 分布式事务、自动分片 | 由 DuckDB ATTACH 与用户自行决定 |
| **多分片放进一个 LB backend** | 单机不存在副本：不同 `.db` 是分片（轮询结果不确定），相同 `.db` 会触发 DuckDB 文件锁冲突（v2.0.0 实测）。`gen-proxy` 只允许单分片端口转发（R-04） |
| 内建 TLS 终止 | `proxy.ssl.enabled: true` 在加载期报错；由外部代理终止。**r12 更正**：引擎（pinned v2.0.0 Quack）原生支持 TLS，产品 `supervisor.go` 硬编码 `disable_ssl = true` 是主动关闭而非能力受限。启用它属产品接线设计决策（证书/指纹分发、健康探测、status.json 契约、HAProxy），不是本次整改范围 |
| 自动安装 DuckDB 扩展 | boot SQL 只 `LOAD quack`；扩展须由运维预装并校验签名（见 §5） |
| Web UI | CLI only |

## 4. 版本基线（本轮已收敛为单一口径）

| 项 | 基线 | 状态 |
|----|------|------|
| Go | `go.mod` = 1.25.0（源码最低）；CI 矩阵 1.25 / 1.26 / 1.27；**发布与漏洞扫描固定 go1.27.4** | `go1.26.11` 实测 11 项 stdlib 告警，`go1.27.4` 实测 0 项；发布产物必须用补丁工具链构建（r7） |
| DuckDB | **v2.0.0 stable (Cyanoptera)**，linux-amd64 CLI SHA256 `c6507206baf7793c6013dbd82368469a45f20dc0e17a6f92f204602f652ed2bc` | **已 pin**；`scripts/quack-e2e.sh` 与 `scripts/prefetch-quack-e2e.sh` 均强制校验（r10） |
| Quack 扩展 | **签名 core 扩展**，预装 `duckdb -c "INSTALL quack;"` | 已验证。`INSTALL quack FROM community` 在 v2.0.0 返回 HTTP 404 —— 旧文档指引是错的（R-19）。r10 pin 解压后 SHA256 `9c893bd5…` |
| **httpfs 扩展（r10 新增运维约束）** | **签名 core 扩展** `httpfs.duckdb_extension`，解压后 SHA256 `77e6c429…` | DuckDB v2.0.0 写**磁盘数据库**需要它提供完整 crypto 后端；缺失时报 `read-only crypto module loaded`。**完全离线部署必须预装 `quack` + `httpfs` 两个扩展**（`:memory:` 不触发）。文件存在即可被 autoload 命中，无需改 boot SQL，也不用 `-unsigned` |
| 历史口径 | ≥1.5.2 / 1.5.2 nightly / 2.0.0 stable 三套 | 已全部标注为历史说明，不再作为支持基线（R-16） |
| 支持平台 | Linux + macOS（amd64 + arm64） | **r9 起不再发布 Windows 产物**：PRD §8 既有 non-goal 的执行。CI 仍交叉编译 windows/amd64 作源码兼容检查，二进制在 Windows 上启动即显式拒绝（R-18）。真实引擎 E2E 亦已用离线 pinned CLI + 签名扩展完整复跑通过 |

## 5. 安全边界（当前实现）

- 默认监听 `127.0.0.1`；对外暴露必须由用户显式配置，并自行做 TLS 终止
- Quack 端点仍是**明文 HTTP**（`disable_ssl=true` 硬编码）；token 是唯一凭据
- status.json 含 token，以 `0600` + 临时文件原子写落盘；`status --json` 默认脱敏，需 `--show-tokens`
- pid 文件同样 `0600` + 原子写，目录 `0700`
- `listener.allow_other_hostname` 默认 `false`；只有经 VIP/DNS 访问才显式开启
- 配置文件视为**可信输入**：能写 YAML 等价于能指定 `duckdb_bin`，不把 `init_sql` 当沙箱边界
- `name` / `health_path` / `bind_host` 在加载期做白名单校验，杜绝路径穿越、配置行注入与 SSRF
- `listener.health_expect` 可选：校验健康响应 body 子串。v2.0.0 实测 greeting 为 `This is a DuckDB Quack RPC endpoint...`，建议固定配置以消除「任意 2xx 服务占端口即假阳性」
- `proxy.ssl.enabled: true` 明确报错，不允许「看起来开了 TLS」
- 扩展**不自动安装**：`-unsigned` 默认关闭，运维须预装签名 core 扩展；CLI 二进制由 SHA256 校验
- 供应链：所有 GitHub Actions 按 commit SHA pin；govulncheck 版本 pin；release 的 `SHA256SUMS` 由 cosign keyless OIDC 签名

## 6. 成功判据（Definition of Done）

| # | 判据 | 状态 |
|---|------|------|
| 1 | 照 README 装 systemd 后 `start` 能成功（非 root + `/var/run`） | ✅ **r7 实机通过**：真实 systemd 255 PID 1 安装 shipped unit，服务以 `quack-proxy` 运行、`RuntimeDirectory` 0750 且属主正确、pid/status 0600、健康到 healthy、停止无孤儿；带「无 RuntimeDirectory 必失败」负例；3/3 通过，CI 必需 job `systemd-drill` |
| 2 | 真实引擎 E2E：起 → kill → 自愈 → 优雅停，无孤儿/端口残留 | ✅ **已实机通过**：Go 集成测试（真实进程组，`-race`）+ `scripts/quack-e2e.sh`（真实 DuckDB v2.0.0 + Quack，token 认证）。R-21 后额外以「忽略 SIGTERM 的后代」回归用例验证 `StopAll` 返回即端口释放 |
| 3 | 间歇故障后重启计数归零；`stopped` 可经 reload 恢复 | ✅ 回归用例覆盖 |
| 4 | token 不以 0644 落盘；status 原子写、并发读不出现半截 JSON | ✅ 回归用例覆盖（POSIX） |
| 5 | `go test -race ./...` 全绿，CI 有覆盖率门槛 | ✅ Linux 容器实跑全绿；CI 已配置 `-race -cover` + 50% 门槛 |
| 6 | README / PRD / 代码逐条对照，无「声称但未实现」 | ✅ 主要条目已对齐；本轮修正了「自动 INSTALL 扩展」「终态不可逆」「副本组」等残留 |
| 7 | 性能数字标注测量对象，不再当作产品性能 | ✅ README 加 caveat；新增可复现的真实 `ATTACH/SELECT` 基准脚本并给出实测口径 |

## 7. 决策记录

| 日期 | 决策 | 依据 |
|------|------|------|
| 2026-11-13 | 不重写 PRD，改为实现状态跟踪文档 | 现有 PRD 把愿景写成既成事实、无验收标准 |
| 2026-11-13 | 定位降级为单机监管器 | daemon 是 SPOF、`Restart=no`、无跨机能力 |
| 2026-11-13 | 先补失败测试再改代码，分三道 Gate | 原 35 个测试全绿却漏掉全部 P0 |
| 2026-11-13 | Gate 1 用「显式拒绝」替代「静默忽略」 | 跨库 LB、未实现 TLS 属于语义错误 |
| 2026-11-13 | Windows 相关测试显式 skip，不伪装通过 | R-18：硬编码 bash/tail、无进程组语义 |
| 2026-11-13 | 便携 Go 工具链 + Linux 容器用于本地验证，不改系统环境 | 需要真实编译/测试/竞态结果作为整改证据 |
| 2026-11-13 | **checkAll 四段式重构**：探测并发且锁外、决策在锁内（纯内存）、重启锁外、status I/O 锁外 | 锁内 2s×N HTTP + 10s Wait 会楔死 status/reload/停机（R-03） |
| 2026-11-13 | **重启不刷新 `lastStart`**：30s grace 只给冷启动 | 每次重启刷新 grace 会吞掉 1s→30s 指数退避（R-17） |
| 2026-11-13 | **StopAll 并行化**，每分片独立 `opMu` | 串行 10s/片在 N≥4 时超 `TimeoutStopSec=30`（R-12） |
| 2026-11-13 | 停机**不做 `CHECKPOINT`** | DuckDB CLI 阻塞在 `quack_serve`，stdin 无 admin 通道，注入的 SQL 不会被执行；数据完整性依赖 DuckDB WAL。真正解决需独立 admin 连接或引擎侧 shutdown hook（R-12 剩余） |
| 2026-11-13 | `health_expect` 采用**可选 body 子串**而非臆造 Quack 握手 | 无协议规范可依；实测 greeting 后给出推荐值，保留 liveness-only 默认 |
| 2026-11-13 | 扩展**预装 + 签名 core**，不在启动时 INSTALL | 远程 INSTALL 与 `-unsigned` 是供应链面；v2.0.0 的 quack 在 core 仓库且签名 |
| 2026-11-13 | **单机无副本**：`gen-proxy` 只允许单分片 | 容器实测两个 DuckDB 进程打开同一 `.db` 报 `Could not set lock on file`；跨库又是分片语义。Gate 1 的「同库同 token 副本组」假设不成立（R-04 / R-20） |
| 2026-11-13 | **停机必须确认整组消亡**，`cmd.Wait()` 不算完成 | 真实引擎 E2E 复跑出现 `shard stopped` 已打印但端口仍占用；组长是 bash wrapper，`Wait()` 不等待 DuckDB / `tail` 后代。改为 `kill(-pid, 0)` 探测 + 超时整组 SIGKILL（R-21）。Windows 无进程组语义，保持 no-op 且属非目标 |
| 2026-11-14 | **reload 做字段级 diff**：变化的分片单独重启，影响进程的全局参数变化重启全部，健康类参数不重启 | 原「只按名称增删」让 port/token/database/init_sql 改动静默不生效。省略的 token 会被解析成随机值并回填，diff 必须先归一化，否则每次 reload 都会误重启所有分片（R-07） |
| 2026-11-14 | **分片端口已被占用时拒绝启动**，不做「看起来健康」的接管 | daemon 崩溃后孤儿仍持端口，新 daemon 的健康探测实际由孤儿应答、status 却报 `healthy`。改为 fail loud 并要求运维清理遗留进程组；自动接管属真 HA，不属本次范围（R-22） |
| 2026-11-14 | daemon crash 演练落成脚本并进入**必需** CI job | 该演练不依赖外部网络，可稳定复现 R-01 的实际影响并守住 R-22 修复，与当时仍为 advisory 的 `quack-e2e` 区别对待（**该差异已于 r10 消除**：`quack-e2e` 也改为严格离线并转必需） |
| 2026-11-15 | 部署产物必须用厂商真实二进制校验 | `haproxy -c` 语法通过但 HAProxy 3.4.7 运行时仍告警 root 无 `chroot`/`user`（R-23）；单测断言生成文本无法替代解析器。落地 `scripts/verify-deploy.sh` + 必需 CI job `deploy-verify`。r5 的「环境阻断」结论作废 |
| 2026-11-15 | 不自动注入 HAProxy 加固指令 | `chroot`/`user` 需主机预置账号与目录；quack-proxy 只生成配置，注入会让未预置的主机直接不可用。改为文档化 + 生成文件内提示（R-23） |
| 2026-11-15 | **加固改为显式 opt-in** `proxy.hardened` | r7：仅提示不足以闭环。新增开关让生成器输出生效的 `chroot`/`user`/`group`，并配套 `scripts/verify-hardening.sh` 预置账号目录 + 用真实 HAProxy 验证降权与转发。负例证明未预置账号时 HAProxy 直接拒绝启动，因此默认值仍为 false（R-23 从「文档化缓解」升级为「可验证的加固路径」） |
| 2026-11-15 | **用补丁工具链清零漏洞基线，扫描转必需** | `go1.26.11` 的 11 项告警是 stdlib 版本问题，不是业务代码问题；`go1.27.4` 实测 `No vulnerabilities found`（SCAN_RC=0）。新增必需 `vuln-scan` job，并让 release 固定用 1.27.4 构建，避免「扫描干净但发布产物仍是旧 stdlib」 |
| 2026-11-16 | **撤下 Windows 发布产物，运行时显式拒绝**（R-18 收口） | PRD §8 从 v0.1 起就把 Windows 列为 non-goal，但 release 仍在发签名 `.exe`。真实 Windows 上 `version` 正常而 `start` 报误导性的「duckdb_bin 不可执行」——根因是 `lookupBin` 用 POSIX `mode&0o111`，Windows 的 `os.Stat` 对普通文件返回 0666/0444，**任何绝对路径引擎都被误判**。分发一个看起来能用的产物比不支持更糟；改为不发、启动即说明原因 |
| 2026-11-16 | **真实引擎 E2E 用离线通道完整复跑** | 容器内 `INSTALL quack` 卡住不等于通道被拦：主机 HTTPS 取 pinned DuckDB CLI（SHA256 与脚本 pin 一致）与官方签名 core 扩展，离线喂入容器后 `ALL_QUACK_E2E_CHECKS_PASSED`。「外部依赖不稳定」应区分为「下载慢」与「逻辑缺陷」，不必据此放宽门禁 |
| 2026-11-15 | **systemd 实机演练落成必需门禁** | r6 的 `systemd-analyze verify` 只解析文本；r7 在真实 systemd 255 PID 1 容器安装并启停 shipped unit，证明非 root + `RuntimeDirectory` 路径真正可用。r5/r6 的「环境无 systemd」结论同样属于通道受限，用 `--cgroupns=host` + cgroup2 可写挂载即可 |
| 2026-11-15 | **LAN 明文风险必须有实证，不能只写在文档里** | r5~r7 一直把「LAN 明文抓包」列为未做，但从未声称做不到。r8 用 Docker bridge 隔离网段 + 独立嗅探容器的 `AF_PACKET` 原始帧解析，在真实 daemon（`bind_host: 0.0.0.0`）下捕获到 token、`Authorization: Bearer` 与 SQL 正文；同一网段的 TLS 控制通道不泄漏 canary（负例）。结论：这是**可验证的已知弱点**，不是待办；落成必需 CI job `plaintext-audit`。审计 ≠ 修复，`disable_ssl` 硬编码仍需设计决策（R-05） |
| 2026-11-15 | **advisory 只保留在 PR 路径**（**r10 已取代，历史记录**） | `quack-e2e` 依赖 GitHub Releases 与扩展 CDN（实测 502）。脚本加 3 次下载/安装重试后，CI 改为仅 `pull_request` 事件 advisory；`push main` 与 `workflow_dispatch` 为必需门禁，主分支不得静默跳过真实引擎校验 |
| 2026-11-16 | **advisory 全部取消，`quack-e2e` 转严格离线** | 清单里最后一项「本环境能做却一直被当作做不到」。新增 `prefetch-quack-e2e.sh` 在 runner 上一次性预取并校验 CLI + `quack` + `httpfs`，`actions/cache` 复用；测试进程 `QUACK_E2E_OFFLINE=1` 下无 `curl` / 无 `INSTALL`，缓存缺失或损坏即硬失败。CI 移除 `continue-on-error`，PR 也为必需。残余风险如实保留：首次 cache miss 仍需上游一次，但不再有逃生口 |
| 2026-11-16 | **「离线」必须指运行离线，不只是产物离线** | r9 的 E2E 产物是离线的，但容器仍联网、由 DuckDB autoload 下载 `httpfs`。`--network none` 复跑才暴露磁盘库分片报 `read-only crypto module loaded`。判别：`:memory:` 正常、磁盘库失败。修法是把 `httpfs` 一并 pin 进缓存，并把结论写成与证据等宽的口径 |
| 2026-11-16 | **不在本轮启用产品 TLS；但必须把「能力受限」口径更正** | R-05 连标多轮「内建 TLS 属需设计决策」，读起来像引擎不支持。r12 用 pinned v2.0.0 引擎实证 `disable_ssl=false` → `https://`、TLSv1.3 握手、`quack_generate_keys()` + `ssl_fingerprint` 校验全链路可用；`supervisor.go` 的 `disable_ssl=true` 是产品主动关闭。因此剩余决策不是「实现 TLS」，而是「把已有能力接进产品」的契约设计（证书/指纹分发、健康探测、status.json、HAProxy），须甲方拍板，工作量与风险边界完全不同。落成必需门禁 `engine-tls-capability` 并含 `PRODUCT_DISABLES_ENGINE_TLS=1` 诚实性断言，防止把引擎审计读成产品已具备 TLS |
| 2026-11-16 | **CI 门禁契约必须机器可验证，不能只写在注释里** | r12 让 `engine-tls-capability` 与 `quack-e2e` 共享同一 pinned 缓存，但 key / pin / `needs` 的一致性只由注释和文档保证；改 pin 不换 key、给 job 加 `if`、action 改回 tag pin 都不会当场失败。r13 新增 `internal/cicontract`（随已必需的 `go test ./...`，零新增 job / 下载）解析真实工作流并断言 9 个门禁、无 advisory、全 SHA pin、共享缓存 `path`/`key` 一致且含三个 pin 前缀、`needs` 链、离线开关；9 个单点篡改负例要求恰好对应检查失败。三段链路在 `--network none` 容器 3/3 RC=0。**边界**：不是新门禁、不改产品、不消除 r10 首次 cache miss 依赖 |
| 2026-11-13 | release 用 cosign keyless OIDC 签 `SHA256SUMS`，Actions 全 SHA pin | 单有 SHA256SUMS 只是完整性，不是真实性（R-14） |
| 2026-11-13 | 真 HA（control plane 冗余、跨机发现、Coordinator 常驻）**不列入本次整改** | 属新产品立项，需含验收标准的 PRD + 架构 RFC |

## 8. 验证记录（2026-11-13 ~ 11-16 实机）

环境：Windows 主机（便携 Go 1.25.0）+ Docker `golang:1.25`（Linux，go1.25.14，gcc 可用）。WSL 被安全策略禁用，改用容器。

| 检查 | 结果 |
|------|------|
| `gofmt -l .` | 空 |
| `go vet ./...` | 通过 |
| `go test ./... -count=1`（Windows） | 5 包全绿（POSIX 相关显式 SKIP） |
| `go test ./... -race -count=1`（Linux 容器，r4 重跑） | **5 包全绿**，含真实进程组集成测试与 R-21 孤儿回收回归（专项 `-v` PASS 2.13s） |
| 交叉构建 | linux / darwin / windows amd64 均通过 |
| YAML 解析 | ci.yml / release.yml / example.yaml 用 `yaml.v3` 实解析通过 |
| CLI E2E | `version`=dev；跨库 `gen-proxy` 拒绝；单分片生成；token 脱敏 / `--show-tokens` |
| **真实 DuckDB + Quack E2E** | pinned v2.0.0 + SHA256 校验 + 签名 core quack；`GET /` → HTTP 200 greeting；health loop 报 healthy；正确 token `ATTACH/SELECT` 返回 42；错误 token 被拒；50 次往返 221ms (~226 qps)；stop 后端口释放无孤儿 → `ALL_QUACK_E2E_CHECKS_PASSED` |
| **R-21 修复后 3 轮复跑** | 3/3 通过（端口 19491 / 19512 / 19513），每轮停止后端口稳定释放；第 1 轮 50 次往返 312ms (~160 qps)。扩展采用离线预装，绕开实测返回 502 的扩展 CDN |
| **R-07 真实换端口 reload**（`-race -v`） | PASS 0.22s：真实子进程组释放旧端口并绑定新端口；8 个 diff 单元用例全绿 |
| **R-22 daemon crash 演练** | `SIGKILL` daemon 后孤儿存活、端口占用、pid/status 残留。修复前 `FALSE_HEALTHY=1`（`healthy` 由旧孤儿应答）；修复后 `RESTART_REFUSED_ON_ORPHAN_PORT=1`、`FALSE_HEALTHY=0`。演练带断言，已作必需 CI job |
| `go test ./... -race -count=1` / `-cover`（r5） | 5 包全绿；67 个测试函数；supervisor 覆盖率 60.0%（r4 为 55.1%） |
| **r6 真实部署产物校验** | HAProxy **3.4.7 LTS**：`haproxy -c -f` RC=0 / `Configuration file is valid`；故意写坏的配置被拒；`--forward` 前端 19600 → 后端 19601 返回 `quack-forward-ok`。systemd **255**：`systemd-analyze verify` RC=0；`KillMode=TOTALLY_BOGUS` 负例输出 `Failed to parse kill mode specification`。脚本 `scripts/verify-deploy.sh`，CI 必需 job `deploy-verify` |
| `go test ./... -race -count=1` / `-cover`（r6） | 5 包全绿；**68** 个测试函数；proxy 覆盖率 **97.8%**、supervisor 60.0% |
| **r5 真实 DuckDB E2E + 自愈** | 5 轮 `ALL_QUACK_E2E_CHECKS_PASSED`；`SIGKILL` 分片整组后 ~31s 自愈（`restarts=1`），证明端口预检不误伤健康循环。1 次冷启动超过原 30s 等待窗口，脚本窗口已加长到 60s 并增强诊断，随后 3/3 通过 |
| **单写者冲突验证** | 两个 DuckDB 进程打开同一 `.db` → `IO Error: Could not set lock on file`，据此收紧 `gen-proxy` |
| **r7 真实 systemd 255 非 root 安装演练** | `scripts/systemd-install-drill.sh`：安装 shipped unit → `systemctl enable/start` → 服务 `User=quack-proxy`、`MainPID` 属主 `quack-proxy`、`/run/quack-proxy` 为 `quack-proxy:quack-proxy 0750`、pid/status 0600、健康到 `healthy`、`status --json` 脱敏且 `--show-tokens` 可还原；`systemctl stop` 后端口释放、无孤儿 stub、pid/status 与 RuntimeDirectory 全清理。**负例**：同一二进制以该账号直接启动且无 `/run/quack-proxy` 时被 `permission denied` 拒绝。3/3 `SYSTEMD_INSTALL_DRILL_PASSED`；CI 必需 job `systemd-drill` |
| **r7 真实 HAProxy 加固验证** | `scripts/verify-hardening.sh`：`proxy.hardened: true` 生成生效的 `chroot`/`user`/`group`；未预置账号的配置被 `haproxy -c -f` 拒绝（`cannot find user id`）；未加固配置在 HAProxy 3.4.7 启动时确实输出 root/chroot/user 告警；加固配置启动后 `/proc/<pid>/status` real uid = `haproxy`(100)、无 root 告警、前端 19620 → 后端 19621 真实转发 `quack-hardening-ok`。HAProxy **3.4.7** 3/3、`2.8.31` 1/1 通过（2.8 的告警负例按主版本显式 SKIPPED）；CI 必需步骤 |
| **r7 govulncheck 基线清零** | `govulncheck@v1.9.0` + `GOTOOLCHAIN=go1.27.4` 实测 `No vulnerabilities found`（**SCAN_RC=0**），对照 r5 的 `go1.26.11` 11 项 Go 标准库告警。新增必需 `vuln-scan` job；`release.yml` 固定 `go-version: '1.27.4'`；CI 矩阵加 1.27 |
| `go test ./... -race -count=1` / `-cover`（r7，Linux 容器） | 5 包全绿；**69** 个测试函数；`proxy` **98.3%**、`supervisor` **80.1%**、`config` 93.8%、`health` 90.9%、`cmd` 27.0%，总计 **71.1%**；gofmt / vet 全绿 |
| **r8 LAN 明文抓包实证**（Docker bridge + `AF_PACKET`） | `scripts/verify-plaintext.sh`：真实 daemon 以 `bind_host: 0.0.0.0` 起分片，另两台容器分别为客户端与 on-path 嗅探器。明文端口捕获 367 B，直接在 TCP payload 中读到 `qp-lan-cleartext-token`、`Authorization: Bearer` 与 `SELECT * FROM secrets` → `PLAINTEXT_TOKEN_VISIBLE=1`；TLS 控制端口捕获 5009 B 但 canary 不可见 → `TLS_CONTROL_HIDDEN=1`；真实 CLI 拒绝 `proxy.ssl.enabled=true` → `PRODUCT_TLS_UNAVAILABLE=1`；`status.json` 为 0600 且 token 明文落盘 → `STATUS_TOKEN_AT_REST=1`。默认镜像 **3/3** 通过，换 `jrei/systemd-ubuntu:24.04` 亦 1/1 通过；CI 必需 job `plaintext-audit` |
| **r8 `quack-e2e` 重试加固** | 下载与 `INSTALL quack` 各 3 次重试（绝不回退 `-unsigned` 或 community 源）；CI `continue-on-error` 改为 `${{ github.event_name == 'pull_request' }}`，push main / 手动触发为必需 |
| **r9 真实 Windows 实证**（本机 MINGW64_NT-10.0-19045，纯净 PATH 与 Git Bash 两组） | 旧产物 `version` = `quack-proxy dev` 正常；`start` 两组均报 `duckdb_bin "C:/.../drill-stub.exe" not found on PATH or not executable`，根因 `lookupBin` 的 POSIX `mode&0o111`。修复后 `scripts/verify-windows.sh` 实跑 `WINDOWS_RUNTIME_GUARD_PASSED`：`start` 输出 `Windows is not a supported runtime platform (R-18)` + WSL2 指引，且负例断言旧误导错误 / `failed to load config` / `all shards started` 均不出现 |
| **r9 真实引擎 E2E 完整复跑** | 主机 HTTPS 取 pinned DuckDB v2.0.0 CLI（24 MB / 48s，SHA256 `c6507206…` 与脚本 pin 一致）+ 官方签名 core 扩展（19 MB gzip → 60 MB）离线喂入容器：`ALL_QUACK_E2E_CHECKS_PASSED`，50 次 SELECT 207 ms（~241 qps），停止后端口释放无孤儿。容器内 `INSTALL quack` 卡住属下载通道慢，不是逻辑缺陷 |
| `go test ./... -race -count=1` / `-cover`（r9，Linux 容器） | 5 包全绿；**71** 个测试函数；`proxy` **98.3%**、`supervisor` **80.1%**、`config` 93.8%、`health` 90.9%、`cmd` 27.0%，总计 **70.9%**（较 r8 的 71.1% 略降：新增 Windows 运行时分支在 Linux 不可达）。gofmt / vet / 三平台交叉构建 / `GOOS=windows go test -c` / 9 个脚本 `bash -n` / 三份 YAML `yaml.v3` 实解析 / `git diff --check` 全绿 |
| **r9 两条真实门禁复跑**（最终源码） | 真实 systemd 255 非 root 安装演练 `SYSTEMD_INSTALL_DRILL_PASSED`（含 `SYSTEMD_DRILL_NEGATIVE_CONTROL_OK=1`）；LAN 明文抓包 `PLAINTEXT_AUDIT_PASSED`（`PLAINTEXT_TOKEN_VISIBLE=1` / `TLS_CONTROL_HIDDEN=1` / `PRODUCT_TLS_UNAVAILABLE=1` / `STATUS_TOKEN_AT_REST=1`） |
| **r10 无网络容器全链路 + 4 个负例** | 单个 `--network none` 容器：prefetch miss/hit/严格离线断言全通（CLI `c6507206…`、`quack` `9c893bd5…`、`httpfs` `77e6c429…` 三个 SHA256 均 `OK`、`OFFLINE_CACHE_USED=1`）；`QUACK_E2E_OFFLINE=1 --benchmark 50` → `ALL_QUACK_E2E_CHECKS_PASSED`（50 SELECT 230 ms ~217 qps、正确/错误 token、healthy、停止无孤儿）。4 个负例（空缓存 / 无 cache-dir / 篡改 httpfs / prefetch 离线断言）全部非零退出且无 `all shards started`。判别负例：不预装 httpfs 时无网络磁盘库写失败，`:memory:` 正常 |
| **r11 HAProxy + VIP 真实联调**（`scripts/verify-vip-failover.sh`，必需 CI job `vip-failover`） | Docker bridge 上两个 `haproxy:2.8-alpine` 节点（`--user root` + `NET_ADMIN`）各跑真实 HAProxy + 独立 stub；真实 CLI 生成的 1004 B 配置经 `haproxy -c -f` 接受并实际转发。真实 `/32` VIP 由 A 漂移到 B（GARP，客户端不做 ARP flush）：`VIP_PRIMARY_NODE=NODE_A` → `VIP_FAILOVER_NODE=NODE_B`，`SESSION_DISCONTINUITY=1` → `VIP_FAILOVER_DRILL_PASSED`。负例：VIP 无人持有 / VIP 撤下后必须不通、多分片配置被 `gen-proxy` 拒绝、坏配置被 `haproxy -c` 拒绝；另做变体负例（两节点同标识 → 脚本 rc=1）证明身份断言非空转。两个不同网段各通过一次、退出无残留。**边界**：VIP 由脚本模拟（keepalived / 云浮动 IP 替身），产品不提供也不感知；入口恢复但会话与数据不连续，R-04 / R-05 均未修复 |

**仍未做的验收**：R-05 的 `disable_ssl` 硬编码 / token 不落盘 / **产品接入 TLS** 属**需设计决策**（r8 的抓包门禁、r11 的 VIP 联调、r12 的引擎能力审计都只证明事实、不修风险）。**r12 已解除「引擎是否支持 TLS」这一隐含前提**：`scripts/verify-engine-tls.sh` 在 `--network none` 容器内用 pinned v2.0.0 引擎证明 `disable_ssl=false` 可起 HTTPS（TLSv1.3）、`quack_generate_keys()` 生成自签证书、客户端 `ssl_fingerprint` 校验生效，且断言产品仍硬编码 `disable_ssl=true`（`PRODUCT_DISABLES_ENGINE_TLS=1`）。剩余决策收窄为产品接线契约：证书生命周期与私钥存放、指纹分发/轮换通道、健康探测改 HTTPS、`status.json` 契约、`gen-proxy` 的 HAProxy 前端与 `proxy.ssl` 死配置。**不得据此宣称 quack-proxy 已具备 TLS**。真 HA 属新产品立项。**r11 已解除**：HAProxy + VIP 联调（Docker bridge 双容器即可实证，不必等物理浮动 IP；但口径是「生成配置在浮动 IP 入口下可用」，**不是**产品具备 HA，脚本实证 `SESSION_DISCONTINUITY=1`）。r10 如实保留的残余风险：CI 首次 cache miss 仍需访问上游一次（runner 侧、带重试），缓存命中后测试进程零网络。**r13 已解除**：「共享缓存 key / pin / `needs` 一致性」不再只靠注释，已由必需 Go 套件断言且三段链路在无网络容器按 CI 顺序复跑；注意这与 r10 的首次 cache miss 依赖是两件事，后者**未消除**。**r10 已解除**：`quack-e2e` 在 PR 上的 advisory。**r9 已解除**：Windows 产物去留（已撤下发布产物并落成必需门禁）、真实引擎 E2E 完整复跑（r10 进一步收紧为严格离线）。**r8 已解除**：LAN 明文抓包实证。**r7 已解除**：非 root systemd 安装演练、R-23 生产加固、govulncheck 基线清零转必需。

**r8 的教训与 r6/r7 不同，但同源**：r5~r7 把「LAN 明文抓包」写成未做，并没有说「环境不允许」——只是没做。本轮证明两台容器 + 一个 `AF_PACKET` 原始套接字就能实证，不需要第二台物理机。前两轮是「通道/参数受限误记为能力受限」，这一轮是「**未做被清单化后默认成了做不到**」。清单里的「未做」必须逐条问一句「现在真的做不到吗」。

**r9 是同一问句的另一半答案**：「Windows 产物去留」被连续四轮标为「属产品决策，待定」，但 PRD §8 从 v0.1 起就写了 Windows 是 non-goal —— 决策早就有了，缺的是执行。本机就是 Windows，一次 `version` + `start` 实测就暴露了比「不支持」更糟的状态：产物看起来能用、失败原因还是错的（POSIX mode 位误判）。**「待决策」有时是「已有决策没落地」的委婉说法**；同理，「外部依赖不稳定」也要拆开看：本轮主机通道 48 秒下完 pinned CLI、5 秒下完签名扩展，E2E 完整通过，容器内卡住只是下载慢。

**r10 是第三种形态，而且指向自己的上一轮**：r5/r6 把「通道受限」误记为「能力受限」，r8 把「未做」误当成「做不到」，r9 则把「产物离线」写成了「运行离线」——结果没造假，但**结论的口径比证据更宽**。只有把容器网络真正掐断（`--network none`），才会看到 DuckDB 偷偷 autoload 了 `httpfs`。修法不是撤掉 r9 的结论，而是补齐缺失的 pin、把条件收紧到可复现，并把新发现的运维约束（离线部署需预装 `quack` + `httpfs`）写进基线表。另一个副产品：本机 CDN 对 `httpfs` 对象反复在 ~8 MB 处断流，最终用精确 `Range` 取尾段 412631 字节拼接、gzip 与解压双 hash 校验通过 —— **下载断流仍不等于拿不到**。

**r11 是第一种形态的回归，但也要防结论反向膨胀**：「HAProxy + VIP 联调」被写成「需真实浮动 IP / 双机网络」多轮，实际 Docker bridge + `NET_ADMIN` + `--user root` 即可（haproxy 镜像默认 uid 99 会让 capability 失效，属参数问题）。清单里的「环境要求」同样要逐条问「现在真的做不到吗」。**同时**：联调通过只说明生成配置经得起入口漂移，不说明产品有 HA —— VIP 是脚本搬的，切换后 `SESSION_DISCONTINUITY=1`（后端身份变化、会话与数据不连续），R-04 仍成立。把「验证范围」写窄，和把「未做」判成「做不到」一样重要。

**r6 已解除 r5 的「环境阻断」结论**：真实 `haproxy -c -f`（HAProxy 3.4.7 LTS）与 `systemd-analyze verify`（systemd 255）均已跑通，并落地为**必需** CI job `deploy-verify`，见 `scripts/verify-deploy.sh` 与 `REMEDIATION.md` §5。r5 的三次失败只是 apt 通道被拦，不等于所有安装通道被拦。

**r7 再次印证同一教训**：r6 曾写「环境无 systemd，实机演练未做」。实际是启动方式不对——Docker Desktop / WSL2 下用 `--privileged --cgroupns=host` + 把 `/sys/fs/cgroup` 以 **rw** 挂成 cgroup2（私有命名空间会因 `Failed to create /init.scope control group: No such file or directory` 退出），systemd 255 即可作为 PID 1 正常 `is-system-running=running`。**连续两轮把「通道/参数受限」误记为「能力受限」**，已在此更正。

## 9. 变更控制

若产品要升级为「真 HA 集群」（control plane 冗余、跨机发现、Coordinator 常驻），视为**新产品立项**：须重新编写含验收标准的 PRD 与架构 RFC，不在本次整改范围内。

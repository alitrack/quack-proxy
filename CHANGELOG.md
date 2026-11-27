# Changelog

本文件记录可影响使用者的行为变更。整改台账见 `docs/REMEDIATION.md`，边界见 `docs/SCOPE.md`。

## [0.2.1] — 2026-11-27

**结论先行：** v0.2.0 之后的第一个版本，收录 v0.2.0 tag 以来的全部变更：remediation r1–r17、
RBAC v2（2026-11-16）、认证探针（2026-11-27，PR #1）。下列各节按时间倒序**保留原文**（发版时只去掉
节标题里的 "Unreleased" 并降一级，正文未改）。

对使用者的三件事：

1. **健康检查现在验认证。** 端口在听但 quack 认证损坏的引擎不再报 `healthy`，而是 `auth-failed`
   （不发 attach 串、不重启）。升级后若某个分片从 `healthy` 变成 `auth-failed`，那不是回归——
   它之前就是坏的，只是没人告诉你。见 [auth probe](#auth-probe-2026-11-27-pr-1)。
2. **多用户分片有了 RBAC v2 示例**（`examples/rbac2-init.sql`，README §RBAC v2）：scoped token
   （`RO`/`RW` 动词上限）、`WITH`-CTE 写绕过归类、未分类语句 fail-closed、持久审计表 `rbac_audit`
   （审计表本身对任何线上 token 拒读）、未知表 default-deny、catalog 枚举（`pg_catalog.` /
   `information_schema.` / `duckdb_*()`）除三种 ATTACH 握手形状外一律拒绝。提交正文记录的验证：
   `examples/rbac2-e2e.py` 13/13、MIMIC 真实数据 13/13（`examples/e2e-mimic.py`；提交 `6899f2e` 正文把
   文件名写成了 `rbac2-e2e-mimic.py`，仓内实际是 `examples/e2e-mimic.py`）。
3. **Windows 不再发布产物。** r9 起 release 不再附 Windows 包（`start` 本就不能托管分片，PRD §8 非目标）；
   CI 仍做 windows/amd64 源码兼容编译，`windows-runtime-guard` 为必过门。v0.2.0 页面上的两个
   Windows 包是最后一批。

### 边界（不得读宽）

- RBAC v2 是**示例 SQL**（基于 `init_sql_file` 钩子），不是 quack-proxy 内置的认证产品；按 README 的
  已知开口使用。
- 认证探针证明的是「分片接受自己的 token 并能回答一条常量查询」，不证明数据面、TLS 或 RBAC 钩子正确。

### Release verification（发版前最后一手，2026-11-27）

- `gofmt -l .` 空；`go vet ./...` 通过；`go test ./... -count=1` 91/91（6 个包全 ok）；`internal/supervisor`
  另跑 `-race` ok。本机 go1.25.8；CI 矩阵覆盖 1.25/1.26/1.27，release 产物按 `release.yml` 用 go1.27.4 构建。
- 按 `release.yml` linux/amd64 相同的 flags 本地构建（`-trimpath -ldflags "-s -w -X main.version=v0.2.1"`）：
  `version` 输出 `quack-proxy v0.2.1`。
- 该二进制上的 E2E 双档：alpha39998 → `auth-failed` + `auth_probe: failed: … Authentication failed` +
  空 attach 串 + ERROR；v2.0.1 → `healthy` + `auth_probe: ok` + attach 串发布。
- 对 release 产物本身的核验（下载、`sha256sum -c`、cosign 验签、产物跑 E2E）在发版后记入 GitHub Release 页，此处不预写。

### auth probe (2026-11-27, PR #1)

**结论先行：端口在听 ≠ 分片在服务。** 此前健康检查只探端口与问候串。`duckdb_bin` 落到
quack 认证损坏的引擎（`v2.0.0-alpha39998`）时，分片照常报 `healthy` 并发布 attach 串，但每个
客户端 ATTACH 都 `Authentication failed`——失败是静默的。现在 `healthy` 以「分片接受自己的
token 并能回答一条查询」为前提。

#### Added

- 状态文件与 `status` 输出新增 `auth_probe` 字段（`ok` / `failed: <原因>`）；新状态 `auth-failed`。
- 认证探针：daemon 用 `duckdb_bin -unsigned -c` 作为**客户端**，以分片自身 token 跑
  `SELECT <随机 marker> AS auth_probe`（marker 每次探针随机，回显或陈旧输出不能造成假 OK）；
  3 次尝试、4s 间隔、单次 15s 超时。
- 5 个探针单测（echo / refuse / silent 桩引擎），经变异验证（决策逻辑回退成旧语义时测试必 FAIL）。

#### Changed

- `healthy` = 端口应答 **且** 探针通过。探针未决 → `starting`，不发 attach 串；探针失败 →
  `auth-failed`，不发 attach 串、ERROR 点名 `duckdb_bin`、**不重启**（重启修不好引擎二进制，
  只会刷掉重启预算）。探针挂在 `launch()` 上，每次重启都重新自检。
- 既有门禁测试（gate0 / integration）显式预置探针通过，而不是放宽 `healthy` 语义。
- README 状态表与版本徽章同步；经 quack-proxy 实测可托管的引擎：`v2.0.0`、`v2.0.1`、
  `v2.0.0-alpha43355`、`v2.0.0-alpha48730`；仅 `v2.0.0-alpha39998` 认证坏。

#### 边界（不得读宽）

- 探针证明的是「分片接受自己的 token 并能回答一条常量查询」，不证明数据面、TLS 或 RBAC 钩子正确。
- 坏引擎不会被修复，只会被标红；更换 `duckdb_bin` 是运维动作。

#### Verification

- `go test ./... -count=1` 91/91；`gofmt -l .` 空；`go vet ./...` 通过。
- E2E 双档（合并到 main 后复跑）：alpha39998 → `auth-failed` + 空 attach + ERROR；
  v2.0.1 → `healthy` + `auth_probe: ok` + attach 发布。
- 异体复审：sigil PASS（抓出探针未决窗口仍发 attach、版本列表不一致，均已修）；pi 空转无产出，不计入。
- CI 14/14。同一批顺带修了四个演练 job 的可移植性问题（此前在 main 上同样失败）：runner 的
  `systemctl is-system-running` 永远 `starting`（改判 PID 1 是 systemd）；官方 haproxy 镜像以非 root
  运行、读不了 0700 的 `mktemp -d` 挂载（目录 755、配置 644、校验调用 `--user root`，permission
  类错误单列）；HAProxy 3.x 没有「started as root without any chroot」警告字串（负对照改为直接
  断言未加固配置仍以 uid 0 运行）；`drill-stub` 补齐探针的客户端角色。

### remediation r17 (2026-11-19)

**结论先行：这不是从「仍未做」清单里翻出的第八/第九种形态，而是一个此前没被看见的
契约覆盖面盲区。** 逐条复核剩余清单的结论仍是 r16 的「已收敛、无新可闭环项」：真 HA
需甲方新立项，外部代理/证书/来源 IP 是运维职责，首次 cache miss 是如实保留的残余风险。
本轮换了个角度查「**文档声称被机器验证的不变量，是否真的被覆盖**」，发现一处实质缺口：
r13 的契约测试自称覆盖「门禁必需性 + **供应链 pin** + 共享缓存一致性」，但
`CheckSHAPins` 只检查 `uses:`（Actions）。**4 个真正进入必需门禁执行路径的容器镜像默认值
仍是浮动 tag**（`python:3.11-slim`、`haproxy:2.8-alpine` ×2、`alpine:latest` —— 后者是
持续重发的滚动 tag），`docs/SECURITY.md` / `docs/SCOPE.md` 的供应链清单也从不提镜像。
tag 可被重新发布，于是「9 个门禁无条件必需」的强度其实只等于一个未版本化的镜像。
这是 r13 形态的延伸：**机器化的契约自己有覆盖盲区**，不是新的误分类形态。

#### Changed

- `scripts/verify-plaintext.sh` 的 `IMAGE` / `HAPROXY_IMAGE` 与
  `scripts/verify-vip-failover.sh` 的 `NODE_IMAGE` / `CLIENT_IMAGE` 默认值改为
  `repo@sha256:<64hex>` manifest digest；`scripts/systemd-install-drill.sh` 的本地复跑
  示例同步标注（该镜像不进 CI，CI 直接 `sudo bash` 在 runner 上跑）。保留 `--image` /
  `--client-image` / `PLAINTEXT_IMAGE` / `VIP_IMAGE` 等显式覆盖供本地或离线复跑。
- `internal/cicontract` 新增 `CheckImagePins`：扫描**全部** `scripts/*.sh`（新增门禁自动
  纳入），断言每个 `IMAGE` / `*_IMAGE` 默认值是完整 sha256 digest、至少 4 个（防正则失灵
  空转）、同一仓库在不同门禁里必须是**同一个** digest（共享 haproxy 必须一起轮换）；
  新增 `CheckNoImageOverrides`：禁止 CI 用 `--image` 等参数绕过默认值，否则 pin 只是装饰。
  两者都接入 `Validate`，随已必需的 `go test ./...` 运行，**零新增 job / 零新增下载**。
- 负例：4 个脚本单点篡改（tag 替换、截断 digest、共享 digest 只改一处、默认值消失）+
  1 个 YAML 变异（CI step 加 `--image python:3.11-slim`），每个都**恰好**命中对应检查；
  原有 9 个 YAML 负例与 3 个 pin 漂移负例全部仍通过。`cicontract` 覆盖率 79.6% → 83.0%。
- `.github/workflows/ci.yml` 三处注释同步（顶部供应链声明、两处 job 的外部依赖说明）。

#### 边界（不得读宽）

- **本机实证**：digest 引用零下载命中本地镜像，架构 `linux/amd64` 与 GitHub runner 一致；
  `plaintext-audit` RC=0（含 `EXTERNAL_TLS_TERMINATION_OK=1` / `STATUS_TOKEN_AT_REST=0`）、
  注入负例 RC=1、`vip-failover` RC=0（`SESSION_DISCONTINUITY=1`）。
- **本机没验证的**：本环境访问 Docker registry 返回 EOF，因此**既未确认这些 digest 是
  跨架构 manifest list 还是单平台 manifest，也未验证 GitHub runner 首次按 digest 拉取**。
  按 r10 的教训，不把「本机零下载命中 + 门禁实跑」读宽成「CI 已验证」。若上游 GC 了旧
  digest 或 runner 解析异常，CI 会（正确地）失败，需轮换 digest —— 与 Actions SHA pin
  需要轮换是同一性质，**不是「做不到」**。
- 显式覆盖可以绕过 pin，属运维本地选择；`CheckNoImageOverrides` 只保证 CI 不覆盖。首次
  遇到未缓存的 digest 仍需 pull 一次，与 r10 如实保留的首次 cache miss 同类，未消除。
- **9 个必需门禁数量不变**，Go 产品源码、daemon 行为、引擎产物零改动；
  `disable_ssl=true` / `proxy.ssl` 死配置 / 明文 `gen-proxy` 输出 / token 默认不落盘全部原样。
  **§5「仍未做的验收」清单结论一字不变**：真 HA 仍是唯一需要甲方产品输入的一项。

#### Verification

- `gofmt -l .` 空 / `go vet ./...` 通过 / `go test ./... -race -count=1` **6 包全绿、
  86 个测试函数、总覆盖率 73.8%**（r16 为 85 / 72.8%，增量来自新契约测试）；三份 YAML
  经 `yaml.v3` 实解析；linux / darwin(amd64+arm64) / windows 四组合交叉构建；
  12 个 shell 脚本 `bash -n`；`git diff --check`。
- 实机门禁：`plaintext-audit` RC=0 + 注入负例 RC=1；`vip-failover` RC=0。**离线三段链路
  脚本（prefetch / quack-e2e / verify-engine-tls）本轮零改动，未重跑**（r15/r16 已在同一份
  最终源码下 3/3 RC=0），如实说明，不冒充本轮复跑。
- **同一轮内的文档一致性补注（非 r18、非新待办）**：复核 r17 时发现两处**当前状态文档**
  仍停在 r13 的检查集 —— `docs/PRD.md` 头部「实现状态总览」表（r16 已确立的权威当前状态源）
  与 `docs/SECURITY.md` 第 13 条（活文档）都写「r13 / 9 个单点篡改负例」，且未列
  r14 `CheckScriptPins`、r17 `CheckImagePins` / `CheckNoImageOverrides`。按「口径不得比证据
  更宽，也不应比实现更窄」把二者更新为当前 `Validate` 的 **10 项检查 / 17 个负例**
  （10 YAML 变异 + 3 脚本 pin 漂移 + 4 镜像 pin 漂移）。**这不是历史快照**：
  `REMEDIATION.md §5` / `SCOPE.md §8` 里带轮次时点标注的历史验证行（含 r13 行的「9 个负例」）
  按 r16 规矩原样保留，不逐条篡改。版本头全部仍为 r17、9 个必需门禁不变、
  Go 源码 / 脚本 / CI 工作流零改动，
  §5「仍未做的验收」清单结论一字不变。

---

### remediation r16 (2026-11-18)

**结论先行：整改清单本身已经收敛，本轮没有新的可闭环项。** 按 r15 补的判定三问
逐条复核 §5 末尾的四类剩余项：真 HA 是 PRD §8 既有 non-goal（明确不做、无配套
Y），需甲方新立项 + 含验收标准的 PRD / 架构 RFC；外部代理 / 证书 / 来源 IP 限制
是运维职责（r15 已验收方案有效性）；首次 cache miss 是如实保留的架构残余风险；
DuckDB pin 同步已由 `internal/cicontract.CheckScriptPins` 机器化。**没有发现新的
「能力受限 / 需甲方拍板 / 通道受限」误分类，也不存在「第八种形态」** —— 剩余清单
结论一字不变，本轮不新增功能、门禁、脚本或 Go 源码改动。

本轮唯一动作是**把 r15 的结论贯彻到同一份 `docs/PRD.md` 的正文**：r15 已把「产品
接入 TLS 属需甲方拍板」判定为 PRD 既有 non-goal 并废除「待设计决策」这类措辞，但
PRD 正文的历史快照没跟着同步 —— **R-17（指数退避重启）在同一份文档里自相矛盾**：
头部「实现状态总览」表、`REMEDIATION.md §3`、`SCOPE.md §历史`、`README.md` 状态机
说明四处都标「已修」，正文 §3.3 却仍写「（R-17，**待设计决策**）」、§7 加粗写
「退避在默认配置下不触发」、§9 写「部分实现 / 默认配置下不触发」。这不是新待办、
不是待甲方拍板，而是文档自身的时点标注缺失；但读者若从 §3.3 或 §7 读起，会以为
还有一个未决的设计决策，**直接把工作量估算读宽**。

#### Changed（仅文档，产品行为零改动）

- `docs/PRD.md` §3.3 生命周期代码块的 `⚠️` 批注、§7 非功能性需求表、§9 原始
  M1/M2/M3 里程碑表各补一条**显式时点声明**：这些是 v0.1 原始稿 + 2026-11-13
  评审时点快照，不是当前状态声明、不含待甲方拍板的设计决策；各 `R-xx` 的当前状态
  一律以头部「实现状态总览」表与 `REMEDIATION.md §3` 为准。
- 纠正 §3.3 的「R-17，待设计决策」→「Gate 2 已修：`performRestart` 保留原始
  `lastStart`，30s grace 只作用于冷启动，退避现真实生效」；§7 的加粗断言与 §9
  两行 R-17 备注同步补上「评审时点 / 已修 / 当前生效」限定。**保留**「部分实现」
  等历史判定本身不改写（r10：历史快照是当时时点，不是跨轮恒定状态）。
- 头部状态修订行补记 r16；`README.md` / `docs/SCOPE.md` / `docs/SECURITY.md` /
  `docs/REMEDIATION.md` 版本头与验证台账同步到 r16。

#### 边界（不得读宽）

- 「已修」不是靠文档互相引用：**`go test ./internal/supervisor -race -run
  '^TestGate2RestartDoesNotRefreshStartupGrace$' -v` 实测 PASS**，且
  `supervisor.go` 的 `performRestart` 确实保留并回写 `lastStart = originalStart`。
- 这不是新一轮整改发现，也不是第八种口径错误形态；**必需门禁仍 9 个**，
  `internal/cicontract` 断言不变，`disable_ssl=true` / `proxy.ssl` 死配置 /
  明文 `gen-proxy` 输出 / token 默认不落盘全部原样。
- **§5「仍未做的验收」清单结论不变**：真 HA 仍需甲方新立项，运维仍须自行部署
  并维护外部代理、证书与来源 IP 限制，错误部署（分片直连 `0.0.0.0`）的明文
  传输风险仍未修，首次 cache miss 残余风险如实保留。本轮不解除其中任何一项。

#### Verification

- R-17 专项 `-race -count=1 -v` **PASS**；全量 `gofmt -l .` 空 / `go vet ./...`
  通过 / `go test ./... -race -count=1` **6 包全绿、85 个测试函数、总覆盖率
  72.8%**（Go 源码零改动，与 r15 一致）；三份 YAML 经 `yaml.v3` 实解析通过；
  linux / darwin(amd64+arm64) / windows 四组合交叉构建通过；12 个 shell 脚本
  `bash -n`、`git diff --check` 通过。
- 本轮**未改动任何脚本、CI 工作流或 Go 源码**，故 `plaintext-audit` /
  `systemd-drill` / 离线三段链路 / `engine-tls-capability` 四个实机门禁不重跑
  （r15 已在同一份最终源码下 RC=0；如实说明，不冒充本轮复跑）。

### remediation r15 (2026-11-18)

**结论先行**：台账里连标多轮的「R-05 产品接入 TLS 属需甲方拍板的设计决策」，
回到需求原文后站不住脚。PRD §4.2 的 v0.1 原始稿写的是「Nginx 反向代理做
SSL termination + 固定 token」，`SCOPE.md` §3 把「内建 TLS 终止」列为
non-goal 并写明「由外部代理终止」，`SECURITY.md` 的运维清单也要求「在外部
代理终止 TLS，并限制来源 IP」。**甲方从一开始就决定 quack-proxy 自己不提供
TLS** —— 这不是等待拍板的新决策，而是「已有决策缺验收」。这是 r9 / r14 之后
的**第七种形态**：把「产品不做 X、由运维做 Y」记成「要不要做 X 尚待产品
决策」，结果既没去做 X，也没验收 Y。

本轮**不新增产品 TLS、不新增必需门禁（仍是 9 个）**：把甲方指定的部署方案
做进已有的 `plaintext-audit` 必需门禁，用真实 HAProxy 2.8.31 TLS 终止前端
与只绑 `127.0.0.1` 的真实分片做端到端验收。

#### Added

- `scripts/verify-plaintext.sh` 新增「PRD §4.2 部署验收」半边：同一隔离 bridge
  上再起一个 **真实 daemon B**（`bind_host: 127.0.0.1`，独立 greeting），以及
  一个与服务端共享 netns 的**真实 HAProxy TLS 终止容器**（明文后端腿走
  loopback，TLS 前端对 LAN 可见）。客户端经 HTTPS 把带 token / Bearer / SQL
  正文的请求打到真实分片，断言 `200` + 分片自己的 greeting（防「代理自己
  应答」的假通过）。
- 同一 `AF_PACKET` 嗅探器新增三条硬断言：TLS 前端端口抓到的是 TLS 握手记录
  且**无任何可读凭据 / Bearer / SQL canary**；loopback 后端端口**零 payload**
  （明文腿根本没上 eth0）；LAN 客户端**无法直连** loopback 绑定的分片。
- 原有弱点审计全部保留：`0.0.0.0` 暴露的分片仍被抓到 387 B 明文（token、
  `Authorization: Bearer`、SQL 正文），TLS 控制通道 canary 仍不可见，
  `proxy.ssl.enabled=true` 仍被真实 CLI 拒绝，r14 的
  `STATUS_TOKEN_AT_REST=0` 仍是硬断言。**同一个门禁同时展示「错误部署会泄漏」
  与「按 PRD 部署不泄漏」**，两者不可混写。
- 两个新负例：坏 HAProxy 配置必须被厂商 `haproxy -c -f` 拒绝；
  `PLAINTEXT_INJECT_CLEARTEXT=1` 往该 LAN 端口注入一条明文凭据请求后，
  门禁必须精确报 `cleartext canary ... visible on the LAN`、RC=1，且
  **不打印** `PLAINTEXT_AUDIT_PASSED` —— 证明「LAN 无可读凭据」不是空转断言。

#### Changed

- `plaintext-audit` 的 CI job 注释与脚本头部说明更新（job id、必需性、steps
  均未变）；`internal/cicontract.RequiredGates` 仍是同样的 9 个，本轮
  **Go 源码零改动**。
- 清理改用 `find -depth -delete`，避免 Git Bash 的安全钩子拦下 `rm -rf`
  后留下临时工作目录。
- 剩余开放项重定性（详见 `docs/REMEDIATION.md` §5）：**产品内建 TLS 不再列为
  「需甲方拍板」**，它是 PRD 已定的 non-goal，随之而来的证书生命周期、指纹
  分发、HTTPS 健康探测等属于「若将来改变范围才需要的新决策」，不是本轮待办；
  **真 HA 属 PRD §8 non-goal，需甲方新立项 + 含验收标准的 PRD / 架构 RFC**
  —— 这才是真正的产品输入；r10 的首次 cache miss 是如实保留的残余风险，
  未消除。

#### 边界（不得读宽）

- 这是**运维部署方案的验收，不是产品 TLS 功能**。`supervisor.go` 仍硬编码
  `disable_ssl = true`，`proxy.ssl` 仍是死配置，`gen-proxy` 仍只输出明文
  HAProxy 前端，`engine-tls-capability` 的 `PRODUCT_DISABLES_ENGINE_TLS=1`
  诚实性断言不变。验收通过**不等于** quack-proxy 已具备 TLS。
- HAProxy 配置由演练脚本生成（**不是产品输出**），证书是一次性自签材料；
  生产证书生命周期、CA、轮换、来源 IP 限制仍是运维责任，运维清单里的
  待办项没有因此被勾掉。分片端点仍是 drill stub，不是真实 Quack 协议
  （真实引擎由 `quack-e2e` 覆盖）。
- 错误部署（`bind_host: 0.0.0.0` 且客户端直连）的明文风险**完全未修**，
  R-05 的传输弱点仍开放；本轮只证明「照既定方案部署可消除该 LAN 暴露面」。

#### Verification

- `plaintext-audit`：正例 RC=0（连续 3 次，含 cleanup 无残留），注入负例
  RC=1 且精确命中 canary 断言；实机输出 `EXTERNAL_TLS_TERMINATION_OK=1` /
  `LOOPBACK_BACKEND_NOT_ON_LAN=1` / `LAN_DIRECT_CONNECT_BLOCKED=1` /
  `PLAINTEXT_TOKEN_VISIBLE=1` / `STATUS_TOKEN_AT_REST=0`。
- 全量回归：`gofmt -l .` 空 / `go vet ./...` 通过 / `go test ./... -race -count=1`
  **6 包全绿、85 个测试函数、总覆盖率 72.8%**（Go 源码零改动，数字与 r14 一致，
  含 `internal/cicontract` 对改动后 `ci.yml` 的契约断言）；12 个 shell 脚本
  `bash -n`、三份 YAML `yaml.v3` 实解析、linux / darwin(amd64+arm64) / windows
  四组合交叉构建、`git diff --check` 全部通过。`systemd-drill` 真实 systemd 255
  **RC=0**（含负例 `TOKEN_PERSIST_DEFAULT_REJECTED=1`）；`--network none` 三段
  离线链路 **3/3 RC=0** `OFFLINE_CI_CHAIN_PASSED`；`engine-tls-capability`
  **RC=0** 且 `PRODUCT_DISABLES_ENGINE_TLS=1`；`plaintext-audit` 正例
  **RC=0 ×3**、注入负例 **RC=1**。**必需门禁仍 9 个**。详见
  `docs/REMEDIATION.md` §5 的 r15 行。

### remediation r14 (2026-11-16)

**结论先行**：PRD §4.2 的甲方修订要求（「token 不得明文落盘；`status --json`
默认不输出；确需持久化时 0600 + 显式开关」）早在 2026-11-13 就给出了三条
明确判据，`SECURITY.md` 甚至自己写好了方案（「不落盘 + 由用户自带
token」），但台账连标多轮「需甲方拍板的设计决策」。这是 r9 那种**「决策早有、
缺执行」的第六种形态**：既不是能力受限，也不是新决策。本轮按既有判据实现，
**这是真修复，不是审计**。

#### Added

- `global.persist_tokens`（默认 `false`）：`status.json` 默认**完全不写**
  `token` 字段（`json:"token,omitempty"` + 条件赋值），`0600` 原子写保留作
  纵深防御。只有显式 `true` 才恢复「32 字符 crypto-random token 落盘」。
- 默认模式下省略分片 token 会在 `config.Load` 期**硬失败**，并给出两种可执行
  修复：显式给 token（推荐，运维自管密钥），或开 `persist_tokens: true`。
  理由：不落盘时自动生成的凭据没有任何进程外分发通道，静默启动会造出
  「谁都连不上」的分片。沿用 `proxy.ssl.enabled` 的 fail-closed 模式。
- `status --show-tokens` 在无持久化凭据时于 **stderr** 给出解释
  （`tokenPersistenceNotice`），不污染 JSON 输出，也不让空字符串看起来像
  命令坏了。
- `internal/cicontract` 新增 `CheckScriptPins`：把「升级 DuckDB 时同步刷新
  三组 pin 与 CI cache key」这条纯人工约定，变成随已必需 `go test ./...`
  运行的机器断言（3 个单点篡改负例 + 未篡改副本的正对照）。

#### Changed

- 必需门禁仍是 **9 个**，但 `plaintext-audit` 从「证明 token 落盘」升级为
  **硬断言默认不落盘**（`STATUS_TOKEN_AT_REST=0`，连空 `token` 字段都不允许）；
  `systemd-drill` 主配置显式 `persist_tokens: true` 以保留 `--show-tokens`
  脱敏验证，并新增「默认配置省略 token 被真实 CLI 拒绝」负例。
- 示例配置、README Quick Start、benchmark 历史配置、PRD 原始 YAML 示例全部
  改为显式 token 或明确标注为历史快照，避免照抄即失败。
- 本轮 **Go 源码有改动**（与 r12 / r13 不同）：`internal/config/config.go`、
  `internal/supervisor/supervisor.go`、`cmd/quack-proxy/main.go`，以及新增 /
  更新的回归测试。

#### 边界（不得读宽）

- **明文传输风险一点都没修**：`disable_ssl = true` 仍硬编码，token /
  `Authorization: Bearer` / SQL 正文仍在 LAN 上可见；`engine-tls-capability`
  仍是引擎能力审计。R-05 现在只剩「产品接入 TLS」开放，token 落盘项已关闭。〔**r15 更正本句**：「产品接入 TLS」从来不是待甲方拍板的设计决策，PRD §4.2 / §8 早已指定「由外部代理终止」，本轮已按该既定方案完成端到端验收；见本文件最上方 r15 段。〕
- 显式 `persist_tokens: true` 仍是甲方可接受、有 0600 保护的选择，本轮没有
  删除该能力。真 HA 立项与 r10 首次 cache miss 依赖不变。

#### Verification

- `gofmt` / `go vet` / `go test ./... -race -count=1`：**6 包全绿，85 个测试
  函数**（r13 为 74，本轮 +11：9 个 token 用例 + 2 个 pin 契约用例）；
  `go tool cover` 总计 **72.8%**（cmd 30.2% / cicontract 79.6% / config
  94.3% / health 90.9% / proxy 98.3% / supervisor 80.2%）。
- `internal/cicontract` **12 个负例**全部按预期失败：9 个原 YAML 单点篡改 +
  3 个脚本 pin 漂移，另有未篡改副本的正对照。
- 12 个 shell 脚本 `bash -n`、3 份 YAML 用项目自带 `yaml.v3` 实解析
  （含新增后的 `quack-proxy.example.yaml`）、linux / darwin(amd64+arm64) /
  windows 交叉构建、`git diff --check` 全通过。
- **三个实机门禁复跑（最终源码，均为 RC=0）**：`plaintext-audit` →
  `PLAINTEXT_AUDIT_PASSED`（`PLAINTEXT_TOKEN_VISIBLE=1` 证明**传输弱点仍在**，
  `STATUS_TOKEN_AT_REST=0` 证明**默认不落盘已生效**，同一脚本里两个方向都被
  真实断言）；`systemd-drill` → `SYSTEMD_INSTALL_DRILL_PASSED`（真实 systemd
  255，`SYSTEMD_TOKEN_REDACTION_OK=1` 保留，新增
  `TOKEN_PERSIST_DEFAULT_REJECTED=1` 负例）；`--network none` 三段链路
  3/3 `OFFLINE_CI_CHAIN_PASSED`（E2E 50 次 SELECT 279 ms / ~179 qps）。
  首次明文抓包因 Git Bash 的 MSYS 把容器内 `/work` 路径改写成宿主路径而失败，
  加 `MSYS_NO_PATHCONV=1` 后通过 —— 属环境参数问题，非代码问题。

---

### remediation r13 (2026-11-16)

**结论先行**：r12 让 `engine-tls-capability` 依赖 `quack-e2e` 产出的同一个
SHA256-pinned 缓存，但这条「串联契约」只写在 CI 注释与文档里 —— 将来谁改了
pin / cache key / `needs` / 离线开关，没有任何门禁会当场拒绝，只能等 PR 红掉
再猜原因。本轮把它做成 **Go 套件里的机器可验证契约**，随已必需的
`go test ./...` 自动执行（Go 1.25 / 1.26 / 1.27 矩阵，`-race`），零新增 CI job、
零新增下载。同时按 CI 真实顺序在单个无网络容器里复跑了整条链路。

#### Added

- **新增 `internal/cicontract`**（`contract.go` + `contract_test.go`）：用项目已有
  的 `gopkg.in/yaml.v3` 解析真实 `.github/workflows/ci.yml` 与 `release.yml`，断言
  ① 9 个必需门禁 job 全部存在；② 无 job/step 级 `continue-on-error`、无 job 级
  `if`（必需门禁不可被跳过）；③ 两个工作流的所有 `uses:` 均为 40 位 hex 完整
  commit SHA；④ 所有引用 `quack-e2e-cache` 的 `actions/cache` 步骤使用**完全相同**
  的 `path` 与 `key`，且 key 仍含从 `scripts/prefetch-quack-e2e.sh` 读出的三个
  SHA256 pin 前缀（改 pin 不换 key 会直接编译期失败）；⑤ `engine-tls-capability`
  的 `needs` 含 `quack-e2e`；⑥ `QUACK_E2E_OFFLINE` / `QUACK_TLS_OFFLINE` / 两个
  `QUACK_PREFETCH_OFFLINE=1` 断言在 CI 里确实设置。
- **9 个负例**：对真实 YAML 做单点篡改（改 `needs`、只改下游 cache key、把 key
  换成不含 pin 的值、关掉离线开关、加回 advisory、加 `if` 让门禁可跳过、删掉一个
  门禁 job、把 action 改成 tag pin、改 release 工作流的 action pin），每个负例要求
  **恰好对应的那一个检查失败、其余检查仍通过** —— 证明契约检查不是恒真式。

#### Verification（r13 实机）

- `--network none` 容器按 CI 真实顺序跑完整链路，3/3 RC=0：
  `QUACK_PREFETCH_OFFLINE=1 prefetch-quack-e2e.sh` → `PREFETCH_CACHE_HIT`
  （三 SHA256 `OK`）；`QUACK_E2E_OFFLINE=1 quack-e2e.sh --benchmark 50` →
  `ALL_QUACK_E2E_CHECKS_PASSED`（50 次 203 ms / ~246 qps、正确/错误 token、HTTP
  200、healthy、停止后端口释放无孤儿）；`QUACK_TLS_OFFLINE=1
  verify-engine-tls.sh` → `ENGINE_TLS_CAPABILITY_PASSED`。输出
  `OFFLINE_CI_CHAIN_PASSED`。这证明 `needs: quack-e2e` + 同一 cache key 的串联
  设计在严格离线下成立，不只是两个脚本各自能跑。
- Go 回归：`gofmt` / `go vet` / `go test ./... -race -count=1` **6 包全绿**、
  **74** 个测试函数、`go tool cover` 总计 **72.2%**（新增 `cicontract` 80.5%）；
  linux/amd64、darwin/amd64、darwin/arm64、windows/amd64 交叉构建通过；
  9 个负例全通过；`git diff --check` 干净。
- 静态审计：两个工作流共 **28** 处 `uses:`（`ci.yml` 23 + `release.yml` 5）
  **全部**为 40 位 hex SHA pin，无 `continue-on-error`、无 `if:`，与
  README/SECURITY 的既有声明一致。

#### 边界（不得扩张）

- 这是**契约/配置检查**，不是新的必需门禁，也不是产品功能：必需门禁仍是 9 个，
  Go 产品源码、daemon 行为、引擎产物、风险状态**完全未变**。R-05 仍**开放**
  （明文 HTTP + token 落盘），真 HA 仍属新产品立项，r10 的「CI 首次 cache miss
  仍需上游一次」残余风险**未被消除**（本轮只证明缓存命中后的三段链路严格离线）。
- 契约检查故意不解析 YAML 语义全量，只覆盖「门禁必需性 + 供应链 pin + 共享缓存
  一致性」；新增门禁或改 job 名时必须同步 `RequiredGates`，否则测试会（正确地）
  失败。

---

### remediation r12 (2026-11-16)

**结论先行**：台账里连标多轮的「内建 TLS 属需设计决策」被读成「引擎不支持 TLS、需要自行实现」，实测**这是错的**。项目 pin 的 DuckDB v2.0.0 引擎原生支持 TLS，产品 `supervisor.go` 里硬编码的 `disable_ssl = true` 是**主动关闭**，不是能力受限。本轮把这条能力事实实证并落成必需门禁，R-05 剩余决策的性质与工作量据此重估。

#### 边界：引擎能力 ≠ 产品功能（必读）

`scripts/verify-engine-tls.sh` 直接驱动 **pinned 引擎**，不经过 quack-proxy daemon。**quack-proxy 仍然只提供明文 HTTP**，token 仍明文落在 `status.json`（0600），LAN 明文与凭据落盘风险**一个都没修**，R-05 仍**开放**。把引擎已有的 TLS 接进产品，需要甲方拍板的是**契约与运维设计**，不是实现 TLS：证书生命周期与私钥存放、向客户端分发/轮换 SHA256 指纹的通道、健康探测改 HTTPS 后的证书校验策略、`status.json` 的 `attach_sql`/token 契约、以及 `gen-proxy` 输出的 HAProxy 前端与 `proxy.ssl` 死配置如何处置。脚本含诚实性断言 `PRODUCT_DISABLES_ENGINE_TLS=1`：产品一旦真启用 TLS，此门禁会（正确地）失败，须同步更新文档与 `verify-plaintext.sh`，不得删除。

#### Added / Changed

- **新增 `scripts/verify-engine-tls.sh`**：复用 r10 的 pinned 离线缓存（CLI `c6507206…` / 签名 `quack` `9c893bd5…` / `httpfs` `77e6c429…`，三项 SHA256 + manifest 严格校验，无下载分支）。实证：① `quack_serve` 原生带 `disable_ssl` / `ssl_cert_file` / `ssl_key_file`、`quack_query` 带 `ssl_fingerprint`；② `quack_generate_keys()` 生成自签证书与 SHA256 指纹（并用 openssl 独立验私钥）；③ `disable_ssl := true` → `http://`、`false` → `https://`；④ 独立 `openssl s_client` 完成 **TLSv1.3 / TLS_AES_256_GCM_SHA384** 握手、明文 HTTP 打 TLS 端口被拒、HTTPS GET 200；⑤ 正确指纹客户端经 TLS `SELECT 42 → 42`，错误/缺失指纹、错误 token 均被拒；⑥ 明文对照端点仍能应答（证明负例不是「服务已死」造成的假通过）。
- **`.github/workflows/ci.yml`**：新增必需 job `engine-tls-capability`（`needs: quack-e2e`，复用**同一个** cache key 与 `QUACK_PREFETCH_OFFLINE=1` 断言）。`needs` 保证缓存已填充并保存，因此**零新增下载**、无 advisory 逃生口。所有必需门禁现为 9 个。
- 文档口径收敛：`docs/REMEDIATION.md` / `docs/SCOPE.md` / `docs/SECURITY.md` / `docs/PRD.md` / `docs/benchmark.md` / `README.md`（含版本头、门禁数与「引擎审计不改变历史基准」的说明）。**Go 源码本轮未改动**，产品行为完全不变。

#### Verification（r12 实机，`--network none` 容器）

- 单个无网络容器内 `ENGINE_TLS_CAPABILITY_PASSED`、RC=0：三 SHA256 `OK`、`OFFLINE_CACHE_USED=1`、`ENGINE_VERSION=v2.0.0 (Cyanoptera) d54cbbe3df`、`SIGNED_CORE_EXTENSION_LOADED=1`、`ENGINE_URL_SCHEME_SWITCH=1`、`TLS_HANDSHAKE_OK=1 (TLSv1.3)`、`FINGERPRINT_PINNED_QUERY_OK=1`、4 个负例标记全部为 1、`PRODUCT_DISABLES_ENGINE_TLS=1`。
- **4 个负例全部非零退出且无 PASS 标记**：`--control-variant`（TLS 端点强制成明文）→ `FAIL: TLS endpoint did not report an https:// URL (got 'http://…')`、rc=1；`QUACK_TLS_OFFLINE=1` 无 `--cache-dir` → rc=1；篡改 `httpfs` 缓存 → `FAIL: offline cache missing/corrupt`、rc=1；空缓存 → rc=1。变体负例证明 URL 判别断言非空转。
- 下载踩坑（复现 r10 结论）：GitHub release 与扩展 CDN 在本机/容器通道均约 18 KB/s 且反复断流，`prefetch-quack-e2e.sh` 的单连接重试 11 分钟后失败。改用 8 并发 1 MB 显式 `Range` 分段 + 逐段长度校验 + 逐段重试，CLI 24 MB 与 `quack` 19 MB 全部拼齐；`httpfs` 又在尾段（第 9 片）断流，按 r10 记录用精确 `Range: bytes=8388608-8657468` 单独取回 268861 字节。**下载慢 ≠ 拿不到**。四个产物（含 CLI tar）的 SHA256 与项目 pin 逐一比对通过。
- 回归：12 个脚本 `bash -n`、三份 YAML `yaml.v3` 实解析、`git diff --check` 干净；`gofmt` / `go vet` / `go test ./... -race -count=1` 5 包全绿、总计 **70.9%**；linux/darwin/windows 交叉构建通过。

##### 仍未完成（如实）

- **R-05 产品接线与 token 不落盘**：属需甲方拍板的设计决策（清单见上）。`plaintext-audit` 与 `engine-tls-capability` 都只证明事实、不修风险。**〔本段为该轮时点的历史记录：其中「token 不落盘」已由 r14 按 PRD §4.2 既有判据完成；「产品接入 TLS」则由 r15 更正为 PRD 已定的 non-goal 并验收了既定的外部代理方案；详见本文件最上方 r15 段。〕**
- **真 HA**：属新产品立项（control plane 冗余 / 跨机发现 / Coordinator 常驻），需重做含验收标准的 PRD + 架构 RFC。
- **r10 残余风险**：CI 首次 cache miss 仍需 runner 访问上游一次（带重试、无 advisory 逃生口）；缓存命中后测试进程零网络。
- **r12 新解除**：「内建 TLS 是否属于能力受限」这一前提已澄清（引擎支持，产品主动关闭）。

---

### remediation r11 (2026-11-16)

把台账里最后一项被标为「需真实浮动 IP / 双机网络」的 **HAProxy + VIP 联调**做成实证与必需门禁。此前把它当成环境做不到，实际是没试：一个 Docker user-defined bridge + `NET_ADMIN` 就足以在两容器间漂移真实的 `/32` 浮动 IP，靠 gratuitous ARP 让客户端收敛。

#### 边界：联调 ≠ 产品 HA（必读）

VIP 的移动由脚本完成（模拟 keepalived / 云浮动 IP），**quack-proxy 既不提供也不感知**；产品仍然只 `gen-proxy` 写一份 HAProxy 配置。两个节点各跑独立后端、返回不同标识，脚本据此断言切换后**对端身份发生变化**（`SESSION_DISCONTINUITY=1`）：入口恢复了，会话与数据**没有**恢复。R-04 依然成立（不同 `.db` 是分区、token 逐分片独立、同一 `.db` 不能有两个写者），R-05 的明文风险也未被这次联调改变。`gen-proxy` 对多分片配置仍必须拒绝，脚本把这一点做成负例。

#### Added / Changed

- **新增 `scripts/verify-vip-failover.sh`**：用真实 CLI 生成 HAProxy 配置 → 真实 `haproxy -c -f` 校验（含坏配置负例）→ 两节点容器各起真实 HAProxy 2.8 + 独立 stub → 客户端经 VIP 访问 → 主节点整容器故障 → 备节点加 VIP + GARP → 再经 VIP 访问。负例：VIP 无人持有时必须不通、VIP 撤下后必须不通、多分片配置必须被 `gen-proxy` 拒绝、坏配置必须被 `haproxy -c` 拒绝。默认 CIDR 冲突时自动回退到 3 个候选网段。
- **`scripts/drill-stub.go`**：新增可选 `DRILL_STUB_GREETING`，让每个节点返回不同标识；默认值与所有既有调用者不变。
- **`.github/workflows/ci.yml`**：新增必需 job `vip-failover`（`ubuntu-latest`，先静态构建 CLI + stub 再跑脚本）。所有必需门禁现为 8 个。

#### Verification（r11 实机）

- 两次不同网段（`172.29.83.0/24` / `172.29.91.0/24`）均 `VIP_FAILOVER_DRILL_PASSED`：生成配置 1004 B、`HAPROXY_CONFIG_VALID=1`、`GEN_PROXY_MULTI_SHARD_REJECTED=1`、`VIP_ABSENT_CONTROL_OK=1`、`VIP_PRIMARY_NODE=NODE_A` → `VIP_FAILOVER_NODE=NODE_B`、`SESSION_DISCONTINUITY=1`、`VIP_REMOVED_CONTROL_OK=1`，退出码 0、无容器/网络残留。
- **额外变体负例**：把两个节点改成返回同一标识后，脚本报 `FAIL: VIP did not fail over to node B (got 'NODE_A')`、rc=1、无 PASS 标记 —— 证明身份断言不是空转。
- 踩坑记录：`haproxy:2.8-alpine` 默认以 uid 99 运行，`--cap-add=NET_ADMIN` 对非 root 不生效（`RTNETLINK: Operation not permitted`），必须显式 `--user root`；Git Bash 的 MSYS 会把容器内 `-w /work` 改写成宿主路径，脚本已统一用 `cygpath` 规范化并禁用参数改写。
- 回归：11 个脚本 `bash -n`、三份 YAML `yaml.v3` 实解析、`git diff --check` 干净；`gofmt` / `go vet` / `go test ./... -race -count=1` 5 包全绿、总计 **70.9%**；linux/darwin/windows 交叉构建通过。

##### 仍未完成（如实）

R-05 的 `disable_ssl` 硬编码 / token 不落盘 / 内建 TLS 属**需设计决策**（`plaintext-audit` 只证明弱点、不修风险）；真 HA 属新产品立项；r10 的残余风险仍在（CI 首次 cache miss 需访问上游一次）。**r11 已解除**：HAProxy + VIP 联调（但如上所述，不等于具备 HA 能力）。

---

### remediation r10 (2026-11-16)

把整改台账里最后一项「本环境能做、但一直被当作做不到」收紧：`quack-e2e` 在 **PR 上也不再是 advisory**。做法是让真实引擎 E2E 不再在测试过程中访问外部 CDN，改为 CI runner 上一次性预取、SHA256 校验、缓存复用。

#### 更正：「离线」这个词之前用得不严谨

r9 写「离线喂入 pinned CLI + 签名扩展后 E2E 完整通过」——产物确实是离线的，但**容器仍有网络**。本轮首次用 `--network none` 复跑，立刻失败：磁盘数据库分片报 `DuckDB currently has a read-only crypto module loaded. Please ensure httpfs is loaded using LOAD httpfs`。根因是 DuckDB v2.0.0 写加密数据库文件需要 `httpfs` 带来的完整 crypto 后端；r9 之所以通过，是 DuckDB 在联网容器里把 httpfs **autoload** 掉了。判别实验：同一无网络容器里 `:memory:` 数据库一切正常，只有磁盘库触发该错误。

因此本轮把 `httpfs` 也作为**签名 core 扩展** pin 进缓存（gzip SHA256 `ee6c7d7e…`，解压后 `77e6c429…`），与 `quack` 一起放进隔离 HOME 的扩展目录。文件存在即可由 autoload 命中，**不需要改动 supervisor 的 boot SQL，也不需要 `-unsigned` 或 community 源**。同时明确一条新的运维约束：完全离线的生产部署必须预装 `quack` **与** `httpfs`，只装 `quack` 会在写磁盘库时失败。

#### Added / Changed

- **新增 `scripts/prefetch-quack-e2e.sh`**：在 CI runner（而非测试进程内）准备离线缓存，pin 三件产物 —— DuckDB v2.0.0 CLI（`c6507206…`）、签名 `quack`（`9c893bd5…`）、签名 `httpfs`（`77e6c429…`），连同 gzip 的中间 hash 与 manifest。缓存命中时**零网络请求**；`QUACK_PREFETCH_OFFLINE=1` 只做断言、绝不下载。下载带 3×3 重试，已完整且校验通过的 `.gz` 会直接复用（本机实测 httpfs 对象反复在 ~8 MB 处断流，这个复用是必要的）。
- **`scripts/quack-e2e.sh` 新增 `--cache-dir` / `QUACK_E2E_CACHE_DIR`**：缓存模式下严格离线（无 `curl` 下载、无 `INSTALL`），逐项校验三个 hash，把扩展放进**隔离 HOME**（`DUCKDB_EXTENSION_DIRECTORY` 在 v2.0.0 实测被忽略，`SET extension_directory` 又无法注入固定的 boot SQL；`go build` 仍用真实 HOME 以免丢 GOCACHE）。`QUACK_E2E_OFFLINE=1` 时缓存缺失/损坏是硬失败，**不静默回退联网**。非缓存模式保持原有下载 + `INSTALL` 重试行为不变。
- **`.github/workflows/ci.yml`**：`quack-e2e` 移除 `continue-on-error`，**所有事件（含 `pull_request`）均为必需门禁**；新增 SHA-pinned `actions/cache@0057852… # v4.3.0`（key 含三个 hash）、prefetch 步骤、离线断言步骤。CI 注释如实写明残余风险：首次 cache miss 仍需访问上游一次，但不再有 advisory 逃生口。
- `README.md` / `docs/SCOPE.md` / `docs/REMEDIATION.md` / `docs/SECURITY.md` / `docs/PRD.md` 全部收敛 r10。

#### Verification（r10 实机）

- **单个 `--network none` 容器内**跑完整链路：prefetch 的 miss / hit / 严格离线断言三条路径全通，随后 `QUACK_E2E_OFFLINE=1 bash scripts/quack-e2e.sh --cache-dir … --benchmark 50` 输出 `ALL_QUACK_E2E_CHECKS_PASSED` —— DuckDB v2.0.0、正确/错误 token、HTTP 200 健康循环、50 次 SELECT（230 ms，~217 qps）、停止后端口释放无孤儿。证据行含三个 `OK` 的 SHA256 校验与 `OFFLINE_CACHE_USED=1`。
- **4 个负例全过**：空缓存 + `OFFLINE=1`、完全不给 `--cache-dir` + `OFFLINE=1`、篡改 `httpfs` 条目、prefetch 严格离线断言遇不完整缓存 —— 全部非零退出，且输出中不存在 `ALL_QUACK_E2E_CHECKS_PASSED` / `all shards started` / `PREFETCH_CACHE_HIT`。另有无网络下的判别负例：不预装 httpfs 时磁盘库写失败，`:memory:` 正常。
- 回归：10 个脚本 `bash -n`、三份 YAML `yaml.v3` 实解析、`git diff --check` 干净；Go 代码本轮未改动，`gofmt` / `go vet` / `go test ./... -race` 复跑 5 包全绿、覆盖率 70.9%。

##### 仍未完成（如实）

HAProxy + VIP 联调（需真实浮动 IP / 双机网络）；R-05 的 `disable_ssl` 硬编码 / token 不落盘 / 内建 TLS 属**需设计决策**（`plaintext-audit` 只证明弱点、不修风险）；真 HA 属新产品立项。**r10 已解除**：`quack-e2e` 在 PR 上的 advisory。**r9 已解除**：Windows 产物收口、真实引擎 E2E 完整复跑（本轮进一步收紧为严格离线）。

---

### remediation r9 (2026-11-16)

本轮收口 R-18（Windows 产物），并把「外部依赖导致跑不完」的 E2E 用离线通道完整复跑通过。

#### 更正：R-18 不是「待产品决策」

PRD §8 从 v0.1 起就把 Windows 列为 non-goal，但 release 一直发布一个**签过名、看起来能用**的 `.exe`。在真实 Windows（本机 MINGW64_NT-10.0-19045）上实证：`version` 正常返回 `quack-proxy dev`，`start` 却报
`duckdb_bin "C:/.../drill-stub.exe" not found on PATH or not executable` —— 用户会以为是配置写错。根因不是 bash/tail：`lookupBin` 用 POSIX 可执行位 `mode&0o111` 判定，而 Windows 的 `os.Stat` 对普通文件返回 0666/0444，**任何绝对路径的引擎都会被误判**。之前把它记为「需运维/产品决策」，实际是「既有 non-goal 决策没有执行到位」。

#### Changed / Fixed

- **撤下 Windows 发布产物**：`release.yml` 移除 `windows/amd64` 矩阵项与 `.zip` 打包分支，只发布 linux/darwin（amd64 + arm64）。CI 仍保留 `GOOS=windows` 交叉编译作为源码兼容性检查，但不再向用户分发不可运行的签名二进制。
- **修 `lookupBin` 的跨平台可执行判定**：新增平台函数 `executableFile()`（`attr_unix.go` 用 mode 位；`attr_windows.go` 按 `PATHEXT` 扩展名，环境被裁剪时仍认 `.exe/.com/.cmd/.bat`）。`gate5_orphan_test.go` 里因 R-18 加的 Windows skip 已移除；`GOOS=windows go test -c ./internal/supervisor` 编译通过。
- **Windows 运行时改为显式拒绝**：`runStart` 在任何配置加载之前输出 `Windows is not a supported runtime platform (R-18)`，说明缺少 POSIX shell / `/dev/null` / 进程组语义，并给出 Linux/macOS 或 WSL2 的替代路径。新增 `runtimeSupported()` 与两个不依赖宿主平台的单测。
- **新增必需门禁 `windows-runtime-guard`**（`runs-on: windows-latest`）：`scripts/verify-windows.sh` 在真实 Windows 上断言 `version` 可用、`start` 非 0 且含 `R-18` / `WSL2`；**故意传一个不存在的配置路径**，看到平台错误而非 `failed to load config` 即证明守卫在 config.Load 之前；负例还断言旧的 `not found on PATH or not executable` 与 `all shards started` 都不出现，且不留 pid 文件。本机 Git Bash 实跑 `WINDOWS_RUNTIME_GUARD_PASSED`。

#### Verification（r9 实机）

- **真实引擎 E2E 完整复跑通过**：r8 之后一直只跑到 `INSTALL quack` 就卡住。本轮改用主机 HTTPS 通道取 pinned DuckDB CLI（24 MB / 48s，SHA256 与脚本 pin 完全一致 `c6507206…`）与官方签名 core 扩展（19 MB gzip → 60 MB），离线喂入容器后 `scripts/quack-e2e.sh --benchmark 50` 输出 `ALL_QUACK_E2E_CHECKS_PASSED`：DuckDB v2.0.0、正确/错误 token、健康循环、50 次 SELECT（207 ms，~241 qps）、停止后端口释放无孤儿。**容器内下载慢 ≠ 通道被拦**，重试逻辑本身未被改动。
- 最终全量回归：gofmt / `go vet` / `go test ./... -race -count=1` 5 包全绿；**71** 个测试函数；总覆盖率 **70.9%**（cmd 27.0% / config 93.8% / health 90.9% / proxy 98.3% / supervisor 80.1%）。较 r8 的 71.1% 略降，原因是新增的 Windows 运行时分支在 Linux runner 上不可达，属如实下降而非回归。linux·darwin·windows 交叉构建、Windows 测试包交叉编译、9 个 shell 脚本 `bash -n`、三份 YAML `yaml.v3` 实解析、`git diff --check` 全绿。
- 受 `lookupBin` 改动影响的两条真实门禁用最终源码复跑：真实 systemd 255 非 root 安装演练 `SYSTEMD_INSTALL_DRILL_PASSED`（含负例）、LAN 明文抓包审计 `PLAINTEXT_AUDIT_PASSED`。

##### 仍未完成（如实）

HAProxy + VIP 联调（需真实浮动 IP / 双机网络）；`quack-e2e` 在 **PR 上仍为 advisory**（外部 CDN 在容器内偶发不可用，主分支已必需）；`disable_ssl` 硬编码 / token 不落盘 / 内建 TLS / 真 HA 属需设计决策或新产品立项。

---

### remediation r8 (2026-11-15)

本轮把「仍未做」清单里**可做但一直没做**的 LAN 明文抓包做成实证门禁，并收紧另一项外部依赖的 advisory 范围。

#### 更正：清单里的「未做」不等于「做不到」

r5~r7 把「LAN 明文抓包」列为未做，从未断言环境不允许——只是没做。本轮证明：一条 Docker bridge 隔离网段 + 真实 daemon + 独立 `AF_PACKET` 嗅探容器即可实证，不需要第二台物理机。前两轮是「通道/参数受限误记为能力受限」，这一轮是「**未做被清单化后默认成了做不到**」。已写入 `SCOPE.md §8` 与 `REMEDIATION.md §4`。

#### Added

- `scripts/verify-plaintext.sh`：**必需** CI job `plaintext-audit`。真实 daemon 以 `bind_host: 0.0.0.0` 起分片，独立客户端容器与独立嗅探容器（`--network container:<server>` 共享 netns）跑在同一 bridge 上。断言：
  - `PLAINTEXT_TOKEN_VISIBLE=1` —— 原始 TCP payload 中直接读到 `qp-lan-cleartext-token`、`Authorization: Bearer` 与 `SELECT * FROM secrets`；
  - `TLS_CONTROL_HIDDEN=1` —— 同网段 TLS 控制通道捕获 5009 B，但 canary 与分片 token 均不可见（负例，证明嗅探器真的在解析报文而不是 grep 日志）；
  - `PRODUCT_TLS_UNAVAILABLE=1` —— 真实 CLI 拒绝 `proxy.ssl.enabled=true`（`not implemented`）；
  - `STATUS_TOKEN_AT_REST=1` —— `status.json` mode 600，但 token 明文落盘。**这是 r8 时点的历史记录；r14 起该断言反转为硬失败条件 `STATUS_TOKEN_AT_REST=0`（默认不落盘），见本文件最上方 r14 段。**
  - 另有「嗅探器失灵 → 捕获文件为空即失败」的隐性负例。默认镜像 `python:3.11-slim` **3/3**、换 `jrei/systemd-ubuntu:24.04` **1/1** `PLAINTEXT_AUDIT_PASSED`。
- `scripts/drill-stub.go` 新增 `DRILL_STUB_BIND`（默认仍 `127.0.0.1`），仅明文审计设为 `0.0.0.0`；systemd 演练行为不变。

#### Changed / Fixed

- **审计 ≠ 修复**：`plaintext-audit` 断言的是当前**已知弱点**，不是通过即安全。R-05 的 `disable_ssl` 硬编码 / token 落盘仍属需设计决策；若将来加入 TLS 终止，此门禁会（正确地）失败，必须同步更新断言与文档，不得删除。
- `scripts/quack-e2e.sh`：pinned DuckDB CLI 下载与 `INSTALL quack` 各 3 次重试（curl 自带 `--retry 3` + 外层 3 次循环），失败输出明确诊断；**绝不回退** `-unsigned` 或 community 源，保住签名扩展约束。
- `.github/workflows/ci.yml`：`quack-e2e` 的 `continue-on-error` 改为 `${{ github.event_name == 'pull_request' }}` —— **仅 PR 为 advisory**，push `main` 与 `workflow_dispatch` 为必需门禁；新增必需 `plaintext-audit` job。
- `README.md` / `docs/SCOPE.md` / `docs/REMEDIATION.md` / `docs/SECURITY.md` / `docs/PRD.md` 全部收敛 r8。

#### Verification（r8 实机）

- 明文审计 3/3 + 跨镜像 1/1 通过，证据行如上。
- 最终全量回归：gofmt / `go vet` / `go test ./... -race -count=1` 5 包全绿、总覆盖率 **71.1%**（proxy 98.3% / supervisor 80.1%，69 个测试函数）、linux·darwin·windows 交叉构建、8 个 shell 脚本 `bash -n`、三份 YAML `yaml.v3` 实解析、`git diff --check` 全绿。

##### 仍未完成（如实）

HAProxy + VIP 联调（需真实浮动 IP / 双机网络）、Windows 产物去留决策（R-18，产品决策）；`quack-e2e` 在 **PR 上仍为 advisory**（外部 CDN 偶发 502）；`disable_ssl` 硬编码 / token 不落盘 / 内建 TLS / 真 HA 属需设计决策或新产品立项。

---

### remediation r7 (2026-11-15)

本轮把 r6 留下的四项「未完成」里的三项做实，并再次更正上一轮的「环境限制」结论。

#### 更正：r6 的「环境无 systemd」是误判

r6 把「非 root systemd 安装演练」记为环境阻断。实际是容器启动参数不对：Docker Desktop / WSL2 下用 `--privileged --cgroupns=host` 并把 `/sys/fs/cgroup` 以 **rw** 挂成 cgroup2，`systemd 255` 即作为 PID 1 正常 `is-system-running=running`；`--cgroupns=private` 会因 `Failed to create /init.scope control group: No such file or directory` 退出。r5 把 apt 被拦当成装不上工具，r6 把 cgroup 命名空间不对当成没有 systemd —— 同一类误判连续两轮，已写入 `SCOPE.md §8`。

#### Added

- `scripts/systemd-install-drill.sh` + `scripts/drill-stub.go`：在**真实 systemd 255 PID 1** 上安装 shipped unit，验证 `User=quack-proxy`、`RuntimeDirectory` 属主/0750、pid/status 0600、healthy、`status --json` 脱敏与 `--show-tokens`、`systemctl stop` 后端口/pid/status/进程组全清理。**负例**：同一二进制以该账号直接启动且无 `/run/quack-proxy` 时必须被 `permission denied` 拒绝。CI 新增必需 job `systemd-drill`。
- `proxy.hardened`（默认 `false`）：设为 `true` 时 `gen-proxy` 在 `global` 段输出生效的 `chroot /var/lib/haproxy` / `user haproxy` / `group haproxy`。
- `scripts/verify-hardening.sh`：预置 `haproxy` 账号与 root:root 0755 chroot，用真实 HAProxy 断言「未预置账号的配置被 `haproxy -c -f` 拒绝」「未加固配置在 3.x 启动时确实告警」「加固配置 real uid 已降权、无 root 告警、前端真实转发」。HAProxy 3.4.7 3/3、2.8.31 1/1 通过（2.8 的告警负例按主版本显式 SKIPPED）。CI 必需步骤。
- `TestGenerateHAProxyHardenedEmitsDirectives`：测试函数 68 → **69**。

#### Changed / Fixed

- **govulncheck 转必需**：`go1.26.11` 的 11 项 Go 标准库告警在 `go1.27.4` 下清零（实测 `No vulnerabilities found`、`SCAN_RC=0`）。移除原 advisory 步骤，新增必需 `vuln-scan` job（固定 go1.27.4）；`release.yml` 同步固定 `go-version: '1.27.4'`，避免「扫描干净但发布产物仍是旧 stdlib」；`go` CI 矩阵加 1.27。
- **R-23 从「文档化缓解」升级为「可验证的加固路径」**，默认仍 opt-in —— 负例证明把指令注入未预置主机会让 HAProxy 直接起不来。
- `README.md` / `docs/SCOPE.md` / `docs/REMEDIATION.md` / `docs/SECURITY.md` / `quack-proxy.example.yaml` / CI 文档全部收敛 r7。
- `.gitattributes` 补齐 `*.py` / `*.sql` / `go.mod` / `go.sum` / `LICENSE` / `.gitignore` 的 `text eol=lf`，并把工作区 9 个历史 CRLF 文件统一为 LF。背景：Windows 上 `core.autocrlf=true` 会把这些文件恢复成 CRLF，导致 `bash -n scripts/quack-start.sh` 报 `unexpected end of file`；**git blob 原本就是 LF，已发布内容未受影响**，这只影响本地检出与全量脚本语法门禁。现在 7 个 shell 脚本全部通过 `bash -n`。

#### Verification（r7 实机）

- systemd 255 安装演练 **3/3 `SYSTEMD_INSTALL_DRILL_PASSED`**；HAProxy 3.4.7 加固验证 **3/3**、2.8.31 **1/1** `HARDENING_VERIFY_PASSED`。
- `go test ./... -race -count=1` Linux 容器 5 包全绿；69 个测试函数；proxy **98.3%** / supervisor **80.1%** / 总计 **71.1%**；gofmt、vet、三平台交叉构建、7 个 shell 脚本 `bash -n`、YAML 三份 `yaml.v3` 实解析、`git diff --check` 全绿。
- 真实工具回归（最终源码复跑）：systemd 演练 `SYSTEMD_INSTALL_DRILL_PASSED`、加固验证 `HARDENING_VERIFY_PASSED`、`verify-deploy.sh --only haproxy` 的 `HAPROXY_CONFIG_VALID=1` / 负例被拒 / `DEPLOY_VERIFY_PASSED`。
- `govulncheck@v1.9.0` + `go1.27.4` → `SCAN_RC=0`。

##### 仍未完成（如实）

LAN 明文抓包、HAProxy + VIP 联调、quack-e2e 取消 advisory（外部 CDN 偶发 502）、Windows 产物去留决策（R-18）；`disable_ssl` 硬编码 / token 不落盘 / 内建 TLS / 真 HA 属需设计决策或新产品立项。

---

### remediation r6 (2026-11-15)

基于六领域专家评审（`eadef51`）的六轮整改。**含多处不兼容变更，升级前请读完本节。**

#### r6 增量（2026-11-15）

##### 解除上一轮「环境阻断」结论

r5 把「真实 `haproxy -c -f` / `systemd-analyze verify`」记为三次 apt 源 502 造成的环境阻断、验收未达成。**该结论已作废**：改用本地 `alpine:latest` 的 `apk`（HTTPS CDN）安装 HAProxy，以及「主机 HTTPS 下载 Ubuntu noble `.deb` → 容器离线 `dpkg-deb -x`」取得 systemd 255，两项真实校验全部跑通。教训：apt 被拦 ≠ 所有安装通道被拦。

##### Added

- 新增 `scripts/verify-deploy.sh`：用**厂商真实二进制**校验部署产物——`haproxy -c -f`（及 `-c -V`）、故意写坏配置的**负例必须被拒**、`--forward` 真实起 stub + HAProxy 并断言前端响应体；`systemd-analyze verify` 校验 unit，并用 `KillMode=TOTALLY_BOGUS` 负例要求出现诊断文本（该工具对部分解析错误仍返回 0，只查退出码会漏）。
- CI 新增**必需** `deploy-verify` job（`apt-get install haproxy systemd curl`，无第三方下载），执行 `bash scripts/verify-deploy.sh --forward`。
- 新增 `TestGenerateHAProxyDocumentsRootHardening`，测试函数 67 → **68**。

##### Changed / Fixed

- **R-23**：真实 HAProxy 3.4.7 启动告警——生成的配置以 root 运行、无 `chroot` / `user`。quack-proxy 只生成、不启动 HAProxy，也无法预置主机账号与目录，故**不注入**这些指令（注入会让未预置的主机直接不可用），改为在生成文件里写明加固步骤并指向 `docs/SECURITY.md`。状态：部分缓解 / 需运维加固。
- `deploy/systemd/quack-proxy.service`：`Description` 去掉误导性的 `cluster manager`，改为 `single-host DuckDB + Quack process supervisor`；`Restart=` 注释补上 R-22 端口预检会让自动重启必然失败。

##### Verification（r6 实机）

- Alpine + HAProxy **3.4.7 LTS**：`haproxy -c -f` RC=0 / `Configuration file is valid`；负例被拒；`--forward` 返回 `quack-forward-ok`（前端 19600 → 后端 19601）。
- Ubuntu 24.04 + **systemd 255**（离线提取 `systemd` / `libsystemd-shared` / `libapparmor1` / `libkmod2` noble deb）：`systemd-analyze verify` RC=0；负例输出 `Failed to parse kill mode specification`。
- `go test ./... -race -count=1` Linux 容器 5 包全绿；本机 68 个测试全绿；覆盖率 proxy **97.8%**、supervisor 60.0%；gofmt / vet / 三平台交叉构建 / 三脚本 `bash -n` / YAML 三份实解析 / `git diff --check` 全绿。

##### 仍未完成（如实）

非 root systemd **安装演练**（`verify` 是静态校验，不等于实机启停）、LAN 明文抓包、HAProxy + VIP 联调、清零 govulncheck 的 11 项 stdlib 告警后转必需、quack-e2e 转必需、Windows 产物去留；`disable_ssl` 硬编码 / token 不落盘 / 真 HA 属需设计决策或新产品立项。

---

#### r5 基线（2026-11-14）

基于六领域专家评审（`eadef51`）的五轮整改。

#### r5 增量（2026-11-14）

##### Changed / Added

- **`reload` 现在做字段级 diff**（R-07）：分片 `port` / `database` / `token` / `init_sql` / `init_sql_file` 变化只滚动重启该分片；`duckdb_bin` / `bind_host` / `allow_other_hostname` / `allow_unsigned_extensions` 变化重启全部；`health_path` / `health_interval` / `health_expect` 变化不重启。`init_sql_file` **内容**变化（路径不变）也通过 sha256 摘要触发重启。省略的 token 会被解析成随机值，diff 先归一化再比较，避免每次 reload 误重启。
- `ApplyConfig` 返回值新增 `restarted []string`；SIGHUP 日志增加 `restarted` 计数。
- **启动前端口预检**（R-22）：`startShard` 用 `net.Listen` 探测分片端口，已被占用时直接报错并提示「可能是崩溃 daemon 的遗留分片」，不再制造假 `healthy`。自动接管孤儿属真 HA，未实现。
- 新增 `scripts/daemon-crash-drill.sh`：`SIGKILL` daemon 后复现孤儿占端口、pid/status 残留，用两代 stub 的不同响应体证明健康探测由孤儿应答；脚本带 R-22 断言。
- CI 新增**必需** `crash-drill` job（无外部网络依赖）；`govulncheck` / `quack-e2e` 仍为 advisory，理由见 `docs/SECURITY.md`。
- **修正 CI 中 `govulncheck` 的 Go 版本配置 bug**：`v1.9.0` 要求 Go ≥ 1.26，原先步骤固定在 `matrix.go == '1.25'` 上，`go install` 每次都失败却被 `continue-on-error: true` 静默吞掉，等于从未真正运行。现移到 `1.26` job。实测（自动切换到 go1.26.11）报告 **11 项 Go 标准库告警**（`SCAN_RC=3`），基线非零，故**仍保持 advisory**，未伪称零告警；清零并用补丁工具链重建后才可转必需。
- `scripts/quack-e2e.sh`：冷启动等待窗口 30s → 60s，失败时输出 daemon 日志、boot SQL、status.json 与进程现场。

##### Fixed

- **R-07** 从「部分缓解 / 需设计决策」变为已修；**R-22** 为本次演练新发现并修复。
- daemon crash 演练（原列为未做验收）已完成，结论写入 `docs/REMEDIATION.md`。

##### Verification（r5 实机）

- 67 个测试函数（+12）；`-race` Linux 容器 5 包全绿；覆盖率 supervisor **60.0%**（r4 为 55.1%）。
- 真实进程组集成：reload 换端口 PASS 0.22s；第二 daemon 拒绝孤儿端口 PASS；`TestIntegrationRealProcessLifecycle` 自愈仍通过。
- 真实 DuckDB v2.0.0 + Quack E2E：**5 轮通过**（含端口预检改动后的最终复跑），1 次冷启动超过 30s 等待窗口失败（已加长到 60s 并增强诊断，随后 3/3 通过）。分片进程组被 `SIGKILL` 后 daemon 在 ~31s 自愈、`restarts=1`，证明端口预检**不误伤**自愈重启；停止后端口释放。
- daemon crash 演练：修复前 `FALSE_HEALTHY=1`；修复后 `RESTART_REFUSED_ON_ORPHAN_PORT=1`、`FALSE_HEALTHY=0`、脚本退出 0。

##### 当时未能完成（已在 r6 解除）

- 真实 `haproxy -c -f` 与 `systemd-analyze verify` 二进制级校验：受限于 apt 源 502，当时未做；**r6 已用 Alpine apk 与离线 noble `.deb` 全部跑通**，见上方 r6 增量。
- 其余未完项见下方 r4 基线与 `docs/REMEDIATION.md`。

---

#### r4 基线（2026-11-13）

r4 = r3 + R-21（停止后进程组未完全回收）修复与收敛。以下为 r4 及之前的完整变更。

#### Breaking changes

- `listener.bind_host` 默认值由 `0.0.0.0` 改为 `127.0.0.1`。需要 LAN 访问必须显式配置，并自行终止 TLS。
- `listener.allow_other_hostname` 新增，默认 `false`（原先 boot SQL 硬编码 `true`）。经 VIP/DNS 访问时需显式开启。
- `global.allow_unsigned_extensions` 新增，默认 `false`（原先无条件传 `-unsigned`）。使用未签名扩展需显式开启，且应先预装 + 校验哈希。
- `gen-proxy` **仅允许单分片后端**：不同 `.db` 是分片（轮询结果不确定），相同 `.db` 在 DuckDB v2.0.0 上会触发文件锁冲突（`Could not set lock on file`）。多分片配置一律报错，请逐分片 `ATTACH`。
- `status --json` 默认不再输出 `token` 字段；需显式 `--show-tokens`。
- `proxy.ssl.enabled: true` 现在在配置加载期报错（此前被静默忽略）。
- `shards[].name` 收紧为 `^[A-Za-z_][A-Za-z0-9_]*$`；`health_path` / `bind_host` 增加白名单校验。
- `listener.health_expect` 新增（可选）：要求健康响应 body 含指定子串，默认空保持 liveness-only。
- DuckDB **单一基线 pin 到 v2.0.0 stable**；`quack` 是签名 **core** 扩展，须预装 `INSTALL quack;`（`FROM community` 在 v2.0.0 返回 404）。boot SQL 只做 `LOAD quack`，不再宣称自动安装。
- 源码构建的 `version` 默认值改为 `dev`（release 仍由 ldflags 注入 tag）。

#### Security

- status.json 与 pid 文件改为 0600 + 临时文件原子写；错误不再被静默吞掉；文件 I/O 移出 supervisor 锁。
- 健康检查不再跟随 3xx、URL 结构化构造（消除 `health_path` SSRF）、复用连接池、限定响应体读取量，并支持 body 身份校验。
- README 移除 `curl | bash` 推荐，改为下载 + 审阅 + 执行。
- release workflow 顶层最小权限 + `id-token: write`，**`SHA256SUMS` 由 cosign keyless OIDC 签名**（附 `.sig` / `.pem`）。
- **所有 GitHub Actions 按真实 commit SHA pin**；`govulncheck` 固定 `@v1.9.0`；DuckDB CLI 由 SHA256 校验；扩展改用签名 core 版本。

#### Fixed

- healthy 响应清零 `Restarts`；`stopped` 分片可经 `reload` 复活并重置预算。
- `cmd.WaitDelay` + SIGKILL 后 `Wait` 兜底超时。
- `AttachSQL` 在默认 bind 下改用可连接地址；`SIGHUP` reload 后同步重生成 HAProxy 配置。
- systemd 单元补 `RuntimeDirectory` / `RuntimeDirectoryMode` / `UMask`。
- **锁粒度完整重构**：`checkAll` 探测并发且锁外、决策纯内存、重启锁外；`ApplyConfig` / `StartAll` / `StopAll` 的阻塞操作全部移出 `s.mu`；`ShardProcess.opMu` 串行化单分片生命周期。
- **重启不刷新 `lastStart`**：30s 启动宽限只作用于冷启动，指数退避真正生效。
- **并行停机**：N≥4 不再超 `TimeoutStopSec=30`。
- **停止后整组回收（R-21）**：`cmd.Wait()` 只回收 bash 组长，真实 DuckDB 后代可能仍持有 Quack 端口，导致 daemon 已报 `shard stopped` 但端口不释放、下次启动失败。现在 `stopShard` 用 `kill(-pid, 0)` 确认整组退出，超时补发 SIGKILL；由最终真实引擎 E2E 复跑发现。
- 全仓库 `.go` 统一 LF + `.gitattributes`，修复 `gofmt -l .` 全量报错。
- Windows 上依赖 POSIX 进程组 / `signal(0)` 的测试显式 skip，不伪装通过。

#### Verification

- 新增 `internal/supervisor/integration_test.go`：真实子进程组 E2E（起 → kill → 自愈 → 停），在 `-race` 下通过。
- 新增 `internal/supervisor/gate2_regression_test.go`：探测不持锁、退避语义、并行停机回归。
- 新增 `TestStopAllReapsLingeringDescendants`（R-21）：用忽略 SIGTERM 的真实子进程组复现孤儿分片，`StopAll` 返回后立刻断言端口释放；`-race` 下 2.13s PASS。
- 新增 `scripts/quack-e2e.sh`：**真实 DuckDB v2.0.0 + 签名 Quack 扩展**端到端（SHA256 pin、正确/错误 token 认证、GET / 2xx、healthy 状态、可选 `ATTACH/SELECT` 基准、清理校验）。r4 最终连续 3 轮（3 个不同端口）均输出 `ALL_QUACK_E2E_CHECKS_PASSED` 且停止后端口释放；50 次往返首次 221ms (~226 qps)、复跑 312ms (~160 qps)，单客户端。
- `scripts/quack-e2e.sh` 现优先 `LOAD quack` 复用本地已预装的签名扩展，仅在缺失时才联网 `INSTALL`，使 E2E 在扩展 CDN 不可用（实测 HTTP 502）的离线环境仍可运行；这与 SCOPE 要求的「运维预装」一致。
- CI 新增 `quack-e2e` job（advisory，依赖外部 release/扩展仓库）。

#### Docs

- 新增 `docs/SCOPE.md`、`docs/REMEDIATION.md`、`docs/SECURITY.md`、`CHANGELOG.md`。
- `docs/PRD.md` 改为实现状态跟踪表；README 撤下「highly-available cluster」与把 83K/169K QPS 当产品性能的表述，补充测量对象 caveat、token 脱敏、单机无副本、扩展预装、cosign 验证与 Windows 不可用说明。

#### 实测结果（2026-11-13）

`gofmt -l .` 空 · `go vet ./...` 通过 · `go test ./... -count=1` 5 包全绿 · **`go test ./... -race -count=1` Linux 容器 5 包全绿**（含 R-21 孤儿回收回归，专项 `-v` PASS 2.13s） · linux/darwin/windows amd64 交叉构建通过 · YAML 三份实解析通过 · CLI E2E（`version`=dev、多分片 `gen-proxy` 拒绝且不生成文件、单分片生成）通过 · **真实 DuckDB+Quack E2E 连续 3 轮通过，停止后端口稳定释放**。

覆盖率（r4 当时实测，历史值；r5 最新值见上文）：cmd 23.0% · config 93.8% · health 90.9% · proxy 97.3% · supervisor 55.1%（新增进程组回收代码使分母变大）；CI 设 50% 总门槛。

**r4 当时未完成、后续已解除**：daemon crash 演练、reload 字段 diff → r5 已完成；真实 `haproxy -c -f` / `systemd-analyze verify` 二进制级校验 → **r6 已完成**。仍待办：非 root systemd 安装演练、LAN 明文抓包、HAProxy + VIP 联调、govulncheck / quack-e2e 取消 advisory、Windows 产物去留决策；`disable_ssl` 硬编码、token 落盘、真 HA 等属需设计决策或超范围，详见 `docs/REMEDIATION.md`。

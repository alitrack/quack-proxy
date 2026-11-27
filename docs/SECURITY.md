# 安全说明（SECURITY）

> 适用版本：v0.2.1（2026-11-27；含 r1–r17 整改、RBAC v2 示例与认证探针）
> 配套阅读：`SCOPE.md`（边界）、`REMEDIATION.md`（缺陷台账与状态）

## 1. 信任模型

| 对象 | 是否可信 | 说明 |
|------|----------|------|
| YAML 配置文件 | **完全可信** | 能写 YAML 就能指定 `duckdb_bin`、任意 `init_sql` 与数据库路径；配置不是沙箱边界 |
| 本机同 UID 的其他进程 | 不完全可信 | status.json / pid 文件已限 0600，但 root 与同 UID 仍可读 |
| 网络客户端 | 不可信 | 仅凭 32 字符 token 认证，且默认走明文 HTTP |
| DuckDB 扩展 | 由 DuckDB 校验签名 | v2.0.0 的 `quack` 是**签名 core 扩展**，须预装；默认不加 `-unsigned` |
| DuckDB CLI 二进制 | 由部署者校验 | E2E 脚本 pin v2.0.0 + SHA256；生产部署应自行做同等校验 |
| quack-proxy daemon | 单点 | 它挂掉后所有分片失去健康检查与自愈（R-01） |

## 2. 默认安全边界

- `listener.bind_host` 默认 `127.0.0.1`（不是 `0.0.0.0`）
- `listener.allow_other_hostname` 默认 `false`
- `global.allow_unsigned_extensions` 默认 `false`
- `status --json` 默认不输出 token；显式 `--show-tokens` 才输出
- **status.json 默认完全不含 token（r14 / PRD §4.2）**：`global.persist_tokens` 默认 `false`，`token` 字段为 `omitempty` 且仅在开关开启时赋值；只有显式 `true` 才写入自动生成的凭据。默认模式下省略分片 token 会在配置加载期被**拒绝**（凭据不落盘就无处分发），运维须自带 token；`--show-tokens` 会在 stderr 解释空结果
- status.json / pid 文件 `0600`、临时文件 + rename 原子写，目录权限收紧（开关开启时 status.json 仍是 0600）
- `shards[].name` 限 `^[A-Za-z_][A-Za-z0-9_]*$`；`health_path` 限纯绝对路径；`bind_host` 限主机名/IP 字符
- 健康检查不跟随 3xx、URL 结构化构造、复用连接池、限流读 body
- `listener.health_expect` 可选：要求响应 body 含指定子串（v2.0.0 推荐 `"This is a DuckDB Quack RPC endpoint"`），防止「任意 2xx 服务占端口」被误判为健康
- `proxy.ssl.enabled: true` 在配置加载期直接报错（TLS 未实现）
- `gen-proxy` 只允许单分片后端，避免跨库轮询与同库双写者冲突
- 停机时确认整个进程组退出：组长 `cmd.Wait()` 之后仍以 `kill(-pid, 0)` 探测，残留后代整组 SIGKILL，避免「已停止」却仍占用分片端口（POSIX；R-21）
- 启动前检查分片端口是否已被占用，占用即拒绝启动并给出明确错误，避免 daemon 崩溃后重启时把「孤儿应答的健康探测」误报为自己的 `healthy`（R-22）
- 供应链：Actions 全 SHA pin、`govulncheck@v1.9.0` 版本 pin 且为**必需**门禁、release 产物 cosign 签名、DuckDB CLI SHA256 pin
- **门禁镜像 digest pin（r17）**：`verify-plaintext.sh` / `verify-vip-failover.sh` 的 4 个默认镜像原本是浮动 tag（含持续重发的 `alpine:latest`），现改为 `repo@sha256:<64hex>`；`internal/cicontract.CheckImagePins` 强制格式与跨脚本一致性，`CheckNoImageOverrides` 禁止 CI 用 `--image` 等参数绕过。本机 digest 零下载命中、两个门禁实跑 RC=0（linux/amd64）；**本环境 registry 探测返回 EOF，未验证 digest 是 manifest list 还是单平台 manifest，也未验证 GitHub runner 首次按 digest 拉取**。digest 需随基础镜像升级主动轮换，上游 GC 旧 digest 会让 CI 失败 —— 与 Actions SHA pin 轮换同性质，不是「做不到」；显式覆盖仅用于本地/离线复跑。
- 发布二进制固定用 **go1.27.4**（或更新的已修补工具链）构建：`go1.26.11` 实测有 11 项 Go 标准库告警，`go1.27.4` 实测 `No vulnerabilities found`
- 部署产物经真实二进制校验：`haproxy -c -f`、`systemd-analyze verify`、systemd 实机安装演练、HAProxy 降权加固验证（均为必需 CI job）
- LAN 明文风险由 `plaintext-audit` 必需 job 用真实抓包守住（证明弱点存在，防止文档悄悄宣称已有 TLS）；**r14 起同一 job 还硬断言 `status.json` 默认不含 token**（`STATUS_TOKEN_AT_REST=0`，连空 `token` 字段都拒绝）

## 3. 仍然存在的风险（不得忽略）

1. **明文传输（r8 已实证，r12 已厘清能力边界，风险未修）**：Quack 端点硬编码 `disable_ssl = true`。默认只监听环回；一旦改成 `0.0.0.0`，LAN 内抓包即可拿到唯一凭据。**r12 更正**：这不是引擎不支持 TLS —— 用同一套 pinned v2.0.0 引擎，`scripts/verify-engine-tls.sh` 实证 `disable_ssl := false` 可起 `https://` 端点、完成 TLSv1.3 握手、并用 `ssl_fingerprint` 校验客户端；是产品主动关掉了引擎已有的能力。但**引擎支持 ≠ 产品已启用**：daemon 仍只提供明文 HTTP，本风险完全未修，见第 12 条。`scripts/verify-plaintext.sh` 用真实 daemon + 独立嗅探容器（`AF_PACKET` 原始帧）证明：token、`Authorization: Bearer` 与 SQL 正文全部出现在 TCP payload 中；同一 L2 网段的 TLS 控制通道不泄漏 canary；真实 CLI 拒绝 `proxy.ssl.enabled=true`（`not implemented`）。默认镜像 3/3 通过，已落为必需 CI job `plaintext-audit`。**该门禁的传输部分断言的是当前已知弱点，不是修复**；若将来加入 TLS 终止，它会（正确地）失败，需同步更新断言与本文档。**r14 补充**：同一门禁的 token-at-rest 部分断言的是**已完成的修复**（`STATUS_TOKEN_AT_REST=0`），两者不可混为一谈 —— 明文传输依然完全未修。**r15 补充**：同一门禁新增「按 §4.2 既定方案部署」的对照半边 —— 第二个真实 daemon 的分片只绑 `127.0.0.1`，真实 HAProxy 2.8.31 在前面终止 TLS，客户端经 HTTPS 拿到分片自己的应答，`AF_PACKET` 抓包证明该 LAN 端口只有 TLS 记录、无可读凭据，后端明文腿零 payload 且 LAN 无法直连分片。这**验收的是运维部署方案，不是产品 TLS 功能**：把分片直接绑 `0.0.0.0` 并直连的风险、以及 `disable_ssl=true` 硬编码，都完全没有改变。
2. **凭据落盘已默认关闭（r14），但显式开关仍在**：`status.json` 默认不含任何 token，PRD §4.2 的甲方修订要求已执行完毕。仅当显式设置 `global.persist_tokens: true` 时才恢复「32 字符 crypto-random 凭据以 0600 落盘」，此时本机同 UID / root 仍可读，`status` 的脱敏只防「随手打印」。默认模式下运维必须**自带 token**：配置文件本身是明文，密钥保管责任转移给运维自己的 secret store，这是显式的运维选择，**不是 quack-proxy 提供的密钥管理**。误开开关不会静默发生 —— `plaintext-audit` 的硬断言会让 CI 立刻变红；`systemd-drill` 另用真实 CLI 验证「默认配置省略 token 被拒绝」。
3. **健康检查不是协议握手**：默认只判 2xx；配置 `health_expect` 后变为「响应 body 子串校验」，仍不等价于 Quack 握手或 token 验证。
4. **daemon 是单点**：无控制面冗余、无自动接管，`systemd` 单元显式 `Restart=no`（R-01）。
5. **停机无 `CHECKPOINT`**：DuckDB CLI 阻塞在 `quack_serve`，没有 admin 通道可注入 SQL；SIGKILL 兜底依赖 WAL 恢复（R-12）。R-21 的整组 SIGKILL 使这条路径更常被调用，备份/快照不应依赖优雅落盘。
6. **CI 门禁已无 advisory（r10）**：`quack-e2e` 曾因每次运行都要访问 GitHub Releases 与 DuckDB 扩展 CDN（实测返回过 HTTP 502）而在 PR 上为 advisory。r10 改为：`scripts/prefetch-quack-e2e.sh` 在 runner 上一次性预取并逐项 SHA256 校验 pinned DuckDB CLI（`c6507206…`）、签名 core `quack`（`9c893bd5…`）与 `httpfs`（`77e6c429…`），由 SHA-pinned `actions/cache` 复用；测试进程在 `QUACK_E2E_OFFLINE=1` 下**严格离线**（无 `curl`、无 `INSTALL`），缓存缺失或被篡改即硬失败、绝不静默回退联网。CI 移除 `continue-on-error`，**PR 也为必需**，并用 `--network none` 容器实证 `ALL_QUACK_E2E_CHECKS_PASSED` 与 4 个负例。供应链约束不变：绝不 `-unsigned`、绝不 community 源，签名仍由 DuckDB 在 `LOAD` 时校验。**口径更正**：r9 所谓「离线复跑」其实容器仍联网、由 DuckDB autoload 下载了 `httpfs`；严格离线时磁盘库会报 `read-only crypto module loaded`，故 r10 把 `httpfs` 也 pin 进缓存。**残余风险如实**：CI 首次 cache miss 仍需访问上游一次（runner 侧、带重试），但不再有 advisory 逃生口。**govulncheck 已转必需**：r5 发现该步骤原先固定在 Go 1.25 job、因版本要求装不上而被静默吞掉；移到 1.26 后实测 `go1.26.11` 报告 11 项 Go 标准库告警。r7 用 `go1.27.4` + `govulncheck@v1.9.0` 实测 `No vulnerabilities found`（`SCAN_RC=0`），基线清零，新增必需 job `vuln-scan`；release 工作流同步固定 `go-version: '1.27.4'`，确保发布产物与扫描用的是同一套已修补 stdlib。**不要沿用旧工具链构建的产物**。必需门禁现为：`crash-drill`（R-22 回归）、`deploy-verify`（真实 HAProxy / systemd 二进制校验 + R-23 加固）、`systemd-drill`（非 root 实机安装演练）、`vuln-scan`、`plaintext-audit`（R-05 明文实证）、`windows-runtime-guard`（R-18 产物诚实性）、**`quack-e2e`（严格离线真实引擎）**、**`vip-failover`（r11：HAProxy + VIP 联调，见第 11 条）**、**`engine-tls-capability`（r12：引擎原生 TLS 能力实证，见第 12 条；`needs: quack-e2e` 复用同一缓存，零新增下载）**。
7. **Windows 产物（r9 已收口）**：release **不再发布** Windows 二进制；CI 只保留 `GOOS=windows` 交叉编译作源码兼容性检查。r9 在真实 Windows 上实证旧产物 `version` 可用但 `start` 因 `lookupBin` 的 POSIX mode 位误判报「duckdb_bin 不可执行」；现已修判定并改为启动即显式拒绝（R-18）。本条在 r9 文档中一度被漏改，r10 一并纠正。
8. **完全离线部署需预装两个扩展（r10 新增运维约束）**：DuckDB v2.0.0 写**磁盘数据库**时必须有 `httpfs` 提供的完整 crypto 后端；只预装 `quack` 会在无网络主机上报 `read-only crypto module loaded`。请把签名 core `quack` **与** `httpfs` 都放进 `$HOME/.duckdb/extensions/v2.0.0/linux_amd64/`（或对应平台目录），文件存在即可被 autoload 命中，无需改 boot SQL；`allow_unsigned_extensions` 保持 false。
9. **daemon 崩溃后的孤儿进程**：`SIGKILL` daemon 不会带走子进程组，孤儿仍占端口且 pid/status 残留。r5 起新 daemon 会拒绝在该端口启动（R-22），但**不会自动清理孤儿**——运维必须先停掉遗留进程组再重启。自动接管属真 HA 能力，本项目不提供；演练复现步骤见 `scripts/daemon-crash-drill.sh`。
10. **生成的 HAProxy 配置默认以 root 运行**（R-23，已提供可验证的加固路径）：真实 HAProxy 3.4.7 启动时会警告缺少 `chroot` / `user`。**`haproxy -c -f` 语法校验通过不等于安全**。加固仍是**显式 opt-in**：`proxy.hardened: true` 让生成器把 `chroot /var/lib/haproxy` / `user haproxy` / `group haproxy` 写成生效指令；开启前必须先在主机预置 `haproxy` 账号与 root 所有、0755 的 `/var/lib/haproxy`，否则 HAProxy 会直接拒绝启动（r7 用真实二进制验证过这条负例）。推荐一条命令完成预置 + 验证：

   ```bash
   sudo bash scripts/verify-hardening.sh --proxy-bin ./quack-proxy
   ```

   脚本会断言：未预置账号时 `haproxy -c -f` 拒绝配置；未加固配置在 HAProxy ≥ 3.x 启动时确实告警；加固配置启动后 `/proc/<pid>/status` 的 real uid 是 `haproxy`、没有 root 告警，且前端仍能真实转发流量。实测 HAProxy **3.4.7** 与 **2.8.31** 均通过（2.8 无 3.x 的告警文案，该负例按主版本自动跳过并明确打印 SKIPPED）。默认输出仍保持三条指令为注释，保证未加固主机可用。

11. **VIP / 浮动 IP 不会带来数据高可用（r11 实证）**：`scripts/verify-vip-failover.sh` 在 Docker bridge 上把真实 `/32` 地址从节点 A 漂移到 B，证明 `gen-proxy` 生成的配置经得起入口切换（必需 CI job `vip-failover`）。但 **VIP 的移动由外部编排完成，quack-proxy 既不提供也不感知**；两个节点是各自独立的后端，脚本断言切换后对端身份变化（`SESSION_DISCONTINUITY=1`）——连接能重建，**会话状态与数据不连续**。多分片 VIP 后端仍被 `gen-proxy` 明确拒绝（R-04）；**明文传输**风险不因 VIP 改变（R-05），token 落盘已由 r14 默认关闭（但显式 `persist_tokens: true` 时仍在，同样不因 VIP 改变）。请勿据此宣称「已具备 HA」；真 HA 需另行立项。
12. **引擎原生 TLS 能力 ≠ quack-proxy 提供 TLS（r12 实证）**：`scripts/verify-engine-tls.sh` 用项目 pin 的 DuckDB v2.0.0 CLI + 签名 `quack` / `httpfs` 扩展，在 `--network none` 容器内证明引擎原生支持 TLS：`quack_serve(disable_ssl := false, ssl_cert_file, ssl_key_file)` 返回 `https://`，独立 `openssl s_client` 完成 TLSv1.3 握手，明文 HTTP 打该端口被拒，`quack_generate_keys()` 生成自签证书与 SHA256 指纹，客户端用正确 `ssl_fingerprint` 经 TLS 查得结果，错误/缺失指纹与错误 token 均被拒（必需 CI job `engine-tls-capability`，复用 `quack-e2e` 的 pinned 缓存，零新增下载）。**这是引擎能力审计，不是产品修复**：脚本绕过 daemon 直接驱动引擎，产品 `supervisor.go` 仍硬编码 `disable_ssl = true`，**R-05 的明文传输风险仍在**；token 落盘已由 r14 默认关闭（仅显式 `persist_tokens: true` 时存在），两者不可混写。**r15 更正**：这里写的「把能力接进产品需甲方决策」不成立 —— PRD §4.2 原始稿与 §8 non-goal 早已决定产品不内建 TLS、由外部代理终止；本轮已把该既定方案做成端到端验收（见第 1 条与运维清单）。证书生命周期、指纹分发、HTTPS 健康探测、`attach_sql` 契约、`gen-proxy` 前端与 `proxy.ssl` 死配置属于「若将来改变范围启用内建 TLS」才需要的新决策，不是待甲方拍板的整改待办。**脚本含 `PRODUCT_DISABLES_ENGINE_TLS=1` 诚实性断言**：产品一旦启用 TLS，此门禁会（正确地）失败，须同步更新本文件、第 1 条、`verify-plaintext.sh` 与台账，不得删除。
13. **CI 门禁契约是机器可验证的（r13 起，r17 补齐覆盖面）**：r12 之后 `engine-tls-capability` 与 `quack-e2e` 共享同一份 SHA256-pinned 缓存；若有人改了 pin 却没换 cache key、给某个 job 加 `if` 使其可跳过、把 action 改回 `@v4`、或悄悄加回 `continue-on-error`，原先**没有任何门禁会当场拒绝**（与 r5 `govulncheck` 被吞掉是同一类配置退化）。`internal/cicontract` 在已必需的 `go test ./...` 里解析真实 `ci.yml` / `release.yml`，断言 9 个门禁齐全、无 advisory、全部 `uses:` 为 40 位 hex SHA、两个缓存步骤的 `path`/`key` 完全一致且 key 含三个 pin 前缀、`engine-tls-capability` 的 `needs` 含 `quack-e2e`、离线开关已设置；r14 补 `CheckScriptPins`（离线脚本引擎 pin 与缓存生产者同步），**r17 补 `CheckImagePins` / `CheckNoImageOverrides`**（4 个门禁镜像默认值须为完整 sha256 digest pin、同一镜像跨门禁 digest 一致、CI 不得用 `--image` 等覆盖）。当前 `Validate` 共 **10 项检查 / 17 个单点篡改负例**（10 YAML 变异 + 3 脚本 pin 漂移 + 4 镜像 pin 漂移），每个负例要求恰好对应检查失败。三段链路（离线缓存断言 → 真实引擎 E2E → 引擎 TLS 审计）已在单个 `--network none` 容器按 CI 顺序 3/3 RC=0。**边界**：这是配置契约检查，**不是第 10 个必需门禁**，不改产品行为，也**不消除**第 6 条如实保留的「CI 首次 cache miss 仍需上游一次」残余风险与第 1/2 条的 R-05 风险；digest 需刻意轮换、本环境未验证 GitHub runner 首次按 digest 拉取，详见 CHANGELOG r17。

## 4. 需要对外暴露时的最小检查清单

- [ ] 在外部代理终止 TLS，并限制来源 IP；quack-proxy 自身不提供 TLS（`proxy.ssl.enabled: true` 会被加载期拒绝）。**这一项仍是运维待办，不因 r15 而勾掉**：r15 只是用真实 HAProxy 2.8.31 + loopback 分片在 `plaintext-audit` 里端到端验收了「照做有效」，证书生命周期、CA/轮换与来源 IP 限制仍由运维负责。`bash scripts/verify-plaintext.sh` 可亲眼看到 token 在错误部署的明文链路上完整可读、在按方案部署的 TLS 链路上不可读；`PLAINTEXT_INJECT_CLEARTEXT=1` 可复现「一旦有明文上网即被门禁抓住」
- [ ] 每个分片使用独立、足够随机的 token，不要复用
- [ ] 确认 status.json / pid 文件的属主与 0600 权限，目录不被其他用户可写
- [ ] 预装签名 core 扩展 `quack` **与 `httpfs`**（后者是 DuckDB v2.0.0 写磁盘库所需，离线主机尤其必须，见上文第 8 条），`allow_unsigned_extensions` 保持 false，并校验 DuckDB CLI 的 SHA256；可用 `bash scripts/prefetch-quack-e2e.sh --cache-dir <dir>` 取得三个已 SHA256 校验的 pinned 产物
- [ ] 若经 VIP/DNS 访问，才开启 `allow_other_hostname`，并理解其含义；VIP 只解决入口可达性，不保证会话/数据连续（见上文第 11 条）。可用 `bash scripts/verify-vip-failover.sh` 复现入口漂移与 `SESSION_DISCONTINUITY=1`
- [ ] 配置 `listener.health_expect`，避免端口被其他 HTTP 服务占用时假阳性
- [ ] systemd 安装后按 `scripts/systemd-install-drill.sh` 验证：服务以非 root 运行、`RuntimeDirectory` 属主/权限正确、停止后端口与 pid/status 全部清理（r7 已在真实 systemd 255 上 3/3 通过）
- [ ] 生成 HAProxy 配置前设 `proxy.hardened: true` 并执行 `scripts/verify-hardening.sh`，确认 HAProxy 已降权
- [ ] 不要从旧 release 下载 Windows 二进制：r9 起已停发；若自行交叉编译，`start` 会明确拒绝并提示改用 Linux/macOS 产物或 WSL2（`scripts/verify-windows.sh` 守住该行为）
- [ ] 监控 status.json 的 `restarts` 与 `status=stopped`；daemon 自身也要有告警
- [ ] 备份/快照 DuckDB 文件，不要依赖 SIGKILL 前的优雅落盘

## 5. 校验下载产物

Release 附带 `SHA256SUMS`、`SHA256SUMS.sig`、`SHA256SUMS.pem`。签名由本仓库 release workflow 的 OIDC 身份通过 cosign keyless 生成：

```bash
sha256sum -c SHA256SUMS
cosign verify-blob \
  --certificate SHA256SUMS.pem --signature SHA256SUMS.sig \
  --certificate-identity-regexp '^https://github.com/alitrack/quack-proxy/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS
```

只用 `sha256sum -c` 只能证明下载完整，不能证明产物确实来自本仓库。

## 6. 不适用场景

要求高可用/容灾、多机集群、合规鉴权与审计、稳定版本依赖，或承载敏感数据的生产环境。当前版本适合本地学习、CI 原型、单机非敏感数据的进程管理。

## 7. 报告漏洞

请通过仓库 GitHub Security Advisories 私下报告，并附上复现步骤与影响范围；不要直接在公开 issue 中贴出可用的攻击载荷。整改进度以 `REMEDIATION.md` 为准。

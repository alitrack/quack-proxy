# 安全说明（SECURITY）

> 适用版本：v0.2.0 + 本次整改补丁（2026-11-16，r9）
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
- status.json / pid 文件 `0600`、临时文件 + rename 原子写，目录权限收紧
- `shards[].name` 限 `^[A-Za-z_][A-Za-z0-9_]*$`；`health_path` 限纯绝对路径；`bind_host` 限主机名/IP 字符
- 健康检查不跟随 3xx、URL 结构化构造、复用连接池、限流读 body
- `listener.health_expect` 可选：要求响应 body 含指定子串（v2.0.0 推荐 `"This is a DuckDB Quack RPC endpoint"`），防止「任意 2xx 服务占端口」被误判为健康
- `proxy.ssl.enabled: true` 在配置加载期直接报错（TLS 未实现）
- `gen-proxy` 只允许单分片后端，避免跨库轮询与同库双写者冲突
- 停机时确认整个进程组退出：组长 `cmd.Wait()` 之后仍以 `kill(-pid, 0)` 探测，残留后代整组 SIGKILL，避免「已停止」却仍占用分片端口（POSIX；R-21）
- 启动前检查分片端口是否已被占用，占用即拒绝启动并给出明确错误，避免 daemon 崩溃后重启时把「孤儿应答的健康探测」误报为自己的 `healthy`（R-22）
- 供应链：Actions 全 SHA pin、`govulncheck@v1.9.0` 版本 pin 且为**必需**门禁、release 产物 cosign 签名、DuckDB CLI SHA256 pin
- 发布二进制固定用 **go1.27.4**（或更新的已修补工具链）构建：`go1.26.11` 实测有 11 项 Go 标准库告警，`go1.27.4` 实测 `No vulnerabilities found`
- 部署产物经真实二进制校验：`haproxy -c -f`、`systemd-analyze verify`、systemd 实机安装演练、HAProxy 降权加固验证（均为必需 CI job）
- LAN 明文风险由 `plaintext-audit` 必需 job 用真实抓包守住（证明弱点存在，防止文档悄悄宣称已有 TLS）

## 3. 仍然存在的风险（不得忽略）

1. **明文传输（r8 已实证，风险未修）**：Quack 端点硬编码 `disable_ssl = true`。默认只监听环回；一旦改成 `0.0.0.0`，LAN 内抓包即可拿到唯一凭据。`scripts/verify-plaintext.sh` 用真实 daemon + 独立嗅探容器（`AF_PACKET` 原始帧）证明：token、`Authorization: Bearer` 与 SQL 正文全部出现在 TCP payload 中；同一 L2 网段的 TLS 控制通道不泄漏 canary；真实 CLI 拒绝 `proxy.ssl.enabled=true`（`not implemented`）。默认镜像 3/3 通过，已落为必需 CI job `plaintext-audit`。**该门禁断言的是当前已知弱点，不是修复**；若将来加入 TLS 终止，它会（正确地）失败，需同步更新断言与本文档。
2. **凭据仍落盘**：status.json 内含所有分片 token，靠 0600 保护。`status` 的脱敏只防「随手打印」，不防本机其他进程读文件。更彻底的方案是不落盘 + 由用户自带 token（需设计决策）。
3. **健康检查不是协议握手**：默认只判 2xx；配置 `health_expect` 后变为「响应 body 子串校验」，仍不等价于 Quack 握手或 token 验证。
4. **daemon 是单点**：无控制面冗余、无自动接管，`systemd` 单元显式 `Restart=no`（R-01）。
5. **停机无 `CHECKPOINT`**：DuckDB CLI 阻塞在 `quack_serve`，没有 admin 通道可注入 SQL；SIGKILL 兜底依赖 WAL 恢复（R-12）。R-21 的整组 SIGKILL 使这条路径更常被调用，备份/快照不应依赖优雅落盘。
6. **advisory 仅剩 PR 上的真实引擎 E2E**：`quack-e2e` 依赖 GitHub Releases 与 DuckDB 扩展 CDN（实测返回过 HTTP 502）。r8 给下载与 `INSTALL quack` 各加 3 次重试，并把 CI 的 `continue-on-error` 收紧为 `${{ github.event_name == 'pull_request' }}`：**只有 PR 是 advisory**，`push main` 与 `workflow_dispatch` 均为必需门禁，主分支不得静默跳过真实引擎校验。**govulncheck 已转必需**：r5 发现该步骤原先固定在 Go 1.25 job、因版本要求装不上而被静默吞掉；移到 1.26 后实测 `go1.26.11` 报告 11 项 Go 标准库告警。r7 用 `go1.27.4` + `govulncheck@v1.9.0` 实测 `No vulnerabilities found`（`SCAN_RC=0`），基线清零，新增必需 job `vuln-scan`；release 工作流同步固定 `go-version: '1.27.4'`，确保发布产物与扫描用的是同一套已修补 stdlib。**不要沿用旧工具链构建的产物**。必需门禁现为：`crash-drill`（R-22 回归）、`deploy-verify`（真实 HAProxy / systemd 二进制校验 + R-23 加固）、`systemd-drill`（非 root 实机安装演练）、`vuln-scan`、`plaintext-audit`（R-05 明文实证）、`windows-runtime-guard`（R-18 产物诚实性）。**r9 补充实证**：`quack-e2e` 用离线 pinned DuckDB v2.0.0 CLI + 官方签名 core 扩展完整复跑 `ALL_QUACK_E2E_CHECKS_PASSED`（50 次 SELECT、停止无孤儿），证明此前只跑到 `INSTALL quack` 就卡住是容器内下载通道慢，不是脚本逻辑缺陷。
7. **Windows 产物**：release 仍交叉编译并发布一个当前无法运行的 Windows 二进制（R-18）。
8. **daemon 崩溃后的孤儿进程**：`SIGKILL` daemon 不会带走子进程组，孤儿仍占端口且 pid/status 残留。r5 起新 daemon 会拒绝在该端口启动（R-22），但**不会自动清理孤儿**——运维必须先停掉遗留进程组再重启。自动接管属真 HA 能力，本项目不提供；演练复现步骤见 `scripts/daemon-crash-drill.sh`。
9. **生成的 HAProxy 配置默认以 root 运行**（R-23，已提供可验证的加固路径）：真实 HAProxy 3.4.7 启动时会警告缺少 `chroot` / `user`。**`haproxy -c -f` 语法校验通过不等于安全**。加固仍是**显式 opt-in**：`proxy.hardened: true` 让生成器把 `chroot /var/lib/haproxy` / `user haproxy` / `group haproxy` 写成生效指令；开启前必须先在主机预置 `haproxy` 账号与 root 所有、0755 的 `/var/lib/haproxy`，否则 HAProxy 会直接拒绝启动（r7 用真实二进制验证过这条负例）。推荐一条命令完成预置 + 验证：

   ```bash
   sudo bash scripts/verify-hardening.sh --proxy-bin ./quack-proxy
   ```

   脚本会断言：未预置账号时 `haproxy -c -f` 拒绝配置；未加固配置在 HAProxy ≥ 3.x 启动时确实告警；加固配置启动后 `/proc/<pid>/status` 的 real uid 是 `haproxy`、没有 root 告警，且前端仍能真实转发流量。实测 HAProxy **3.4.7** 与 **2.8.31** 均通过（2.8 无 3.x 的告警文案，该负例按主版本自动跳过并明确打印 SKIPPED）。默认输出仍保持三条指令为注释，保证未加固主机可用。

## 4. 需要对外暴露时的最小检查清单

- [ ] 在外部代理终止 TLS，并限制来源 IP；quack-proxy 自身不提供 TLS（`proxy.ssl.enabled: true` 会被加载期拒绝）。可用 `bash scripts/verify-plaintext.sh` 亲眼看到 token 在明文链路上完整可读，以及 TLS 通道不再泄漏
- [ ] 每个分片使用独立、足够随机的 token，不要复用
- [ ] 确认 status.json / pid 文件的属主与 0600 权限，目录不被其他用户可写
- [ ] 预装签名 core 扩展（`INSTALL quack;`），`allow_unsigned_extensions` 保持 false，并校验 DuckDB CLI 的 SHA256
- [ ] 若经 VIP/DNS 访问，才开启 `allow_other_hostname`，并理解其含义
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

# Changelog

本文件记录可影响使用者的行为变更。整改台账见 `docs/REMEDIATION.md`，边界见 `docs/SCOPE.md`。

## Unreleased — remediation r10 (2026-11-16)

把整改台账里最后一项「本环境能做、但一直被当作做不到」收紧：`quack-e2e` 在 **PR 上也不再是 advisory**。做法是让真实引擎 E2E 不再在测试过程中访问外部 CDN，改为 CI runner 上一次性预取、SHA256 校验、缓存复用。

### 更正：「离线」这个词之前用得不严谨

r9 写「离线喂入 pinned CLI + 签名扩展后 E2E 完整通过」——产物确实是离线的，但**容器仍有网络**。本轮首次用 `--network none` 复跑，立刻失败：磁盘数据库分片报 `DuckDB currently has a read-only crypto module loaded. Please ensure httpfs is loaded using LOAD httpfs`。根因是 DuckDB v2.0.0 写加密数据库文件需要 `httpfs` 带来的完整 crypto 后端；r9 之所以通过，是 DuckDB 在联网容器里把 httpfs **autoload** 掉了。判别实验：同一无网络容器里 `:memory:` 数据库一切正常，只有磁盘库触发该错误。

因此本轮把 `httpfs` 也作为**签名 core 扩展** pin 进缓存（gzip SHA256 `ee6c7d7e…`，解压后 `77e6c429…`），与 `quack` 一起放进隔离 HOME 的扩展目录。文件存在即可由 autoload 命中，**不需要改动 supervisor 的 boot SQL，也不需要 `-unsigned` 或 community 源**。同时明确一条新的运维约束：完全离线的生产部署必须预装 `quack` **与** `httpfs`，只装 `quack` 会在写磁盘库时失败。

### Added / Changed

- **新增 `scripts/prefetch-quack-e2e.sh`**：在 CI runner（而非测试进程内）准备离线缓存，pin 三件产物 —— DuckDB v2.0.0 CLI（`c6507206…`）、签名 `quack`（`9c893bd5…`）、签名 `httpfs`（`77e6c429…`），连同 gzip 的中间 hash 与 manifest。缓存命中时**零网络请求**；`QUACK_PREFETCH_OFFLINE=1` 只做断言、绝不下载。下载带 3×3 重试，已完整且校验通过的 `.gz` 会直接复用（本机实测 httpfs 对象反复在 ~8 MB 处断流，这个复用是必要的）。
- **`scripts/quack-e2e.sh` 新增 `--cache-dir` / `QUACK_E2E_CACHE_DIR`**：缓存模式下严格离线（无 `curl` 下载、无 `INSTALL`），逐项校验三个 hash，把扩展放进**隔离 HOME**（`DUCKDB_EXTENSION_DIRECTORY` 在 v2.0.0 实测被忽略，`SET extension_directory` 又无法注入固定的 boot SQL；`go build` 仍用真实 HOME 以免丢 GOCACHE）。`QUACK_E2E_OFFLINE=1` 时缓存缺失/损坏是硬失败，**不静默回退联网**。非缓存模式保持原有下载 + `INSTALL` 重试行为不变。
- **`.github/workflows/ci.yml`**：`quack-e2e` 移除 `continue-on-error`，**所有事件（含 `pull_request`）均为必需门禁**；新增 SHA-pinned `actions/cache@0057852… # v4.3.0`（key 含三个 hash）、prefetch 步骤、离线断言步骤。CI 注释如实写明残余风险：首次 cache miss 仍需访问上游一次，但不再有 advisory 逃生口。
- `README.md` / `docs/SCOPE.md` / `docs/REMEDIATION.md` / `docs/SECURITY.md` / `docs/PRD.md` 全部收敛 r10。

### Verification（r10 实机）

- **单个 `--network none` 容器内**跑完整链路：prefetch 的 miss / hit / 严格离线断言三条路径全通，随后 `QUACK_E2E_OFFLINE=1 bash scripts/quack-e2e.sh --cache-dir … --benchmark 50` 输出 `ALL_QUACK_E2E_CHECKS_PASSED` —— DuckDB v2.0.0、正确/错误 token、HTTP 200 健康循环、50 次 SELECT（230 ms，~217 qps）、停止后端口释放无孤儿。证据行含三个 `OK` 的 SHA256 校验与 `OFFLINE_CACHE_USED=1`。
- **4 个负例全过**：空缓存 + `OFFLINE=1`、完全不给 `--cache-dir` + `OFFLINE=1`、篡改 `httpfs` 条目、prefetch 严格离线断言遇不完整缓存 —— 全部非零退出，且输出中不存在 `ALL_QUACK_E2E_CHECKS_PASSED` / `all shards started` / `PREFETCH_CACHE_HIT`。另有无网络下的判别负例：不预装 httpfs 时磁盘库写失败，`:memory:` 正常。
- 回归：10 个脚本 `bash -n`、三份 YAML `yaml.v3` 实解析、`git diff --check` 干净；Go 代码本轮未改动，`gofmt` / `go vet` / `go test ./... -race` 复跑 5 包全绿、覆盖率 70.9%。

#### 仍未完成（如实）

HAProxy + VIP 联调（需真实浮动 IP / 双机网络）；R-05 的 `disable_ssl` 硬编码 / token 不落盘 / 内建 TLS 属**需设计决策**（`plaintext-audit` 只证明弱点、不修风险）；真 HA 属新产品立项。**r10 已解除**：`quack-e2e` 在 PR 上的 advisory。**r9 已解除**：Windows 产物收口、真实引擎 E2E 完整复跑（本轮进一步收紧为严格离线）。

---

## Unreleased — remediation r9 (2026-11-16)

本轮收口 R-18（Windows 产物），并把「外部依赖导致跑不完」的 E2E 用离线通道完整复跑通过。

### 更正：R-18 不是「待产品决策」

PRD §8 从 v0.1 起就把 Windows 列为 non-goal，但 release 一直发布一个**签过名、看起来能用**的 `.exe`。在真实 Windows（本机 MINGW64_NT-10.0-19045）上实证：`version` 正常返回 `quack-proxy dev`，`start` 却报
`duckdb_bin "C:/.../drill-stub.exe" not found on PATH or not executable` —— 用户会以为是配置写错。根因不是 bash/tail：`lookupBin` 用 POSIX 可执行位 `mode&0o111` 判定，而 Windows 的 `os.Stat` 对普通文件返回 0666/0444，**任何绝对路径的引擎都会被误判**。之前把它记为「需运维/产品决策」，实际是「既有 non-goal 决策没有执行到位」。

### Changed / Fixed

- **撤下 Windows 发布产物**：`release.yml` 移除 `windows/amd64` 矩阵项与 `.zip` 打包分支，只发布 linux/darwin（amd64 + arm64）。CI 仍保留 `GOOS=windows` 交叉编译作为源码兼容性检查，但不再向用户分发不可运行的签名二进制。
- **修 `lookupBin` 的跨平台可执行判定**：新增平台函数 `executableFile()`（`attr_unix.go` 用 mode 位；`attr_windows.go` 按 `PATHEXT` 扩展名，环境被裁剪时仍认 `.exe/.com/.cmd/.bat`）。`gate5_orphan_test.go` 里因 R-18 加的 Windows skip 已移除；`GOOS=windows go test -c ./internal/supervisor` 编译通过。
- **Windows 运行时改为显式拒绝**：`runStart` 在任何配置加载之前输出 `Windows is not a supported runtime platform (R-18)`，说明缺少 POSIX shell / `/dev/null` / 进程组语义，并给出 Linux/macOS 或 WSL2 的替代路径。新增 `runtimeSupported()` 与两个不依赖宿主平台的单测。
- **新增必需门禁 `windows-runtime-guard`**（`runs-on: windows-latest`）：`scripts/verify-windows.sh` 在真实 Windows 上断言 `version` 可用、`start` 非 0 且含 `R-18` / `WSL2`；**故意传一个不存在的配置路径**，看到平台错误而非 `failed to load config` 即证明守卫在 config.Load 之前；负例还断言旧的 `not found on PATH or not executable` 与 `all shards started` 都不出现，且不留 pid 文件。本机 Git Bash 实跑 `WINDOWS_RUNTIME_GUARD_PASSED`。

### Verification（r9 实机）

- **真实引擎 E2E 完整复跑通过**：r8 之后一直只跑到 `INSTALL quack` 就卡住。本轮改用主机 HTTPS 通道取 pinned DuckDB CLI（24 MB / 48s，SHA256 与脚本 pin 完全一致 `c6507206…`）与官方签名 core 扩展（19 MB gzip → 60 MB），离线喂入容器后 `scripts/quack-e2e.sh --benchmark 50` 输出 `ALL_QUACK_E2E_CHECKS_PASSED`：DuckDB v2.0.0、正确/错误 token、健康循环、50 次 SELECT（207 ms，~241 qps）、停止后端口释放无孤儿。**容器内下载慢 ≠ 通道被拦**，重试逻辑本身未被改动。
- 最终全量回归：gofmt / `go vet` / `go test ./... -race -count=1` 5 包全绿；**71** 个测试函数；总覆盖率 **70.9%**（cmd 27.0% / config 93.8% / health 90.9% / proxy 98.3% / supervisor 80.1%）。较 r8 的 71.1% 略降，原因是新增的 Windows 运行时分支在 Linux runner 上不可达，属如实下降而非回归。linux·darwin·windows 交叉构建、Windows 测试包交叉编译、9 个 shell 脚本 `bash -n`、三份 YAML `yaml.v3` 实解析、`git diff --check` 全绿。
- 受 `lookupBin` 改动影响的两条真实门禁用最终源码复跑：真实 systemd 255 非 root 安装演练 `SYSTEMD_INSTALL_DRILL_PASSED`（含负例）、LAN 明文抓包审计 `PLAINTEXT_AUDIT_PASSED`。

#### 仍未完成（如实）

HAProxy + VIP 联调（需真实浮动 IP / 双机网络）；`quack-e2e` 在 **PR 上仍为 advisory**（外部 CDN 在容器内偶发不可用，主分支已必需）；`disable_ssl` 硬编码 / token 不落盘 / 内建 TLS / 真 HA 属需设计决策或新产品立项。

---

## Unreleased — remediation r8 (2026-11-15)

本轮把「仍未做」清单里**可做但一直没做**的 LAN 明文抓包做成实证门禁，并收紧另一项外部依赖的 advisory 范围。

### 更正：清单里的「未做」不等于「做不到」

r5~r7 把「LAN 明文抓包」列为未做，从未断言环境不允许——只是没做。本轮证明：一条 Docker bridge 隔离网段 + 真实 daemon + 独立 `AF_PACKET` 嗅探容器即可实证，不需要第二台物理机。前两轮是「通道/参数受限误记为能力受限」，这一轮是「**未做被清单化后默认成了做不到**」。已写入 `SCOPE.md §8` 与 `REMEDIATION.md §4`。

### Added

- `scripts/verify-plaintext.sh`：**必需** CI job `plaintext-audit`。真实 daemon 以 `bind_host: 0.0.0.0` 起分片，独立客户端容器与独立嗅探容器（`--network container:<server>` 共享 netns）跑在同一 bridge 上。断言：
  - `PLAINTEXT_TOKEN_VISIBLE=1` —— 原始 TCP payload 中直接读到 `qp-lan-cleartext-token`、`Authorization: Bearer` 与 `SELECT * FROM secrets`；
  - `TLS_CONTROL_HIDDEN=1` —— 同网段 TLS 控制通道捕获 5009 B，但 canary 与分片 token 均不可见（负例，证明嗅探器真的在解析报文而不是 grep 日志）；
  - `PRODUCT_TLS_UNAVAILABLE=1` —— 真实 CLI 拒绝 `proxy.ssl.enabled=true`（`not implemented`）；
  - `STATUS_TOKEN_AT_REST=1` —— `status.json` mode 600，但 token 明文落盘。
  - 另有「嗅探器失灵 → 捕获文件为空即失败」的隐性负例。默认镜像 `python:3.11-slim` **3/3**、换 `jrei/systemd-ubuntu:24.04` **1/1** `PLAINTEXT_AUDIT_PASSED`。
- `scripts/drill-stub.go` 新增 `DRILL_STUB_BIND`（默认仍 `127.0.0.1`），仅明文审计设为 `0.0.0.0`；systemd 演练行为不变。

### Changed / Fixed

- **审计 ≠ 修复**：`plaintext-audit` 断言的是当前**已知弱点**，不是通过即安全。R-05 的 `disable_ssl` 硬编码 / token 落盘仍属需设计决策；若将来加入 TLS 终止，此门禁会（正确地）失败，必须同步更新断言与文档，不得删除。
- `scripts/quack-e2e.sh`：pinned DuckDB CLI 下载与 `INSTALL quack` 各 3 次重试（curl 自带 `--retry 3` + 外层 3 次循环），失败输出明确诊断；**绝不回退** `-unsigned` 或 community 源，保住签名扩展约束。
- `.github/workflows/ci.yml`：`quack-e2e` 的 `continue-on-error` 改为 `${{ github.event_name == 'pull_request' }}` —— **仅 PR 为 advisory**，push `main` 与 `workflow_dispatch` 为必需门禁；新增必需 `plaintext-audit` job。
- `README.md` / `docs/SCOPE.md` / `docs/REMEDIATION.md` / `docs/SECURITY.md` / `docs/PRD.md` 全部收敛 r8。

### Verification（r8 实机）

- 明文审计 3/3 + 跨镜像 1/1 通过，证据行如上。
- 最终全量回归：gofmt / `go vet` / `go test ./... -race -count=1` 5 包全绿、总覆盖率 **71.1%**（proxy 98.3% / supervisor 80.1%，69 个测试函数）、linux·darwin·windows 交叉构建、8 个 shell 脚本 `bash -n`、三份 YAML `yaml.v3` 实解析、`git diff --check` 全绿。

#### 仍未完成（如实）

HAProxy + VIP 联调（需真实浮动 IP / 双机网络）、Windows 产物去留决策（R-18，产品决策）；`quack-e2e` 在 **PR 上仍为 advisory**（外部 CDN 偶发 502）；`disable_ssl` 硬编码 / token 不落盘 / 内建 TLS / 真 HA 属需设计决策或新产品立项。

---

## Unreleased — remediation r7 (2026-11-15)

本轮把 r6 留下的四项「未完成」里的三项做实，并再次更正上一轮的「环境限制」结论。

### 更正：r6 的「环境无 systemd」是误判

r6 把「非 root systemd 安装演练」记为环境阻断。实际是容器启动参数不对：Docker Desktop / WSL2 下用 `--privileged --cgroupns=host` 并把 `/sys/fs/cgroup` 以 **rw** 挂成 cgroup2，`systemd 255` 即作为 PID 1 正常 `is-system-running=running`；`--cgroupns=private` 会因 `Failed to create /init.scope control group: No such file or directory` 退出。r5 把 apt 被拦当成装不上工具，r6 把 cgroup 命名空间不对当成没有 systemd —— 同一类误判连续两轮，已写入 `SCOPE.md §8`。

### Added

- `scripts/systemd-install-drill.sh` + `scripts/drill-stub.go`：在**真实 systemd 255 PID 1** 上安装 shipped unit，验证 `User=quack-proxy`、`RuntimeDirectory` 属主/0750、pid/status 0600、healthy、`status --json` 脱敏与 `--show-tokens`、`systemctl stop` 后端口/pid/status/进程组全清理。**负例**：同一二进制以该账号直接启动且无 `/run/quack-proxy` 时必须被 `permission denied` 拒绝。CI 新增必需 job `systemd-drill`。
- `proxy.hardened`（默认 `false`）：设为 `true` 时 `gen-proxy` 在 `global` 段输出生效的 `chroot /var/lib/haproxy` / `user haproxy` / `group haproxy`。
- `scripts/verify-hardening.sh`：预置 `haproxy` 账号与 root:root 0755 chroot，用真实 HAProxy 断言「未预置账号的配置被 `haproxy -c -f` 拒绝」「未加固配置在 3.x 启动时确实告警」「加固配置 real uid 已降权、无 root 告警、前端真实转发」。HAProxy 3.4.7 3/3、2.8.31 1/1 通过（2.8 的告警负例按主版本显式 SKIPPED）。CI 必需步骤。
- `TestGenerateHAProxyHardenedEmitsDirectives`：测试函数 68 → **69**。

### Changed / Fixed

- **govulncheck 转必需**：`go1.26.11` 的 11 项 Go 标准库告警在 `go1.27.4` 下清零（实测 `No vulnerabilities found`、`SCAN_RC=0`）。移除原 advisory 步骤，新增必需 `vuln-scan` job（固定 go1.27.4）；`release.yml` 同步固定 `go-version: '1.27.4'`，避免「扫描干净但发布产物仍是旧 stdlib」；`go` CI 矩阵加 1.27。
- **R-23 从「文档化缓解」升级为「可验证的加固路径」**，默认仍 opt-in —— 负例证明把指令注入未预置主机会让 HAProxy 直接起不来。
- `README.md` / `docs/SCOPE.md` / `docs/REMEDIATION.md` / `docs/SECURITY.md` / `quack-proxy.example.yaml` / CI 文档全部收敛 r7。
- `.gitattributes` 补齐 `*.py` / `*.sql` / `go.mod` / `go.sum` / `LICENSE` / `.gitignore` 的 `text eol=lf`，并把工作区 9 个历史 CRLF 文件统一为 LF。背景：Windows 上 `core.autocrlf=true` 会把这些文件恢复成 CRLF，导致 `bash -n scripts/quack-start.sh` 报 `unexpected end of file`；**git blob 原本就是 LF，已发布内容未受影响**，这只影响本地检出与全量脚本语法门禁。现在 7 个 shell 脚本全部通过 `bash -n`。

### Verification（r7 实机）

- systemd 255 安装演练 **3/3 `SYSTEMD_INSTALL_DRILL_PASSED`**；HAProxy 3.4.7 加固验证 **3/3**、2.8.31 **1/1** `HARDENING_VERIFY_PASSED`。
- `go test ./... -race -count=1` Linux 容器 5 包全绿；69 个测试函数；proxy **98.3%** / supervisor **80.1%** / 总计 **71.1%**；gofmt、vet、三平台交叉构建、7 个 shell 脚本 `bash -n`、YAML 三份 `yaml.v3` 实解析、`git diff --check` 全绿。
- 真实工具回归（最终源码复跑）：systemd 演练 `SYSTEMD_INSTALL_DRILL_PASSED`、加固验证 `HARDENING_VERIFY_PASSED`、`verify-deploy.sh --only haproxy` 的 `HAPROXY_CONFIG_VALID=1` / 负例被拒 / `DEPLOY_VERIFY_PASSED`。
- `govulncheck@v1.9.0` + `go1.27.4` → `SCAN_RC=0`。

#### 仍未完成（如实）

LAN 明文抓包、HAProxy + VIP 联调、quack-e2e 取消 advisory（外部 CDN 偶发 502）、Windows 产物去留决策（R-18）；`disable_ssl` 硬编码 / token 不落盘 / 内建 TLS / 真 HA 属需设计决策或新产品立项。

---

## Unreleased — remediation r6 (2026-11-15)

基于六领域专家评审（`eadef51`）的六轮整改。**含多处不兼容变更，升级前请读完本节。**

### r6 增量（2026-11-15）

#### 解除上一轮「环境阻断」结论

r5 把「真实 `haproxy -c -f` / `systemd-analyze verify`」记为三次 apt 源 502 造成的环境阻断、验收未达成。**该结论已作废**：改用本地 `alpine:latest` 的 `apk`（HTTPS CDN）安装 HAProxy，以及「主机 HTTPS 下载 Ubuntu noble `.deb` → 容器离线 `dpkg-deb -x`」取得 systemd 255，两项真实校验全部跑通。教训：apt 被拦 ≠ 所有安装通道被拦。

#### Added

- 新增 `scripts/verify-deploy.sh`：用**厂商真实二进制**校验部署产物——`haproxy -c -f`（及 `-c -V`）、故意写坏配置的**负例必须被拒**、`--forward` 真实起 stub + HAProxy 并断言前端响应体；`systemd-analyze verify` 校验 unit，并用 `KillMode=TOTALLY_BOGUS` 负例要求出现诊断文本（该工具对部分解析错误仍返回 0，只查退出码会漏）。
- CI 新增**必需** `deploy-verify` job（`apt-get install haproxy systemd curl`，无第三方下载），执行 `bash scripts/verify-deploy.sh --forward`。
- 新增 `TestGenerateHAProxyDocumentsRootHardening`，测试函数 67 → **68**。

#### Changed / Fixed

- **R-23**：真实 HAProxy 3.4.7 启动告警——生成的配置以 root 运行、无 `chroot` / `user`。quack-proxy 只生成、不启动 HAProxy，也无法预置主机账号与目录，故**不注入**这些指令（注入会让未预置的主机直接不可用），改为在生成文件里写明加固步骤并指向 `docs/SECURITY.md`。状态：部分缓解 / 需运维加固。
- `deploy/systemd/quack-proxy.service`：`Description` 去掉误导性的 `cluster manager`，改为 `single-host DuckDB + Quack process supervisor`；`Restart=` 注释补上 R-22 端口预检会让自动重启必然失败。

#### Verification（r6 实机）

- Alpine + HAProxy **3.4.7 LTS**：`haproxy -c -f` RC=0 / `Configuration file is valid`；负例被拒；`--forward` 返回 `quack-forward-ok`（前端 19600 → 后端 19601）。
- Ubuntu 24.04 + **systemd 255**（离线提取 `systemd` / `libsystemd-shared` / `libapparmor1` / `libkmod2` noble deb）：`systemd-analyze verify` RC=0；负例输出 `Failed to parse kill mode specification`。
- `go test ./... -race -count=1` Linux 容器 5 包全绿；本机 68 个测试全绿；覆盖率 proxy **97.8%**、supervisor 60.0%；gofmt / vet / 三平台交叉构建 / 三脚本 `bash -n` / YAML 三份实解析 / `git diff --check` 全绿。

#### 仍未完成（如实）

非 root systemd **安装演练**（`verify` 是静态校验，不等于实机启停）、LAN 明文抓包、HAProxy + VIP 联调、清零 govulncheck 的 11 项 stdlib 告警后转必需、quack-e2e 转必需、Windows 产物去留；`disable_ssl` 硬编码 / token 不落盘 / 真 HA 属需设计决策或新产品立项。

---

### r5 基线（2026-11-14）

基于六领域专家评审（`eadef51`）的五轮整改。

### r5 增量（2026-11-14）

#### Changed / Added

- **`reload` 现在做字段级 diff**（R-07）：分片 `port` / `database` / `token` / `init_sql` / `init_sql_file` 变化只滚动重启该分片；`duckdb_bin` / `bind_host` / `allow_other_hostname` / `allow_unsigned_extensions` 变化重启全部；`health_path` / `health_interval` / `health_expect` 变化不重启。`init_sql_file` **内容**变化（路径不变）也通过 sha256 摘要触发重启。省略的 token 会被解析成随机值，diff 先归一化再比较，避免每次 reload 误重启。
- `ApplyConfig` 返回值新增 `restarted []string`；SIGHUP 日志增加 `restarted` 计数。
- **启动前端口预检**（R-22）：`startShard` 用 `net.Listen` 探测分片端口，已被占用时直接报错并提示「可能是崩溃 daemon 的遗留分片」，不再制造假 `healthy`。自动接管孤儿属真 HA，未实现。
- 新增 `scripts/daemon-crash-drill.sh`：`SIGKILL` daemon 后复现孤儿占端口、pid/status 残留，用两代 stub 的不同响应体证明健康探测由孤儿应答；脚本带 R-22 断言。
- CI 新增**必需** `crash-drill` job（无外部网络依赖）；`govulncheck` / `quack-e2e` 仍为 advisory，理由见 `docs/SECURITY.md`。
- **修正 CI 中 `govulncheck` 的 Go 版本配置 bug**：`v1.9.0` 要求 Go ≥ 1.26，原先步骤固定在 `matrix.go == '1.25'` 上，`go install` 每次都失败却被 `continue-on-error: true` 静默吞掉，等于从未真正运行。现移到 `1.26` job。实测（自动切换到 go1.26.11）报告 **11 项 Go 标准库告警**（`SCAN_RC=3`），基线非零，故**仍保持 advisory**，未伪称零告警；清零并用补丁工具链重建后才可转必需。
- `scripts/quack-e2e.sh`：冷启动等待窗口 30s → 60s，失败时输出 daemon 日志、boot SQL、status.json 与进程现场。

#### Fixed

- **R-07** 从「部分缓解 / 需设计决策」变为已修；**R-22** 为本次演练新发现并修复。
- daemon crash 演练（原列为未做验收）已完成，结论写入 `docs/REMEDIATION.md`。

#### Verification（r5 实机）

- 67 个测试函数（+12）；`-race` Linux 容器 5 包全绿；覆盖率 supervisor **60.0%**（r4 为 55.1%）。
- 真实进程组集成：reload 换端口 PASS 0.22s；第二 daemon 拒绝孤儿端口 PASS；`TestIntegrationRealProcessLifecycle` 自愈仍通过。
- 真实 DuckDB v2.0.0 + Quack E2E：**5 轮通过**（含端口预检改动后的最终复跑），1 次冷启动超过 30s 等待窗口失败（已加长到 60s 并增强诊断，随后 3/3 通过）。分片进程组被 `SIGKILL` 后 daemon 在 ~31s 自愈、`restarts=1`，证明端口预检**不误伤**自愈重启；停止后端口释放。
- daemon crash 演练：修复前 `FALSE_HEALTHY=1`；修复后 `RESTART_REFUSED_ON_ORPHAN_PORT=1`、`FALSE_HEALTHY=0`、脚本退出 0。

#### 当时未能完成（已在 r6 解除）

- 真实 `haproxy -c -f` 与 `systemd-analyze verify` 二进制级校验：受限于 apt 源 502，当时未做；**r6 已用 Alpine apk 与离线 noble `.deb` 全部跑通**，见上方 r6 增量。
- 其余未完项见下方 r4 基线与 `docs/REMEDIATION.md`。

---

### r4 基线（2026-11-13）

r4 = r3 + R-21（停止后进程组未完全回收）修复与收敛。以下为 r4 及之前的完整变更。

### Breaking changes

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

### Security

- status.json 与 pid 文件改为 0600 + 临时文件原子写；错误不再被静默吞掉；文件 I/O 移出 supervisor 锁。
- 健康检查不再跟随 3xx、URL 结构化构造（消除 `health_path` SSRF）、复用连接池、限定响应体读取量，并支持 body 身份校验。
- README 移除 `curl | bash` 推荐，改为下载 + 审阅 + 执行。
- release workflow 顶层最小权限 + `id-token: write`，**`SHA256SUMS` 由 cosign keyless OIDC 签名**（附 `.sig` / `.pem`）。
- **所有 GitHub Actions 按真实 commit SHA pin**；`govulncheck` 固定 `@v1.9.0`；DuckDB CLI 由 SHA256 校验；扩展改用签名 core 版本。

### Fixed

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

### Verification

- 新增 `internal/supervisor/integration_test.go`：真实子进程组 E2E（起 → kill → 自愈 → 停），在 `-race` 下通过。
- 新增 `internal/supervisor/gate2_regression_test.go`：探测不持锁、退避语义、并行停机回归。
- 新增 `TestStopAllReapsLingeringDescendants`（R-21）：用忽略 SIGTERM 的真实子进程组复现孤儿分片，`StopAll` 返回后立刻断言端口释放；`-race` 下 2.13s PASS。
- 新增 `scripts/quack-e2e.sh`：**真实 DuckDB v2.0.0 + 签名 Quack 扩展**端到端（SHA256 pin、正确/错误 token 认证、GET / 2xx、healthy 状态、可选 `ATTACH/SELECT` 基准、清理校验）。r4 最终连续 3 轮（3 个不同端口）均输出 `ALL_QUACK_E2E_CHECKS_PASSED` 且停止后端口释放；50 次往返首次 221ms (~226 qps)、复跑 312ms (~160 qps)，单客户端。
- `scripts/quack-e2e.sh` 现优先 `LOAD quack` 复用本地已预装的签名扩展，仅在缺失时才联网 `INSTALL`，使 E2E 在扩展 CDN 不可用（实测 HTTP 502）的离线环境仍可运行；这与 SCOPE 要求的「运维预装」一致。
- CI 新增 `quack-e2e` job（advisory，依赖外部 release/扩展仓库）。

### Docs

- 新增 `docs/SCOPE.md`、`docs/REMEDIATION.md`、`docs/SECURITY.md`、`CHANGELOG.md`。
- `docs/PRD.md` 改为实现状态跟踪表；README 撤下「highly-available cluster」与把 83K/169K QPS 当产品性能的表述，补充测量对象 caveat、token 脱敏、单机无副本、扩展预装、cosign 验证与 Windows 不可用说明。

### 实测结果（2026-11-13）

`gofmt -l .` 空 · `go vet ./...` 通过 · `go test ./... -count=1` 5 包全绿 · **`go test ./... -race -count=1` Linux 容器 5 包全绿**（含 R-21 孤儿回收回归，专项 `-v` PASS 2.13s） · linux/darwin/windows amd64 交叉构建通过 · YAML 三份实解析通过 · CLI E2E（`version`=dev、多分片 `gen-proxy` 拒绝且不生成文件、单分片生成）通过 · **真实 DuckDB+Quack E2E 连续 3 轮通过，停止后端口稳定释放**。

覆盖率（r4 当时实测，历史值；r5 最新值见上文）：cmd 23.0% · config 93.8% · health 90.9% · proxy 97.3% · supervisor 55.1%（新增进程组回收代码使分母变大）；CI 设 50% 总门槛。

**r4 当时未完成、后续已解除**：daemon crash 演练、reload 字段 diff → r5 已完成；真实 `haproxy -c -f` / `systemd-analyze verify` 二进制级校验 → **r6 已完成**。仍待办：非 root systemd 安装演练、LAN 明文抓包、HAProxy + VIP 联调、govulncheck / quack-e2e 取消 advisory、Windows 产物去留决策；`disable_ssl` 硬编码、token 落盘、真 HA 等属需设计决策或超范围，详见 `docs/REMEDIATION.md`。

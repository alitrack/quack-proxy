# quack-proxy PRD

> Product Requirements Document | v0.1 | 2026-05-29
> Author: alitrack

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
| 跨机器联邦 | 手动 ATTACH 每台机器 | Coordinator DuckDB 一键联邦 |

### 目标用户

- 数据分析团队：有多台机器上的 DuckDB 分区数据，需要统一查询入口
- 小规模 SaaS：用 DuckDB 做嵌入式 OLAP，需要多客户端并发写入
- duckdb_fdw 用户：需要通过 Quack 解决多个 PG backend 的并发写入问题
- 个人开发者：本地开发时需要 Quack server 进程的便捷管理

---

## 2. 产品愿景

**通过一个 YAML 文件和一条命令，把任意数量的 DuckDB 数据库文件变成高可用的 Quack 服务集群。**

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
│  └── Optional: Coordinator DuckDB        │
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
    ├── signal handler (SIGTERM, SIGHUP)
    ├── config watcher (inotify on config file)
    ├── health check loop (每 N 秒轮询所有端点)
    │
    └── child processes (每个 DuckDB 实例一个 goroutine)
        ├── duckdb -- Quack on :9491 ← /data/shard_1.db
        ├── duckdb -- Quack on :9492 ← /data/shard_2.db
        └── duckdb -- Quack on :9493 ← /data/shard_3.db
```

### 3.3 生命周期

```
START:
  1. 解析 YAML 配置
  2. 验证所有 .duckdb 文件存在
  3. 为每个 shard 启动 duckdb 子进程
     - 生成随机 token（如果未指定）
     - duckdb -c "INSTALL quack; LOAD quack; CALL quack_serve('quack:0.0.0.0:PORT', token='TOKEN');"
  4. 等待所有进程就绪（HTTP health check 通过）
  5. 进入运行循环

RUNNING:
  1. 每 N（可配置）秒 HTTP GET 所有端点
  2. 不健康的端点：kill 子进程 → 重启 → 指数退避
  3. 写状态文件 /var/run/quack-proxy/status.json

STOP:
  1. SIGTERM → 所有子进程 → 等待 graceful shutdown
  2. SIGKILL 超时 timer（默认 10s）

RELOAD:
  1. SIGHUP → 重新解析配置 → 增量更新子进程
    - 新增 shard → 启动新进程
    - 移除 shard → graceful shutdown
    - 未变化的 shard → 保持运行
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
  bind_host: 0.0.0.0            # Quack 端点监听地址
  port_start: 9491              # 端口起始（递增分配）
  health_path: /                # 健康检查路径 (Quack HTTP)
  health_interval: 5s           # 健康检查间隔

shards:
  - name: orders_2024           # 逻辑名称
    database: /data/orders_2024.db
    port: 9491                  # 可选，自动分配
    token: ""                   # 可选，空则自动生成
    readonly: false             # 可选

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
  mode: roundrobin              # roundrobin | leastconn
  ssl:
    enabled: false
    cert: /etc/ssl/quack.pem
```

### 4.2 Token 安全模型

- 未指定 token → 启动时自动生成 32 字符随机 token，写入 `status.json`
- 指定 token → 使用指定值
- 生产环境建议：Nginx 反向代理做 SSL termination + 固定 token

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

---

## 6. 与 duckdb_fdw 的集成

quack-proxy 管理者 Quack 端点后，duckdb_fdw 用户可以直接连接：

```sql
-- PG 中配置 duckdb_fdw
CREATE SERVER quack_cluster FOREIGN DATA WRAPPER duckdb_fdw
OPTIONS (quack_host 'localhost:9490');  -- HAProxy VIP

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

| 需求 | 目标 |
|------|------|
| 启动时间 | <5s（含所有 DuckDB 进程启动 + extension 加载） |
| 健康检查间隔 | 可配置，默认 5s |
| 故障恢复 | 崩溃自动重启，指数退避（1s→2s→4s→…→30s max） |
| 内存占用 | <50MB（不含 DuckDB 子进程） |
| 二进制大小 | <15MB（Go 静态编译） |
|| 平台 | Linux + macOS + Windows |
| DuckDB 版本 | >= 1.5.2（Quack 可用版本） |
| 并发 shard 数 | 1-1000 |

---

## 8. 不做什么

| 不做 | 理由 |
|------|------|
| 分布式事务协调 | 超出范围，DuckDB 单事务只能写一个 ATTACH 数据库 |
| 自动分区 / sharding | 用户自己决定数据分区策略 |
| Web UI / Dashboard | v0.1 只做 CLI |
| Quack 协议实现 | 用 DuckDB 自身的 Quack 客户端（ATTACH 模式） |
| 跨机器 Quack 端点发现 | v0.1 专注单机多文件管理 |
| 持久化 Coordinator | v0.1 的 Coordinator 是无状态的，用户自己启动 DuckDB + ATTACH |

---

## 9. 里程碑

### M1: 最小可用（1-2 天）

- [ ] YAML 配置解析
- [ ] 启动 N 个 DuckDB+Quack 子进程
- [ ] 命令行 `start` / `stop` / `status`
- [ ] 基础健康检查 + 自动重启
- [ ] `status --json` 输出 `coordinator_attach_sql`

### M2: 运维能力（1 天）

- [ ] SIGHUP 热重载（增量更新子进程）
- [ ] `gen-proxy` 生成 HAProxy 配置
- [ ] 指数退避重启策略
- [ ] PID 文件 + 优雅关闭

### M3: 生产就绪（1 天）

- [ ] systemd unit 文件模板
- [ ] 日志结构化输出
- [ ] README + 快速开始指南
- [ ] 二进制发布（GitHub Releases）

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
| 跨平台 | Linux + macOS + Windows | Quack 扩展三平台全支持 |

---

*文档版本 v0.1 | 下一步：go mod init + 脚手架代码*

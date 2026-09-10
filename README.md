# oracle-keeper

Oracle Cloud 永久免费实例（Always Free）保活守护进程。针对 Oracle 的三条闲置回收判据，
每小时制造一轮"真实负载"，忙时自动让路。

- **CPU**：7 天内 95% 时间利用率 < 10% → 回收。默认每小时满核 70% 占空比自旋 5 分钟，
  每周活跃 ≈ 8.4h+（判据要求活跃 > 8.4h 才脱险），叠加其他阶段的系统开销留有余量。
- **网络**：利用率 < 10% → 闲置。默认每轮从多个源下载 700MB±20%。
- **内存**（仅 A1/ARM）：利用率 < 10% → 闲置。默认每轮触碰并持有总内存的 30%，
  时长与 CPU 自旋相同。

## 单轮流程

```
上一轮结束 → 随机间隔 48-72 分钟后触发 → 随机 jitter 0-2min → 采样宿主机负载
  ├─ CPU% ≥ 40% 或 load1 ≥ 0.8×核数 → 宿主机忙，本轮跳过（业务负载本身就是保活）
  └─ 空闲 → 两块盘各建临时目录 run-* → 并发执行（各量带随机抖动）：
       ├─ CPU 自旋      全核 × 70% 占空比 × 3.5-7 分钟（30s 爬坡/回落，±30% 时长抖动）
       ├─ 内存触碰      30% 总内存，保持时长同本轮 CPU 自旋（保留 25% 余量，不足缩量/跳过）
       ├─ 多源下载      560-840MB，6 个源洗牌轮换，单源 ≤250MB，随机 Range 偏移+分块，间歇 0.5-5s
       └─ 随机写盘      205-307MB 分摊到两盘（下载全失败时也保证双盘有 I/O）
     → 汇总日志 → 随机间隔后进入下一轮
     → 默认保留模式：文件留在 run 目录（占盘作为持续数据）；任一盘占用率 ≥40%
       时下轮开始前清空保留目录（`DISK_RETAIN_FILES=false` 恢复用完即删）
```

### 反指纹（时间与行为随机化）

回收判定不透明，所以默认配置不留下任何可预测的模式：

| 维度 | 随机化 |
|------|--------|
| 轮次间隔 | 48–72 分钟均匀随机（与时钟整点、小时边界都不对齐；偶尔一小时两轮或空一轮） |
| 轮内延迟 | 触发后再随机 0–2 分钟 |
| CPU 时长 | 配置值的 70%–140%，且带 30 秒线性爬坡/回落（不是方波） |
| 下载预算 | ±20% 抖动；分块大小取容量 40%–100% 随机 |
| 下载字节区间 | 固定源随机 Range 起始偏移（同一文件每轮取不同段，像断点续传） |
| 下载节奏 | 源序洗牌 + 请求间歇 0.5–5 秒随机 |
| 写盘量 | ±20% 抖动 |

- 下载源：Cloudflare 测速端点、nodejs.org 官方 tarball、GitHub codeload（Go 源码包）、
  npm registry（typescript 包）、Alpine CDN（minirootfs）、OVH 测速文件——都是真实软件
  分发产物/官方测速端点，每轮随机洗牌轮转，不怼单一站点。
- 临时文件即用即删；进程被 SIGKILL 杀掉的残留目录由下一轮启动时清扫（>26h 的 `run-*`）。

## 部署（Docker）

前置：宿主机 `/data` 是数据盘挂载点（不同的话在 `.env` 里设 `DATA_DIR`）。

```bash
cd oracle-keeper
cp .env.example .env          # 按需修改，全部变量都有默认值

# A1 (ARM64) 机器上构建并启动
docker compose up -d --build

# 部署后先冒烟：跑一轮看日志（会立即执行；后台调度仍是随机间隔）
docker compose exec oracle-keeper /app/oracle-keeper once
docker compose logs -f oracle-keeper
```

x86 实例同样可用（构建时自动交叉编译，无需改 Dockerfile）。国内网络构建慢时在 `.env`
设置 `GOPROXY=https://goproxy.cn,direct`。

### 部署约束（重要）

| 约束 | 原因 |
|------|------|
| **不要加 `mem_limit`** | 内存组件按宿主机总内存比例分配；容器限额会导致过量分配 → OOMKill |
| **不要加 `cpus` 限制** | CPU 限额直接压低保活效果 |
| 需要 root uid 写挂载目录 | 两个宿主机临时目录由 Docker 以 root 创建；compose 已 `cap_drop: [ALL]` + `no-new-privileges`，容器只剩"写两个目录 + 出网"的能力 |
| `/proc` 需为宿主机视角 | 标准 Docker 满足；若宿主机装了 lxcfs 虚拟化 /proc，忙检测会退化为容器视角，需去掉 lxcfs 或改用 host network |

## 配置

优先级：环境变量（前缀 `ORACLE_KEEPER_`）> 配置文件 > 默认值。完整示例见
[config.example.yaml](config.example.yaml) 与 [.env.example](.env.example)。

| 环境变量 | 默认 | 说明 |
|----------|------|------|
| `ORACLE_KEEPER_SCHEDULE_SPEC` | 空 | 空 = 随机间隔模式（推荐）；填 cron 表达式则固定调度 |
| `ORACLE_KEEPER_SCHEDULE_INTERVAL_MIN` | `48m` | 随机间隔下限 |
| `ORACLE_KEEPER_SCHEDULE_INTERVAL_MAX` | `72m` | 随机间隔上限（平均约一小时一轮） |
| `ORACLE_KEEPER_SCHEDULE_JITTER_MINUTES` | `2` | 触发后随机延迟上限（分钟） |
| `ORACLE_KEEPER_SCHEDULE_MAX_RUN_DURATION` | `45m` | 单轮硬超时 |
| `ORACLE_KEEPER_BUSY_CPU_PERCENT` | `40` | 采样 CPU ≥ 此值 → 跳过本轮 |
| `ORACLE_KEEPER_BUSY_LOAD_FACTOR` | `0.8` | load1 ≥ 此值×核数 → 跳过本轮 |
| `ORACLE_KEEPER_CPU_BURN_DURATION` | `5m` | CPU 自旋时长 |
| `ORACLE_KEEPER_CPU_CORES` | `0` | 参与自旋核数，0=全部 |
| `ORACLE_KEEPER_CPU_DUTY_CYCLE` | `0.7` | 自旋占空比 (0,1] |
| `ORACLE_KEEPER_MEM_ALLOC_PERCENT` | `30` | 触碰内存占总内存 %，0 关闭 |
| `ORACLE_KEEPER_MEM_MIN_FREE_PERCENT` | `25` | 内存安全余量 % |
| `ORACLE_KEEPER_NET_TOTAL_MB` | `700` | 每轮下载预算 MB（±20% 抖动），0 关闭 |
| `ORACLE_KEEPER_NET_MAX_PER_HOST_MB` | `250` | 单源单轮上限 MB |
| `ORACLE_KEEPER_NET_REQUEST_TIMEOUT` | `5m` | 单请求超时 |
| `ORACLE_KEEPER_DISK_ROOTS` | `/var/tmp/oracle-keeper,/data/oracle-keeper` | 双盘临时根目录 |
| `ORACLE_KEEPER_DISK_WRITE_MB` | `256` | 额外写盘量 MB，0 关闭 |
| `ORACLE_KEEPER_DISK_MIN_FREE_MB` | `2048` | 盘空闲低于此值跳过该盘 |
| `ORACLE_KEEPER_DISK_RETAIN_FILES` | `true` | 保留模式：文件轮末不删除，高水位才清理 |
| `ORACLE_KEEPER_DISK_PURGE_PERCENT` | `40` | 任一盘占用率 ≥ 此值时，下轮开始前清空保留的 run 目录 |

调参建议：

- 担心 CPU 判据余量不足 → 调大 `CPU_BURN_DURATION`（随机抖动按比例跟着放大）。
- 流量配额紧张 → 调小 `NET_TOTAL_MB`（e.g. 300），保底靠 CPU/内存阶段。
- 想让节奏更碎/更散 → 收窄 `INTERVAL_MIN/MAX`（如 30–90m）或加大 `JITTER_MINUTES`。
- 需要确定性调度（如只在夜间） → 设 `SCHEDULE_SPEC`（如 `"0 2-7 * * *"`），间隔配置被忽略。
- 想恢复"用完即删" → `DISK_RETAIN_FILES=false`（Oracle 官方闲置判据不含存储项，保留
  文件只是让实例持续持有数据的额外保险；保留模式由高水位 `DISK_PURGE_PERCENT` 兜底，
  不会写满 200GB 免费块存储额度）。

## CI/CD（GitHub Actions 自动部署）

[.github/workflows/deploy.yml](.github/workflows/deploy.yml)：push 到 `main`（或手动
workflow_dispatch）→ `go test -race` → buildx 构建 `linux/amd64,linux/arm64` 镜像推到
`ghcr.io/servekit/oracle-keeper`（tag `latest` + commit sha）→ SSH 到 Oracle VM 同步
compose 文件、拉新镜像重启。PR 只跑测试，不部署。

一次性准备：

1. Secrets 建在**组织层**（org → Settings → Secrets and variables → Actions →
   New organization secret，Repository access 选 All 或 Selected），全组织仓库共用、
   无需逐仓配置：`ORACLE_HOST`（VM 公网 IP）、`ORACLE_USER`（SSH 用户，需在
   docker 组）、`ORACLE_SSH_KEY`（私钥完整内容）、`ORACLE_SSH_PORT`（可选，默认 22）。
   个别仓库要指向不同机器时，在仓库层配同名 secret 覆盖（仓库 > 组织）。
2. 生成部署专用密钥（一次性；不要复用日常个人密钥）：

   ```bash
   # 本地生成 ED25519 密钥对，passphrase 留空（CI 无法交互输入）
   ssh-keygen -t ed25519 -C "gha-deploy/oracle-keeper" -f ~/.ssh/oracle_keeper_deploy

   # 公钥装到 VM 的 ORACLE_USER 下
   ssh-copy-id -i ~/.ssh/oracle_keeper_deploy.pub <user>@<vm-ip>

   # 验证免密 + docker 权限
   ssh -i ~/.ssh/oracle_keeper_deploy <user>@<vm-ip> 'docker ps >/dev/null && echo ok'

   # 私钥完整内容（含 BEGIN/END 行）拷去填 ORACLE_SSH_KEY secret
   pbcopy < ~/.ssh/oracle_keeper_deploy   # macOS；Linux 用 xclip 或 cat 后手动复制
   ```

3. VM 上：SSH 用户 `sudo usermod -aG docker <user>`。ghcr 包保持 Private 即可——
   镜像由 runner 拉取后 `docker save | ssh docker load` 直送 VM，VM 不需要任何
   registry 凭证。
4. VM 安全组放行 SSH 端口（GitHub 托管 runner 出口 IP 段很广，无法精确白名单，
   建议直接对 0.0.0.0 放行 22 并依赖密钥认证，或改用自托管 runner）。

首次部署会在 VM 的 `~/oracle-keeper` 生成 `.env`（从 `.env.example` 复制），之后
不会覆盖——改 VM 侧配置直接编辑该文件，改完 `docker compose up -d` 生效。

## 本地开发

```bash
make test      # 单测 + 冒烟（-race）
make lint      # golangci-lint
make once      # 本机跑一轮（临时目录按 config.yaml/环境变量）
```

代码结构（详见 [设计文档](../specs/2026-09-10-oracle-keeper-design.md)）：

```
cmd/keeper/          入口：serve 常驻 / once 单轮
internal/app/        cronx 调度 + 生命周期 (signalx)
internal/keeper/     引擎：busy / cpu / mem / net / disk / keeper 编排
pkg/config/          configx 配置结构与校验
```

## 常见问题

- **为什么平均每小时活跃几分钟就够？** 判据是"95% 的**时间**低于 10%"，不是平均利用率。
  默认平均每小时约 5 分钟满核（3.5–7 分钟随机）= 每周 ~8.4h 活跃，正好越过 5% 闲置线，
  网络/磁盘阶段还额外贡献。
- **启动后第一轮什么时候跑？** 随机间隔模式下，启动后 48–72 分钟内随机；要立即验证用
  `docker compose exec oracle-keeper /app/oracle-keeper once`。
- **日志里 `net_failures` 高？** 个别源偶发 403/超时是正常的（轮换机制会跳过它继续）；
  持续全失败检查实例出网或 DNS。
- **想加自己的下载源？** 配置 `net.sources: ["my-mirror=https://example.com/big.bin"]`
  （URL 含 `%d` 则按请求字节数参数化）。

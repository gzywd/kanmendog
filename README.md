# 看门狗 KanmenDog v1.9.0

> **零误杀 NAS 看门狗 —— 死机自动重启，正常使用/升级/重启绝不误判**

[![Version](https://img.shields.io/badge/version-1.9.0-blue.svg)](https://github.com/gzywd/kanmendog/releases/tag/v1.9.0)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

## ✨ v1.9.0 更新（质量审计修复）

> 本次更新集中修复了一轮质量审计中发现的高危与体验问题，核心目标仍是「零误杀」。

| 类别 | 关键修复 |
|---|---|
| 体验 | 页面「参数保存不生效」修复：自动刷新只更新运行指标，不再覆盖你正在编辑的参数表单 |
| 防误杀 | **D 状态检查默认关闭**（mdadm scrub / rsync / 虚拟机直通易触发 D 状态计数误杀）；死机判定改为「≥2 个独立检查族同时失败」才计一次，单检查超时按弃权处理 |
| 兜底可靠 | `triggerReboot` 硬件兜底重写：保持 fd 打开 + 停喂狗，按 `reboot` 阶梯兜底，不再盲目 `close` 导致 nowayout 下失活；维护窗口最长 60 分钟自动退场 |
| 健壮性 | 全 goroutine 加 `recover()` 防 panic 致死；日志按分钟轮转；修复数据竞争（`go test -race` 确认） |
| 安全 | 接口绑定 `127.0.0.1` + 同源/CSRF 校验 + 探针命令白名单，堵住无鉴权与任意 root 命令注入 |
| 接线 | 安装向导补回 Web 探针开关；版本号全局统一；二进制剥离调试符号 |

## ✨ v1.3.0 更新（产品视角闭环）

### 三大目标达成度

| 目标 | v1.2.0 | v1.3.0 | 说明 |
|---|---|---|---|
| ① 异常自动重启 | ★★★★☆ | ★★★★★ | 探针知情决策 + 学习期，覆盖不再静默缺失 |
| ② 不误报 | ★★★★★ | ★★★★★ | 维护模式按钮给用户最终控制权 |
| ③ 可完整追溯 | ★★★☆☆ | ★★★★☆ | 开机来源标注 + 两段式落盘，硬件复位盲区已文档化 |

### 新增能力

1. **开机重启来源标注** — 每次开机自动判断重启来源（看门狗判定/硬件复位/内核 panic/正常关机/未知），页面顶部醒目展示。**清除旧原因误导**：正常关机/重启后自动清空上次死机原因。
2. **手动维护模式** — 页面一键进入 30 分钟维护窗口（可取消），升级/大文件操作前主动启用，期间只喂狗不判定。
3. **探针学习期** — 开启探针后前 3 次失败不计入判定，适应开机/升级后 Web 服务慢启动。
4. **端口迁移检测** — 探针命令中的端口与配置端口不一致时，页面醒目警告。
5. **安装向导探针选择** — 安装时弹出 checkbox 让用户知情决定是否开启 Web 探针（预填自动探测的端口）。
6. **两段式 triggerReboot** — 先毫秒级写入"时间+原因"轻量文件，再收集完整快照，防濒死系统 IO 卡死导致证据全丢。
7. **维护窗口退出时连续失败清零** — 避免升级结束瞬间残留失败计数误触发。

## 核心设计原则

> **宁可 20 分钟才发现异常，绝不误判重启一次。**

## 工作原理

```
┌─────────────────────────────────────────────────────┐
│                  三层复位架构                        │
│                                                     │
│  Layer 1: 用户态健康检查（每 10 秒）                 │
│    ├── fork 存活、内存可用率                         │
│    ├── D 状态进程（默认关闭，易误杀）               │
│    ├── 系统负载（默认关闭）                          │
│    └── Web 服务探针（默认关闭，用户知情选择）        │
│    ↓ 需 ≥2 个独立检查族同时失败才计一次（防误判）   │
│  Layer 2: 内核 lockup → panic → reboot（保守配置） │
│    ├── hardlockup_panic=1（NMI 级硬锁死）           │
│    └── softlockup_panic=0（防高负载假阳性）         │
│    ↓ panic 后 10 秒自动重启                         │
│  Layer 3: 硬件看门狗（60 秒超时 → 硬复位）          │
│    └── 仅当存在【真实硬件看门狗】时，才能覆盖        │
│       全核冻结；softdog（软件）内核冻结时同样不递减， │
│       无法硬复位——届时仅 Layer 2 兜底，仍可能卡死。  │
└─────────────────────────────────────────────────────┘
```

> **诚实声明（重要）**：三层架构中，**真正能覆盖「进程全卡住 + 内核完全冻结」的只有真实硬件看门狗**（重启后页面会标注来源）。若机器只有 `softdog`（软件看门狗），内核冻结时它自身也不再递减，无法触发复位；此时最终兜底退化为 Layer 2（内核 panic→reboot），而 panic 本身在极端冻结下也可能不发生。无硬件看门狗的机器建议额外准备带外断电（智能插座/ESP32）以覆盖全核冻结。

## 零误杀保障机制

| 场景 | 保护机制 | 效果 |
|---|---|---|
| 正常停止应用 | SIGTERM handler → magic close 解除看门狗 | 停止后不会被硬复位 |
| 正常重启/关机 | `/run/systemd/shutdown` 检测 + SIGTERM handler | 关机路径安全 |
| fnOS 系统升级 | dpkg fcntl 锁 + 升级进程检测 + 手动维护模式 | 升级中不判定 |
| 开机启动阶段 | `boot_grace_min=5` 冷却期 | 启动高负载不误判 |
| 高负载/转码/scrub | 负载检查默认关 + 3 分钟阈值 + 内存 2% | 瞬时不误判 |
| 单次网络抖动 | 失败期间持续喂狗 + 3 分钟连续阈值 | 抖动不启动硬件倒计时 |

## 安装

### 方式一：飞牛应用中心手动安装（推荐）

1. 下载对应版本的 `.fpk` 文件（以 [Release v1.9.0](https://github.com/gzywd/kanmendog/releases/tag/v1.9.0) 为例）：
   `https://github.com/gzywd/kanmendog/releases/download/v1.9.0/com.gzywd.kanmendog-v1.9.0.fpk`
2. 飞牛应用中心 → 手动安装 → 选择 fpk
3. 安装向导会询问是否启用 Web 探针（已自动探测端口）
4. 打开应用页面确认状态

### 方式二：命令行安装

```bash
# 下载 v1.9.0 release 包（文件名含版本号）
wget https://github.com/gzywd/kanmendog/releases/download/v1.9.0/com.gzywd.kanmendog-v1.9.0.fpk

# 通过 fnOS 命令安装（需要 fnOS 环境）
fnos install com.gzywd.kanmendog-v1.9.0.fpk
```

## 配置说明

### 基础参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `interval_sec` | 10 | 健康检查周期（秒） |
| `watchdog_timeout_sec` | 60 | 硬件看门狗超时（秒） |
| `fail_threshold` | 18 | 连续失败次数阈值（18×10s=3 分钟） |
| `boot_grace_min` | 5 | 开机冷却期（分钟），期间不判定 |
| `auto_reboot` | true | 判定死机后自动重启 |

### 检查项

| 检查项 | 默认 | 阈值 | 说明 |
|---|---|---|---|
| fork 存活 | ✓ 开 | — | 子进程存活检测 |
| 系统负载 | ✗ 关 | 16 | 高负载≠死机，建议保持关闭 |
| D 状态进程 | ✗ 关 | 30 | 不可中断睡眠进程数；mdadm scrub/rsync/虚拟机直通易误杀，v1.9.0 起默认关闭 |
| 内存可用率 | ✓ 开 | 2% | 防 OOM 死机 |
| Web 服务探针 | ✗ 关（向导选择） | — | 需确认端口正确（默认 5666） |

### 重要提示

⚠️ **Web 探针端口必须是飞牛真实管理端口！**
- 新版 fnOS 默认：HTTP **5666** / HTTPS **5667**
- 旧版 fnOS：HTTP **8000** / HTTPS **8001**
- **80 端口只是可选重定向，可能未开启！**

## API 接口

| 接口 | 方法 | 说明 |
|---|---|---|
| `/api/status` | GET | 运行状态（含开机来源、维护模式、学习期等） |
| `/api/config` | GET/POST | 读取/保存配置 |
| `/api/toggle` | POST | 开关监控 |
| `/api/maintenance?minutes=N` | POST | 进入/取消维护模式（0=取消） |
| `/api/service?action=X` | POST | 服务控制（start/stop/restart） |
| `/api/logs?lines=N` | GET | 读取日志 |

## 日志与取证

- 运行日志：`/var/lib/kanmendog/kanmendog.log`
- 死机快照：`/var/lib/kanmendog/snapshots/`（时间戳目录）
- OOM 日志：`/var/lib/kanmendog/oom_logs/`（时间戳文件）
- 重启原因：`/var/lib/kanmendog/reboot_reason.txt`（两段式：先写原因再补快照）
- 内核参数备份：`/etc/kanmendog/sysctl_backup`（卸载时恢复原值）

## 开发

```bash
# 克隆仓库
git clone https://github.com/gzywd/kanmendog.git
cd kanmendog

# 编译
cd src/kanmendog
go build -o ../../app/kanmendog .

# 打包
bash scripts/build.sh
```

## 版本历史

### v1.9.0 — 质量审计修复
- 页面「参数保存不生效」根因修复：自动刷新不再覆盖正在编辑的参数表单
- D 状态检查默认关闭（防 mdadm scrub/rsync/虚拟机直通误杀）
- 死机判定改为「≥2 个独立检查族同时失败」才计一次，单检查超时按弃权处理
- triggerReboot 硬件兜底重写（保持 fd + 停喂狗 + reboot 阶梯），不再盲目 close
- 维护窗口最长 60 分钟自动退场，杜绝常驻进程锁死后永久静默
- 全 goroutine 加 recover()；日志按分钟轮转；修复数据竞争（go test -race 确认）
- 接口绑定 127.0.0.1 + 同源/CSRF 校验 + 探针命令白名单（堵无鉴权与命令注入）
- 安装向导补回 Web 探针开关；版本号全局统一；二进制剥离调试符号

### v1.3.0 — 产品闭环
- 开机重启来源标注（wtmp/journalctl 分类）
- 手动维护模式（API + 页面按钮 + 自动恢复）
- 探针首探学习期（前 3 次失败宽容）
- 端口迁移检测与警告
- 安装向导探针知情选择（wizard checkbox）
- triggerReboot 两段式落盘（先原因后快照）
- 维护窗口退出时连续失败清零
- trend 日志滚动（固定 200 条）

### v1.2.0 — 零误杀重构
- SIGTERM 优雅退出（magic close 安全解除看门狗）
- 维护窗口检测（dpkg 锁 / 升级进程 / systemd shutdown）
- 探针默认关闭 + 端口自动探测
- sysctl 保守化（softlockup_panic=0）
- 保守默认值（3 分钟阈值、内存 2%、负载默认关）
- 喂狗与判定解耦（失败期间持续喂狗）
- 配置字段级合并防丢失

### v1.1.0 — 初始版本
- 三层复位架构（用户态 + 内核 lockup + 硬件看门狗）
- 多维度健康检查
- 死机快照与 OOM 取证
- Web 管理界面

## License

MIT License

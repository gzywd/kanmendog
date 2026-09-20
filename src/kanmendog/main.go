// 看门狗 KanmenDog —— fnOS 死机自动重启守护进程
//
// v1.8.1 严重 Bug 修复 + 增强：（1）修复 ui/config 未声明 port 导致 fnOS 回退到飞牛默认端口 5666、打开时弹出飞牛桌面的严重 bug——现正确声明 port=8900，fnOS 直连应用自身 Web 服务；Go 路由同时兼容 /apps/{appname}/main/ 反代前缀，飞牛 App 远程亦可打开；（2）UI 强化看门狗类型显示：明确区分硬件看门狗（内核冻结可硬复位，真兜底）/软件看门狗 softdog（内核冻结无法复位，无真兜底）/无设备，并展示 nowayout 锁定状态。
// v1.8.0 网关接入 + 修复：（1）接入飞牛网关——ui/config 不声明 port（此方案在部分 fnOS 版本会回退 5666，v1.8.1 已纠正为显式声明 port）；（2）修复自定义端口保存 bug。
//
// v1.7.2 体验优化：（1）关机检测加运行时间过滤，防止 /run/systemd/shutdown 残留导致误报；（2）探针端口选择器支持自定义端口输入（如 16666），自动生成 curl 命令。
// v1.7.1 Bug 修复：修复 querySelector CSS 选择器语法错误（属性选择器需 [value="..."] 格式）
// v1.7.0 Bug 修复（输入框 Enter 键防刷新 + 探针命令按 Enter 自动保存）：
//   - 全局拦截 input/select 的 Enter 键，防止浏览器隐式表单提交导致页面刷新
//   - 探针命令输入框按 Enter 时自动触发保存（便利操作）
//
// v1.6.0 规范修复（卸载流程符合 fnOS 官方规范）：
//   - 【规范】卸载流程重构：uninstall_init 优雅停进程（magic close 看门狗），
//     uninstall_callback 仅负责清理单元/内核参数/条件删数据。
//   - 【规范】wizard/uninstall 新增"删除应用数据"开关（uninstall_purge_data），
//     用户在卸载向导中直接选择，符合官方"在 wizard 中收集选择"的推荐做法。
//   - 【规范】manifest 新增 service_port=8900 声明。
//   - 【工程】构建脚本增加自动编译步骤，杜绝"旧二进制被打包"的问题。
//
// v1.5.0 体验修复（默认值一致性 + 重启来源友好化 + 探针 UX 重构）：
//   - 【体验】classifyBoot 无法判定时（wtmp/last 不可用）默认为"正常关机后开机"
//     而非显示吓人的"未知" —— fnOS 绝大多数重启是用户主动操作，
//     缺乏证据时应假设正常而非异常（证据优先原则）。
//   - 【体验】UI 探针配置重构：新增端口选择器（自动区分 HTTP/HTTPS），
//     用户选端口即自动生成 curl 命令，不再需要手写完整命令。
//   - 【体验】bootUnknown 标题文案改为友好提示（不再出现"未知"字样）。
//
// v1.4.0 修复（安装后三大失效问题）：
//   - 【致命】loadOrInitConfig 改为合并模式：旧版/不完整 config.json 缺失的新字段
//     不再被零值覆盖，彻底解决"安装后所有设定无默认值"的问题。
//   - 【严重】UI 全部 async 函数加 try-catch 错误处理：任一 API 失败不再静默崩溃，
//     用户能看到明确错误提示（而非配置全空/日志空白/按钮无反应）。
//   - 【严重】handleService 兼容飞牛环境：检测 systemctl 可用性，不可用时提供
//     明确降级方案和错误信息（原行为：静默失败，按钮点了完全没反应）。
//   - 【改进】启动顺序优化：先启动 HTTP server（用户立即看到页面），
//     再做 classifyBoot/ensureWatchdog 等耗时初始化（防初始化阻塞导致 API 超时）。
//   - 【改进】日志 tailLines 增加磁盘文件回退：内存缓冲区为空时从文件读取
//     （解决"日志未加载"——刚启动时内存缓冲区几乎为空）。
//   - 【改进】新增 /api/health 诊断端点：返回配置路径、文件存在性、看门狗状态、
//     环境变量、systemctl 可用性等关键诊断信息。
//   - 【严重】auto_reboot=false 时不再关闭看门狗 fd（原行为导致 60s 后硬复位，
//     auto_reboot 开关形同虚设）；现仅记录+告警，主循环继续喂狗。
//   - 【严重】status API 补充缺失字段：maintain_until, probe_fail_count,
//     port_migrate_detected —— 修复维护倒计时/学习期提示/端口迁移警告三个 UI 功能
//     因字段名不匹配而完全不可用的 bug。
//   - 【中危】triggerReboot 释放 mutex 前捕获 WatchdogTimeoutSec 到局部变量，
//     消除释放锁后访问 d.cfg 的数据竞争。
//   - 探针失败计数 probeFailCount 现在正确递增/归零，UI 学习期进度条可工作。
//
// v1.3.1 修复（致命缺陷+硬件信息增强）：
//   - 【致命】修复 triggerReboot 返回后主循环继续喂狗导致重启失效的 bug：
//     原 triggerReboot 设 monitoring=false 但 petWatchdog() 只检查 wd!=nil，
//     返回后 failCount 被归零、下一周期继续喂狗，若 reboot 系统调用失败则
//     系统永远不会被重启。现改为：决定重启后设 wd=nil（彻底停止喂狗），
//     若所有重启方式均失败则阻塞不返回，等待硬件看门狗兜底复位。
//   - 【高危】probeCandidatePorts 加总超时（5s），防主循环阻塞导致喂狗延迟。
//   - 【改进】状态 API 新增硬件信息字段（内核版本/CPU/总内存/nowayout）。
//   - 【改进】启动时完整记录硬件环境到日志；配置值合法性校验。
//   - 【改进】fpk 文件名带版本号。
//
// v1.3.0 新增（信任闭环三件套 + 防误报加固）：
//   - 开机自检"重启来源标注"：每次开机用 wtmp/journalctl 分类本次重启来源
//     （本程序判定 / 内核 panic / 异常复位 / 正常关机），写入 boot_reason 并在
//     状态页展示 —— 消除"硬件复位后旧 last_reboot_reason 误导排查"的盲区。
//   - 手动维护模式：一键暂停死机判定 N 小时（继续喂狗），到期自动恢复，
//     给飞牛升级/手工维护一个不依赖自动检测的保底开关。
//   - triggerReboot 两段式落盘：先毫秒级写入"时间+原因"最小事实，再收集完整
//     快照追加 —— 濒死系统上快照收集卡死也保得住证据。
//   - 探针首探学习期：探针从未成功过之前，失败不计入判定（防配置错误误杀）；
//     探针持续失败过半时自动检测"飞牛端口迁移"，发现端口变更只告警清零不重启。
//   - 维护窗口退出时清零失败计数（窗口前积累的历史样本作废）。
//   - 趋势日志按大小滚动；暂停监控每日提醒一次。
//
// v1.2.0 设计哲学（零误杀优先，继续有效）：
//
//	宁可 3 分钟后发现异常，绝不把正常使用/升级/重启误判成死机。
//	- 停止服务/正常关机重启先 magic close 解除看门狗（SIGTERM 处理器）。
//	- 升级（dpkg 锁/升级进程）/关机流程/开机冷却期/手动维护模式：只喂狗不判定。
//	- 判定与喂狗解耦：失败期间持续喂狗，达阈值决定重启才停喂。
//	- 默认 18 个周期（3 分钟）连续异常才判定；负载检查默认关；内存阈值 2%。
package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

//go:embed ui/index.html ui/config ui/images
var uiFS embed.FS

const (
	appName        = "com.gzywd.kanmendog"
	appVersion     = "1.9.0"
	defaultPort    = 8900
	watchdogDevice = "/dev/watchdog"
	logMaxBytes    = 5 * 1024 * 1024  // 日志滚动阈值
	trendMaxBytes  = 20 * 1024 * 1024 // 趋势 csv 滚动阈值
	// 维护窗口连续占用上限：超过则强制退出维护状态并告警，防止被常驻进程永久锁死
	maxBusyDuration = 60 * time.Minute
	// 探针学习期最大宽容次数：前 N 次失败不计入判定（有退出条件，避免静默废掉探针）
	probeLearnLimit = 3
)

// 看门狗 ioctl 编号（'W' = 0x57）
const (
	wdIOCGetSupport = 0x80285700 // _IOR('W',0,struct watchdog_info)
	wdOptMagicClose = 0x08       // WDIOF_MAGICCLOSE：close 时若未写 'V' 则保持看门狗运行
	wdOptNoWayOut   = 0x0020     // WDIOF_NO_WAY_OUT：nowayout，close 也停不下来
)

// watchdogInfo 对应内核 struct watchdog_info
type watchdogInfo struct {
	Options  uint32
	Firmware uint32
	Identity [32]byte
}

// Config 应用配置，持久化到 TRIM_PKGETC/config.json
type Config struct {
	Enabled               bool    `json:"enabled"`                 // 监控总开关（网页一键开关）
	IntervalSec           int     `json:"interval_sec"`            // 健康检查/喂狗周期
	WatchdogTimeoutSec    int     `json:"watchdog_timeout_sec"`    // 看门狗硬件超时（ioctl 设置，非所有驱动支持）
	FailThreshold         int     `json:"fail_threshold"`          // 连续失败多少次判定死机（默认18=3分钟）
	Port                  int     `json:"port"`                    // Web 服务端口
	AutoReboot            bool    `json:"auto_reboot"`             // 判定死机后是否自动重启
	BootGraceMin          int     `json:"boot_grace_min"`          // 开机冷却期（分钟）：期间只喂狗不判定
	CheckFork             bool    `json:"check_fork"`              // fork 存活测试（零误报）
	CheckLoad             bool    `json:"check_load"`              // 负载测试（高负载≠死机，默认关）
	LoadThreshold         float64 `json:"load_threshold"`          // 1 分钟负载均值阈值
	CheckDState           bool    `json:"check_dstate"`            // 不可中断(D)进程数测试
	DStateThreshold       int     `json:"dstate_threshold"`        // D 状态进程数阈值
	CheckMemory           bool    `json:"check_memory"`            // 内存可用率测试（针对 OOM 死机）
	MemAvailableThreshold int     `json:"mem_available_threshold"` // 可用内存低于该百分比(%)判定异常
	CheckServiceProbe     bool    `json:"check_service_probe"`     // 外部命令探针（默认关！端口须与飞牛一致）
	ServiceProbeCmd       string  `json:"service_probe_cmd"`       // 必须返回 0 的命令
	TrendInterval         int     `json:"trend_interval"`          // 趋势日志间隔（周期数，0=关闭）
	MaintainUntil         int64   `json:"maintain_until"`          // 手动维护模式截止时刻（unix 秒，0=无；期间只喂狗不判定）
}

// defaultConfig 保守默认值：宁可晚 3 分钟发现死机，绝不误杀正常负载。
// 注意：探针默认关闭 —— fnOS 真实管理端口因机器而异（新版默认 5666，旧版 8000/8001，
// 80 仅是可选的重定向）。安装向导会让用户知情勾选，安装脚本探测端口后预填命令。
func defaultConfig() Config {
	return Config{
		Enabled:            true,
		IntervalSec:        10,
		WatchdogTimeoutSec: 60,
		FailThreshold:      18, // 10s 周期 × 18 = 3 分钟连续异常才判定
		Port:               defaultPort,
		AutoReboot:         true,
		BootGraceMin:       5, // 开机 5 分钟内只喂狗不判定
		CheckFork:          true,
		CheckLoad:          false, // 高负载不等于死机，默认关闭
		LoadThreshold:      16,
		// v1.9.0：D 状态检查默认关闭。D 状态进程数在 mdadm scrub / 大批量 rsync /
		// Docker·VM 存储透传时常态性偏高并持续数分钟——这正是 README 强调要规避的
		// "高负载≠死机"场景。它比负载检查更容易误杀，故与负载检查一致默认关闭，
		// 由用户在知情后手动开启。若开启，仍受"≥2 独立检查族同时失败才计数"保护。
		CheckDState:           false,
		DStateThreshold:       30,
		CheckMemory:           true,
		MemAvailableThreshold: 2, // page cache 可回收；2% 持续 3 分钟才是真耗尽
		CheckServiceProbe:     false,
		ServiceProbeCmd:       "", // 安装脚本探测端口后预填
		TrendInterval:         5,
		MaintainUntil:         0,
	}
}

// ---- 路径（遵循飞牛 TRIM_ 环境变量，缺失时本地测试兜底）----

func envOr(env, fallback string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return fallback
}

func (c *Config) etcDir() string { return envOr("TRIM_PKGETC", "/etc/kanmendog") }
func (c *Config) varDir() string { return envOr("TRIM_PKGVAR", "/var/lib/kanmendog") }
func (c *Config) configPath() string {
	return filepath.Join(c.etcDir(), "config.json")
}
func (c *Config) logPath() string {
	return filepath.Join(c.varDir(), "kanmendog.log")
}
func (c *Config) reasonPath() string {
	return filepath.Join(c.varDir(), "last_reboot_reason")
}
func (c *Config) trendPath() string {
	return filepath.Join(c.varDir(), "trend.csv")
}
func (c *Config) bootReasonPath() string {
	return filepath.Join(c.varDir(), "boot_reason")
}
func (c *Config) lastBootIDPath() string {
	return filepath.Join(c.varDir(), "last_boot_id")
}

// readLastBootID 读上次运行持久化的 boot_id 基线（用于判别"系统是否真的重启过"）。
func (c *Config) readLastBootID() string {
	if b, err := os.ReadFile(c.lastBootIDPath()); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

// writeLastBootID 持久化当前 boot_id，作为下次运行判断重启的基线。
func (c *Config) writeLastBootID(id string) {
	_ = os.WriteFile(c.lastBootIDPath(), []byte(id), 0o644)
}

// ---- 日志 ----

type logger struct {
	mu   sync.Mutex
	f    *os.File
	path string
	buf  *strings.Builder // 最近日志内存副本，供网页实时查看
}

func newLogger(path string) *logger {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		// 日志打不开也不能崩，退化到 stderr
		f = os.Stderr
	}
	return &logger{f: f, path: path, buf: &strings.Builder{}}
}

func (l *logger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, err := l.f.Write(p)
	if l.buf.Len() < 200*1024 {
		l.buf.Write(p)
	} else {
		// 丢弃旧内容，保留尾部
		s := l.buf.String()
		l.buf.Reset()
		l.buf.WriteString(s[len(s)-100*1024:])
		l.buf.Write(p)
	}
	return n, err
}

// tailLines 返回最近 n 行日志；优先从内存缓冲区读（实时），
// 缓冲区为空或行数不足时从磁盘文件补充（覆盖进程重启后/刚启动时内存无历史的问题）。
func (l *logger) tailLines(n int) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.buf.String()
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	// 内存缓冲区足够：直接返回
	if len(lines) >= n {
		return strings.Join(lines[len(lines)-n:], "\n")
	}
	// 内存不足：尝试从磁盘文件读取更多
	var fileLines []string
	if fb, err := os.ReadFile(l.path); err == nil {
		fileLines = strings.Split(strings.TrimRight(string(fb), "\n"), "\n")
	}
	if len(fileLines) > len(lines) {
		combined := append(fileLines, lines...)
		if len(combined) > n {
			combined = combined[len(combined)-n:]
		}
		return strings.Join(combined, "\n")
	}
	if len(lines) > 0 && lines[0] != "" {
		return strings.Join(lines, "\n")
	}
	return "(暂无日志)"
}

func (l *logger) Rotate() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if fi, err := l.f.Stat(); err == nil && fi.Size() > logMaxBytes {
		l.f.Close()
		// 保留一个备份
		_ = os.Rename(l.path, l.path+".1")
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err == nil {
			l.f = f
		}
	}
}

var gLog *logger

func logf(format string, args ...any) {
	gLog.Write([]byte(fmt.Sprintf("[%s] %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))))
}

// throttleKey 限频日志：同 key 在 every 内最多记录一条，避免刷屏
var throttleMu sync.Mutex
var throttleLast = map[string]time.Time{}

func logThrottledEvery(key string, every time.Duration, msg string) {
	throttleMu.Lock()
	defer throttleMu.Unlock()
	if t, ok := throttleLast[key]; ok && time.Since(t) < every {
		return
	}
	throttleLast[key] = time.Now()
	logf("%s", msg)
}

func logThrottled(key, msg string) { logThrottledEvery(key, 10*time.Minute, msg) }

func logDaily(key, msg string) { logThrottledEvery(key, 24*time.Hour, msg) }

// ---- 看门狗 ----

type watchdog struct {
	fd     int
	device string
}

// ioctl 编号（'W' = 0x57）
const (
	wdIOCKeepAlive  = 0x5705     // _IO('W',5)
	wdIOCSetTimeout = 0xC0045706 // _IOWR('W',6,sizeof(int))
	wdIOCGetTimeout = 0x80045707 // _IOR('W',7,sizeof(int))
)

func ioctlSetIntPtr(fd int, req uint, val *int) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(unsafe.Pointer(val)))
	if errno != 0 {
		return errno
	}
	return nil
}

// ioctlGetIntVal 读回看门狗驱动实际生效的超时（iTCO 等只支持档位值，写入值可能被调整）
func ioctlGetIntVal(fd int, req uint) (int, error) {
	v := 0
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(unsafe.Pointer(&v)))
	if errno != 0 {
		return 0, errno
	}
	return v, nil
}

var watchdogDevices = []string{"/dev/watchdog", "/dev/watchdog0"}

func openWatchdog(timeoutSec int) (w *watchdog, actualTimeout int, err error) {
	var lastErr error
	for _, dev := range watchdogDevices {
		fd, err := syscall.Open(dev, syscall.O_WRONLY, 0)
		if err != nil {
			lastErr = err
			continue
		}
		w = &watchdog{fd: fd, device: dev}
		if timeoutSec > 0 {
			// 尽力设置超时；不支持的驱动会忽略
			t := timeoutSec
			_ = ioctlSetIntPtr(fd, wdIOCSetTimeout, &t)
		}
		// 回读驱动实际生效的超时（iTCO 部分固件只支持档位值）
		if v, err := ioctlGetIntVal(fd, wdIOCGetTimeout); err == nil && v > 0 {
			actualTimeout = v
		} else {
			actualTimeout = timeoutSec
		}
		// 立即喂一次，确认设备可用
		if err := w.pet(); err != nil {
			syscall.Close(fd)
			w = nil
			lastErr = err
			continue
		}
		logf("看门狗设备已选择: %s", dev)
		return w, actualTimeout, nil
	}
	return nil, 0, fmt.Errorf("打开看门狗失败（已尝试 %v）: %v", watchdogDevices, lastErr)
}

func (w *watchdog) pet() error {
	if w == nil || w.fd < 0 {
		return fmt.Errorf("看门狗未打开")
	}
	_, err := syscall.Write(w.fd, []byte{'1'})
	return err
}

// disarm 通过 magic close 关闭看门狗（仅驱动支持 magic close 且未开启 nowayout 时生效）
func (w *watchdog) disarm() {
	if w == nil || w.fd < 0 {
		return
	}
	_, _ = syscall.Write(w.fd, []byte{'V'})
	_ = syscall.Close(w.fd)
	w.fd = -1
}

// ---- 维护窗口检测（防误杀升级/正常重启，零误杀哲学的核心）----

// busyProcessNames 只列"确定仅在升级期间运行"的进程名，避免匹配常驻服务
// 导致监控永久失效。comm 字段最长 15 字符。
var busyProcessNames = map[string]string{
	"apt":             "Debian/apt 升级",
	"apt-get":         "Debian/apt 升级",
	"dpkg":            "dpkg 包管理",
	"dpkg-deb":        "dpkg 解包",
	"unattended-upgr": "自动安全更新",
	"aptitude":        "aptitude 包管理",
	"synaptic":        "synaptic 包管理",
	"trim_update":     "飞牛系统更新",
	"trim-upgrade":    "飞牛系统升级",
	"trim_upgrade":    "飞牛系统升级",
	"fnos-upgrade":    "飞牛系统升级",
	"fnos_upgrade":    "飞牛系统升级",
	"fnupdate":        "飞牛系统更新",
}

// fnosUpdaterLike 前缀+关键子串匹配飞牛自研升级器（命名未经实机穷举，
// 宁多勿漏：多匹配只会让判定更保守，不会引入误报风险）。
func fnosUpdaterLike(comm string) bool {
	c := strings.ToLower(comm)
	for _, prefix := range []string{"trim_", "trim-", "fnos", "fnnas", "fn_"} {
		if strings.HasPrefix(c, prefix) &&
			(strings.Contains(c, "upd") || strings.Contains(c, "upgr") || strings.Contains(c, "inst")) {
			return true
		}
	}
	return false
}

// systemBusyReason 检测系统是否处于升级/关机/重启等维护窗口。
// 返回非空原因时：继续喂狗（防硬件看门狗打断升级），但绝不判定死机。
// 窗口内若系统真冻结，喂狗进程随之冻结，硬件看门狗自然兜底，两不误。
func systemBusyReason() string {
	// 1) systemd 正在执行关机/重启流程：此时 Web 等服务陆续停止，探针/负载必然异常
	//     但 /run/systemd/shutdown 可能因上次关机中断（断电等）而残留，
	//     所以仅当系统刚启动不久（< 5 分钟）时才视为真正的关机流程
	if fi, err := os.Stat("/run/systemd/shutdown"); err == nil && fi.IsDir() {
		if uptimeMinutes() < 5 {
			return "systemd 正在关机/重启流程中"
		}
		// 运行已超 5 分钟但 shutdown 目录仍在：可能是残留，忽略
	}
	// 2) dpkg 锁被占用（fnOS 基于 Debian，系统升级走 apt/dpkg，锁必然持有）
	for _, lock := range []string{"/var/lib/dpkg/lock-frontend", "/var/lib/dpkg/lock"} {
		if dpkgLockHeld(lock) {
			return "系统软件升级中（dpkg 锁被占用: " + lock + "）"
		}
	}
	// 3) 升级相关进程存在（comm 精确匹配 + 飞牛升级器前缀匹配）
	if kw := processBusyMatch(); kw != "" {
		return kw
	}
	return ""
}

// dpkgLockHeld 用 fcntl 查询（F_GETLK，纯查询无副作用）dpkg 锁是否被占用。
// dpkg 使用 POSIX fcntl 锁而非 flock；用 F_GETLK 而非 F_SETLK 抢锁，避免与 apt 之间的 TOCTOU 竞态。
func dpkgLockHeld(path string) bool {
	fd, err := syscall.Open(path, syscall.O_RDWR, 0)
	if err != nil {
		return false // 文件不存在等：不视为升级中
	}
	defer syscall.Close(fd)
	const seekSet = 0 // SEEK_SET
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: seekSet, Start: 0, Len: 0}
	// F_GETLK 把冲突的锁信息写回 lk；若无冲突，lk.Type 被内核置为 F_UNLCK
	if err := syscall.FcntlFlock(uintptr(fd), syscall.F_GETLK, &lk); err != nil {
		return false
	}
	// Type 仍为 F_WRLCK 表示存在冲突持有者
	return lk.Type == syscall.F_WRLCK
}

// processBusyMatch 扫描 /proc/*/comm，精确匹配升级进程名 + 飞牛升级器前缀匹配
func processBusyMatch() string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return ""
	}
	self := os.Getpid()
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		comm := strings.TrimSpace(string(b))
		if desc, ok := busyProcessNames[comm]; ok {
			return "检测到升级进程（" + desc + "）"
		}
		if fnosUpdaterLike(comm) {
			return "检测到疑似飞牛升级进程（" + comm + "）"
		}
	}
	return ""
}

// uptimeMinutes 系统开机分钟数（用于开机冷却期）
func uptimeMinutes() float64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0 // 读不到视为冷却中：最保守（不判定）
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	sec, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return sec / 60
}

// ---- 开机自检：重启来源标注（v1.3.0 信任闭环核心）----

// bootKind 本次重启来源分类
type bootKind string

const (
	bootApp      bootKind = "app"      // 本程序判定死机触发
	bootPanic    bootKind = "panic"    // 内核 panic（lockup→panic 路径）
	bootAbnormal bootKind = "abnormal" // 无干净关机记录：疑似硬件看门狗复位/断电/冻结
	bootNormal   bootKind = "normal"   // 正常关机后开机
	bootUnknown  bootKind = "unknown"  // 无法判定（last/journalctl 不可用）：默认视为正常
)

var bootKindTitle = map[bootKind]string{
	bootApp:      "本程序判定死机触发重启",
	bootPanic:    "内核 panic 重启（lockup→panic 路径）",
	bootAbnormal: "异常重启（无干净关机记录，疑似硬件看门狗复位/冻结/断电）",
	bootNormal:   "正常关机后开机",
	bootUnknown:  "正常关机后开机（无法读取重启历史，按正常处理）",
}

// parseLastShutdownTime 解析 `last -x -F shutdown` 最新一条 shutdown 记录的时间。
// 兼容 util-linux 的 -F 全时间格式及其后可能附加的 " - crash (00:17)" 等后缀，
// 不再依赖"最后 5 个字段"的脆弱切片（该切法在 crash 后缀下会取到垃圾）。
// 配合 classifyBoot 中强制 LC_ALL=C，避免中文 locale 下 weekday/month 解析失败。
var shutdownTimeRe = regexp.MustCompile(`(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun)\s+(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)\s+\d{1,2}\s+\d{2}:\d{2}:\d{2}\s+\d{4}`)

func parseLastShutdownTime(out string) (time.Time, bool) {
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "shutdown") {
			continue
		}
		m := shutdownTimeRe.FindString(line)
		if m == "" {
			continue
		}
		if t, err := time.Parse("Mon Jan _2 15:04:05 2006", m); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// lastBootHadPanic 检查上一次启动的内核日志里是否有 panic/oops
func lastBootHadPanic() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/sh", "-c",
		"journalctl -b -1 -k --no-pager 2>/dev/null | grep -icE 'kernel panic|general protection|oops'").Output()
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	return err == nil && n > 0
}

// classifyBoot 分析本次重启来源：
//  1. last_reboot_reason 的时间戳落在本次开机前 30 分钟内 → 本程序判定触发；
//  2. 最近一次 wtmp shutdown 记录在本次开机前 10 分钟内 → 正常关机；
//  3. journalctl 上次 boot 内核日志含 panic → panic 重启；
//  4. 有 shutdown 记录但距离太远 → 异常重启（疑似断电/复位）；
//  5. 否则（含 wtmp/last 不可用）→ 默认正常（证据优先：无异常证据=正常）。
//
// 结果写入 boot_reason 文件（状态页优先展示它而非旧的 last_reboot_reason，
// 消除"硬件复位后旧原因误导排查方向"的问题）。
func (d *daemon) classifyBoot() {
	now := time.Now()
	upSec := uptimeMinutes() * 60
	bootTime := now.Add(-time.Duration(upSec) * time.Second)

	curBootID := readBootID()
	prevBootID := d.cfg.readLastBootID()

	// —— 重启判定前置：用 boot_id 基线区分"系统真的重启过"还是"只是看门狗/应用被重新拉起" ——
	// 关键修复（v1.9.1）：classifyBoot 在每次进程启动时都会跑，而安装/重启应用时系统往往并未重启。
	// 若不加 boot_id 基线，就会拿 wtmp 关机记录直接比对，把"app 重拉"误判成"异常重启"，
	// 表现为"刚装上应用就提示异常重启（无干净关机记录）"。

	// 1) 无历史基线（全新安装/首次运行）：没有证据就默认正常，绝不吓用户。
	if prevBootID == "" {
		d.writeBootReason(now, bootTime, bootNormal,
			"首次运行：无历史基线，按正常处理（证据优先：无异常证据=正常）")
		d.cfg.writeLastBootID(curBootID)
		gLog.Write([]byte(fmt.Sprintf("[%s] 开机自检：首次运行，无历史基线，默认正常\n", now.Format(time.RFC3339))))
		return
	}

	// 2) 系统未发生重启（boot_id 未变）：只是看门狗/应用重启，不重新判定重启来源，
	//    保留上次真实开机的结论，杜绝"刚装上就提示异常重启"的误报。
	if prevBootID == curBootID {
		d.cfg.writeLastBootID(curBootID) // 基线不变，保证幂等
		gLog.Write([]byte(fmt.Sprintf("[%s] 开机自检：boot_id 未变化（仅应用重启，系统未重启），保留上次重启来源结论\n", now.Format(time.RFC3339))))
		return
	}

	// 3) 真正发生了系统重启（boot_id 改变）：执行原有证据分析。
	// v1.5.0：证据优先原则 —— 默认正常，只有找到异常证据才降级
	kind, detail := bootNormal, ""

	// 3.1) 本程序判定？读 last_reboot_reason 首行时间戳
	if b, err := os.ReadFile(d.cfg.reasonPath()); err == nil {
		firstLine := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
		if t, err := time.Parse(time.RFC3339, firstLine); err == nil {
			if t.Before(bootTime) && bootTime.Sub(t) < 30*time.Minute {
				kind = bootApp
				detail = "判定时刻 " + t.Format("2006-01-02 15:04:05") + "，原因详见上次死机判定记录"
			}
		}
	}

	// 3.2) 正常关机？（有 wtmp shutdown 记录且时间吻合）
	if kind == bootNormal {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, "last", "-x", "-F", "shutdown")
		cmd.Env = append(os.Environ(), "LC_ALL=C") // 强制英文输出，避免中文 locale 下时间解析失败
		out, err := cmd.Output()
		cancel()
		if err == nil {
			if st, ok := parseLastShutdownTime(string(out)); ok {
				if st.Before(bootTime) && bootTime.Sub(st) < 10*time.Minute {
					detail = "上次关机时刻 " + st.Format("2006-01-02 15:04:05")
				} else {
					// 有 shutdown 记录但离本次开机太远 —— 本次开机前发生过异常断电/复位
					kind = bootAbnormal
					detail = "最近一次干净关机在 " + st.Format("2006-01-02 15:04:05") + "，与本次开机间隔过长"
				}
			}
		}
		// last 命令失败/无 wtmp：保持 bootNormal（默认正常，不吓用户）
	}

	// 3.3) panic 细分（仅异常重启时查，journalctl 查询有成本）
	if kind == bootAbnormal && lastBootHadPanic() {
		kind = bootPanic
		detail = "上次启动的内核日志含 panic/oops 记录"
	}

	d.writeBootReason(now, bootTime, kind, detail)
	d.cfg.writeLastBootID(curBootID)

	// 若本次重启与本程序判定无关，把旧 last_reboot_reason 归档，避免状态页误导
	if kind != bootApp {
		if _, err := os.Stat(d.cfg.reasonPath()); err == nil {
			_ = os.Rename(d.cfg.reasonPath(), d.cfg.reasonPath()+".stale")
			gLog.Write([]byte(fmt.Sprintf("[%s] 旧死机判定记录与本次重启无关，已归档为 last_reboot_reason.stale（防止误导排查）\n",
				now.Format(time.RFC3339))))
		}
	}
}

// writeBootReason 写开机自检结论到 boot_reason 文件（状态页优先展示）。
// 文件格式：第 0 行时间戳、第 1 行来源标题、第 2 行证据明细、第 3 行开机时刻、第 4 行提示。
func (d *daemon) writeBootReason(now, bootTime time.Time, kind bootKind, detail string) {
	title := bootKindTitle[kind]
	detailSuffix := ""
	if detail != "" {
		detailSuffix = " —— " + detail
	}
	gLog.Write([]byte(fmt.Sprintf("[%s] 开机自检：本次重启来源 = %s%s\n",
		now.Format(time.RFC3339), title, detailSuffix)))
	content := fmt.Sprintf("%s\n%s\n%s\n开机时刻: %s\n%s\n",
		now.Format(time.RFC3339), title, detail,
		bootTime.Format("2006-01-02 15:04:05"),
		"提示: 异常重启时请在飞牛终端执行 journalctl -b -1 -k 与 last -x 复查根因")
	_ = os.WriteFile(d.cfg.bootReasonPath(), []byte(content), 0o644)
}

// ---- 健康检查 ----

type checkResult struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail"`
	Family  string `json:"family"`  // 检查族：用于法定人数规则（≥2 族同时失败才计数）
	Timeout bool   `json:"timeout"` // 是否因基础设施超时（慢）而失败：按"弃权"处理，不增不减 failCount
}

// runFork 尝试 fork 一个新进程；内核调度挂死会失败
func runFork(ctx context.Context) checkResult {
	r := checkResult{Name: "fork 存活", OK: true, Family: "sched"}
	done := make(chan error, 1)
	go func() {
		cmd := exec.CommandContext(ctx, "/bin/true")
		done <- cmd.Run()
	}()
	select {
	case err := <-done:
		if err != nil {
			r.OK, r.Detail = false, fmt.Sprintf("fork/exec 失败: %v", err)
		}
	case <-ctx.Done():
		r.OK, r.Detail, r.Timeout = false, "fork 测试超时（调度可能卡死）", true
	}
	return r
}

// runLoad 读取 /proc/loadavg 的一分钟负载
func runLoad(ctx context.Context, threshold float64) checkResult {
	r := checkResult{Name: "系统负载", OK: true, Family: "pressure"}
	done := make(chan string, 1)
	go func() {
		b, err := os.ReadFile("/proc/loadavg")
		if err != nil {
			done <- ""
			return
		}
		done <- strings.TrimSpace(string(b))
	}()
	select {
	case s := <-done:
		fields := strings.Fields(s)
		if len(fields) == 0 {
			r.OK, r.Detail = false, "无法读取 /proc/loadavg"
			return r
		}
		load, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			r.OK, r.Detail = false, "解析负载失败"
			return r
		}
		r.Detail = fmt.Sprintf("1分钟负载 %.2f（阈值 %.2f）", load, threshold)
		if load > threshold {
			r.OK = false
		}
	case <-ctx.Done():
		r.OK, r.Detail, r.Timeout = false, "读取负载超时", true
	}
	return r
}

// runDState 统计不可中断(D)状态进程数
func runDState(ctx context.Context, threshold int) checkResult {
	r := checkResult{Name: "D状态进程", OK: true, Family: "pressure"}
	done := make(chan struct {
		n   int
		top string
	}, 1)
	go func() {
		procs, _ := os.ReadDir("/proc")
		n := 0
		var tops []string
		for _, p := range procs {
			if !p.IsDir() {
				continue
			}
			if _, err := strconv.Atoi(p.Name()); err != nil {
				continue
			}
			b, err := os.ReadFile(filepath.Join("/proc", p.Name(), "stat"))
			if err != nil {
				continue
			}
			// stat: pid (comm) state ... —— 第3字段为状态字符
			s := string(b)
			l := strings.LastIndex(s, ")")
			if l < 0 {
				continue
			}
			fields := strings.Fields(s[l+1:])
			if len(fields) >= 1 && fields[0] == "D" {
				n++
				if len(tops) < 5 {
					comm := p.Name()
					if idx := strings.Index(s, "("); idx >= 0 {
						if end := strings.Index(s[idx:], ")"); end >= 0 {
							comm = s[idx+1 : idx+end]
						}
					}
					tops = append(tops, fmt.Sprintf("%s(%s)", comm, p.Name()))
				}
			}
		}
		done <- struct {
			n   int
			top string
		}{n, strings.Join(tops, ", ")}
	}()
	select {
	case res := <-done:
		r.Detail = fmt.Sprintf("D状态进程 %d（阈值 %d）%s", res.n, threshold, res.top)
		if res.n > threshold {
			r.OK = false
		}
	case <-ctx.Done():
		r.OK, r.Detail, r.Timeout = false, "扫描 /proc 超时", true
	}
	return r
}

// runServiceProbe 运行用户命令，必须返回 0。
// 注意：命令里的端口/地址必须与飞牛真实管理端口一致，否则会误判！
// 学习期保护：探针前几次失败不计入判定（见 loop）。
func runServiceProbe(ctx context.Context, cmdStr string) checkResult {
	r := checkResult{Name: "服务探针", OK: true, Detail: cmdStr, Family: "web"}
	if cmdStr == "" {
		r.OK = true
		r.Detail = "未配置命令"
		return r
	}
	c := exec.CommandContext(ctx, "/bin/sh", "-c", cmdStr)
	if err := c.Run(); err != nil {
		r.OK = false
		r.Detail = fmt.Sprintf("%s -> %v", cmdStr, err)
	}
	return r
}

// probeCandidatePorts 探测飞牛管理 Web 端口是否发生迁移：
// 返回"配置端口已死但候选端口有活"的候选端口（0=无）。
// 用于探针持续失败时判断"飞牛 Web 活着只是换了端口"（改端口场景），防误报。
// v1.3.1：加总超时 5 秒，防主循环阻塞导致喂狗延迟。
func probeCandidatePorts() int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resultCh := make(chan int, 1)
	go func() {
		port := 0
		for _, p := range []int{5666, 8000, 80, 5667, 8001, 443} {
			var scheme string
			if p == 5667 || p == 8001 || p == 443 {
				scheme = "https"
			} else {
				scheme = "http"
			}
			k := ""
			if scheme == "https" {
				k = "-k"
			}
			cmd := fmt.Sprintf("curl %s -s -o /dev/null -m 2 %s://127.0.0.1:%d/", k, scheme, p)
			if err := exec.CommandContext(ctx, "/bin/sh", "-c", cmd).Run(); err == nil {
				port = p
				break
			}
		}
		resultCh <- port
	}()
	select {
	case <-ctx.Done():
		return 0 // 超时不阻塞主循环
	case p := <-resultCh:
		return p
	}
}

// runMemory 检查内存可用率；可用率低于阈值(%)判定异常，直接命中 OOM 类死机。
// 若内核未提供 MemAvailable（老内核）则跳过判定，不误杀。
func runMemory(ctx context.Context, thresholdPct int) checkResult {
	r := checkResult{Name: "内存可用率", OK: true, Family: "mem"}
	type mem struct {
		availPct float64
		availMB  float64
		totalMB  float64
		has      bool
	}
	done := make(chan mem, 1)
	go func() {
		b, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			done <- mem{}
			return
		}
		var total, avail int64
		has := false
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) < 2 {
				continue
			}
			switch f[0] {
			case "MemTotal:":
				total, _ = strconv.ParseInt(f[1], 10, 64)
			case "MemAvailable:":
				avail, _ = strconv.ParseInt(f[1], 10, 64)
				has = true
			}
		}
		if !has || total <= 0 {
			done <- mem{}
			return
		}
		ap := float64(avail) / float64(total) * 100
		done <- mem{availPct: ap, availMB: float64(avail) / 1024, totalMB: float64(total) / 1024, has: true}
	}()
	select {
	case m := <-done:
		if !m.has {
			r.Detail = "内核未提供 MemAvailable，跳过内存检查"
			return r
		}
		if m.availPct < float64(thresholdPct) {
			r.OK = false
			r.Detail = fmt.Sprintf("内存即将耗尽：可用 %.1f%% < %d%%（可用 %.0f/%.0f MB，已用 %.1f%%）",
				m.availPct, thresholdPct, m.availMB, m.totalMB, 100-m.availPct)
		} else {
			r.Detail = fmt.Sprintf("可用 %.1f%%（%.0f/%.0f MB）", m.availPct, m.availMB, m.totalMB)
		}
	case <-ctx.Done():
		r.OK, r.Detail, r.Timeout = false, "读取 /proc/meminfo 超时", true
	}
	return r
}

// readOOMLog 抓取内核 dmesg 中最近的 OOM / 进程被 kill 记录，用于复盘死机根因。
// 重启后 dmesg 会被清空，因此需在死亡当刻（本进程仍存活）捕获。
func readOOMLog() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/sh", "-c",
		"dmesg 2>/dev/null | grep -iE 'out of memory|killed process|oom-kill|oom_reaper|memory cgroup out of memory' | tail -20").Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = "  " + lines[i]
	}
	return strings.Join(lines, "\n") + "\n"
}

// readWatchdogIdentity 读取看门狗驱动身份（如 iTCO_wdt=硬件，softdog=软件），
// 用于状态页明确当前是硬件还是软件复位能力。
func readWatchdogIdentity() string {
	matches, _ := filepath.Glob("/sys/class/watchdog/*/identity")
	for _, m := range matches {
		if b, err := os.ReadFile(m); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	for _, dev := range watchdogDevices {
		if _, err := os.Stat(dev); err == nil {
			return "unknown(dev-exists)"
		}
	}
	return "none"
}

// readNowayout 读取看门狗 nowayout 状态（true=不可 magic close，停止服务也会硬复位）。
// 这对用户至关重要：nowayout=1 时，即使 SIGTERM 正常退出也无法解除看门狗。
func readNowayout() bool {
	matches, _ := filepath.Glob("/sys/class/watchdog/*/nowayout")
	for _, m := range matches {
		if b, err := os.ReadFile(m); err == nil {
			return strings.TrimSpace(string(b)) == "1"
		}
	}
	return false
}

// wdSupportsMagicClose 查询看门狗驱动能力（WDIOC_GETSUPPORT）。
// 返回 true 表示：close 时若未写 'V'（magic close），看门狗仍保持运行；
// 或内核以 nowayout 编译，close 也无法解除。两种情况都满足"停喂即硬复位"的前提，
// 即决定重启时关闭 fd 是安全的（硬件倒计时继续）。否则 close 会直接解除看门狗，
// 必须改为保持 fd 打开、停止喂狗，并依赖 reboot/sysrq 兜底。
func wdSupportsMagicClose(fd int) bool {
	info := watchdogInfo{}
	// 注意：_IOR 的第三个参数是指针；这里用固定大小的 watchdog_info
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(wdIOCGetSupport), uintptr(unsafe.Pointer(&info)))
	if errno != 0 {
		return false
	}
	return info.Options&wdOptMagicClose != 0 || info.Options&wdOptNoWayOut != 0
}

// readKernelVersion 读取内核版本字符串
func readKernelVersion() string {
	if b, err := os.ReadFile("/proc/version"); err == nil {
		// 输出形如: "Linux version 5.15.0-... (gcc version ...)"
		// 取前 80 字符够用
		s := strings.TrimSpace(string(b))
		if len(s) > 80 {
			return s[:80]
		}
		return s
	}
	return "unknown"
}

// readCPUModel 读取 CPU 型号（取第一个 processor 的 model name）
func readCPUModel() string {
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "model name") {
				if idx := strings.Index(line, ":"); idx >= 0 {
					return strings.TrimSpace(line[idx+1:])
				}
			}
		}
	}
	return "unknown"
}

// readTotalMemoryMB 读取物理内存总量（MB）
func readTotalMemoryMB() int64 {
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 && f[0] == "MemTotal:" {
				if kb, err := strconv.ParseInt(f[1], 10, 64); err == nil {
					return kb / 1024
				}
			}
		}
	}
	return 0
}

// readBootID 读取 /proc/sys/kernel/random/boot_id，作为跨重启稳健判别依据。
// 比 wall clock 更可靠：NAS 开机后 NTP 校时常跳变，纯时间比较会被打乱。
func readBootID() string {
	if b, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

// memUsage 返回 (已用百分比, 可用MB)，供状态页展示。
func memUsage() (usedPct int, availMB int) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return -1, -1
	}
	var total, avail int64
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total, _ = strconv.ParseInt(f[1], 10, 64)
		case "MemAvailable:":
			avail, _ = strconv.ParseInt(f[1], 10, 64)
		}
	}
	if total <= 0 {
		return -1, -1
	}
	return int(100 - float64(avail)/float64(total)*100), int(float64(avail) / 1024)
}

// trendMu 防止趋势写入重入堆积（异步执行时）
var trendMu sync.Mutex

// writeTrendOnce 异步执行趋势记录：写日志的 IO 若卡住（数据盘故障很常见），
// 绝不能阻塞主循环的喂狗路径。
func (d *daemon) writeTrendOnce() {
	if !trendMu.TryLock() {
		return
	}
	defer trendMu.Unlock()
	d.writeTrend()
}

// writeTrend 周期性记录系统趋势到 trend.csv（超 20MB 滚动），便于复盘"内存爬升→OOM"。
func (d *daemon) writeTrend() {
	// 滚动：超阈值时保留一份备份
	if fi, err := os.Stat(d.cfg.trendPath()); err == nil && fi.Size() > trendMaxBytes {
		_ = os.Rename(d.cfg.trendPath(), d.cfg.trendPath()+".1")
	}
	usedPct, availMB := memUsage()
	load := d.currentLoad()
	dstate := d.dstateCount()
	procs := 0
	if ds, err := os.ReadDir("/proc"); err == nil {
		for _, p := range ds {
			if p.IsDir() {
				if _, e := strconv.Atoi(p.Name()); e == nil {
					procs++
				}
			}
		}
	}
	oom := ""
	if readOOMLog() != "" {
		oom = "OOM"
	}
	line := fmt.Sprintf("%s,%d,%d,%s,%d,%d,%s\n",
		time.Now().Format(time.RFC3339), usedPct, availMB, load, dstate, procs, oom)
	path := d.cfg.trendPath()
	_, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if statErr != nil {
		// 首行写表头
		_, _ = f.WriteString("time,memory_used_pct,mem_available_mb,load1,dstate_count,proc_count,oom_flag\n")
	}
	_, _ = f.WriteString(line)
}

// ---- 状态 ----

type status struct {
	Running                 bool   `json:"running"`
	Enabled                 bool   `json:"enabled"`
	WatchdogOpen            bool   `json:"watchdog_open"`
	WatchdogDevice          string `json:"watchdog_device"`
	WatchdogIdentity        string `json:"watchdog_identity"`    // 硬件(iTCO_wdt) / 软件(softdog)
	WatchdogTimeout         int    `json:"watchdog_timeout"`     // 驱动实际生效超时
	WatchdogTimeoutCfg      int    `json:"watchdog_timeout_cfg"` // 用户配置的超时（对比实际值用）
	Nowayout                bool   `json:"nowayout"`             // 看门狗 nowayout 状态（true=不可 magic close）
	Version                 string `json:"version"`
	KernelVersion           string `json:"kernel_version"`  // 内核版本
	CPUModel                string `json:"cpu_model"`       // CPU 型号
	TotalMemoryMB           int64  `json:"total_memory_mb"` // 物理内存总量 MB
	Uptime                  string `json:"uptime"`
	CurrentLoad             string `json:"current_load"`
	DStateCount             int    `json:"dstate_count"`
	MemoryUsedPct           int    `json:"memory_used_pct"`
	MemAvailableMB          int    `json:"mem_available_mb"`
	TrendEnabled            bool   `json:"trend_enabled"`
	LastRebootReason        string `json:"last_reboot_reason"`
	ConsecutiveFails        int    `json:"consecutive_fails"`
	Pid                     int    `json:"pid"`
	InBootGrace             bool   `json:"in_boot_grace"`              // 开机冷却期内
	UpgradeBusy             string `json:"upgrade_busy"`               // 非空=维护窗口原因（升级/关机中）
	BootReason              string `json:"boot_reason"`                // 本次开机自检结论（来源标注）
	MaintainLeftMin         int    `json:"maintain_left_min"`          // 手动维护模式剩余分钟（0=无）
	MaintainUntil           int64  `json:"maintain_until"`             // 手动维护模式截止 unix 秒（0=无；UI 倒计时用）
	ProbeLearning           bool   `json:"probe_learning"`             // 探针处于学习期（未计入判定）
	ProbeFailCount          int    `json:"probe_fail_count"`           // 探针连续失败次数（UI 学习期进度条用）
	PortMigrateDetected     string `json:"port_migrate_detected"`      // 检测到端口迁移时的候选端口提示（空=无）
	CheckServiceProbe       bool   `json:"check_service_probe"`        // 服务探针是否开启（v1.9.0：用于前端学习期横幅判断）
	BootID                  string `json:"boot_id"`                    // 本次开机 boot_id（用于跨重启稳健判别，避免 NTP 校时导致的时间误判）
	LastRebootReasonSummary string `json:"last_reboot_reason_summary"` // 重启原因摘要（仅首行），不随状态轮询下发整文件
}

// ---- 主守护进程 ----

type daemon struct {
	cfg             Config
	mu              sync.Mutex
	wd              *watchdog
	wdTimeout       int  // 驱动实际生效的看门狗超时
	wdMagicSafe     bool // 看门狗是否支持 magic close / nowayout（close 后仍能保持硬复位）
	monitoring      bool // 当前是否允许喂狗（由独立 supervisor 读取；决定重启时置 false）
	failCount       int
	lastReason      string
	startTime       time.Time
	cycle           int       // 健康检查周期计数，用于趋势日志
	probeEverOK     bool      // 探针自本次进程启动以来是否成功过（学习期判定）
	probeLearnCount int       // 探针失败计入学习期的次数（达 probeLearnLimit 后退出学习期）
	probeFailCount  int       // 探针连续失败次数（UI 学习期进度展示用）
	wasBusy         bool      // 上一周期是否处于维护窗口（边沿检测用）
	busySince       time.Time // 维护窗口连续开始的时刻（用于超时强制退出）
	portWarned      bool      // 端口迁移警告是否已发出（避免重复告警）
	portMigrateInfo string    // 端口迁移检测到的候选端口信息（UI 展示用，空=无）
}

// loadOrInitConfig 加载配置文件；若文件不存在则写入默认配置。
// v1.4.0：改为合并模式 —— 文件中存在的字段使用文件值，缺失字段保留 defaultConfig() 默认值。
// 这解决了旧版升级后新字段（如 boot_grace_min、maintain_until）被 JSON unmarshal 为零值导致
// "安装后所有设定无默认值"的问题。同时兼容 install_callback 未执行（手动运行）的场景。
//
// 实现关键：先用 map[string]json.RawMessage 解析原始文件，只提取文件中**实际存在的键**，
// 再用这些键去覆盖默认配置。避免 Go struct Unmarshal→Marshal 全量输出（含零值）导致
// 缺失字段被零值污染的问题。
func loadOrInitConfig(cfg *Config) {
	p := cfg.configPath()
	b, err := os.ReadFile(p)
	if err != nil {
		// 首次运行或 install_callback 未执行：写默认配置
		_ = os.MkdirAll(cfg.etcDir(), 0o755)
		saveConfig(cfg)
		return
	}

	// 先尝试解析为 map，检测文件中实际存在哪些键
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(b, &rawMap); err != nil {
		// 配置文件存在但格式错误（损坏/手动编辑出错）：备份坏文件并用默认配置
		logf("警告：配置文件 %s 格式错误（%s），备份为 %s.bad 并使用默认配置", p, err.Error(), p)
		_ = os.Rename(p, p+".bad")
		saveConfig(cfg)
		return
	}

	// 用默认值作为基底
	def := defaultConfig()
	defBytes, _ := json.Marshal(def)
	var defMap map[string]json.RawMessage
	_ = json.Unmarshal(defBytes, &defMap)

	// 只覆盖文件中实际存在的键（不引入零值污染）
	for k := range rawMap {
		if _, existsInDef := defMap[k]; existsInDef {
			defMap[k] = rawMap[k] // 文件中的值覆盖默认值
		} else {
			logf("配置文件包含未知字段 '%s'，已忽略", k)
		}
	}

	mergedBytes, _ := json.Marshal(defMap)
	if err := json.Unmarshal(mergedBytes, cfg); err != nil {
		// 合并失败（极端情况）：回退到默认配置
		logf("警告：配置合并失败（%s），使用默认配置", err.Error())
		*cfg = def
	}
}

// atomicWrite 先写临时文件再 rename，避免掉电产生半截文件（配置/原因记录都走这里）
func atomicWrite(path string, data []byte) error {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// probeCmdRe 白名单：探针命令只允许是 curl 访问本机(127.0.0.1/localhost)固定端口，
// 禁止任意 shell 命令，杜绝通过配置接口拿到 root 命令执行（P0 安全项）。
// 关键：参数部分只允许 [A-Za-z0-9./_-]，绝不接受空格分隔的额外"命令"或 ; | & $ ` 等
// shell 元字符，因此 "curl ... ; rm -rf /" 这类注入无法匹配而直接被拒。
var probeCmdRe = regexp.MustCompile(`^curl\s+(-[A-Za-z]+(?:\s+[A-Za-z0-9./_-]+)?\s+)*https?://(127\.0\.0\.1|localhost):\d+/\s*$`)

// validateProbeCmd 校验探针命令是否在白名单内；空字符串（未启用）始终允许。
func validateProbeCmd(cmd string) bool {
	if cmd == "" {
		return true
	}
	return probeCmdRe.MatchString(cmd)
}

func saveConfig(cfg *Config) error {
	_ = os.MkdirAll(cfg.etcDir(), 0o755)
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(cfg.configPath(), b)
}

// systemSnapshot 收集重启前的系统快照，便于排查死机根因
func systemSnapshot() string {
	var sb strings.Builder
	sb.WriteString("time: " + time.Now().Format(time.RFC3339) + "\n")
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		sb.WriteString("loadavg: " + strings.TrimSpace(string(b)) + "\n")
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "MemTotal:") || strings.HasPrefix(l, "MemAvailable:") ||
				strings.HasPrefix(l, "MemFree:") || strings.HasPrefix(l, "Buffers:") ||
				strings.HasPrefix(l, "Cached:") || strings.HasPrefix(l, "SwapTotal:") ||
				strings.HasPrefix(l, "SwapFree:") {
				sb.WriteString(l + "\n")
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d := runDState(ctx, 999999)
	sb.WriteString("dstate: " + d.Detail + "\n")
	// 内存占用最高的进程（定位是谁在吃内存，例如 Nginx 崩溃循环刷日志）
	if out, err := exec.Command("ps", "aux", "--sort=-%mem").Output(); err == nil {
		lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
		sb.WriteString("top_mem_procs:\n")
		for i, l := range lines {
			if i >= 8 {
				break
			}
			sb.WriteString("  " + l + "\n")
		}
	}
	// 内核 OOM / 进程被 kill 证据
	if oom := readOOMLog(); oom != "" {
		sb.WriteString("oom_kernel_log:\n" + oom)
	}
	sb.WriteString("uptime: " + readUptime() + "\n")
	return sb.String()
}

func readUptime() string {
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		return strings.TrimSpace(string(b))
	}
	return "unknown"
}

func (d *daemon) ensureWatchdog() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cfg.Enabled {
		if d.wd == nil {
			w, actual, err := openWatchdog(d.cfg.WatchdogTimeoutSec)
			if err != nil {
				logThrottled("wd-open-fail", "看门狗打开失败（将无法硬复位，稍后自动重试）: "+err.Error())
				d.wd = nil
				d.wdMagicSafe = false
			} else {
				d.wd = w
				d.wdTimeout = actual
				d.wdMagicSafe = wdSupportsMagicClose(w.fd)
				id := readWatchdogIdentity()
				kind := "软件看门狗(softdog)"
				if !strings.Contains(id, "softdog") && id != "none" && id != "" {
					kind = "硬件看门狗"
				}
				logf("看门狗已开启: %s identity=%s (%s, 驱动实际超时 %d 秒, magic-close/nowayout=%v)",
					w.device, id, kind, actual, d.wdMagicSafe)
				if actual != d.cfg.WatchdogTimeoutSec {
					logf("注意：驱动实际超时 %d 秒与配置 %d 秒不同（iTCO 等仅支持档位值），已按实际值工作", actual, d.cfg.WatchdogTimeoutSec)
				}
			}
		}
		d.monitoring = d.wd != nil
	} else {
		if d.wd != nil {
			d.wd.disarm()
			d.wd = nil
		}
		d.wdMagicSafe = false
		d.monitoring = false
	}
}

// petWatchdog 喂狗（读当前句柄）
func (d *daemon) petWatchdog() {
	d.mu.Lock()
	w := d.wd
	d.mu.Unlock()
	if w != nil {
		_ = w.pet()
	}
}

// inMaintainMode 手动维护模式是否生效（含到期自动恢复的边沿日志）
func (d *daemon) inMaintainMode() bool {
	d.mu.Lock()
	mu := d.cfg.MaintainUntil
	d.mu.Unlock()
	if mu == 0 {
		return false
	}
	if time.Now().Unix() >= mu {
		// 到期：自动恢复判定
		d.mu.Lock()
		d.cfg.MaintainUntil = 0
		snap := d.cfg
		d.mu.Unlock()
		_ = saveConfig(&snap)
		logf("手动维护模式已到期，自动恢复死机判定")
		return false
	}
	return true
}

// triggerReboot 记录原因并尝试重启；若系统已冻，停止喂狗由硬件复位兜底。
// 两段式落盘（v1.3.0）：先毫秒级写入最小事实，再收集完整快照 —— 濒死系统上
// 快照收集卡死也保得住"何时、为何重启"的核心证据。
// 执行前的最后一道防线：若此刻系统恰处于升级/关机/维护窗口，放弃本次重启。
//
// v1.3.1 致命修复：决定重启后设 wd=nil（彻底停止喂狗，不依赖 monitoring 标志），
// 若所有重启方式均失败则阻塞不返回（等待硬件看门狗兜底复位），
// 绝不让主循环有机会在重启失败后继续喂狗导致永远无法重启。
//
// v1.3.2 修复：auto_reboot=false 时仅记录+告警，不关闭看门狗 fd、不阻塞，
// 主循环继续喂狗 —— 用户关掉自动重启的意图是"只观察不重启"，不是"延迟 60 秒后硬复位"。
func (d *daemon) triggerReboot(reason string) {
	if busy := systemBusyReason(); busy != "" {
		logf("判定达到阈值，但系统处于维护窗口（%s），放弃本次重启并清零计数（防误杀升级/正常重启）", busy)
		d.mu.Lock()
		d.failCount = 0
		d.mu.Unlock()
		return
	}
	if d.inMaintainMode() {
		logf("判定达到阈值，但处于手动维护模式，放弃本次重启并清零计数")
		d.mu.Lock()
		d.failCount = 0
		d.mu.Unlock()
		return
	}

	ts := time.Now().Format(time.RFC3339)

	// v1.3.2 + v1.9.0：auto_reboot=false → 仅记录，不碰看门狗，不阻塞（用户意图：只观察）。
	// v1.9.0：在锁内读取 auto_reboot 配置值，修复并发读数据竞争（原代码在锁外读 d.cfg.AutoReboot）。
	d.mu.Lock()
	autoReboot := d.cfg.AutoReboot
	d.mu.Unlock()
	if !autoReboot {
		logf("!!! 判定系统死机（auto_reboot=off，仅记录不重启）！！！")
		logf("原因: %s", reason)
		logf("若后续需要自动重启，请在页面开启 auto_reboot 或到参数配置勾选'判定死机后自动重启'")
		_ = atomicWrite(d.cfg.reasonPath(), []byte(ts+"\n"+reason+"\n\n(auto_reboot=off: 本次仅记录，未执行重启)\n"))
		return // ← 关键：返回主循环继续喂狗，绝不触发硬件复位
	}

	// === 决定重启的不可逆点：从此刻起彻底停止喂狗 ===
	// v1.9.0 硬件兜底可靠性修复（针对原"syscall.Close(fd) + 注释'硬件倒计时开始'"的致命缺陷）：
	//   内核看门狗的"停喂即硬复位"对*所有*看门狗类型都成立——只要 fd 保持打开且不再喂狗，
	//   计时器必然到期触发复位（与 magic close / nowayout 无关）。
	//   而原逻辑中盲目 syscall.Close(fd)：在驱动既不支持 magic close 也未开 nowayout 时，
	//   close 会直接 DISARM 看门狗，导致进程永久阻塞、系统却永不复位（比死机更糟）。
	//   因此这里**绝不关闭 fd**，仅从 daemon 摘除引用并停止喂狗，由独立 supervisor 停止 petting，
	//   硬件计时器照常到期复位。wdMagicSafe 仅用于状态展示，不再参与此决策。
	d.mu.Lock()
	wdTimeoutSec := d.cfg.WatchdogTimeoutSec
	hadWd := d.wd != nil
	d.wd = nil           // 摘除引用：supervisor 此后不再喂狗
	d.monitoring = false // 停止喂狗的最终开关
	d.mu.Unlock()

	// 第一段：毫秒级落盘最小事实（防快照收集卡死丢证据）
	minimal := ts + "\n" + reason + "\n\n(完整快照采集于判定时刻，若下方缺失说明系统在采集时已濒死)\n"
	_ = atomicWrite(d.cfg.reasonPath(), []byte(minimal))
	gLog.Write([]byte(fmt.Sprintf("[%s] !!! 判定系统死机，准备重启 !!!\n原因: %s\n", ts, reason)))
	if hadWd {
		gLog.Write([]byte(fmt.Sprintf("[%s] 看门狗 fd 保持打开、停止喂狗：硬件计时器到期后硬复位（约 %d 秒，对所有看门狗类型均生效）\n", ts, wdTimeoutSec)))
	} else {
		gLog.Write([]byte(fmt.Sprintf("[%s] 无硬件看门狗：完全依赖下方 reboot/sysrq 强制重启链\n", ts)))
	}

	// 第二段：完整快照（可能较慢）追加落盘
	snapshot := systemSnapshot()
	if oom := readOOMLog(); oom != "" {
		logf("快照含内核OOM证据（详见 oom_kernel_log）")
	}
	_ = atomicWrite(d.cfg.reasonPath(), []byte(ts+"\n"+reason+"\n\n"+snapshot))
	gLog.Write([]byte(fmt.Sprintf("[%s] 重启前快照已落盘:\n%s\n", ts, snapshot)))

	// 强制重启链（依次尝试，越靠后越暴力，确保软件层优先于硬件硬复位）
	attempted := []string{}
	rebootOK := false
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART); err == nil {
		rebootOK = true
	} else {
		attempted = append(attempted, "syscall.Reboot")
		if err := exec.Command("/sbin/reboot").Run(); err == nil {
			rebootOK = true
		} else {
			attempted = append(attempted, "/sbin/reboot")
			if err := exec.Command("/sbin/reboot", "-f").Run(); err == nil {
				rebootOK = true
			} else {
				attempted = append(attempted, "/sbin/reboot -f")
				if err := exec.Command("/bin/sh", "-c", "echo b > /proc/sysrq-trigger").Run(); err == nil {
					rebootOK = true
				} else {
					attempted = append(attempted, "sysrq-trigger")
				}
			}
		}
	}

	if rebootOK {
		gLog.Write([]byte(fmt.Sprintf("[%s] 重启指令已发出，等待系统重启...（若 %d 秒内未重启，硬件看门狗将硬复位）\n",
			time.Now().Format(time.RFC3339), wdTimeoutSec)))
		time.Sleep(time.Duration(wdTimeoutSec+5) * time.Second)
		gLog.Write([]byte(fmt.Sprintf("[%s] *** 异常：等待超时系统仍未重启，交由硬件看门狗 ***\n", time.Now().Format(time.RFC3339))))
	}

	// 所有重启方式均失败：硬件看门狗（若已开启）计时器仍在走，必然硬复位；
	// 若无硬件看门狗，则已尽力尝试 reboot/sysrq，只能永久阻塞（避免返回主循环恢复喂狗）。
	gLog.Write([]byte(fmt.Sprintf("[%s] *** 致命：所有重启方式(%v)均失败；%s，永久阻塞中（防止返回主循环恢复喂狗导致无法重启）***\n",
		time.Now().Format(time.RFC3339), attempted,
		func() string {
			if hadWd {
				return "硬件看门狗已武装，约 " + strconv.Itoa(wdTimeoutSec) + " 秒后硬复位"
			}
			return "无硬件看门狗，无法自动硬复位"
		}())))
	select {} // 永久阻塞 —— 核心安全保证
}

// keysOf 返回 map 的 key 列表（用于日志展示检查族）
func keysOf(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// loop 主循环：负责"判定"逻辑（是否死机），喂狗由独立 supervisor 完成，二者解耦。
// v1.9.0 关键设计：
//   - 判定与喂狗彻底分离：即使本循环 panic 或卡死，supervisor 仍持续喂狗，绝不会饿死硬件看门狗。
//   - 每项检查独立超时：某检查因系统"慢"而超时不算失败，按"弃权"处理，杜绝"慢被当失败"。
//   - 法定人数规则：≥2 个独立检查族同时真正失败，才计入 failCount；单一检查失败或基础设施
//     超时均不计数（宁可漏判也绝不单点/瞬时误杀，符合零误杀优先的核心原则）。
//   - 维护窗口设连续占用上限：被常驻进程永久锁死时强制退出并告警，防止监控静默失效。
func (d *daemon) loop() {
	defer func() {
		if r := recover(); r != nil {
			logf("!!! loop 发生 panic（已捕获；看门狗由独立 supervisor 继续喂狗，不会被饿死）；5 秒后重启判定循环: %v", r)
			time.Sleep(5 * time.Second)
			go d.loop()
		}
	}()
	for {
		d.mu.Lock()
		cfg := d.cfg
		wd := d.wd
		d.mu.Unlock()

		d.cycle++
		// 周期性记录趋势（异步执行，日志 IO 卡住不阻塞喂狗）
		if cfg.TrendInterval > 0 && d.cycle%cfg.TrendInterval == 0 {
			go d.writeTrendOnce()
		}

		if !cfg.Enabled {
			logDaily("paused-remind", "监控处于暂停状态：当前无任何死机保护，如非刻意请重新开启（网页一键开关）")
			time.Sleep(2 * time.Second)
			continue
		}

		// 看门狗尚未打开（开机时驱动可能晚就绪）：周期性重试打开
		if wd == nil {
			d.ensureWatchdog()
		}

		// ① 开机冷却期：只喂狗不判定（开机 fsck/服务初始化期负载与 D 状态偏高，是误判高发期）
		if uptimeMinutes() < float64(cfg.BootGraceMin) {
			logThrottled("boot-grace", "开机冷却期内：只喂狗不判定（防开机初始化高负载误判）")
			time.Sleep(time.Duration(cfg.IntervalSec) * time.Second)
			continue
		}

		// ② 手动维护模式：用户显式要求暂停判定（飞牛升级/手工维护的保底开关）
		if d.inMaintainMode() {
			logThrottled("maintain", "手动维护模式中：只喂狗不判定（到期自动恢复）")
			time.Sleep(time.Duration(cfg.IntervalSec) * time.Second)
			continue
		}

		// ③ 自动维护窗口（系统升级/关机/重启进行中）：继续喂狗但绝不判定死机。
		//    若窗口内系统真冻结，本进程随之冻结停止喂狗，硬件看门狗自然兜底。
		if busy := systemBusyReason(); busy != "" {
			d.mu.Lock()
			if d.busySince.IsZero() {
				d.busySince = time.Now()
			}
			wasBusy := d.wasBusy
			d.wasBusy = true
			d.mu.Unlock()
			if !wasBusy {
				logf("进入维护窗口（%s）：历史失败计数清零", busy)
				d.mu.Lock()
				d.failCount = 0
				d.mu.Unlock()
			}
			// v1.9.0：维护窗口连续占用上限——防止被常驻进程（如 trim_* 升级守护）永久锁死，
			// 导致监控静默失效（看门狗变装饰）。超过上限强制退出并告警。
			d.mu.Lock()
			since := d.busySince
			d.mu.Unlock()
			if time.Since(since) > maxBusyDuration {
				logf("⚠️ 维护窗口已连续占用超过 %v（疑似常驻进程误锁死），强制退出维护状态并恢复死机判定！", maxBusyDuration)
				d.mu.Lock()
				d.busySince = time.Time{}
				d.wasBusy = false
				d.failCount = 0
				d.mu.Unlock()
				// 不 continue：落到下方正常检查逻辑
			} else {
				logThrottled("busy", "维护窗口（"+busy+"）：继续喂狗，暂停死机判定（已持续 "+since.Format("15:04:05")+"）")
				time.Sleep(time.Duration(cfg.IntervalSec) * time.Second)
				continue
			}
		}
		// 维护窗口退出边沿：清零失败计数（窗口前积累的历史样本作废）
		d.mu.Lock()
		wasBusy := d.wasBusy
		d.wasBusy = false
		d.busySince = time.Time{}
		d.mu.Unlock()
		if wasBusy {
			logf("维护窗口结束：恢复死机判定，历史失败计数已清零")
			d.mu.Lock()
			d.failCount = 0
			d.mu.Unlock()
		}

		// ④ 健康检查：每项检查使用独立超时（= 一个完整周期），彼此不抢占预算。
		//    检查并发执行（各自 goroutine + 独立 context 超时），整体循环周期 ≈ IntervalSec，
		//    避免串行累计导致周期漂移（"3 分钟 = 18 周期"的判定窗口才准确）。
		//    某检查因系统"慢"而超时 → 标记 Timeout，按"弃权"处理（不增不减 failCount），
		//    绝不把"慢"误判为"失败"（核心原则：零误杀优先）。
		cycleStart := time.Now()
		results := make([]checkResult, 0, 6)
		resCh := make(chan checkResult, 8)
		launched := 0
		runWithTimeout := func(fn func(context.Context) checkResult) {
			launched++
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.IntervalSec)*time.Second)
				defer cancel()
				resCh <- fn(ctx)
			}()
		}
		if cfg.CheckFork {
			runWithTimeout(runFork)
		}
		if cfg.CheckLoad {
			runWithTimeout(func(ctx context.Context) checkResult { return runLoad(ctx, cfg.LoadThreshold) })
		}
		if cfg.CheckDState {
			runWithTimeout(func(ctx context.Context) checkResult { return runDState(ctx, cfg.DStateThreshold) })
		}
		if cfg.CheckMemory {
			runWithTimeout(func(ctx context.Context) checkResult { return runMemory(ctx, cfg.MemAvailableThreshold) })
		}
		if cfg.CheckServiceProbe {
			runWithTimeout(func(ctx context.Context) checkResult { return runServiceProbe(ctx, cfg.ServiceProbeCmd) })
		}
		for i := 0; i < launched; i++ {
			results = append(results, <-resCh)
		}

		// 收集"真正失败"（排除超时）；按检查族去重用于法定人数判定
		genuineFamilies := map[string]bool{}
		var genuineFailed, timedOut []string
		for _, r := range results {
			if r.Timeout {
				timedOut = append(timedOut, r.Name+": "+r.Detail)
			} else if !r.OK {
				genuineFailed = append(genuineFailed, fmt.Sprintf("%s: %s", r.Name, r.Detail))
				if r.Family != "" {
					genuineFamilies[r.Family] = true
				}
			}
		}

		// 探针学习期（v1.9.0：固定前 N 次失败不计入判定，有退出条件，不再"永不成功就永久学习"）
		d.mu.Lock()
		probeOn := cfg.CheckServiceProbe && cfg.ServiceProbeCmd != ""
		probeFailed := false
		for _, r := range results {
			if r.Name == "服务探针" && !r.OK && !r.Timeout {
				probeFailed = true
			}
		}
		if probeOn {
			if probeFailed {
				if d.probeLearnCount < probeLearnLimit {
					d.probeLearnCount++
				}
				d.probeFailCount++
			} else {
				d.probeEverOK = true
				d.probeFailCount = 0
				d.probeLearnCount = 0
			}
		}
		learningActive := probeOn && d.probeLearnCount < probeLearnLimit
		d.mu.Unlock()

		// 学习期内：把探针从"真正失败"集合里剔除，避免配置错误/服务慢启动误杀
		if learningActive && probeFailed {
			delete(genuineFamilies, "web")
			kept := genuineFailed[:0]
			for _, f := range genuineFailed {
				if !strings.HasPrefix(f, "服务探针") {
					kept = append(kept, f)
				}
			}
			genuineFailed = kept
			logThrottled("probe-learn", "探针处于学习期（前 "+strconv.Itoa(probeLearnLimit)+" 次失败不计入判定）：失败暂不计入，请核对探针命令与端口（新版飞牛默认 5666，非 80）")
		}

		// 端口迁移检测（v1.9.0：解除对"学习期"的耦合，探针失败即检测，防配置错误静默废掉探针）
		if probeFailed {
			d.mu.Lock()
			fc := d.failCount + 1
			warned := d.portWarned
			d.mu.Unlock()
			if fc >= cfg.FailThreshold/2 && fc < cfg.FailThreshold && !warned {
				if port := probeCandidatePorts(); port > 0 {
					msg := fmt.Sprintf("探针端口无响应但 %d 端口有 Web 响应，可能端口已变更", port)
					logf("⚠️ 疑似飞牛管理端口已迁移：%s。请到页面更新探针命令。本次不计入（防误报）", msg)
					d.mu.Lock()
					d.portWarned = true
					d.portMigrateInfo = msg
					d.failCount = 0
					d.mu.Unlock()
				}
			}
		}

		// ⑤ 判定：法定人数规则（v1.9.0）
		//    - ≥2 个独立检查族同时真正失败 → 计入一个失败周期（failCount++）
		//    - 仅 1 族失败或全过 → 不计为失败（清零），宁可漏判也不单点误杀
		//    - 全部超时（无真正失败）→ 弃权：failCount 不变（系统只是慢）
		d.mu.Lock()
		fc := d.failCount
		switch {
		case len(genuineFamilies) >= 2:
			fc++
			d.portWarned = false
			d.portMigrateInfo = ""
		case len(genuineFailed) == 0:
			if len(timedOut) == 0 {
				fc = 0 // 全过 → 清零
			}
			// 全超时 → 弃权，failCount 保持不变
		default:
			fc = 0 // 仅 1 族真正失败（不足法定人数）→ 清零
		}
		d.failCount = fc
		fcSnapshot := fc
		d.mu.Unlock()

		if len(genuineFailed) > 0 || len(timedOut) > 0 {
			switch {
			case len(genuineFamilies) >= 2:
				logf("健康检查失败(%d/%d)，命中法定人数%v: %s", fcSnapshot, cfg.FailThreshold, keysOf(genuineFamilies), strings.Join(genuineFailed, " | "))
			case len(timedOut) > 0:
				logThrottled("slow", "部分检查超时（系统偏慢，按弃权处理，不计数）: "+strings.Join(timedOut, " | "))
			default:
				logThrottled("single-fail", "仅单族检查失败（不足法定人数，不计数，防单点误杀）: "+strings.Join(genuineFailed, " | "))
			}
		}

		if fcSnapshot >= cfg.FailThreshold {
			reason := strings.Join(genuineFailed, " | ")
			if reason == "" {
				reason = "多个独立检查族持续异常（详见日志）"
			}
			d.triggerReboot(reason)
			d.mu.Lock()
			d.failCount = 0
			d.mu.Unlock()
		}

		// 维持稳定周期：扣除本次判定已用时间，补足剩余 IntervalSec（避免周期漂移）
		if remain := time.Duration(cfg.IntervalSec)*time.Second - time.Since(cycleStart); remain > 0 {
			time.Sleep(remain)
		}
	}
}

// supervisor 独立喂狗协程：只负责"喂狗"这一件事，与判定循环（loop）完全解耦。
// 这样即使 loop panic/卡死或 UI/HTTP 层崩溃，喂狗也不会中断——绝不饿死硬件看门狗。
// v1.9.0 新增：把"最小喂狗进程"从业务循环里拆出来，是看门狗可靠性的底线保障。
func (d *daemon) supervisor() {
	defer func() {
		if r := recover(); r != nil {
			logf("!!! supervisor panic（已捕获，5 秒后重启喂狗协程）: %v", r)
			time.Sleep(5 * time.Second)
			go d.supervisor()
		}
	}()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	rot := time.NewTicker(time.Minute)
	defer rot.Stop()
	for {
		select {
		case <-tick.C:
			d.mu.Lock()
			feed := d.monitoring && d.wd != nil
			w := d.wd
			d.mu.Unlock()
			if feed && w != nil {
				_ = w.pet()
			}
		case <-rot.C:
			gLog.Rotate()
		}
	}
}

// ---- HTTP / Web UI ----

func (d *daemon) currentLoad() string {
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		return strings.Fields(string(b))[0]
	}
	return "?"
}

func (d *daemon) dstateCount() int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res := runDState(ctx, 999999)
	// 解析数量
	n := 0
	fmt.Sscanf(res.Detail, "D状态进程 %d", &n)
	return n
}

// readBootReasonSummary 读开机自检结论摘要（标题行 + 证据明细行）。
// boot_reason 文件格式：第 0 行时间戳、第 1 行来源标题、第 2 行证据明细。
func (d *daemon) readBootReasonSummary() string {
	b, err := os.ReadFile(d.cfg.bootReasonPath())
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) >= 2 {
		title := lines[1]
		detail := ""
		if len(lines) >= 3 {
			detail = strings.TrimSpace(lines[2])
		}
		if detail != "" {
			return title + "\n" + detail
		}
		return title
	}
	return ""
}

func (d *daemon) buildStatus() status {
	// v1.9.0：修复数据竞争——在锁内一次性取出所有需要的配置/状态快照，
	// 锁外不再触碰 d.cfg / d.portMigrateInfo 等共享字段（原代码在 Unlock 后多处读 d.cfg）。
	d.mu.Lock()
	enabled := d.cfg.Enabled
	wd := d.wd
	fails := d.failCount
	bootGrace := d.cfg.BootGraceMin
	maintainUntil := d.cfg.MaintainUntil
	wdTimeout := d.wdTimeout
	watchdogTimeoutCfg := d.cfg.WatchdogTimeoutSec
	trendInterval := d.cfg.TrendInterval
	probeEverOK := d.probeEverOK
	probeOn := d.cfg.CheckServiceProbe && d.cfg.ServiceProbeCmd != ""
	portMigrateInfo := d.portMigrateInfo
	checkServiceProbe := d.cfg.CheckServiceProbe
	d.mu.Unlock()
	reason := ""
	if b, err := os.ReadFile(d.cfg.reasonPath()); err == nil {
		reason = strings.TrimSpace(string(b))
	}
	reasonSummary := reason
	if idx := strings.Index(reason, "\n"); idx >= 0 {
		reasonSummary = reason[:idx]
	}
	id := readWatchdogIdentity()
	usedPct, availMB := memUsage()
	upMin := uptimeMinutes()
	busy := systemBusyReason()
	maintainLeft := 0
	if maintainUntil > time.Now().Unix() {
		maintainLeft = int((time.Duration(maintainUntil-time.Now().Unix()) * time.Second).Minutes())
	}
	timeoutShown := wdTimeout
	if timeoutShown == 0 {
		timeoutShown = watchdogTimeoutCfg
	}
	return status{
		Running:      true,
		Enabled:      enabled,
		WatchdogOpen: wd != nil,
		WatchdogDevice: func() string {
			if wd != nil && wd.device != "" {
				return wd.device
			}
			return strings.Join(watchdogDevices, ", ")
		}(),
		WatchdogIdentity:        id,
		WatchdogTimeout:         timeoutShown,
		WatchdogTimeoutCfg:      watchdogTimeoutCfg,
		Nowayout:                readNowayout(),
		Version:                 appVersion,
		KernelVersion:           readKernelVersion(),
		CPUModel:                readCPUModel(),
		TotalMemoryMB:           readTotalMemoryMB(),
		Uptime:                  readUptime(),
		CurrentLoad:             d.currentLoad(),
		DStateCount:             d.dstateCount(),
		MemoryUsedPct:           usedPct,
		MemAvailableMB:          availMB,
		TrendEnabled:            trendInterval > 0,
		LastRebootReason:        reasonSummary,
		LastRebootReasonSummary: reasonSummary,
		ConsecutiveFails:        fails,
		Pid:                     os.Getpid(),
		InBootGrace:             upMin < float64(bootGrace),
		UpgradeBusy:             busy,
		BootReason:              d.readBootReasonSummary(),
		MaintainLeftMin:         maintainLeft,
		MaintainUntil:           maintainUntil,
		ProbeLearning:           probeOn && !probeEverOK,
		ProbeFailCount:          d.probeFailCount,
		PortMigrateDetected:     portMigrateInfo,
		CheckServiceProbe:       checkServiceProbe,
		BootID:                  readBootID(),
	}
}

// sameOrigin 校验写操作的跨站安全（P0 安全项）：
// 同源请求（fnOS 网关 / 反代均同源）要么不带 Origin 头，要么 Origin 主机名与本机一致；
// 跨站请求（攻击者页面诱导浏览器发起的"简单请求"，如 <form enctype=text/plain>）会带不同
// Origin → 直接拒绝。这样即便 8900 端口可被局域网访问，也无法被 CSRF 利用。
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // 同源导航/简单请求常不带 Origin
	}
	ou, err := url.Parse(origin)
	if err != nil || ou.Host == "" {
		return false
	}
	rh := r.Host
	if h, _, e := net.SplitHostPort(r.Host); e == nil {
		rh = h
	}
	oh := ou.Host
	if h, _, e := net.SplitHostPort(ou.Host); e == nil {
		oh = h
	}
	return strings.EqualFold(rh, oh)
}

// requireSafeWrite 校验写操作的同源与 Content-Type，失败直接 403/415。
func (d *daemon) requireSafeWrite(w http.ResponseWriter, r *http.Request) bool {
	if !sameOrigin(r) {
		http.Error(w, "forbidden: cross-origin write rejected", http.StatusForbidden)
		return false
	}
	return true
}

func (d *daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(d.buildStatus())
}

func (d *daemon) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	cfg := d.cfg
	d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cfg)
}

// handleConfigSet 字段级合并：仅覆盖请求里出现的字段，
// 防止旧版页面/第三方调用漏字段时把配置（如阈值）意外清成零值导致误判。
func (d *daemon) handleConfigSet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	// P0 安全：同源校验 + 必须 JSON（拒绝 <form enctype=text/plain> 类 CSRF 简单请求）
	if !d.requireSafeWrite(w, r) {
		return
	}
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "unsupported content-type (require application/json)", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), 400)
		return
	}
	var incoming map[string]json.RawMessage
	if err := json.Unmarshal(body, &incoming); err != nil {
		http.Error(w, "bad json: "+err.Error(), 400)
		return
	}

	d.mu.Lock()
	orig, _ := json.Marshal(d.cfg)
	var origMap map[string]json.RawMessage
	if err := json.Unmarshal(orig, &origMap); err != nil {
		d.mu.Unlock()
		http.Error(w, "merge failed: "+err.Error(), 500)
		return
	}
	for k, v := range incoming {
		origMap[k] = v
	}
	merged, _ := json.Marshal(origMap)
	var newCfg Config
	if err := json.Unmarshal(merged, &newCfg); err != nil {
		d.mu.Unlock()
		http.Error(w, "bad fields: "+err.Error(), 400)
		return
	}
	// v1.3.1：配置值合法性校验（防零值/极端值导致立即误判或永远不判定）
	validated := false
	if newCfg.IntervalSec < 2 {
		newCfg.IntervalSec = 2
		validated = true
	}
	if newCfg.IntervalSec > 300 {
		newCfg.IntervalSec = 300
		validated = true
	}
	if newCfg.WatchdogTimeoutSec < 5 {
		newCfg.WatchdogTimeoutSec = 5
		validated = true
	}
	if newCfg.WatchdogTimeoutSec > 600 {
		newCfg.WatchdogTimeoutSec = 600
		validated = true
	}
	if newCfg.FailThreshold < 1 {
		newCfg.FailThreshold = 1
		validated = true
	}
	if newCfg.FailThreshold > 120 {
		newCfg.FailThreshold = 120
		validated = true
	}
	if newCfg.BootGraceMin < 0 {
		newCfg.BootGraceMin = 0
		validated = true
	}
	if newCfg.BootGraceMin > 60 {
		newCfg.BootGraceMin = 60
		validated = true
	}
	if newCfg.LoadThreshold < 0 {
		newCfg.LoadThreshold = 0
		validated = true
	}
	if newCfg.DStateThreshold < 0 {
		newCfg.DStateThreshold = 0
		validated = true
	}
	if newCfg.DStateThreshold > 500 {
		newCfg.DStateThreshold = 500
		validated = true
	}
	if newCfg.MemAvailableThreshold < 1 {
		newCfg.MemAvailableThreshold = 1
		validated = true
	}
	if newCfg.MemAvailableThreshold > 100 {
		newCfg.MemAvailableThreshold = 100
		validated = true
	}
	if newCfg.TrendInterval < 0 {
		newCfg.TrendInterval = 0
		validated = true
	}
	if newCfg.TrendInterval > 200 {
		newCfg.TrendInterval = 200
		validated = true
	}
	if newCfg.Port < 1 || newCfg.Port > 65535 {
		newCfg.Port = defaultPort
		validated = true
	}
	if validated {
		logf("配置校验：部分值被修正到合法范围（详见日志/当前配置）")
	}
	// 探针命令白名单校验（P0 安全）：只允许 curl 探测本机(127.0.0.1/localhost)端口，
	// 杜绝通过配置接口获得任意 root shell 命令执行（原实现对所有命令照单全收）。
	if !validateProbeCmd(newCfg.ServiceProbeCmd) {
		d.mu.Unlock()
		http.Error(w, "invalid service_probe_cmd: 只允许 curl 探测本机(127.0.0.1)端口，禁止任意命令", http.StatusBadRequest)
		return
	}
	// 探针命令变更后重新进入学习期（新命令从未验证过）
	if newCfg.ServiceProbeCmd != d.cfg.ServiceProbeCmd {
		d.probeEverOK = false
		d.portWarned = false
	}
	d.cfg = newCfg
	d.mu.Unlock()

	if err := saveConfig(&newCfg); err != nil {
		http.Error(w, "save failed: "+err.Error(), 500)
		return
	}
	d.ensureWatchdog() // 开关/超时变化即时生效
	logf("配置已更新并保存")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"result": "ok"})
}

func (d *daemon) handleLogs(w http.ResponseWriter, r *http.Request) {
	lines := 200
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			lines = n
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, gLog.tailLines(lines))
}

// handleToggle 网页一键开关：开启/暂停监控（pause 时 disarm 看门狗，不重启系统）
func (d *daemon) handleToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !d.requireSafeWrite(w, r) {
		return
	}
	enable := r.URL.Query().Get("enabled") == "1"
	d.mu.Lock()
	d.cfg.Enabled = enable
	cfgSnapshot := d.cfg
	d.mu.Unlock()
	_ = saveConfig(&cfgSnapshot)
	d.ensureWatchdog()
	state := "已开启监控"
	if !enable {
		state = "已暂停监控（看门狗已解除，系统不会自动重启；请记得恢复）"
	}
	logf("%s", state)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"result": "ok", "state": state})
}

// handleMaintain 手动维护模式：?hours=N（进入 N 小时维护，期间只喂狗不判定，
// 到期自动恢复）；?off=1 立即退出。飞牛升级/手工维护前的保底开关。
func (d *daemon) handleMaintain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !d.requireSafeWrite(w, r) {
		return
	}
	q := r.URL.Query()
	// 支持三种取消方式：?off=1 或 ?minutes=0 或 ?hours=0
	if q.Get("off") == "1" || q.Get("minutes") == "0" || q.Get("hours") == "0" {
		d.mu.Lock()
		d.cfg.MaintainUntil = 0
		d.failCount = 0
		snap := d.cfg
		d.mu.Unlock()
		_ = saveConfig(&snap)
		logf("手动维护模式已退出，恢复死机判定")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"result": "ok", "state": "维护模式已退出"})
		return
	}
	// 支持 ?minutes=N 或 ?hours=N，minutes 优先
	duration := 2 * time.Hour // 默认 2 小时
	if v := q.Get("minutes"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 480 { // 最长 8 小时
			duration = time.Duration(n) * time.Minute
		}
	} else if v := q.Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 48 {
			duration = time.Duration(n) * time.Hour
		}
	}
	until := time.Now().Add(duration).Unix()
	d.mu.Lock()
	d.cfg.MaintainUntil = until
	d.failCount = 0
	snap := d.cfg
	d.mu.Unlock()
	_ = saveConfig(&snap)
	minutes := int(duration.Minutes())
	logf("进入手动维护模式 %d 分钟（期间只喂狗不判定死机，到期自动恢复）", minutes)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"result": "ok",
		"state": fmt.Sprintf("维护模式已开启：%d 分钟内不判定死机（继续喂狗）", minutes)})
}

// handlePurgeData v1.5.0：创建"卸载时清理数据"标记文件（辅助功能）。
// ⚠️ v1.6.0 起，主要卸载数据删除机制已改为官方 wizard/uninstall 表单（uninstall_purge_data 字段）。
// 本 API 保留作为运行时便利功能：用户可在不卸载的情况下预标记清理。
func (d *daemon) handlePurgeData(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !d.requireSafeWrite(w, r) {
		return
	}
	marker := filepath.Join(d.cfg.varDir(), ".purge_on_uninstall")
	if err := os.WriteFile(marker, []byte(fmt.Sprintf("created: %s\n", time.Now().Format(time.RFC3339))), 0o644); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "写入标记文件失败: " + err.Error()})
		return
	}
	logf("用户已标记：卸载时彻底清理应用数据（标记文件: %s）", marker)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"result": "ok",
		"msg":    "已标记：下次卸载应用时将自动删除所有数据（日志/配置/重启原因）。如需取消，使用「取消数据清理」按钮。",
	})
}

// handleClearData v1.5.0：立即清除应用数据（日志、重启原因、趋势等），
// 但**保留配置文件 config.json**（用户可能还要用）。
// 需要二次确认参数 ?confirm=1 防止误操作。
func (d *daemon) handleClearData(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !d.requireSafeWrite(w, r) {
		return
	}
	// 二次确认：必须带 confirm=1
	if r.URL.Query().Get("confirm") != "1" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"need_confirm": true,
			"msg":          "⚠️ 此操作将立即删除所有运行日志、重启原因记录和趋势数据！如确认，请再次点击（会弹出最终确认）。",
			"data_size":    dataSize(d.cfg.varDir()),
		})
		return
	}

	varDir := d.cfg.varDir()
	removed := []string{}
	failed := []string{}

	// 删除数据目录下的文件（保留目录本身）
	for _, f := range []string{"kanmendog.log", "last_reboot_reason", "boot_reason", "trend.csv", ".purge_on_uninstall"} {
		path := filepath.Join(varDir, f)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			failed = append(failed, f)
		} else if err == nil {
			removed = append(removed, f)
		}
	}

	logf("用户手动清除应用数据：已删除 %v，失败 %v", removed, failed)

	w.Header().Set("Content-Type", "application/json")
	resp := map[string]interface{}{"result": "ok", "removed": removed}
	if len(failed) > 0 {
		resp["failed"] = failed
		resp["msg"] = fmt.Sprintf("已删除 %d 项，%d 项失败（可能需要手动删除）", len(removed), len(failed))
	} else {
		resp["msg"] = fmt.Sprintf("已成功清除 %d 项数据（配置文件已保留）", len(removed))
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// dataSize 返回目录的磁盘占用（人类可读格式）
func dataSize(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "du", "-sh", dir).Output()
	if err != nil {
		return "未知"
	}
	return strings.Fields(string(out))[0]
}

// handleService 控制 systemd 服务（真实启停整个进程）
// handleHealth 健康检查端点（v1.4.0）：用于诊断 API 是否正常工作，
// 以及确认配置加载、看门狗状态、环境变量等关键信息。
func (d *daemon) handleHealth(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	health := map[string]interface{}{
		"status":        "ok",
		"version":       appVersion,
		"pid":           os.Getpid(),
		"uptime_sec":    int(time.Since(d.startTime).Seconds()),
		"config_path":   d.cfg.configPath(),
		"config_exists": false,
		"log_path":      d.cfg.logPath(),
		"log_exists":    false,
		"watchdog_open": d.wd != nil,
		"watchdog_device": func() string {
			if d.wd != nil && d.wd.device != "" {
				return d.wd.device
			}
			return strings.Join(watchdogDevices, ", ")
		}(),
		"env_trim_pkgetc":  os.Getenv("TRIM_PKGETC"),
		"env_trim_pkgvar":  os.Getenv("TRIM_PKGVAR"),
		"env_trim_appdest": os.Getenv("TRIM_APPDEST"),
	}
	if _, err := os.Stat(d.cfg.configPath()); err == nil {
		health["config_exists"] = true
	}
	if _, err := os.Stat(d.cfg.logPath()); err == nil {
		health["log_exists"] = true
	}
	d.mu.Unlock()

	// 检测 systemctl 可用性
	if _, err := exec.LookPath("systemctl"); err == nil {
		health["systemctl"] = "available"
	} else {
		health["systemctl"] = "unavailable"
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(health)
}

// handleService 服务控制（start/stop/restart）。
// v1.4.0：兼容飞牛环境 —— 检测 systemctl 可用性，不可用时提供明确错误信息；
//
//	同时支持直接信号控制（非 systemd 环境下的降级方案）。
func (d *daemon) handleService(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !d.requireSafeWrite(w, r) {
		return
	}
	action := r.URL.Query().Get("action")
	if action != "start" && action != "stop" && action != "restart" {
		http.Error(w, "bad action", 400)
		return
	}

	result := map[string]string{"result": "ok", "action": action}

	// 检测 systemctl 是否可用（Docker 容器中通常不可用）
	systemctlOK := false
	if _, err := exec.LookPath("systemctl"); err == nil {
		// 进一步确认不是 busybox 的伪 systemctl
		if out, err := exec.Command("systemctl", "--version").CombinedOutput(); err == nil && len(out) > 10 {
			systemctlOK = true
		}
	}

	if systemctlOK {
		out, err := exec.Command("systemctl", action, appName+".service").CombinedOutput()
		result["output"] = strings.TrimSpace(string(out))
		result["error"] = errStr(err)
	} else {
		// 降级方案：非 systemd 环境（如 Docker/手动运行）
		switch action {
		case "stop":
			result["output"] = "systemctl 不可用，发送 SIGTERM 请求进程退出"
			result["warn"] = "当前环境无 systemctl；已发送停止信号。若通过飞牛应用中心安装，请使用应用中心的启动/停止按钮控制服务。"
			// 向自身发送 SIGTERM（优雅退出会 disarm 看门狗）
			_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
			result["output"] = fmt.Sprintf("已向 PID %d 发送 SIGTERM", os.Getpid())
		case "restart":
			result["output"] = "systemctl 不可用，无法重启"
			result["warn"] = "当前环境无 systemctl；无法通过页面重启。请使用飞牛应用中心的重启按钮。"
		case "start":
			result["output"] = "进程已在运行（本页面即由该进程提供服务）"
			result["warn"] = "无需操作：看门狗进程正在运行中（PID=" + strconv.Itoa(os.Getpid()) + "）。"
		}
		result["error"] = ""
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (d *daemon) startHTTP() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if b, err := uiFS.ReadFile("ui/index.html"); err == nil {
			_, _ = w.Write(b)
		}
	})
	mux.HandleFunc("/api/status", d.handleStatus)
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			d.handleConfigSet(w, r)
			return
		}
		d.handleConfigGet(w, r)
	})
	mux.HandleFunc("/api/logs", d.handleLogs)
	mux.HandleFunc("/api/toggle", d.handleToggle)
	mux.HandleFunc("/api/maintain", d.handleMaintain)
	mux.HandleFunc("/api/service", d.handleService)
	mux.HandleFunc("/api/health", d.handleHealth)
	mux.HandleFunc("/api/purge-data", d.handlePurgeData) // v1.5.0: 标记卸载时清理数据
	mux.HandleFunc("/api/clear-data", d.handleClearData) // v1.5.0: 立即清除应用数据

	// 反代/网关兼容：兼容三种访问方式，后端统一归一化到根路径后再交给 mux：
	//   1. 直接访问（无前缀）：/api/status、/
	//   2. fnOS 桌面网关：/apps/{appname}/main/api/status、/apps/{appname}/main/
	//   3. 任意域名反代子路径（nginx/隧道等「未 strip 前缀」配置）：
	//      /mypath/api/status、/mypath/ —— 把首个 /api/ 之前整段当作反代前缀剥离
	// 关键：无论反代是否 strip 前缀，前端都用 location.pathname 算出子路径作为 API 前缀，
	//      后端同时兼容 strip（收到干净 /api/...）与未 strip（收到 /子路径/api/...）两种转发。
	gatewayPrefix := fmt.Sprintf("/apps/%s/main", appName)
	normalizePath := func(raw string) string {
		if strings.HasPrefix(raw, gatewayPrefix) {
			raw = strings.TrimPrefix(raw, gatewayPrefix)
			if raw == "" {
				raw = "/"
			}
			return raw
		}
		// 反代未 strip 前缀：/子路径/api/status → /api/status（idx>=0 含根访问 /api/... 的情况）
		if idx := strings.Index(raw, "/api/"); idx >= 0 {
			return raw[idx:]
		}
		// 其余（含 / 与任意未识别子路径根访问）一律交给 "/" 路由返回 index.html
		return "/"
	}
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// favicon 直接 204，避免被归一化为 "/" 后回吐 index.html 污染日志
		if r.URL.Path == "/favicon.ico" || strings.HasSuffix(r.URL.Path, "/favicon.ico") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		r.URL.Path = normalizePath(r.URL.Path)
		mux.ServeHTTP(w, r)
	})

	d.mu.Lock()
	port := d.cfg.Port
	d.mu.Unlock()
	// P0 安全（v1.9.0）：默认只绑 127.0.0.1。fnOS 网关与用户反代都在本机转发到 127.0.0.1:port，
	// 因此远程访问不受影响；但局域网内裸 IP:8900 不再可被任意设备访问，杜绝未授权读写。
	// 仅当用户确需"直连非本机访问"时，设置环境变量 KANMENDOG_BIND_ADDR=0.0.0.0 放开。
	bindHost := envOr("KANMENDOG_BIND_ADDR", "127.0.0.1")
	if bindHost == "" {
		bindHost = "127.0.0.1"
	}
	addr := net.JoinHostPort(bindHost, strconv.Itoa(port))
	logf("Web 服务启动于 %s（网关前缀 %s，已兼容域名反代子路径；绑定仅本机 127.0.0.1，如需局域网直连设 KANMENDOG_BIND_ADDR=0.0.0.0）", addr, gatewayPrefix)

	// 端口监听失败重试；仍失败则解除看门狗并阻塞，避免在 nowayout 机器上陷入"硬复位循环"。
	var ln net.Listener
	var err error
	for i := 0; i < 5; i++ {
		ln, err = net.Listen("tcp", addr)
		if err == nil {
			break
		}
		logf("HTTP 端口 %s 监听失败（第 %d 次重试）: %v", addr, i+1, err)
		time.Sleep(time.Second)
	}
	if err != nil {
		logf("!!! HTTP 端口 %s 持续无法监听：已解除看门狗并阻塞进程，避免 nowayout 机器陷入硬复位循环（请通过 fnOS 应用中心重启服务或检查端口占用）", addr)
		d.mu.Lock()
		if d.wd != nil {
			d.wd.disarm()
			d.wd = nil
		}
		d.monitoring = false
		d.mu.Unlock()
		select {}
	}
	srv := &http.Server{Handler: root, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(ln); err != nil {
		logf("HTTP 服务错误: %v", err)
	}
}

func systemctlControl(action string) int {
	cmd := exec.Command("systemctl", action, appName+".service")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return 1
	}
	return 0
}

// gracefulShutdown 安全退出：先解除看门狗（magic close）再退出进程。
// 任何"停止服务/系统正常关机重启"路径（systemctl stop、飞牛应用中心停止、
// systemd shutdown 发来的 SIGTERM）都走这里，杜绝停止后 60 秒被硬件看门狗硬复位。
func (d *daemon) gracefulShutdown(sig os.Signal) {
	logf("收到信号 %v：安全解除看门狗后退出（避免停止服务/正常重启触发硬复位）", sig)
	d.mu.Lock()
	if d.wd != nil {
		d.wd.disarm()
		d.wd = nil
	}
	d.monitoring = false
	d.mu.Unlock()
	// 给日志落盘一点时间
	time.Sleep(200 * time.Millisecond)
	os.Exit(0)
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "start", "stop", "restart", "status":
			os.Exit(systemctlControl(os.Args[1]))
		}
	}

	cfg := defaultConfig()
	gLog = newLogger(cfg.logPath())
	loadOrInitConfig(&cfg)

	d := &daemon{cfg: cfg, startTime: time.Now()}

	// v1.4.0：先启动 HTTP 服务（让用户立即能看到页面），再做耗时初始化
	go func() {
		// 开机自检：本次重启来源标注
		d.classifyBoot()

		// 启动时完整记录硬件环境信息（对复盘"为什么没兜住"至关重要）
		logf("=== 硬件环境信息 ===")
		logf("内核: %s", readKernelVersion())
		logf("CPU: %s", readCPUModel())
		logf("总内存: %d MB", readTotalMemoryMB())
		logf("看门狗身份: %s", readWatchdogIdentity())
		logf("看门狗 nowayout: %v（true=不可 magic close，停止服务也会在超时后硬复位）", readNowayout())
		logf("配置超时: %d 秒", cfg.WatchdogTimeoutSec)
		logf("配置路径: %s", cfg.configPath())
		logf("日志路径: %s", cfg.logPath())
		logf("=== 硬件环境信息结束 ===")

		d.ensureWatchdog()

		logf("初始化完成，开始健康检查循环")
	}()

	// 优雅退出（v1.2.0 核心修复）：systemd 停止/关机/重启先发 SIGTERM，
	// 这里必须在退出前 magic close 解除看门狗。
	// 注意：真死机时本进程与 systemd 一并被冻结，本 handler 不会执行，
	// fd 保持打开且停止喂狗，硬件看门狗照常兜底复位 —— 两不冲突。
	// v1.9.0：signal 处理 goroutine 加 recover()，防止 panic 导致进程退出后看门狗被饿死。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logf("!!! signal goroutine panic（已捕获）: %v", r)
			}
		}()
		for sig := range sigCh {
			d.gracefulShutdown(sig)
		}
	}()

	// v1.9.0：独立喂狗 supervisor——与判定循环解耦，任何业务层 panic 都不会饿死硬件看门狗
	go d.supervisor()

	go d.loop()
	d.startHTTP()
}

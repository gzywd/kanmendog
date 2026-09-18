// 看门狗 KanmenDog —— fnOS 死机自动重启守护进程
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
//   宁可 3 分钟后发现异常，绝不把正常使用/升级/重启误判成死机。
//   - 停止服务/正常关机重启先 magic close 解除看门狗（SIGTERM 处理器）。
//   - 升级（dpkg 锁/升级进程）/关机流程/开机冷却期/手动维护模式：只喂狗不判定。
//   - 判定与喂狗解耦：失败期间持续喂狗，达阈值决定重启才停喂。
//   - 默认 18 个周期（3 分钟）连续异常才判定；负载检查默认关；内存阈值 2%。
package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

//go:embed ui/index.html
var uiFS embed.FS

const (
	appName        = "com.gzywd.kanmendog"
	appVersion     = "1.3.0"
	defaultPort    = 8900
	watchdogDevice = "/dev/watchdog"
	logMaxBytes    = 5 * 1024 * 1024  // 日志滚动阈值
	trendMaxBytes  = 20 * 1024 * 1024 // 趋势 csv 滚动阈值
)

// Config 应用配置，持久化到 TRIM_PKGETC/config.json
type Config struct {
	Enabled               bool    `json:"enabled"`                // 监控总开关（网页一键开关）
	IntervalSec           int     `json:"interval_sec"`           // 健康检查/喂狗周期
	WatchdogTimeoutSec    int     `json:"watchdog_timeout_sec"`   // 看门狗硬件超时（ioctl 设置，非所有驱动支持）
	FailThreshold         int     `json:"fail_threshold"`         // 连续失败多少次判定死机（默认18=3分钟）
	Port                  int     `json:"port"`                   // Web 服务端口
	AutoReboot            bool    `json:"auto_reboot"`            // 判定死机后是否自动重启
	BootGraceMin          int     `json:"boot_grace_min"`         // 开机冷却期（分钟）：期间只喂狗不判定
	CheckFork             bool    `json:"check_fork"`             // fork 存活测试（零误报）
	CheckLoad             bool    `json:"check_load"`             // 负载测试（高负载≠死机，默认关）
	LoadThreshold         float64 `json:"load_threshold"`         // 1 分钟负载均值阈值
	CheckDState           bool    `json:"check_dstate"`           // 不可中断(D)进程数测试
	DStateThreshold       int     `json:"dstate_threshold"`       // D 状态进程数阈值
	CheckMemory           bool    `json:"check_memory"`           // 内存可用率测试（针对 OOM 死机）
	MemAvailableThreshold int     `json:"mem_available_threshold"` // 可用内存低于该百分比(%)判定异常
	CheckServiceProbe     bool    `json:"check_service_probe"`    // 外部命令探针（默认关！端口须与飞牛一致）
	ServiceProbeCmd       string  `json:"service_probe_cmd"`      // 必须返回 0 的命令
	TrendInterval         int     `json:"trend_interval"`         // 趋势日志间隔（周期数，0=关闭）
	MaintainUntil         int64   `json:"maintain_until"`         // 手动维护模式截止时刻（unix 秒，0=无；期间只喂狗不判定）
}

// defaultConfig 保守默认值：宁可晚 3 分钟发现死机，绝不误杀正常负载。
// 注意：探针默认关闭 —— fnOS 真实管理端口因机器而异（新版默认 5666，旧版 8000/8001，
// 80 仅是可选的重定向）。安装向导会让用户知情勾选，安装脚本探测端口后预填命令。
func defaultConfig() Config {
	return Config{
		Enabled:               true,
		IntervalSec:           10,
		WatchdogTimeoutSec:    60,
		FailThreshold:         18, // 10s 周期 × 18 = 3 分钟连续异常才判定
		Port:                  defaultPort,
		AutoReboot:            true,
		BootGraceMin:          5, // 开机 5 分钟内只喂狗不判定
		CheckFork:             true,
		CheckLoad:             false, // 高负载不等于死机，默认关闭
		LoadThreshold:         16,
		CheckDState:           true,
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

func (c *Config) etcDir() string  { return envOr("TRIM_PKGETC", "/etc/kanmendog") }
func (c *Config) varDir() string  { return envOr("TRIM_PKGVAR", "/var/lib/kanmendog") }
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

func (l *logger) tailLines(n int) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.buf.String()
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
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
	fd int
}

// ioctl 编号（'W' = 0x57）
const (
	wdIOCKeepAlive  = 0x5705      // _IO('W',5)
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

func openWatchdog(timeoutSec int) (w *watchdog, actualTimeout int, err error) {
	fd, err := syscall.Open(watchdogDevice, syscall.O_WRONLY, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("打开 %s 失败: %v", watchdogDevice, err)
	}
	w = &watchdog{fd: fd}
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
		return nil, 0, err
	}
	return w, actualTimeout, nil
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
	if fi, err := os.Stat("/run/systemd/shutdown"); err == nil && fi.IsDir() {
		return "systemd 正在关机/重启流程中"
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

// dpkgLockHeld 用 fcntl 写锁探测 dpkg 锁是否被占用（dpkg 使用 POSIX fcntl 锁而非 flock）
func dpkgLockHeld(path string) bool {
	fd, err := syscall.Open(path, syscall.O_RDWR, 0)
	if err != nil {
		return false // 文件不存在等：不视为升级中
	}
	defer syscall.Close(fd)
	const seekSet = 0 // SEEK_SET
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: seekSet, Start: 0, Len: 0}
	if err := syscall.FcntlFlock(uintptr(fd), syscall.F_SETLK, &lk); err != nil {
		return true // EAGAIN/EACCES：锁被他人持有
	}
	ul := syscall.Flock_t{Type: syscall.F_UNLCK, Whence: seekSet, Start: 0, Len: 0}
	_ = syscall.FcntlFlock(uintptr(fd), syscall.F_SETLK, &ul)
	return false
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
	bootApp       bootKind = "app"       // 本程序判定死机触发
	bootPanic     bootKind = "panic"     // 内核 panic（lockup→panic 路径）
	bootAbnormal  bootKind = "abnormal"  // 无干净关机记录：疑似硬件看门狗复位/断电/冻结
	bootNormal    bootKind = "normal"    // 正常关机后开机
	bootUnknown   bootKind = "unknown"   // 无法判定（last/journalctl 不可用）
)

var bootKindTitle = map[bootKind]string{
	bootApp:      "本程序判定死机触发重启",
	bootPanic:    "内核 panic 重启（lockup→panic 路径）",
	bootAbnormal: "异常重启（无干净关机记录，疑似硬件看门狗复位/冻结/断电）",
	bootNormal:   "正常关机后开机",
	bootUnknown:  "未知（无法读取 wtmp/上次启动日志）",
}

// parseLastShutdownTime 解析 `last -x -F shutdown` 最新一条 shutdown 记录的时间。
// 输出形如：`shutdown system down  Wed Sep 17 09:12:34 2026`（-F 全时间格式）
func parseLastShutdownTime(out string) (time.Time, bool) {
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "shutdown") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		// 时间字段为最后 5 个：周 月 日 时:分:秒 年
		ts := strings.Join(fields[len(fields)-5:], " ")
		if t, err := time.Parse("Mon Jan _2 15:04:05 2006", ts); err == nil {
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
//   1) last_reboot_reason 的时间戳落在本次开机前 30 分钟内 → 本程序判定触发；
//   2) 最近一次 wtmp shutdown 记录在本次开机前 10 分钟内 → 正常关机；
//   3) journalctl 上次 boot 内核日志含 panic → panic 重启；
//   4) 否则 → 异常重启（疑似硬件看门狗复位/断电/冻结）。
// 结果写入 boot_reason 文件（状态页优先展示它而非旧的 last_reboot_reason，
// 消除"硬件复位后旧原因误导排查方向"的问题）。
func (d *daemon) classifyBoot() {
	now := time.Now()
	upSec := uptimeMinutes() * 60
	bootTime := now.Add(-time.Duration(upSec) * time.Second)

	kind, detail := bootUnknown, ""

	// 1) 本程序判定？读 last_reboot_reason 首行时间戳
	if b, err := os.ReadFile(d.cfg.reasonPath()); err == nil {
		firstLine := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
		if t, err := time.Parse(time.RFC3339, firstLine); err == nil {
			if t.Before(bootTime) && bootTime.Sub(t) < 30*time.Minute {
				kind = bootApp
				detail = "判定时刻 " + t.Format("2006-01-02 15:04:05") + "，原因详见上次死机判定记录"
			}
		}
	}

	// 2) 正常关机？
	if kind == bootUnknown {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := exec.CommandContext(ctx, "last", "-x", "-F", "shutdown").Output()
		cancel()
		if err == nil {
			if st, ok := parseLastShutdownTime(string(out)); ok {
				if st.Before(bootTime) && bootTime.Sub(st) < 10*time.Minute {
					kind = bootNormal
					detail = "上次关机时刻 " + st.Format("2006-01-02 15:04:05")
				} else {
					// 有 shutdown 记录但离本次开机太远 —— 本次开机前发生过异常断电/复位
					kind = bootAbnormal
					detail = "最近一次干净关机在 " + st.Format("2006-01-02 15:04:05") + "，与本次开机间隔过长"
				}
			}
		}
	}

	// 3) panic 细分（仅异常重启时查，journalctl 查询有成本）
	if kind == bootAbnormal && lastBootHadPanic() {
		kind = bootPanic
		detail = "上次启动的内核日志含 panic/oops 记录"
	}

	title := bootKindTitle[kind]
	gLog.Write([]byte(fmt.Sprintf("[%s] 开机自检：本次重启来源 = %s%s%s\n",
		now.Format(time.RFC3339), title,
		func() string { if detail != "" { return " —— " + detail } ; return "" }(),
		func() string { if detail != "" { return "（" + detail + "）" } ; return "" }(),
	)))

	// 写 boot_reason（每次开机重写；状态页优先展示）
	content := fmt.Sprintf("%s\n%s\n%s\n开机时刻: %s\n%s\n",
		now.Format(time.RFC3339), title, detail,
		bootTime.Format("2006-01-02 15:04:05"),
		"提示: 异常重启时请在飞牛终端执行 journalctl -b -1 -k 与 last -x 复查根因")
	_ = os.WriteFile(d.cfg.bootReasonPath(), []byte(content), 0o644)

	// 若本次重启与本程序判定无关，把旧 last_reboot_reason 归档，避免状态页误导
	if kind != bootApp {
		if _, err := os.Stat(d.cfg.reasonPath()); err == nil {
			_ = os.Rename(d.cfg.reasonPath(), d.cfg.reasonPath()+".stale")
			gLog.Write([]byte(fmt.Sprintf("[%s] 旧死机判定记录与本次重启无关，已归档为 last_reboot_reason.stale（防止误导排查）\n",
				now.Format(time.RFC3339))))
		}
	}
}

// ---- 健康检查 ----

type checkResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// runFork 尝试 fork 一个新进程；内核调度挂死会失败
func runFork(ctx context.Context) checkResult {
	r := checkResult{Name: "fork 存活", OK: true}
	done := make(chan error, 1)
	go func() {
		cmd := exec.Command("/bin/true")
		done <- cmd.Run()
	}()
	select {
	case err := <-done:
		if err != nil {
			r.OK, r.Detail = false, fmt.Sprintf("fork/exec 失败: %v", err)
		}
	case <-ctx.Done():
		r.OK, r.Detail = false, "fork 测试超时（调度可能卡死）"
	}
	return r
}

// runLoad 读取 /proc/loadavg 的一分钟负载
func runLoad(ctx context.Context, threshold float64) checkResult {
	r := checkResult{Name: "系统负载", OK: true}
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
		r.OK, r.Detail = false, "读取负载超时"
	}
	return r
}

// runDState 统计不可中断(D)状态进程数
func runDState(ctx context.Context, threshold int) checkResult {
	r := checkResult{Name: "D状态进程", OK: true}
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
		r.OK, r.Detail = false, "扫描 /proc 超时"
	}
	return r
}

// runServiceProbe 运行用户命令，必须返回 0。
// 注意：命令里的端口/地址必须与飞牛真实管理端口一致，否则会误判！
// 学习期保护：探针从未成功过之前，失败不计入判定（见 loop）。
func runServiceProbe(ctx context.Context, cmdStr string) checkResult {
	r := checkResult{Name: "服务探针", OK: true, Detail: cmdStr}
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
func probeCandidatePorts() int {
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
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		cmd := fmt.Sprintf("curl %s -s -o /dev/null -m 3 %s://127.0.0.1:%d/", k, scheme, p)
		err := exec.CommandContext(ctx, "/bin/sh", "-c", cmd).Run()
		cancel()
		if err == nil {
			return p
		}
	}
	return 0
}

// runMemory 检查内存可用率；可用率低于阈值(%)判定异常，直接命中 OOM 类死机。
// 若内核未提供 MemAvailable（老内核）则跳过判定，不误杀。
func runMemory(ctx context.Context, thresholdPct int) checkResult {
	r := checkResult{Name: "内存可用率", OK: true}
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
		r.OK, r.Detail = false, "读取 /proc/meminfo 超时"
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
	if _, err := os.Stat(watchdogDevice); err == nil {
		return "unknown(dev-exists)"
	}
	return "none"
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
	Running          bool   `json:"running"`
	Enabled          bool   `json:"enabled"`
	WatchdogOpen     bool   `json:"watchdog_open"`
	WatchdogDevice   string `json:"watchdog_device"`
	WatchdogIdentity string `json:"watchdog_identity"` // 硬件(iTCO_wdt) / 软件(softdog)
	WatchdogTimeout  int    `json:"watchdog_timeout"`  // 驱动实际生效超时
	Version          string `json:"version"`
	Uptime           string `json:"uptime"`
	CurrentLoad      string `json:"current_load"`
	DStateCount      int    `json:"dstate_count"`
	MemoryUsedPct    int    `json:"memory_used_pct"`
	MemAvailableMB   int    `json:"mem_available_mb"`
	TrendEnabled     bool   `json:"trend_enabled"`
	LastRebootReason string `json:"last_reboot_reason"`
	ConsecutiveFails int    `json:"consecutive_fails"`
	Pid              int    `json:"pid"`
	InBootGrace      bool   `json:"in_boot_grace"`  // 开机冷却期内
	UpgradeBusy      string `json:"upgrade_busy"`   // 非空=维护窗口原因（升级/关机中）
	BootReason       string `json:"boot_reason"`    // 本次开机自检结论（来源标注）
	MaintainLeftMin  int    `json:"maintain_left_min"` // 手动维护模式剩余分钟（0=无）
	ProbeLearning    bool   `json:"probe_learning"`    // 探针处于学习期（未计入判定）
}

// ---- 主守护进程 ----

type daemon struct {
	cfg        Config
	mu         sync.Mutex
	wd         *watchdog
	wdTimeout  int // 驱动实际生效的看门狗超时
	monitoring bool // 当前是否在喂狗（enabled 且已开狗）
	failCount  int
	lastReason string
	startTime  time.Time
	cycle      int      // 健康检查周期计数，用于趋势日志
	probeEverOK bool    // 探针自本次进程启动以来是否成功过（学习期判定）
	wasBusy    bool     // 上一周期是否处于维护窗口（边沿检测用）
	portWarned  bool    // 端口迁移警告是否已发出（避免重复告警）
}

func loadOrInitConfig(cfg *Config) {
	p := cfg.configPath()
	b, err := os.ReadFile(p)
	if err != nil {
		// 首次运行：写默认配置
		_ = os.MkdirAll(cfg.etcDir(), 0o755)
		saveConfig(cfg)
		return
	}
	var loaded Config
	if err := json.Unmarshal(b, &loaded); err == nil {
		*cfg = loaded
	}
}

func saveConfig(cfg *Config) error {
	_ = os.MkdirAll(cfg.etcDir(), 0o755)
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cfg.configPath(), b, 0o644)
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
			} else {
				d.wd = w
				d.wdTimeout = actual
				id := readWatchdogIdentity()
				kind := "软件看门狗(softdog)"
				if !strings.Contains(id, "softdog") && id != "none" && id != "" {
					kind = "硬件看门狗"
				}
				logf("看门狗已开启: %s identity=%s (%s, 驱动实际超时 %d 秒)", watchdogDevice, id, kind, actual)
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

	// 第一段：毫秒级落盘最小事实（防快照收集卡死丢证据）
	minimal := ts + "\n" + reason + "\n\n(完整快照采集于判定时刻，若下方缺失说明系统在采集时已濒死)\n"
	_ = os.WriteFile(d.cfg.reasonPath(), []byte(minimal), 0o644)
	gLog.Write([]byte(fmt.Sprintf("[%s] !!! 判定系统死机，准备重启 !!!\n原因: %s\n", ts, reason)))

	if !d.cfg.AutoReboot {
		logf("auto_reboot=false，仅记录不重启")
		return
	}

	// 第二段：完整快照（可能较慢）追加落盘
	snapshot := systemSnapshot()
	if readOOMLog() != "" {
		logf("快照含内核OOM证据（详见 oom_kernel_log）")
	}
	_ = os.WriteFile(d.cfg.reasonPath(), []byte(ts+"\n"+reason+"\n\n"+snapshot), 0o644)
	gLog.Write([]byte(fmt.Sprintf("[%s] 重启前快照已落盘:\n%s\n", ts, snapshot)))

	// 停止喂狗（保留 fd 打开、不 magic close），让硬件看门狗在超时后强制复位
	d.mu.Lock()
	d.monitoring = false
	d.mu.Unlock()

	// 尝试优雅重启
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART); err != nil {
		logf("syscall reboot 失败(%v)，尝试 /sbin/reboot", err)
		_ = exec.Command("/sbin/reboot").Run()
	}
	// 若仍未重启（系统已冻），硬件看门狗将兜底复位
	time.Sleep(time.Duration(d.cfg.WatchdogTimeoutSec+5) * time.Second)
}

// loop 主循环。判定与喂狗解耦：健康检查失败期间仍持续喂狗，
// 只有真正决定重启时才停止喂狗 —— 单次抖动绝不启动硬件复位倒计时。
func (d *daemon) loop() {
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
			d.petWatchdog()
			logThrottled("boot-grace", "开机冷却期内：只喂狗不判定（防开机初始化高负载误判）")
			time.Sleep(time.Duration(cfg.IntervalSec) * time.Second)
			continue
		}

		// ② 手动维护模式：用户显式要求暂停判定（飞牛升级/手工维护的保底开关）
		if d.inMaintainMode() {
			d.petWatchdog()
			logThrottled("maintain", "手动维护模式中：只喂狗不判定（到期自动恢复）")
			time.Sleep(time.Duration(cfg.IntervalSec) * time.Second)
			continue
		}

		// ③ 自动维护窗口（系统升级/关机/重启进行中）：继续喂狗但绝不判定死机。
		//    若窗口内系统真冻结，本进程随之冻结停止喂狗，硬件看门狗自然兜底。
		if busy := systemBusyReason(); busy != "" {
			d.petWatchdog()
			logThrottled("busy", "维护窗口（"+busy+"）：继续喂狗，暂停死机判定")
			d.mu.Lock()
			wasBusy := d.wasBusy
			d.wasBusy = true
			d.mu.Unlock()
			if !wasBusy {
				logf("进入维护窗口（%s）：历史失败计数清零", busy)
				d.mu.Lock()
				d.failCount = 0
				d.mu.Unlock()
			}
			time.Sleep(time.Duration(cfg.IntervalSec) * time.Second)
			continue
		}
		// 维护窗口退出边沿：清零失败计数（窗口前积累的历史样本作废）
		d.mu.Lock()
		wasBusy := d.wasBusy
		d.wasBusy = false
		d.mu.Unlock()
		if wasBusy {
			logf("维护窗口结束：恢复死机判定，历史失败计数已清零")
			d.mu.Lock()
			d.failCount = 0
			d.mu.Unlock()
		}

		// ④ 健康检查（带整体超时，防止单次检查卡死导致喂狗中断）
		cycleCtx, cancel := context.WithTimeout(context.Background(),
			time.Duration(cfg.IntervalSec)*time.Second*4/5)
		var results []checkResult
		if cfg.CheckFork {
			results = append(results, runFork(cycleCtx))
		}
		if cfg.CheckLoad {
			results = append(results, runLoad(cycleCtx, cfg.LoadThreshold))
		}
		if cfg.CheckDState {
			results = append(results, runDState(cycleCtx, cfg.DStateThreshold))
		}
		if cfg.CheckMemory {
			results = append(results, runMemory(cycleCtx, cfg.MemAvailableThreshold))
		}
		if cfg.CheckServiceProbe {
			results = append(results, runServiceProbe(cycleCtx, cfg.ServiceProbeCmd))
		}
		cancel()

		allOK := true
		var failed []string
		probeFailed := false
		for _, r := range results {
			if !r.OK {
				allOK = false
				if r.Name == "服务探针" {
					probeFailed = true
				}
				failed = append(failed, fmt.Sprintf("%s: %s", r.Name, r.Detail))
			}
		}

		// 探针学习期：探针开启但从未成功过 → 失败不计入判定（配置错误/服务未起防误杀）
		d.mu.Lock()
		if !probeFailed {
			d.probeEverOK = true
		}
		probeLearning := cfg.CheckServiceProbe && cfg.ServiceProbeCmd != "" && !d.probeEverOK
		d.mu.Unlock()
		if probeLearning && probeFailed && len(failed) > 0 {
			// 剔除探针失败项，仅当其余检查全通过时不计数
			othersOK := true
			for _, r := range results {
				if !r.OK && r.Name != "服务探针" {
					othersOK = false
				}
			}
			if othersOK {
				allOK = true
				logThrottled("probe-learn", "探针尚未成功过（学习期）：失败暂不计入判定，请核对探针命令与端口（新版飞牛默认 5666，非 80）")
			}
		}

		// 端口迁移检测（v1.3.0 防误报）：探针失败次数过半时，检查飞牛管理端口是否变了
		if probeFailed && !probeLearning {
			d.mu.Lock()
			fc := d.failCount + 1
			warned := d.portWarned
			d.mu.Unlock()
			if fc >= cfg.FailThreshold/2 && fc < cfg.FailThreshold && !warned {
				if port := probeCandidatePorts(); port > 0 {
					logf("⚠️ 疑似飞牛管理端口已迁移：探针配置的端口无响应，但端口 %d 有飞牛 Web 响应。这可能是端口变更而非死机！请到页面更新探针命令。本次失败计数清零（防误报）", port)
					d.mu.Lock()
					d.portWarned = true
					d.failCount = 0
					d.mu.Unlock()
				}
			}
		}

		d.mu.Lock()
		if allOK {
			d.failCount = 0
			d.portWarned = false // 状态恢复后重置端口迁移警告
		} else {
			d.failCount++
		}
		fc := d.failCount
		d.mu.Unlock()

		if allOK {
			d.petWatchdog()
		} else {
			reason := strings.Join(failed, " | ")
			logf("健康检查失败(%d/%d): %s", fc, cfg.FailThreshold, reason)
			// 关键：失败期间仍继续喂狗。只有达到阈值决定重启才停喂（见 triggerReboot）。
			d.petWatchdog()
			if fc >= cfg.FailThreshold {
				d.triggerReboot(reason)
				d.mu.Lock()
				d.failCount = 0
				d.mu.Unlock()
			}
		}

		time.Sleep(time.Duration(cfg.IntervalSec) * time.Second)
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

// readBootReasonSummary 读开机自检结论摘要（前两行：时间+来源标题）
func (d *daemon) readBootReasonSummary() string {
	b, err := os.ReadFile(d.cfg.bootReasonPath())
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) >= 2 {
		return lines[1] // 来源标题行
	}
	return ""
}

func (d *daemon) buildStatus() status {
	d.mu.Lock()
	enabled := d.cfg.Enabled
	wd := d.wd
	fails := d.failCount
	bootGrace := d.cfg.BootGraceMin
	maintainUntil := d.cfg.MaintainUntil
	wdTimeout := d.wdTimeout
	probeEverOK := d.probeEverOK
	probeOn := d.cfg.CheckServiceProbe && d.cfg.ServiceProbeCmd != ""
	d.mu.Unlock()
	reason := ""
	if b, err := os.ReadFile(d.cfg.reasonPath()); err == nil {
		reason = strings.TrimSpace(string(b))
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
		timeoutShown = d.cfg.WatchdogTimeoutSec
	}
	return status{
		Running:          true,
		Enabled:          enabled,
		WatchdogOpen:     wd != nil,
		WatchdogDevice:   watchdogDevice,
		WatchdogIdentity: id,
		WatchdogTimeout:  timeoutShown,
		Version:          appVersion,
		Uptime:           readUptime(),
		CurrentLoad:      d.currentLoad(),
		DStateCount:      d.dstateCount(),
		MemoryUsedPct:    usedPct,
		MemAvailableMB:   availMB,
		TrendEnabled:     d.cfg.TrendInterval > 0,
		LastRebootReason: reason,
		ConsecutiveFails: fails,
		Pid:              os.Getpid(),
		InBootGrace:      upMin < float64(bootGrace),
		UpgradeBusy:      busy,
		BootReason:       d.readBootReasonSummary(),
		MaintainLeftMin:  maintainLeft,
		ProbeLearning:    probeOn && !probeEverOK,
	}
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

// handleService 控制 systemd 服务（真实启停整个进程）
func (d *daemon) handleService(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	action := r.URL.Query().Get("action")
	if action != "start" && action != "stop" && action != "restart" {
		http.Error(w, "bad action", 400)
		return
	}
	out, err := exec.Command("systemctl", action, appName+".service").CombinedOutput()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"result": "ok", "action": action,
		"output": strings.TrimSpace(string(out)), "error": errStr(err),
	})
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

	d.mu.Lock()
	port := d.cfg.Port
	d.mu.Unlock()
	addr := fmt.Sprintf(":%d", port)
	logf("Web 服务启动于 %s", addr)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
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

	// 开机自检：本次重启来源标注（v1.3.0 信任闭环 —— 消除旧原因误导 + 补硬件复位盲区）
	d.classifyBoot()

	d.ensureWatchdog()

	// 优雅退出（v1.2.0 核心修复）：systemd 停止/关机/重启先发 SIGTERM，
	// 这里必须在退出前 magic close 解除看门狗。
	// 注意：真死机时本进程与 systemd 一并被冻结，本 handler 不会执行，
	// fd 保持打开且停止喂狗，硬件看门狗照常兜底复位 —— 两不冲突。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for sig := range sigCh {
			d.gracefulShutdown(sig)
		}
	}()

	go d.loop()
	d.startHTTP()
}

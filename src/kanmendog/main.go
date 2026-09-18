// 看门狗 KanmenDog —— fnOS 死机自动重启守护进程
//
// 设计要点（严格复用飞牛底层系统轮子，不重复造）：
//   - 内核看门狗 /dev/watchdog（硬件 iTCO_wdt 或软件 softdog）作为终极复位手段，
//     独立于内核调度：守护进程周期"喂狗"，一旦卡死不再喂狗，硬件倒计时归零强制重启。
//   - systemd 负责进程拉起（Restart=always），本程序由 cmd/main 通过 systemctl 管理。
//   - 内核 lockup 检测（softlockup_panic / hardlockup_panic，由 install_callback 写入
//     /etc/sysctl.d）把内核级卡死转化为 panic，进而触发看门狗复位。
//   - 健康检查用标准原语：/proc/loadavg、/proc/*/stat、fork()、可选外部命令。
//
// 运行模式：
//   不带参数            —— 以守护进程方式运行（健康检查 + 喂狗 + 内置 HTTP 服务）
//   start|stop|status   —— 供 cmd/main 调用，等价于 systemctl 控制（保留兼容）
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
	appVersion     = "1.0.0"
	defaultPort    = 8900
	watchdogDevice = "/dev/watchdog"
	logMaxBytes    = 5 * 1024 * 1024 // 日志滚动阈值
)

// Config 应用配置，持久化到 TRIM_PKGETC/config.json
type Config struct {
	Enabled            bool    `json:"enabled"`              // 监控总开关（网页一键开关）
	IntervalSec        int     `json:"interval_sec"`         // 健康检查/喂狗周期
	WatchdogTimeoutSec int     `json:"watchdog_timeout_sec"` // 看门狗硬件超时（ioctl 设置，非所有驱动支持）
	FailThreshold      int     `json:"fail_threshold"`       // 连续失败多少次判定死机
	Port               int     `json:"port"`                 // Web 服务端口
	AutoReboot         bool    `json:"auto_reboot"`          // 判定死机后是否自动重启
	CheckFork          bool    `json:"check_fork"`           // fork 存活测试
	CheckLoad          bool    `json:"check_load"`           // 负载测试
	LoadThreshold      float64 `json:"load_threshold"`       // 1 分钟负载均值阈值
	CheckDState        bool    `json:"check_dstate"`         // 不可中断(D)进程数测试
	DStateThreshold    int     `json:"dstate_threshold"`     // D 状态进程数阈值
	CheckServiceProbe  bool    `json:"check_service_probe"`  // 外部命令探针
	ServiceProbeCmd    string  `json:"service_probe_cmd"`    // 必须返回 0 的命令
}

func defaultConfig() Config {
	return Config{
		Enabled:            true,
		IntervalSec:        10,
		WatchdogTimeoutSec: 60,
		FailThreshold:      3,
		Port:               defaultPort,
		AutoReboot:         true,
		CheckFork:          true,
		CheckLoad:          true,
		LoadThreshold:      16,
		CheckDState:        true,
		DStateThreshold:    20,
		CheckServiceProbe:  false,
		ServiceProbeCmd:    "",
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

// ---- 日志 ----

type logger struct {
	mu  sync.Mutex
	f   *os.File
	path string
	buf *strings.Builder // 最近日志内存副本，供网页实时查看
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

// logPathSafe 避免递归调用 logPath（logger 未存路径），由调用方传
var gLog *logger

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

// ioctlSetIntPtr 通过 SYS_IOCTL 设置看门狗超时（按指针回写实际值）
func ioctlSetIntPtr(fd int, req uint, val *int) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(unsafe.Pointer(val)))
	if errno != 0 {
		return errno
	}
	return nil
}

func openWatchdog(timeoutSec int) (*watchdog, error) {
	fd, err := syscall.Open(watchdogDevice, syscall.O_WRONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("打开 %s 失败: %v", watchdogDevice, err)
	}
	w := &watchdog{fd: fd}
	if timeoutSec > 0 {
		// 尽力设置超时；不支持的驱动会忽略
		t := timeoutSec
		_ = ioctlSetIntPtr(fd, wdIOCSetTimeout, &t)
	}
	// 立即喂一次，确认设备可用
	if err := w.pet(); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	return w, nil
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

// ---- 健康检查 ----

type checkResult struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail"`
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

// runServiceProbe 运行用户命令，必须返回 0
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

// ---- 状态 ----

type status struct {
	Running          bool     `json:"running"`
	Enabled          bool     `json:"enabled"`
	WatchdogOpen     bool     `json:"watchdog_open"`
	WatchdogDevice   string   `json:"watchdog_device"`
	WatchdogTimeout  int      `json:"watchdog_timeout"`
	Version          string   `json:"version"`
	Uptime           string   `json:"uptime"`
	CurrentLoad      string   `json:"current_load"`
	DStateCount      int      `json:"dstate_count"`
	LastRebootReason string   `json:"last_reboot_reason"`
	ConsecutiveFails int      `json:"consecutive_fails"`
	Pid              int      `json:"pid"`
}

// ---- 主守护进程 ----

type daemon struct {
	cfg           Config
	mu            sync.Mutex
	wd            *watchdog
	monitoring    bool // 当前是否在喂狗（enabled 且已开狗）
	failCount     int
	lastReason    string
	startTime     time.Time
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

// systemSnapshot 收集重启前的系统快照，便于排查
func systemSnapshot() string {
	var sb strings.Builder
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		sb.WriteString("loadavg: " + strings.TrimSpace(string(b)) + "\n")
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		lines := strings.Split(string(b), "\n")
		for _, l := range lines {
			if strings.HasPrefix(l, "MemTotal:") || strings.HasPrefix(l, "MemAvailable:") {
				sb.WriteString(l + "\n")
			}
		}
	}
	d := runDState(context.Background(), 999999)
	sb.WriteString("dstate: " + d.Detail + "\n")
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
			w, err := openWatchdog(d.cfg.WatchdogTimeoutSec)
			if err != nil {
				gLog.Write([]byte(fmt.Sprintf("[%s] 看门狗打开失败（将无法硬复位）: %v\n",
					time.Now().Format(time.RFC3339), err)))
				d.wd = nil
			} else {
				d.wd = w
				gLog.Write([]byte(fmt.Sprintf("[%s] 看门狗已开启: %s (超时约%d秒)\n",
					time.Now().Format(time.RFC3339), watchdogDevice, d.cfg.WatchdogTimeoutSec)))
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

// triggerReboot 记录原因并尝试重启；若系统已冻，停止喂狗由硬件复位兜底
func (d *daemon) triggerReboot(reason string) {
	ts := time.Now().Format(time.RFC3339)
	snapshot := systemSnapshot()
	msg := fmt.Sprintf("[%s] !!! 判定系统死机，准备重启 !!!\n原因: %s\n快照:\n%s",
		ts, reason, snapshot)
	gLog.Write([]byte(msg + "\n"))
	_ = os.WriteFile(d.cfg.reasonPath(), []byte(ts+"\n"+reason+"\n\n"+snapshot), 0o644)

	if !d.cfg.AutoReboot {
		gLog.Write([]byte(fmt.Sprintf("[%s] auto_reboot=false，仅记录不重启\n", ts)))
		return
	}

	// 停止喂狗（保留 fd 打开、不 magic close），让硬件看门狗在超时后强制复位
	d.mu.Lock()
	d.monitoring = false
	d.mu.Unlock()

	// 尝试优雅重启
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART); err != nil {
		gLog.Write([]byte(fmt.Sprintf("[%s] syscall reboot 失败(%v)，尝试 /sbin/reboot\n", ts, err)))
		_ = exec.Command("/sbin/reboot").Run()
	}
	// 若仍未重启（系统已冻），硬件看门狗将兜底复位
	time.Sleep(time.Duration(d.cfg.WatchdogTimeoutSec+5) * time.Second)
}

func (d *daemon) loop() {
	for {
		d.mu.Lock()
		cfg := d.cfg
		d.mu.Unlock()

		if !cfg.Enabled {
			time.Sleep(2 * time.Second)
			continue
		}

		// 健康检查（带整体超时，防止单次检查卡死导致喂狗中断）
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
		if cfg.CheckServiceProbe {
			results = append(results, runServiceProbe(cycleCtx, cfg.ServiceProbeCmd))
		}
		cancel()

		allOK := true
		var failed []string
		for _, r := range results {
			if !r.OK {
				allOK = false
				failed = append(failed, fmt.Sprintf("%s: %s", r.Name, r.Detail))
			}
		}

		d.mu.Lock()
		if allOK {
			d.failCount = 0
			if d.wd != nil {
				_ = d.wd.pet()
			}
		} else {
			d.failCount++
			reason := strings.Join(failed, " | ")
			gLog.Write([]byte(fmt.Sprintf("[%s] 健康检查失败(%d/%d): %s\n",
				time.Now().Format(time.RFC3339), d.failCount, cfg.FailThreshold, reason)))
			if d.failCount >= cfg.FailThreshold {
				d.mu.Unlock()
				d.triggerReboot(reason)
				// triggerReboot 不会返回（除非 auto_reboot=false）
				d.mu.Lock()
				d.failCount = 0
			}
		}
		d.mu.Unlock()

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
	res := runDState(context.Background(), 999999)
	// 解析数量
	n := 0
	fmt.Sscanf(res.Detail, "D状态进程 %d", &n)
	return n
}

func (d *daemon) buildStatus() status {
	d.mu.Lock()
	enabled := d.cfg.Enabled
	wd := d.wd
	fails := d.failCount
	d.mu.Unlock()
	reason := ""
	if b, err := os.ReadFile(d.cfg.reasonPath()); err == nil {
		reason = strings.TrimSpace(string(b))
	}
	return status{
		Running:          true,
		Enabled:          enabled,
		WatchdogOpen:     wd != nil,
		WatchdogDevice:   watchdogDevice,
		WatchdogTimeout:  d.cfg.WatchdogTimeoutSec,
		Version:          appVersion,
		Uptime:           readUptime(),
		CurrentLoad:      d.currentLoad(),
		DStateCount:      d.dstateCount(),
		LastRebootReason: reason,
		ConsecutiveFails: fails,
		Pid:              os.Getpid(),
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

func (d *daemon) handleConfigSet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var incoming Config
	if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
		http.Error(w, "bad json: "+err.Error(), 400)
		return
	}
	d.mu.Lock()
	d.cfg = incoming
	d.mu.Unlock()
	if err := saveConfig(&d.cfg); err != nil {
		http.Error(w, "save failed: "+err.Error(), 500)
		return
	}
	d.ensureWatchdog() // 开关/超时变化即时生效
	gLog.Write([]byte(fmt.Sprintf("[%s] 配置已更新并保存\n", time.Now().Format(time.RFC3339))))
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
	d.mu.Unlock()
	_ = saveConfig(&d.cfg)
	d.ensureWatchdog()
	state := "已开启监控"
	if !enable {
		state = "已暂停监控（看门狗已解除，系统不会自动重启）"
	}
	gLog.Write([]byte(fmt.Sprintf("[%s] %s\n", time.Now().Format(time.RFC3339), state)))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"result": "ok", "state": state})
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
	mux.HandleFunc("/api/service", d.handleService)

	addr := fmt.Sprintf(":%d", d.cfg.Port)
	gLog.Write([]byte(fmt.Sprintf("[%s] Web 服务启动于 %s\n", time.Now().Format(time.RFC3339), addr)))
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		gLog.Write([]byte(fmt.Sprintf("HTTP 服务错误: %v\n", err)))
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
	d.ensureWatchdog()

	go d.loop()
	d.startHTTP()
}

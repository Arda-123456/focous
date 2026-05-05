// focous 是一个番茄钟 + 防熄屏的桌面工具。
//
// 架构概述：
//   - 后端：Go HTTP 服务器（纯标准库，零外部依赖）
//   - 前端：单页 HTML + 原生 JS + CSS（通过 embed 打包进二进制）
//   - 实时通信：SSE（Server-Sent Events）
//   - 防熄屏：调用 Windows SetThreadExecutionState API
//
// 核心组件：
//   - internal/pomodoro  — 番茄钟计时引擎
//   - internal/screenawake — Windows 防熄屏
//   - static/            — 前端静态资源（编译时 embed）
//
// API 设计：RESTful JSON API + SSE 长连接
//   操作类接口：POST /api/start|pause|resume|reset|mode|skip|settings|screen-awake
//   查询类接口：GET  /api/state|screen-awake-state
//   实时推送：  GET  /sse (text/event-stream)
package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"focous/internal/pomodoro"
	"focous/internal/screenawake"
)

//go:embed static/*
var staticFiles embed.FS

// Server 是 HTTP 服务器的顶层结构，组合番茄钟引擎和防熄屏模块。
//
// SSE 客户端管理：
//   - clients: 保存所有活跃的 SSE 通道
//   - clientCount: 活跃客户端计数（用于自动关闭判断）
//   - shutdown: 当所有客户端断开且 3 秒内无新连接时，关闭此 channel 触发进程退出
type Server struct {
	pomo        *pomodoro.Pomodoro      // 番茄钟核心引擎
	screenAwake *screenawake.ScreenAwake // 防熄屏模块
	mu          sync.Mutex               // 保护 clients map 和 clientCount
	clients     map[chan string]bool     // SSE 客户端通道集合
	clientCount int                      // 当前活跃客户端数
	shutdown    chan struct{}            // 自动关闭信号
}

// NewServer 创建服务器实例，初始化番茄钟（25/5/15 分钟）和客户端管理。
func NewServer() *Server {
	return &Server{
		pomo:        pomodoro.New(25, 5, 15),
		screenAwake: screenawake.New(),
		clients:     make(map[chan string]bool),
		shutdown:    make(chan struct{}),
	}
}

// broadcastState 向所有 SSE 客户端广播当前完整状态。
//
// completed 参数：
//   - true:  阶段刚刚完成（timer 归零），前端据此触发通知/弹窗
//   - false: 常规 tick 更新或手动操作后的状态同步
//
// 消息格式：SSE 标准 "data: {json}\n\n"
func (s *Server) broadcastState(completed bool) {
	state := s.pomo.GetState()
	data := map[string]interface{}{
		"mode":              state.Mode.String(),
		"timeRemaining":     state.TimeRemaining,
		"totalDuration":     s.pomo.TotalDuration(),
		"isRunning":         state.IsRunning,
		"isPaused":          state.IsPaused,
		"completedSessions": state.CompletedSessions,
		"cyclesCompleted":   state.CyclesCompleted,
		"autoMode":          s.pomo.IsAutoMode(),
		"maxCycles":         s.pomo.GetMaxCycles(),
		"screenAwakeActive": s.screenAwake.IsActive(),
		"completed":         completed,
	}

	jsonData, _ := json.Marshal(data)
	msg := fmt.Sprintf("data: %s", jsonData)

	s.mu.Lock()
	defer s.mu.Unlock()

	// 非阻塞广播：若客户端 channel 已满则跳过（由 15 秒心跳检测断线）
	for client := range s.clients {
		select {
		case client <- msg:
		default:
			// 跳过慢的客户端，不关闭连接
		}
	}
}

// AddClient 注册一个新的 SSE 客户端通道。
func (s *Server) AddClient(ch chan string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[ch] = true
	s.clientCount++
	fmt.Printf("[+] 客户端连接，当前在线: %d\n", s.clientCount)
}

// RemoveClient 移除一个 SSE 客户端通道。
// 当 clientCount 降至 0 时，3 秒后若无新连接则发送 shutdown 信号。
func (s *Server) RemoveClient(ch chan string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, ch)
	s.clientCount--
	fmt.Printf("[-] 客户端断开，当前在线: %d\n", s.clientCount)

	if s.clientCount <= 0 {
		fmt.Println("[!] 所有客户端已断开，准备关闭服务...")
		go func() {
			time.Sleep(3 * time.Second)
			s.mu.Lock()
			shouldShutdown := s.clientCount <= 0
			s.mu.Unlock()
			if shouldShutdown {
				fmt.Println("[!] 无客户端连接，正在关闭服务...")
				close(s.shutdown)
			}
		}()
	}
}

// ---- HTTP Handlers ----

// handleStart POST /api/start
// 启动番茄钟计时，注册 onTick（每秒状态推送）和 onComplete（阶段完成通知）。
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.pomo.Start(
		func(timeRemaining int) {
			s.broadcastState(false) // 每秒 tick 推送
		},
		func(mode pomodoro.Mode, sessions int) {
			s.broadcastState(true) // 阶段完成推送
		},
	)

	s.broadcastState(false)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "started"})
}

// handlePause POST /api/pause
// 暂停当前计时，保留进度。
func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.pomo.Pause()
	s.broadcastState(false)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "paused"})
}

// handleResume POST /api/resume
// 从暂停状态恢复计时。
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state := s.pomo.GetState()
	if !state.IsPaused {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "not_paused"})
		return
	}

	s.pomo.Resume(
		func(timeRemaining int) {
			s.broadcastState(false)
		},
		func(mode pomodoro.Mode, sessions int) {
			s.broadcastState(true)
		},
	)

	s.broadcastState(false)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "resumed"})
}

// handleReset POST /api/reset
// 完全重置：回到 Work 模式，清零所有计数器。
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.pomo.Reset()
	s.broadcastState(false)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "reset"})
}

// handleMode POST /api/mode?mode=work|shortBreak|longBreak
// 手动切换到指定模式（会停止当前计时）。
func (s *Server) handleMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	modeStr := r.FormValue("mode")
	var mode pomodoro.Mode
	switch modeStr {
	case "work":
		mode = pomodoro.Work
	case "shortBreak":
		mode = pomodoro.ShortBreak
	case "longBreak":
		mode = pomodoro.LongBreak
	default:
		http.Error(w, "Invalid mode", http.StatusBadRequest)
		return
	}

	s.pomo.SwitchMode(mode)
	s.broadcastState(false)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "mode_switched"})
}

// handleSkip POST /api/skip
// 跳过当前阶段进入下一模式。自动模式下会自行启动计时，广播由 pomodoro 层的回调触发。
func (s *Server) handleSkip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.pomo.Skip()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "skipped"})
}

// handleSettings POST /api/settings?work=25&shortBreak=5&longBreak=15&autoMode=true&maxCycles=0
// 批量更新时长、自动模式和循环次数。
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	workMin, _ := strconv.Atoi(r.FormValue("work"))
	shortBreakMin, _ := strconv.Atoi(r.FormValue("shortBreak"))
	longBreakMin, _ := strconv.Atoi(r.FormValue("longBreak"))
	autoMode := r.FormValue("autoMode")
	maxCyclesStr := r.FormValue("maxCycles")

	if workMin > 0 && shortBreakMin > 0 && longBreakMin > 0 {
		s.pomo.SetDurations(workMin, shortBreakMin, longBreakMin)
	}

	if autoMode == "true" || autoMode == "false" {
		s.pomo.SetAutoMode(autoMode == "true")
	}

	if maxCyclesStr != "" {
		maxCycles, err := strconv.Atoi(maxCyclesStr)
		if err == nil && maxCycles >= 0 {
			s.pomo.SetMaxCycles(maxCycles)
		}
	}

	s.broadcastState(false)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "settings_updated"})
}

// handleState GET /api/state
// 返回当前完整状态（用于初始加载和轮询回退）。
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	state := s.pomo.GetState()
	data := map[string]interface{}{
		"mode":              state.Mode.String(),
		"timeRemaining":     state.TimeRemaining,
		"totalDuration":     s.pomo.TotalDuration(),
		"isRunning":         state.IsRunning,
		"isPaused":          state.IsPaused,
		"completedSessions": state.CompletedSessions,
		"cyclesCompleted":   state.CyclesCompleted,
		"autoMode":          s.pomo.IsAutoMode(),
		"maxCycles":         s.pomo.GetMaxCycles(),
		"screenAwakeActive": s.screenAwake.IsActive(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

// handleScreenAwake POST /api/screen-awake?action=enable|disable
// 启用/禁用防熄屏功能。
func (s *Server) handleScreenAwake(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	action := r.FormValue("action")
	switch action {
	case "enable":
		s.screenAwake.Enable()
	case "disable":
		s.screenAwake.Disable()
	default:
		http.Error(w, "Invalid action", http.StatusBadRequest)
		return
	}

	s.broadcastState(false)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "screen_awake_" + action})
}

// handleScreenAwakeState GET /api/screen-awake-state
// 返回防熄屏的当前状态（独立查询，不返回完整 pomodoro 状态）。
func (s *Server) handleScreenAwakeState(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{
		"screenAwakeActive": s.screenAwake.IsActive(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

// handleSSE GET /sse
//
// 建立 SSE（Server-Sent Events）长连接，持续推送番茄钟状态变更。
// 特性：
//   - 缓冲 channel（64）避免慢客户端被误剔除
//   - 每 15 秒发送 SSE 注释心跳，防止代理/浏览器超时断连
//   - 客户端断开时由 r.Context().Done() 检测并清理
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := make(chan string, 64) // 缓冲 64 条消息，避免广播时阻塞
	s.AddClient(ch)
	defer s.RemoveClient(ch)

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprint(w, msg+"\n\n")
			flusher.Flush()
		case <-heartbeat.C:
			// SSE 注释行，浏览器忽略但保持连接活跃
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// openBrowser 根据操作系统自动打开默认浏览器访问指定 URL。
func openBrowser(url string) error {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}

	return cmd.Start()
}

func main() {
	server := NewServer()

	// ---- 路由注册 ----
	mux := http.NewServeMux()

	// 番茄钟操作 API
	mux.HandleFunc("/api/start", server.handleStart)
	mux.HandleFunc("/api/pause", server.handlePause)
	mux.HandleFunc("/api/resume", server.handleResume)
	mux.HandleFunc("/api/reset", server.handleReset)
	mux.HandleFunc("/api/mode", server.handleMode)
	mux.HandleFunc("/api/skip", server.handleSkip)
	mux.HandleFunc("/api/settings", server.handleSettings)
	mux.HandleFunc("/api/state", server.handleState)

	// 防熄屏 API
	mux.HandleFunc("/api/screen-awake", server.handleScreenAwake)
	mux.HandleFunc("/api/screen-awake-state", server.handleScreenAwakeState)

	// SSE 实时推送
	mux.HandleFunc("/sse", server.handleSSE)

	// 静态资源（从 embed.FS 提供）
	staticFS, _ := fs.Sub(staticFiles, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// SPA fallback：所有未匹配路由返回 index.html
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, staticFiles, "static/index.html")
	})

	// ---- 启动 ----
	addr := ":8080"
	fmt.Println("番茄钟服务已启动")
	fmt.Printf("访问: http://localhost%s\n", addr)

	// 500ms 后自动打开浏览器
	go func() {
		time.Sleep(500 * time.Millisecond)
		if err := openBrowser(fmt.Sprintf("http://localhost%s", addr)); err != nil {
			log.Printf("无法自动打开浏览器: http://localhost%s", addr)
		}
	}()

	// ---- 优雅关闭 ----
	// 方式一：系统信号（Ctrl+C 或 SIGTERM）
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\n收到关闭信号，正在清理...")
		server.screenAwake.Disable() // 恢复电源策略
		os.Exit(0)
	}()

	// 方式二：所有客户端断开后自动关闭
	go func() {
		<-server.shutdown
		fmt.Println("\n所有客户端已断开，正在关闭服务...")
		server.screenAwake.Disable()
		os.Exit(0)
	}()

	// HTTP 服务器配置
	// WriteTimeout = 0：SSE 长连接需要无限写超时
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("服务启动失败: %v", err)
	}
}

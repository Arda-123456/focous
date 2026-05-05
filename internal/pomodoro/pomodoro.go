// Package pomodoro 实现了番茄钟核心计时引擎。
//
// 核心设计：
//   - 三种模式（Work / ShortBreak / LongBreak）按番茄工作法规则自动切换
//   - 支持手动模式和自动模式（autoMode），自动模式下阶段自动衔接
//   - 通过回调函数（onTick / onComplete）与外部通信，不依赖任何具体传输层
//   - 使用 goroutine + channel 实现异步计数，Stop() 通过 sync.Once 确保安全关闭
//
// 模式切换规则：
//   - 完成 Work → CompletedSessions++，每 4 个 session 进入 LongBreak，否则 ShortBreak
//   - 完成任意休息 → 回到 Work
//   - 从 LongBreak 退出且 sessions >= 4 → CyclesCompleted++
package pomodoro

import (
	"sync"
	"time"
)

// Mode 表示番茄钟的当前运行模式。
type Mode int

const (
	Work       Mode = iota // 专注模式（默认 25 分钟）
	ShortBreak             // 短休息模式（默认 5 分钟）
	LongBreak              // 长休息模式（默认 15 分钟）
)

// String 返回模式的中文名称，用于前端展示。
func (m Mode) String() string {
	switch m {
	case Work:
		return "专注"
	case ShortBreak:
		return "短休息"
	case LongBreak:
		return "长休息"
	default:
		return "未知"
	}
}

// State 是番茄钟的完整可观测状态，通过 GetState() 获取并序列化发送给前端。
type State struct {
	Mode              Mode // 当前模式
	TimeRemaining     int  // 当前阶段剩余秒数
	IsRunning         bool // 计时器是否正在运行
	IsPaused          bool // 计时器是否处于暂停状态
	CompletedSessions int  // 累计完成的专注 session 数（不清零）
	CyclesCompleted   int  // 累计完成的完整循环数（每 4 session + 长休息 = 1 循环）
}

// Pomodoro 是番茄钟计时器的核心结构。
//
// 内部字段分为三类：
//  1. 配置参数（时长、模式开关）
//  2. 运行时状态（state）
//  3. 控制原语（ticker、stopCh、stopOnce、回调）
type Pomodoro struct {
	// ---- 时长配置（以秒存储） ----
	workDuration            int // 专注时长
	shortBreakDuration      int // 短休息时长
	longBreakDuration       int // 长休息时长
	sessionsBeforeLongBreak int // 多少次专注后进入长休息（默认 4）

	// ---- 运行模式 ----
	autoMode  bool // 是否启用自动模式（阶段完成后自动进入下一阶段）
	maxCycles int  // 最大循环次数，0 表示无限循环

	// ---- 当前状态 ----
	state State

	// ---- 计时控制 ----
	ticker   *time.Ticker  // 每秒触发一次
	stopCh   chan struct{} // 关闭此 channel 以停止当前 run goroutine
	stopOnce sync.Once     // 确保 Stop() 只执行一次 close(stopCh)

	// ---- 回调函数 ----
	// onTick 每秒触发一次，参数为新的 TimeRemaining
	onTick func(int)
	// onComplete 计时归零时触发，参数为切换后的新 Mode 和更新后的 CompletedSessions
	onComplete func(Mode, int)
}

// New 创建一个新的 Pomodoro 实例。
// 参数 workMin/shortBreakMin/longBreakMin 为分钟数，内部转换为秒。
func New(workMin, shortBreakMin, longBreakMin int) *Pomodoro {
	p := &Pomodoro{
		workDuration:            workMin * 60,
		shortBreakDuration:      shortBreakMin * 60,
		longBreakDuration:       longBreakMin * 60,
		sessionsBeforeLongBreak: 4,
		autoMode:                false,
		maxCycles:               0, // 0 = 无限循环
	}

	// 初始化为 Work 模式、未运行状态
	p.state = State{
		Mode:          Work,
		TimeRemaining: p.workDuration,
		IsRunning:     false,
		IsPaused:      false,
	}

	return p
}

// ---- 状态查询 ----

// GetState 返回当前 Pomodoro 状态（值拷贝，并发安全由调用方保证）。
func (p *Pomodoro) GetState() State {
	return p.state
}

// TotalDuration 返回当前模式下阶段的总时长（秒）。
func (p *Pomodoro) TotalDuration() int {
	switch p.state.Mode {
	case Work:
		return p.workDuration
	case ShortBreak:
		return p.shortBreakDuration
	case LongBreak:
		return p.longBreakDuration
	default:
		return p.workDuration
	}
}

// ---- 配置方法 ----

// SetDurations 更新三种模式的时长（分钟），仅在未运行时同步更新剩余时间。
func (p *Pomodoro) SetDurations(workMin, shortBreakMin, longBreakMin int) {
	p.workDuration = workMin * 60
	p.shortBreakDuration = shortBreakMin * 60
	p.longBreakDuration = longBreakMin * 60

	// 如果当前没有在运行/paused，直接刷新显示的时间
	if !p.state.IsRunning && !p.state.IsPaused {
		p.state.TimeRemaining = p.workDuration
	}
}

// SetAutoMode 启用/禁用自动模式。
func (p *Pomodoro) SetAutoMode(enabled bool) {
	p.autoMode = enabled
}

// SetMaxCycles 设置最大循环次数，0 = 无限。
func (p *Pomodoro) SetMaxCycles(cycles int) {
	p.maxCycles = cycles
}

// IsAutoMode 返回当前是否处于自动模式。
func (p *Pomodoro) IsAutoMode() bool {
	return p.autoMode
}

// GetMaxCycles 返回当前设置的最大循环次数。
func (p *Pomodoro) GetMaxCycles() int {
	return p.maxCycles
}

// ---- 生命周期控制 ----

// Stop 安全地停止当前 run goroutine。
// 使用 sync.Once 保证 close(stopCh) 只执行一次，避免 panic。
func (p *Pomodoro) Stop() {
	p.stopOnce.Do(func() {
		if p.stopCh != nil {
			close(p.stopCh)
		}
	})
}

// Start 启动计时器。
// onTick: 每秒触发，参数为新的 TimeRemaining
// onComplete: 计时归零后触发，参数为切换后的 Mode 和新的 CompletedSessions
// 如果已在运行则忽略。
func (p *Pomodoro) Start(onTick func(int), onComplete func(Mode, int)) {
	if p.state.IsRunning {
		return
	}

	p.onTick = onTick
	p.onComplete = onComplete
	p.state.IsRunning = true
	p.state.IsPaused = false
	p.stopCh = make(chan struct{})
	p.stopOnce = sync.Once{}

	go p.run()
}

// Pause 暂停当前计时，保留已消耗时间。
func (p *Pomodoro) Pause() {
	if p.state.IsRunning && !p.state.IsPaused {
		p.state.IsPaused = true
		p.Stop()
	}
}

// Resume 从暂停状态恢复计时。
func (p *Pomodoro) Resume(onTick func(int), onComplete func(Mode, int)) {
	if p.state.IsPaused {
		p.onTick = onTick
		p.onComplete = onComplete
		p.state.IsPaused = false
		p.stopCh = make(chan struct{})
		p.stopOnce = sync.Once{}
		go p.run()
	}
}

// Reset 完全重置：停止计时、回到 Work 模式、清零所有计数器。
func (p *Pomodoro) Reset() {
	if p.state.IsRunning || p.state.IsPaused {
		p.Stop()
	}
	p.state.IsRunning = false
	p.state.IsPaused = false
	p.state.Mode = Work
	p.state.TimeRemaining = p.workDuration
	p.state.CompletedSessions = 0
	p.state.CyclesCompleted = 0
}

// SwitchMode 直接切换到指定模式（不触发回调、清零运行状态）。
func (p *Pomodoro) SwitchMode(mode Mode) {
	if p.state.IsRunning || p.state.IsPaused {
		p.Stop()
	}
	p.state.IsRunning = false
	p.state.IsPaused = false
	p.state.Mode = mode

	switch mode {
	case Work:
		p.state.TimeRemaining = p.workDuration
	case ShortBreak:
		p.state.TimeRemaining = p.shortBreakDuration
	case LongBreak:
		p.state.TimeRemaining = p.longBreakDuration
	}
}

// Skip 跳过当前阶段，立即切换到下一模式。
// 会触发 onComplete 回调；若 autoMode 启用则自动启动下一阶段。
func (p *Pomodoro) Skip() {
	if p.state.IsRunning || p.state.IsPaused {
		p.Stop()
	}
	p.state.IsRunning = false
	p.state.IsPaused = false

	// 切换到下一模式，记录切换前的会话数
	completedBefore := p.switchMode()
	if p.onComplete != nil {
		p.onComplete(p.state.Mode, completedBefore)
	}

	// 自动模式下且未完成全部循环时，立刻开始下一阶段
	if p.autoMode && !p.isCyclesComplete() {
		p.autoStartNext()
		// 立即广播新状态，前端无缝切换
		if p.onTick != nil {
			p.onTick(p.state.TimeRemaining)
		}
	}
}

// isCyclesComplete 判断是否已达到设定的循环次数上限。
// maxCycles == 0 表示无限循环，永远返回 false。
func (p *Pomodoro) isCyclesComplete() bool {
	if p.maxCycles == 0 {
		return false
	}
	return p.state.CyclesCompleted >= p.maxCycles
}

// autoStartNext 自动模式专用：在上一阶段完成后立即启动下一阶段。
// 复用已有的 onTick/onComplete 回调（来自 Start/Resume）。
func (p *Pomodoro) autoStartNext() {
	p.stopCh = make(chan struct{})
	p.stopOnce = sync.Once{}
	p.state.IsRunning = true
	p.state.IsPaused = false
	go p.run()
}

// run 是计时器的核心循环，在独立的 goroutine 中运行。
//
// 执行流程：
//  1. 每秒 tick，TimeRemaining--，调用 onTick 通知外部
//  2. TimeRemaining == 0 时：switchMode 切换模式 → onComplete 通知外部
//  3. 若 autoMode 且未完成全部循环 → autoStartNext 立即继续
//  4. stopCh 关闭 → 退出循环（用于 Pause/Stop/Skip）
func (p *Pomodoro) run() {
	p.ticker = time.NewTicker(time.Second)
	defer p.ticker.Stop()

	for {
		select {
		case <-p.ticker.C:
			// 每秒递减剩余时间
			if p.state.TimeRemaining > 0 {
				p.state.TimeRemaining--
				if p.onTick != nil {
					p.onTick(p.state.TimeRemaining)
				}
			}

			// 归零：切换模式并处理自动逻辑
			if p.state.TimeRemaining <= 0 {
				p.state.IsRunning = false
				p.Stop()

				prevSessions := p.switchMode()
				if p.onComplete != nil {
					p.onComplete(p.state.Mode, prevSessions)
				}

				// 自动模式：立即启动下一阶段并广播新状态
				if p.autoMode && !p.isCyclesComplete() {
					p.autoStartNext()
					if p.onTick != nil {
						p.onTick(p.state.TimeRemaining)
					}
				}
				return
			}
		case <-p.stopCh:
			// 外部调用 Stop/Pause 时退出
			return
		}
	}
}

// switchMode 按照番茄工作法规则切换到下一模式，返回当前的 CompletedSessions。
//
// 切换规则：
//   - 当前 Work → CompletedSessions++
//     → 若 sessions 是 4 的倍数 → LongBreak
//     → 否则 → ShortBreak
//   - 当前 非Work（任意休息） → Work
//     → 若刚从 LongBreak 退出且 sessions >= 4 → CyclesCompleted++
func (p *Pomodoro) switchMode() int {
	if p.state.Mode == Work {
		p.state.CompletedSessions++
		if p.state.CompletedSessions%p.sessionsBeforeLongBreak == 0 {
			p.state.Mode = LongBreak
			p.state.TimeRemaining = p.longBreakDuration
		} else {
			p.state.Mode = ShortBreak
			p.state.TimeRemaining = p.shortBreakDuration
		}
	} else {
		wasLongBreak := p.state.Mode == LongBreak
		p.state.Mode = Work
		p.state.TimeRemaining = p.workDuration

		// 一次完整循环 = 4 次专注 + 1 次长休息
		if wasLongBreak && p.state.CompletedSessions >= p.sessionsBeforeLongBreak {
			p.state.CyclesCompleted++
		}
	}
	return p.state.CompletedSessions
}

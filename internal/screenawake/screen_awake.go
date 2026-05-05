// Package screenawake 提供防止 Windows 系统熄屏/休眠的功能。
//
// 核心原理：
//   - 调用 Windows kernel32.dll 的 SetThreadExecutionState API
//   - 传入 ES_CONTINUOUS | ES_SYSTEM_REQUIRED | ES_DISPLAY_REQUIRED 启用防熄屏
//   - 传入 ES_CONTINUOUS 恢复系统默认电源策略
//
// 注意事项：
//   - 仅支持 Windows 平台
//   - SetThreadExecutionState 作用于调用线程，因此必须在主线程或固定线程中周期调用
//   - 本实现中的 Enable/Disable 由 HTTP handler 线程调用，只要该线程存活则持续生效
package screenawake

import (
	"sync"
	"syscall"
)

// Windows SetThreadExecutionState 的 API 常量
const (
	ES_CONTINUOUS       = 0x80000000 // 通知系统该状态持续有效（直到下次调用）
	ES_SYSTEM_REQUIRED  = 0x00000001 // 阻止系统进入睡眠
	ES_DISPLAY_REQUIRED = 0x00000002 // 阻止关闭显示器
)

// ScreenAwake 管理防熄屏状态的启用/禁用，线程安全。
type ScreenAwake struct {
	mu       sync.Mutex // 保护 isActive 的并发访问
	isActive bool       // 当前是否处于防熄屏激活状态
}

// New 创建一个新的 ScreenAwake 实例（默认未激活）。
func New() *ScreenAwake {
	return &ScreenAwake{}
}

// Enable 激活防熄屏：阻止系统睡眠和显示器关闭。
// 如果已激活则无效果。
func (sa *ScreenAwake) Enable() error {
	sa.mu.Lock()
	defer sa.mu.Unlock()

	// 延迟加载 kernel32.dll，获取 SetThreadExecutionState 函数指针
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	setThreadExecState := kernel32.NewProc("SetThreadExecutionState")

	// 组合三个 flag 调用 API
	ret, _, err := setThreadExecState.Call(
		ES_CONTINUOUS | ES_SYSTEM_REQUIRED | ES_DISPLAY_REQUIRED,
	)

	// ret == 0 表示调用失败
	if ret == 0 {
		return err
	}

	sa.isActive = true
	return nil
}

// Disable 取消防熄屏，恢复系统默认电源管理策略。
func (sa *ScreenAwake) Disable() error {
	sa.mu.Lock()
	defer sa.mu.Unlock()

	// 如果未激活则跳过
	if !sa.isActive {
		return nil
	}

	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	setThreadExecState := kernel32.NewProc("SetThreadExecutionState")

	// 仅传 ES_CONTINUOUS 表示恢复默认策略
	ret, _, err := setThreadExecState.Call(ES_CONTINUOUS)

	if ret == 0 {
		return err
	}

	sa.isActive = false
	return nil
}

// IsActive 返回当前防熄屏是否处于激活状态。
func (sa *ScreenAwake) IsActive() bool {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	return sa.isActive
}

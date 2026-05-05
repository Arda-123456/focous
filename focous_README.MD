# focous — 番茄钟 + 防熄屏桌面工具

## 项目简介

`focous` 是一个基于 Go 语言开发的桌面级番茄钟工具，同时集成了 Windows 防熄屏功能。采用 B/S 架构，Go 后端内嵌前端静态资源，编译为单文件二进制（exe），启动后自动打开浏览器。

### 核心特性

| 特性 | 说明 |
|---|---|
| 番茄工作法 | 25 分钟专注 / 5 分钟短休息 / 15 分钟长休息（可自定义） |
| 自动模式 | 阶段完成后自动切换到下一阶段，无需手动操作 |
| 循环控制 | 支持设置最大循环次数（1-4）或无限循环 |
| 防熄屏 | 调用 Windows API 阻止系统睡眠和显示器关闭 |
| 实时推送 | SSE（Server-Sent Events）实现状态实时同步 |
| 通知系统 | 阶段完成时触发：提示音 → 浏览器通知 → 居中弹窗 |
| 单文件部署 | 前端资源编译时 embed 进二进制，零外部依赖 |
| 自动退出 | 关闭浏览器页面后 3 秒自动退出进程 |

---

## 项目结构

```
focous/
├── main.go                          # 程序入口 + HTTP 服务器 + SSE 管理
├── go.mod                           # Go module 定义（Go 1.26）
├── go.sum                           # 依赖校验（空，零外部依赖）
├── static/
│   ├── index.html                   # 前端 SPA 骨架（Material Design 3）
│   ├── app.js                       # 前端逻辑（状态管理 + API 调用 + SSE 客户端）
│   └── style.css                    # 样式表（MD3 设计令牌体系）
└── internal/
    ├── pomodoro/
    │   └── pomodoro.go              # 番茄钟核心计时引擎
    └── screenawake/
        └── screen_awake.go          # Windows 防熄屏模块
```

---

## 架构设计

### 总体架构

```
┌─────────────────────────────────────────┐
│                  focous.exe              │
│  ┌────────────────────────────────────┐ │
│  │          Go HTTP Server            │ │
│  │  ┌──────────┐  ┌────────────────┐ │ │
│  │  │ pomodoro │  │  screenawake   │ │ │
│  │  │  engine  │  │  (Windows API) │ │ │
│  │  └────┬─────┘  └───────┬────────┘ │ │
│  │       │                │          │ │
│  │  ┌────┴────────────────┴────────┐ │ │
│  │  │       SSE Broadcast          │ │ │
│  │  │   (clients map + channels)   │ │ │
│  │  └──────────────┬───────────────┘ │ │
│  │                 │                  │ │
│  │  ┌──────────────┴───────────────┐ │ │
│  │  │    REST API Handlers         │ │ │
│  │  │  /api/start|pause|resume...  │ │ │
│  │  └──────────────────────────────┘ │ │
│  └────────────────────────────────────┘ │
│  ┌────────────────────────────────────┐ │
│  │  embed.FS: static/* (HTML/JS/CSS)  │ │
│  └────────────────────────────────────┘ │
└─────────────────────────────────────────┘
           │ HTTP :8080
           ▼
┌─────────────────────┐
│    Browser (SSE)    │
│  index.html + app.js│
└─────────────────────┘
```

### 数据流

```
用户点击"开始" → POST /api/start
                       │
               Server.handleStart()
                       │
          pomodoro.Start(onTick, onComplete)
                       │
          ┌────────────┴────────────┐
          ▼                         ▼
   每秒 onTick                timer 归零 onComplete
   broadcastState(false)      broadcastState(true)
          │                         │
          └────────┬────────────────┘
                   ▼
           SSE → 所有客户端
                   │
           app.js: onmessage
                   │
           ┌───────┴────────┐
           ▼                ▼
    completed=false    completed=true
    updateState()      判断完成类型
                       通知 + 弹窗
```

---

## 模块详解

### 1. `internal/pomodoro/pomodoro.go` — 番茄钟引擎

**核心结构 `Pomodoro`**

| 字段分类 | 字段名 | 说明 |
|---|---|---|
| 配置 | `workDuration` | 专注时长（秒） |
| 配置 | `shortBreakDuration` | 短休息时长（秒） |
| 配置 | `longBreakDuration` | 长休息时长（秒） |
| 配置 | `sessionsBeforeLongBreak` | 几个 session 后长休息（固定 4） |
| 配置 | `autoMode` | 是否自动模式 |
| 配置 | `maxCycles` | 最大循环数，0=无限 |
| 状态 | `state` | 当前 `State` 结构体 |
| 控制 | `ticker` | `time.Ticker`，每秒触发 |
| 控制 | `stopCh` | 关闭以停止 `run()` goroutine |
| 控制 | `stopOnce` | `sync.Once` 保证 `stopCh` 只关一次 |
| 回调 | `onTick` | 每秒调用，传 `TimeRemaining` |
| 回调 | `onComplete` | 阶段完成调用，传新 Mode 和 sessions |

**模式切换规则 (`switchMode`)**

```
当前 Work ──→ CompletedSessions++
              ├─ sessions % 4 == 0 → LongBreak
              └─ 否则 → ShortBreak

当前 ShortBreak / LongBreak ──→ Work
                                ├─ 若从 LongBreak 退出且 sessions≥4 → CyclesCompleted++
```

**自动模式流程 (`run` 中的自动逻辑)**

```
timer 归零 → switchMode() → onComplete(通知外部)
                           → 若 autoMode 且未达循环上限:
                              autoStartNext() → 立即启动下一阶段
                              onTick() → 立即广播新状态
```

### 2. `internal/screenawake/screen_awake.go` — 防熄屏

调用 `kernel32.dll` 的 `SetThreadExecutionState` API：

| 状态 | 参数 | 效果 |
|---|---|---|
| 启用 | `ES_CONTINUOUS \| ES_SYSTEM_REQUIRED \| ES_DISPLAY_REQUIRED` | 阻止睡眠 + 屏幕常亮 |
| 禁用 | `ES_CONTINUOUS` | 恢复系统默认策略 |

**注意**：仅支持 Windows 平台，API 作用于调用线程。

### 3. `main.go` — HTTP 服务器

**API 路由表**

| 方法 | 路由 | 功能 | 广播 |
|---|---|---|---|
| POST | `/api/start` | 启动计时 | ✅ |
| POST | `/api/pause` | 暂停计时 | ✅ |
| POST | `/api/resume` | 恢复计时 | ✅ |
| POST | `/api/reset` | 全部重置 | ✅ |
| POST | `/api/mode?mode=work\|shortBreak\|longBreak` | 切换模式 | ✅ |
| POST | `/api/skip` | 跳过当前阶段 | pomodoro 层回调 |
| POST | `/api/settings?work=&shortBreak=&longBreak=&autoMode=&maxCycles=` | 更新设置 | ✅ |
| GET | `/api/state` | 获取完整状态 | — |
| POST | `/api/screen-awake?action=enable\|disable` | 防熄屏开关 | ✅ |
| GET | `/api/screen-awake-state` | 防熄屏状态 | — |
| GET | `/sse` | SSE 长连接 | 持续推送 |

**SSE 特性**
- 缓冲 channel（64），避免慢客户端被误踢
- 15 秒心跳（SSE 注释行），防止代理/浏览器超时断连
- CORS 头 `Access-Control-Allow-Origin: *`
- 非阻塞广播：channel 满时跳过，不关闭连接

**优雅关闭**
- **系统信号**：`Ctrl+C` / `SIGTERM` → 禁用防熄屏 → 退出
- **自动关闭**：所有浏览器页面关闭后 3 秒 → 发送 shutdown → 退出

**HTTP Server 配置**
| 参数 | 值 | 说明 |
|---|---|---|
| `ReadTimeout` | 10s | 普通请求读取超时 |
| `WriteTimeout` | 0 | SSE 需要无限写超时 |
| `IdleTimeout` | 120s | 空闲连接保持时间 |

### 4. `static/app.js` — 前端应用

**状态管理**

维护一份与服务器同步的本地状态副本：
- `isRunning` / `isPaused` / `timeRemaining` / `totalSeconds`
- `currentMode` / `autoMode` / `maxCycles` / `cyclesCompleted`
- `prevCyclesCompleted` — 用于检测循环是否推进

**SSE 完成类型判断**

```
data.completed === true:
  ├─ mode 为 '短休息' 或 '长休息' → 通知 "专注完成"
  ├─ cyclesCompleted 变化         → 通知 "循环完成"
  └─ 其他                         → 通知 "休息结束"
```

**通知系统**

```
阶段完成 → playGentleChime()          (Web Audio API 4 音阶提示)
         → showBrowserNotification()   (浏览器 Notification API)
         → showModal()                 (居中弹窗)
```

---

## 构建与运行

### 前置条件

- Go 1.26+
- Windows 操作系统（防熄屏功能依赖）

### 构建

```bash
cd focous
go build -o focous.exe .
```

生成单个 `focous.exe`（约 9 MB），包含所有静态资源。

### 运行

```bash
./focous.exe
```

程序将：
1. 启动 HTTP 服务（`http://localhost:8080`）
2. 自动打开默认浏览器
3. 关闭浏览器页面后 3 秒自动退出

---

## 使用说明

### 基本操作

| 操作 | 方式 |
|---|---|
| 开始专注 | 点击蓝色"开始"按钮 |
| 暂停 | 再次点击（按钮变为"暂停"） |
| 继续 | 暂停后再次点击（按钮变为"继续"） |
| 重置 | 点击"重置"按钮 |
| 跳过当前阶段 | 点击"跳过"按钮 |
| 手动切换模式 | 点击顶部"专注/短休息/长休息"标签 |

### 防熄屏

- 打开"防止电脑熄屏"开关 → 系统不会自动睡眠/关闭显示器
- 关闭浏览器或退出程序 → 自动恢复系统电源策略

### 设置面板（点击右上角齿轮图标）

| 设置项 | 默认值 | 范围 |
|---|---|---|
| 专注时长 | 25 分钟 | 1-60 |
| 短休息 | 5 分钟 | 1-30 |
| 长休息 | 15 分钟 | 1-60 |
| 自动模式 | 关闭 | 开/关 |
| 循环次数 | 无限 | 0-4 |

---

## 已知限制

1. **仅 Windows**：防熄屏依赖 `kernel32.dll`，其他平台编译会失败
2. **端口固定**：`:8080`，若被占用需手动修改代码
3. **无持久化**：所有配置和进度在内存中，重启丢失
4. **单客户端优化**：设计为个人使用，多标签页同时开启可能导致状态不一致
5. **无并发锁保护 pomodoro state**：HTTP handler 与 `run()` goroutine 共享 `state` 字段，存在理论上的 data race

---

## 变更历史

| 版本 | 日期 | 变更 |
|---|---|---|
| 1.0 | 2026-04-11 | 初始版本 |
| 1.1 | 2026-05-05 | 修复 SSE 连接稳定性（心跳 + 缓冲 channel + CORS）；修复自动模式状态同步；修复前端完成通知误屏蔽 |

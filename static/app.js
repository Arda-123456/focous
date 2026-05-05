/**
 * focous 番茄钟前端应用
 *
 * 职责：
 *   1. 通过 GET /api/state 获取初始状态
 *   2. 通过 GET /sse (EventSource) 订阅实时状态推送
 *   3. 通过 POST /api/start|pause|resume|reset|mode|skip|settings 发送操作指令
 *   4. 管理 UI 渲染：环形进度条、计时器、模式标签、番茄圆点、弹窗通知
 *
 * 状态流转：
 *   - 本地维护一份与服务器同步的状态副本（isRunning, isPaused, timeRemaining 等）
 *   - SSE 消息驱动 UI 更新，POST 操作后同步 fetchState 兜底
 *   - completed 标志区分"常规 tick"和"阶段完成"，后者触发通知+弹窗
 *
 * 通知机制：
 *   - 阶段完成时依次触发：Web Audio 提示音 → 浏览器 Notification → 居中弹窗
 */

// ---- 本地状态副本（与服务器同步） ----
let isRunning = false;      // 计时器是否正在运行
let isPaused = false;       // 计时器是否暂停
let totalSeconds = 25 * 60; // 当前阶段总秒数
let timeRemaining = 25 * 60;// 当前阶段剩余秒数
let currentMode = 'work';   // 当前模式（字符串，"专注"/"短休息"/"长休息"）
let prevCompletedSessions = 0; // 上次已完成 session 数（预留，用于变化检测）
let autoMode = false;       // 是否启用自动模式
let maxCycles = 0;          // 最大循环次数，0=无限
let cyclesCompleted = 0;    // 服务器端已完成的循环数
let prevCyclesCompleted = 0;// 上次循环数（用于检测 cycle 是否推进）

// 环形进度条的周长（r=130）
const circumference = 2 * Math.PI * 130;

// ======================== 通知系统 ========================

/**
 * playGentleChime - 使用 Web Audio API 播放温和的上行提示音
 * 四个音符 C5(523) → E5(659) → G5(784) → C6(1047)，间隔 0.2s
 */
function playGentleChime() {
    try {
        const audioCtx = new (window.AudioContext || window.webkitAudioContext)();
        const notes = [523.25, 659.25, 783.99, 1046.50];
        notes.forEach((freq, i) => {
            const oscillator = audioCtx.createOscillator();
            const gainNode = audioCtx.createGain();
            oscillator.type = 'sine';
            oscillator.frequency.setValueAtTime(freq, audioCtx.currentTime + i * 0.2);
            gainNode.gain.setValueAtTime(0, audioCtx.currentTime + i * 0.2);
            gainNode.gain.linearRampToValueAtTime(0.15, audioCtx.currentTime + i * 0.2 + 0.05);
            gainNode.gain.exponentialRampToValueAtTime(0.001, audioCtx.currentTime + i * 0.2 + 1.5);
            oscillator.connect(gainNode);
            gainNode.connect(audioCtx.destination);
            oscillator.start(audioCtx.currentTime + i * 0.2);
            oscillator.stop(audioCtx.currentTime + i * 0.2 + 1.5);
        });
    } catch (e) {
        console.log('Audio not supported');
    }
}

// 请求浏览器通知权限（在用户首次交互时调用）
function requestNotificationPermission() {
    if ('Notification' in window && Notification.permission === 'default') {
        Notification.requestPermission();
    }
}

// 显示浏览器原生通知（需已授权）
function showBrowserNotification(title, body) {
    if ('Notification' in window && Notification.permission === 'granted') {
        new Notification(title, {
            body: body,
            icon: 'data:image/svg+xml,<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><text y=".9em" font-size="90">🍅</text></svg>',
            tag: 'pomodoro-complete',
            requireInteraction: true
        });
    }
}

// 显示居中弹窗，内容根据完成类型变化
function showModal(isWorkComplete, sessions, isCycleComplete) {
    const overlay = document.getElementById('modalOverlay');
    const icon = document.getElementById('modalIcon');
    const iconSymbol = document.getElementById('modalIconSymbol');
    const title = document.getElementById('modalTitle');
    const message = document.getElementById('modalMessage');

    if (isCycleComplete) {
        icon.className = 'modal-icon work';
        iconSymbol.textContent = 'check_circle';
        title.textContent = '太棒了！';
        message.textContent = '已完成一组循环，开始下一轮吧~';
    } else if (isWorkComplete) {
        icon.className = 'modal-icon work';
        iconSymbol.textContent = 'check_circle';
        title.textContent = '太棒了！';
        message.textContent = `你已完成 ${sessions} 个番茄钟，休息一下吧~`;
    } else {
        icon.className = 'modal-icon break';
        iconSymbol.textContent = 'coffee';
        title.textContent = '休息结束';
        message.textContent = '准备好开始专注了吗？';
    }
    overlay.classList.add('show');
}

function closeModal() {
    document.getElementById('modalOverlay').classList.remove('show');
}

// 统一通知入口：依次执行提示音 → 浏览器通知 → 弹窗
function notifyComplete(isWorkComplete, sessions, isCycleComplete) {
    playGentleChime();
    if (isCycleComplete) {
        showBrowserNotification('循环完成！', '已完成一组循环，开始下一轮吧~');
    } else if (isWorkComplete) {
        showBrowserNotification('番茄钟完成！', `已完成 ${sessions} 个番茄钟，休息一下吧~`);
    } else {
        showBrowserNotification('休息结束', '准备好开始专注了吗？');
    }
    showModal(isWorkComplete, sessions, isCycleComplete);
}

// ======================== UI 渲染 ========================

// 将秒数格式化为 MM:SS
function formatTime(seconds) {
    const mins = Math.floor(seconds / 60);
    const secs = seconds % 60;
    return `${mins.toString().padStart(2, '0')}:${secs.toString().padStart(2, '0')}`;
}

// 更新环形进度条 stroke-dashoffset
function updateProgress(remaining, total) {
    const progress = document.getElementById('timerProgress');
    const offset = circumference * (1 - remaining / total);
    progress.style.strokeDashoffset = offset;
}

// 更新底部番茄圆点（4 个点表示当前周期的进度）
function updatePomodoroDots(completed, mode) {
    const dots = document.querySelectorAll('.pomodoro-dot');
    const isLongBreak = mode === '长休息';
    const count = isLongBreak ? 4 : completed % 4;
    dots.forEach((dot, i) => {
        dot.classList.toggle('filled', i < count);
    });

    // 更新文字说明（含循环信息）
    let countText = `${isLongBreak ? 4 : count} / 4 个番茄`;
    if (autoMode && maxCycles > 0) {
        countText += `（第 ${Math.min(cyclesCompleted + 1, maxCycles)}/${maxCycles} 循环）`;
    } else if (autoMode && maxCycles === 0 && cyclesCompleted > 0) {
        countText += `（第 ${cyclesCompleted + 1} 轮）`;
    }
    document.getElementById('pomodoroCount').textContent = countText;
}

// 更新防熄屏图标状态
function updateScreenAwakeIcon(active) {
    const icon = document.getElementById('screenAwakeIcon');
    if (active) {
        icon.textContent = 'screen_share';
        icon.classList.add('active');
    } else {
        icon.textContent = 'screen_lock_rotation';
        icon.classList.remove('active');
    }
}

// 根据服务器状态更新所有 UI 元素
function updateState(state) {
    timeRemaining = state.timeRemaining;
    isRunning = state.isRunning;
    isPaused = state.isPaused;
    currentMode = state.mode;

    if (state.totalDuration) {
        totalSeconds = state.totalDuration;
    }

    // 模式标签高亮
    document.querySelectorAll('.mode-tab').forEach(tab => {
        const modeMap = { '专注': 'work', '短休息': 'shortBreak', '长休息': 'longBreak' };
        tab.classList.toggle('active', modeMap[state.mode] === tab.dataset.mode);
    });

    updatePomodoroDots(state.completedSessions, state.mode);
    document.getElementById('screenAwakeToggle').checked = state.screenAwakeActive;
    updateScreenAwakeIcon(state.screenAwakeActive);

    if (state.autoMode !== undefined) {
        document.getElementById('autoModeToggle').checked = state.autoMode;
        autoMode = state.autoMode;
    }
    if (state.maxCycles !== undefined) {
        maxCycles = state.maxCycles;
        updateCycleDropdown(state.maxCycles);
    }
    if (state.cyclesCompleted !== undefined) {
        cyclesCompleted = state.cyclesCompleted;
    }

    // 按钮文字和状态栏
    if (isRunning) {
        document.getElementById('startText').textContent = '暂停';
        document.getElementById('startIcon').textContent = 'pause';
        document.getElementById('timerProgress').classList.add('running');
        if (currentMode === '短休息') {
            document.getElementById('timerStatus').textContent = '短休息中';
            document.getElementById('statusBar').textContent = '短休息中...';
        } else if (currentMode === '长休息') {
            document.getElementById('timerStatus').textContent = '长休息中';
            document.getElementById('statusBar').textContent = '长休息中...';
        } else {
            document.getElementById('timerStatus').textContent = '专注中';
            document.getElementById('statusBar').textContent = '专注中...';
        }
    } else if (isPaused) {
        document.getElementById('startText').textContent = '继续';
        document.getElementById('startIcon').textContent = 'play_arrow';
        document.getElementById('timerProgress').classList.remove('running');
        document.getElementById('timerStatus').textContent = '已暂停';
        document.getElementById('statusBar').textContent = '已暂停';
    } else {
        document.getElementById('startText').textContent = '开始';
        document.getElementById('startIcon').textContent = 'play_arrow';
        document.getElementById('timerProgress').classList.remove('running');
        document.getElementById('timerStatus').textContent = '准备就绪';
        document.getElementById('statusBar').textContent = '就绪';
    }
    updateTimerDisplay();
}

// 刷新时间显示和进度环
function updateTimerDisplay() {
    document.getElementById('timeDisplay').textContent = formatTime(timeRemaining);
    if (totalSeconds > 0) {
        updateProgress(timeRemaining, totalSeconds);
    }
}

// ======================== API 调用 ========================

// 从服务器获取当前完整状态
async function fetchState() {
    try {
        const response = await fetch('/api/state');
        const state = await response.json();
        totalSeconds = state.totalDuration || (25 * 60);
        updateState(state);
    } catch (error) {
        console.error('Failed to fetch state:', error);
    }
}

// 主按钮：根据当前状态执行 开始/暂停/继续
async function toggleTimer() {
    if (!isRunning && !isPaused) await startTimer();
    else if (isRunning && !isPaused) await pauseTimer();
    else if (isPaused) await resumeTimer();
}

async function startTimer() {
    try {
        const response = await fetch('/api/start', { method: 'POST' });
        if (response.ok) { showToast('计时开始'); fetchState(); }
    } catch (error) { showToast('操作失败'); }
}

async function pauseTimer() {
    try {
        const response = await fetch('/api/pause', { method: 'POST' });
        if (response.ok) { showToast('已暂停'); fetchState(); }
    } catch (error) { showToast('操作失败'); }
}

async function resumeTimer() {
    try {
        const response = await fetch('/api/resume', { method: 'POST' });
        if (response.ok) { showToast('继续计时'); fetchState(); }
    } catch (error) { showToast('操作失败'); }
}

async function resetTimer() {
    try {
        const response = await fetch('/api/reset', { method: 'POST' });
        if (response.ok) {
            showToast('已重置');
            prevCompletedSessions = 0;
            prevCyclesCompleted = 0;
            fetchState();
        }
    } catch (error) { showToast('操作失败'); }
}

async function skipTimer() {
    try {
        const response = await fetch('/api/skip', { method: 'POST' });
        if (response.ok) { showToast('已跳过'); fetchState(); }
    } catch (error) { showToast('操作失败'); }
}

async function switchMode(mode) {
    try {
        const response = await fetch(`/api/mode?mode=${mode}`, { method: 'POST' });
        if (response.ok) {
            showToast(`切换到${document.querySelector(`[data-mode="${mode}"]`).textContent}模式`);
            fetchState();
        }
    } catch (error) { showToast('操作失败'); }
}

// ======================== 循环次数下拉菜单 ========================

let dropdownOpen = false;

function toggleCycleDropdown() {
    const menu = document.getElementById('cycleDropdownMenu');
    const trigger = document.getElementById('cycleDropdownTrigger');
    if (dropdownOpen) {
        menu.classList.remove('show');
        trigger.classList.remove('open');
        dropdownOpen = false;
        return;
    }
    dropdownOpen = true;
    trigger.classList.add('open');
    const rect = trigger.getBoundingClientRect();
    menu.style.top = (rect.bottom + 4) + 'px';
    menu.style.left = (rect.left + rect.width / 2) + 'px';
    menu.classList.add('show');
}

function selectCycle(value) {
    maxCycles = value;
    updateCycleDropdown(value);
    toggleCycleDropdown();
}

function updateCycleDropdown(value) {
    const label = document.getElementById('cycleDropdownLabel');
    const items = document.querySelectorAll('.custom-dropdown-item');
    const labels = { 0: '无限循环', 1: '1 个循环', 2: '2 个循环', 3: '3 个循环', 4: '4 个循环' };
    label.textContent = labels[value] || '无限循环';
    items.forEach(item => {
        item.classList.toggle('selected', parseInt(item.dataset.value) === value);
    });
}

// 点击外部关闭下拉菜单
document.addEventListener('click', function (e) {
    if (!e.target.closest('#cycleDropdown') && dropdownOpen) {
        toggleCycleDropdown();
    }
});

// ======================== 设置 ========================

async function applySettings() {
    const work = document.getElementById('workDuration').value;
    const shortBreak = document.getElementById('shortBreak').value;
    const longBreak = document.getElementById('longBreak').value;
    const autoModeVal = document.getElementById('autoModeToggle').checked ? 'true' : 'false';
    const maxCyclesVal = maxCycles;

    try {
        const response = await fetch(`/api/settings?work=${work}&shortBreak=${shortBreak}&longBreak=${longBreak}&autoMode=${autoModeVal}&maxCycles=${maxCyclesVal}`, { method: 'POST' });
        if (response.ok) { showToast('设置已应用'); fetchState(); }
        else showToast('设置无效');
    } catch (error) { showToast('操作失败'); }
}

async function toggleScreenAwake(enabled) {
    try {
        const action = enabled ? 'enable' : 'disable';
        const response = await fetch(`/api/screen-awake?action=${action}`, { method: 'POST' });
        if (response.ok) { showToast(enabled ? '防熄屏已启用' : '防熄屏已关闭'); updateScreenAwakeIcon(enabled); }
    } catch (error) { showToast('操作失败'); }
}

async function toggleAutoMode(enabled) {
    autoMode = enabled;
    await applySettings();
}

function toggleSettings() {
    const panel = document.getElementById('settingsPanel');
    const menu = document.getElementById('cycleDropdownMenu');
    const trigger = document.getElementById('cycleDropdownTrigger');
    if (dropdownOpen) {
        menu.classList.remove('show');
        trigger.classList.remove('open');
        dropdownOpen = false;
    }
    panel.classList.toggle('open');
}

// Toast 提示（底部居中，3 秒自动消失）
function showToast(message) {
    const toast = document.getElementById('toast');
    toast.textContent = message;
    toast.classList.add('show');
    setTimeout(() => toast.classList.remove('show'), 3000);
}

// ======================== SSE 实时连接 ========================

/**
 * connectSSE - 建立 EventSource 连接，订阅服务器实时状态推送
 *
 * 消息处理逻辑：
 *   当 data.completed === true 时判断完成类型：
 *     - 进入休息（mode 为短休息/长休息）→ 通知"专注完成"
 *     - 循环推进（cyclesCompleted 变化）→ 通知"循环完成"
 *     - 其他（休息结束）→ 通知"休息结束"
 *   断线时 3 秒后自动重连
 */
function connectSSE() {
    const eventSource = new EventSource('/sse');
    eventSource.onmessage = function (event) {
        const data = JSON.parse(event.data);
        if (data.completed) {
            const isEnteringBreak = data.mode === '短休息' || data.mode === '长休息';
            const cycleAdvanced = data.cyclesCompleted !== prevCyclesCompleted;
            if (isEnteringBreak) notifyComplete(true, data.completedSessions, false);
            else if (cycleAdvanced) notifyComplete(true, data.completedSessions, true);
            else notifyComplete(false, data.completedSessions, false);
            prevCyclesCompleted = data.cyclesCompleted;
        }
        prevCompletedSessions = data.completedSessions;
        if (data.autoMode !== undefined) autoMode = data.autoMode;
        if (data.maxCycles !== undefined) maxCycles = data.maxCycles;
        if (data.cyclesCompleted !== undefined) cyclesCompleted = data.cyclesCompleted;
        updateState(data);
    };
    eventSource.onerror = function () {
        eventSource.close();
        setTimeout(connectSSE, 3000);
    };
}

// ======================== 初始化 ========================

document.addEventListener('DOMContentLoaded', function () {
    requestNotificationPermission();
    fetchState();
    connectSSE();
});

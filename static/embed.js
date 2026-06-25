const state = {
  title: '模型状态监控',
  defaultModels: [],
  refreshSeconds: 60,
  countdown: 60,
  timer: null,
  models: [],
  timeWindow: '24h',
}

const els = {
  title: document.getElementById('page-title'),
  subtitle: document.getElementById('subtitle'),
  refreshButton: document.getElementById('refresh-button'),
  countdown: document.getElementById('countdown'),
  models: document.getElementById('models'),
  empty: document.getElementById('empty'),
  tooltip: document.getElementById('slot-tooltip'),
}

async function api(path, options = {}) {
  const response = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...options,
  })
  const payload = await response.json().catch(() => ({}))
  if (!response.ok || payload.success === false) {
    throw new Error(payload.error || `HTTP ${response.status}`)
  }
  return payload
}

async function loadConfig() {
  const cfg = await api('api/config')
  state.title = cfg.title || state.title
  state.defaultModels = Array.isArray(cfg.default_models) ? cfg.default_models : []
  state.refreshSeconds = Number.isFinite(cfg.refresh_seconds) ? cfg.refresh_seconds : 60
  state.countdown = state.refreshSeconds
  state.timeWindow = cfg.default_window || state.timeWindow
  els.title.textContent = state.title
  document.title = state.title
  renderSubtitle(0)
  renderCountdown()
}

async function loadModelList() {
  if (state.defaultModels.length > 0) {
    state.models = state.defaultModels
    return
  }
  const payload = await api('api/models')
  state.models = (payload.data || []).map(item => item.model_name).filter(Boolean)
}

async function refresh() {
  setBusy(true)
  try {
    if (state.models.length === 0) {
      await loadModelList()
    }
    const payload = await api('api/status', {
      method: 'POST',
      body: JSON.stringify({
        models: state.models,
        window: state.timeWindow,
      }),
    })
    const data = payload.data || []
    render(data)
    state.countdown = state.refreshSeconds
    renderCountdown()
  } catch (error) {
    renderError(error)
  } finally {
    setBusy(false)
  }
}

function setBusy(busy) {
  els.refreshButton.disabled = busy
  if (busy) {
    els.countdown.textContent = '...'
  } else {
    renderCountdown()
  }
}

function render(data) {
  els.models.innerHTML = ''
  els.empty.hidden = data.length > 0
  renderSubtitle(data.length)
  for (const item of data) {
    els.models.appendChild(renderRow(item))
  }
}

function renderSubtitle(count) {
  const label = timeWindowLabel(state.timeWindow)
  els.subtitle.textContent = `${label} · ${count} models`
}

function renderCountdown() {
  els.countdown.textContent = state.refreshSeconds > 0 ? `${state.countdown}s` : 'off'
}

function renderRow(item) {
  const row = document.createElement('article')
  row.className = 'model-row'
  const slots = Array.isArray(item.slot_data) ? item.slot_data : []

  row.innerHTML = `
    <div class="model-head">
      <div class="name-wrap">
        <h3 class="model-name" title="${escapeHTML(item.model_name || '')}">${escapeHTML(item.model_name || '')}</h3>
      </div>
      <div class="metrics">
        <strong>${escapeHTML(String(item.success_rate ?? 100))}%</strong><span class="sep">·</span>${formatNumber(item.total_requests || 0)}
      </div>
    </div>
    <div class="timeline">
      ${slots.map(slot => renderSlot(slot)).join('')}
    </div>
    <div class="time-labels">
      ${timeLabels(item.time_window || state.timeWindow).map(label => `<span>${label}</span>`).join('')}
    </div>
  `
  return row
}

function renderSlot(slot) {
  const cls = Number(slot.total_requests || 0) === 0 ? 'none' : normalizeStatus(slot.status)
  const rateLabel = cls === 'none' ? '--' : `${slot.success_rate ?? 100}%`
  return `<span
    class="slot ${cls}"
    data-start="${escapeHTML(formatTime(slot.start_time))}"
    data-end="${escapeHTML(formatTime(slot.end_time))}"
    data-total="${escapeHTML(String(slot.total_requests || 0))}"
    data-success="${escapeHTML(String(slot.success_count || 0))}"
    data-rate="${escapeHTML(rateLabel)}"
    data-status="${escapeHTML(statusLabel(cls))}"
  ></span>`
}

function renderError(error) {
  els.models.innerHTML = ''
  els.empty.hidden = false
  els.empty.textContent = `加载失败：${error.message}`
  renderSubtitle(0)
}

function setupAutoRefresh() {
  if (state.timer) window.clearInterval(state.timer)
  if (state.refreshSeconds <= 0) return
  state.timer = window.setInterval(() => {
    state.countdown -= 1
    if (state.countdown <= 0) {
      refresh()
      return
    }
    renderCountdown()
  }, 1000)
}

function normalizeStatus(status) {
  if (status === 'green' || status === 'good') return 'good'
  if (status === 'yellow' || status === 'warn') return 'warn'
  if (status === 'red' || status === 'bad') return 'bad'
  return 'good'
}

function statusLabel(status) {
  if (status === 'good') return '可用'
  if (status === 'warn') return '波动'
  if (status === 'bad') return '不可用'
  return '无请求'
}

function timeWindowLabel(windowValue) {
  if (windowValue === '1h') return '1小时'
  if (windowValue === '6h') return '6小时'
  if (windowValue === '12h') return '12小时'
  return '24小时'
}

function timeLabels(windowValue) {
  if (windowValue === '1h') return ['1h', '30m', 'now']
  if (windowValue === '6h') return ['6h', '3h', 'now']
  if (windowValue === '12h') return ['12h', '6h', 'now']
  return ['24h', '12h', 'now']
}

function formatTime(timestamp) {
  if (!timestamp) return '-'
  return new Date(timestamp * 1000).toLocaleTimeString('zh-CN', {
    hour: '2-digit',
    minute: '2-digit',
  })
}

function formatNumber(value) {
  return Number(value || 0).toLocaleString('zh-CN')
}

function escapeHTML(value) {
  return String(value).replace(/[&<>'"]/g, char => ({
    '&': '&amp;',
    '<': '&lt;',
    '>': '&gt;',
    "'": '&#39;',
    '"': '&quot;',
  }[char]))
}

els.refreshButton.addEventListener('click', refresh)
els.models.addEventListener('pointerover', handleSlotEnter)
els.models.addEventListener('pointermove', handleSlotMove)
els.models.addEventListener('pointerout', handleSlotLeave)

loadConfig()
  .then(loadModelList)
  .then(refresh)
  .then(setupAutoRefresh)
  .catch(renderError)

function handleSlotEnter(event) {
  if (!(event.target instanceof Element)) return
  const slot = event.target.closest('.slot')
  if (!slot || !els.models.contains(slot)) return
  slot.classList.add('active')
  els.tooltip.innerHTML = `
    <div class="tooltip-time">${escapeHTML(slot.dataset.start)} - ${escapeHTML(slot.dataset.end)}</div>
    <div class="tooltip-grid">
      <span>状态<strong>${escapeHTML(slot.dataset.status)}</strong></span>
      <span>成功率<strong>${escapeHTML(slot.dataset.rate)}</strong></span>
      <span>请求<strong>${escapeHTML(slot.dataset.total)}</strong></span>
      <span>成功<strong>${escapeHTML(slot.dataset.success)}</strong></span>
    </div>
  `
  els.tooltip.hidden = false
  positionTooltip(slot)
}

function handleSlotMove(event) {
  if (!(event.target instanceof Element)) return
  const slot = event.target.closest('.slot')
  if (!slot || els.tooltip.hidden) return
  positionTooltip(slot)
}

function handleSlotLeave(event) {
  if (!(event.target instanceof Element)) return
  const slot = event.target.closest('.slot')
  if (!slot) return
  slot.classList.remove('active')
  els.tooltip.hidden = true
}

function positionTooltip(slot) {
  const rect = slot.getBoundingClientRect()
  const x = Math.max(96, Math.min(rect.left + rect.width / 2, window.innerWidth - 96))
  const y = rect.top - 8
  els.tooltip.style.left = `${x}px`
  els.tooltip.style.top = `${y}px`
}

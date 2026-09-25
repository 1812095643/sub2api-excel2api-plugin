(() => {
  "use strict"
  const bridge = new window.Sub2APIPluginBridge()
  const timezones = [
    "Africa/Cairo", "Africa/Johannesburg", "America/Argentina/Buenos_Aires", "America/Chicago", "America/Denver", "America/Los_Angeles", "America/Mexico_City", "America/New_York", "America/Sao_Paulo", "America/Toronto", "America/Vancouver", "Asia/Bangkok", "Asia/Dubai", "Asia/Ho_Chi_Minh", "Asia/Jakarta", "Asia/Kolkata", "Asia/Kuala_Lumpur", "Asia/Manila", "Asia/Seoul", "Asia/Singapore", "Asia/Tokyo", "Australia/Perth", "Australia/Sydney", "Europe/Berlin", "Europe/Istanbul", "Europe/London", "Europe/Moscow", "Europe/Paris", "Pacific/Auckland", "Pacific/Honolulu"
  ]
  const form = document.getElementById("config-form")
  const defaultTimezone = document.getElementById("default-timezone")
  const accountTimezones = document.getElementById("account-timezones")
  const directEnabled = document.getElementById("direct-enabled")
  const directAccountIds = document.getElementById("direct-account-ids")
  const diagnosticEnabled = document.getElementById("diagnostic-enabled")
  const diagnosticOptions = document.getElementById("diagnostic-options")
  const diagnosticModels = document.getElementById("diagnostic-models")
  const diagnosticAccountIds = document.getElementById("diagnostic-account-ids")
  const diagnosticSchedulable = document.getElementById("diagnostic-schedulable")
  const diagnosticConcurrency = document.getElementById("diagnostic-concurrency")
  const scheduleMode = document.getElementById("schedule-mode")
  const scheduleCron = document.getElementById("schedule-cron")
  const scheduleCondition = document.getElementById("schedule-condition")
  const scheduleCronField = document.getElementById("schedule-cron-field")
  const scheduleConditionField = document.getElementById("schedule-condition-field")
  const saveButton = document.getElementById("save-button")
  const testButton = document.getElementById("test-button")
  const excel2apiTestButton = document.getElementById("excel2api-test-button")
  const refreshButton = document.getElementById("refresh-button")
  const status = document.getElementById("status")
  let loaded = false
  let savedConfig = {}
  let dirty = false
  let saving = false
  let testPending = false
  let runtimeRunning = false
  let disposed = false

  for (const timezone of timezones) {
    const option = document.createElement("option")
    option.value = timezone
    option.textContent = timezone
    defaultTimezone.append(option)
  }

  function resize() { bridge.resize(Math.ceil(document.body.scrollHeight)) }
  function setStatus(text, kind = "") { status.textContent = text; status.dataset.kind = kind; resize() }
  function lines(value) { return value.split(/\r?\n/).map(item => item.trim()).filter(Boolean) }
  function markDirty() { dirty = true; setStatus("有未保存的修改"); }
  function updateDiagnosticControls() {
    directEnabled.disabled = !loaded || saving
    directAccountIds.disabled = !loaded || saving || !directEnabled.checked
    diagnosticEnabled.disabled = !loaded || saving
    diagnosticOptions.disabled = !loaded || saving || !diagnosticEnabled.checked
    saveButton.disabled = !loaded || saving
    testButton.disabled = !loaded || saving || testPending || runtimeRunning || !diagnosticEnabled.checked
    excel2apiTestButton.disabled = !loaded || saving || testPending || runtimeRunning || !diagnosticEnabled.checked || !directEnabled.checked || !lines(directAccountIds.value).length
  }
  function updateScheduleFields() {
    scheduleCronField.hidden = scheduleMode.value !== "cron"
    scheduleConditionField.hidden = scheduleMode.value !== "condition"
    resize()
  }
  function fill(config) {
    savedConfig = { ...config }
    defaultTimezone.value = config.default_timezone || "Asia/Singapore"
    accountTimezones.value = Object.entries(config.account_timezones || {}).map(([id, timezone]) => `${id} = ${timezone}`).join("\n")
    directEnabled.checked = config.direct_enabled === true
    directAccountIds.value = (config.direct_account_ids || []).join("\n")
    diagnosticEnabled.checked = config.diagnostic_enabled !== false
    diagnosticModels.value = (config.diagnostic_models || []).join("\n")
    diagnosticAccountIds.value = (config.diagnostic_account_ids || []).join("\n")
    diagnosticSchedulable.checked = config.diagnostic_schedulable_only === true
    diagnosticConcurrency.value = String(config.diagnostic_concurrency || 4)
    scheduleMode.value = config.schedule_mode || "disabled"
    scheduleCron.value = config.schedule_cron || ""
    scheduleCondition.value = config.schedule_condition || "has_schedulable_accounts"
    updateScheduleFields()
    dirty = false
    loaded = true
    updateDiagnosticControls()
    setStatus("配置已加载", "ok")
  }
  function collect() {
    if (!loaded) throw new Error("配置尚未读取完成")
    const overrides = {}
    for (const line of lines(accountTimezones.value)) {
      const match = line.match(/^([1-9][0-9]*)\s*=\s*(\S+)$/)
      if (!match || !timezones.includes(match[2])) throw new Error(`账号时区行无效：${line}`)
      overrides[match[1]] = match[2]
    }
    const config = { ...savedConfig, default_timezone: defaultTimezone.value, account_timezones: overrides, diagnostic_enabled: diagnosticEnabled.checked, direct_enabled: directEnabled.checked }
    if (directEnabled.checked) {
      config.direct_account_ids = collectAccountIds(directAccountIds.value, "Excel2API")
      if (!config.direct_account_ids.length) throw new Error("开启 Excel2API 前，请至少填写一个 Sub2API 账号 ID")
    }
    if (!diagnosticEnabled.checked) return config
    const models = lines(diagnosticModels.value)
    if (models.length > 32 || models.some(model => !model || model.length > 256 || /[\s*]/u.test(model))) throw new Error("检测模型需逐行填写，最多 32 个，且不能包含空白或通配符")
    const accountIds = collectAccountIds(diagnosticAccountIds.value, "检测")
    const concurrency = Number(diagnosticConcurrency.value || 4)
    if (!Number.isInteger(concurrency) || concurrency < 1 || concurrency > 16) throw new Error("并行数必须在 1 到 16 之间")
    if (scheduleMode.value === "cron" && !scheduleCron.value.trim()) throw new Error("Cron 定时模式需要填写表达式")
    return { ...config, diagnostic_models: models, diagnostic_account_ids: accountIds, diagnostic_schedulable_only: diagnosticSchedulable.checked, diagnostic_concurrency: concurrency, schedule_mode: scheduleMode.value, schedule_cron: scheduleCron.value.trim(), schedule_condition: scheduleCondition.value }
  }
  function collectAccountIds(value, kind) {
    const raw = lines(value)
    if (raw.length > 128 || raw.some(id => !/^[1-9][0-9]*$/u.test(id) || !Number.isSafeInteger(Number(id)))) throw new Error(`${kind}账号 ID 需逐行填写正整数，最多 128 个`)
    const ids = raw.map(Number)
    if (new Set(ids).size !== ids.length) throw new Error(`${kind}账号 ID 不能重复`)
    return ids
  }
  function renderResults(items) {
    const rows = Array.isArray(items) ? items : []
    const normal = rows.filter(item => item.status === "normal").length
    const degraded = rows.filter(item => item.status === "degraded").length
    const failed = rows.filter(item => item.status === "failed").length
    document.getElementById("stat-total").textContent = String(rows.length)
    document.getElementById("stat-normal").textContent = String(normal)
    document.getElementById("stat-degraded").textContent = String(degraded)
    document.getElementById("stat-failed").textContent = String(failed)
    document.getElementById("results-summary").textContent = rows.length ? `已完成 ${rows.length} 个并行检测请求：正常 ${normal}，疑似降智 ${degraded}，失败或跳过 ${rows.length - normal - degraded}` : "尚未运行检测。"
    const list = document.getElementById("results-list")
    list.replaceChildren()
    for (const item of rows) {
      const row = document.createElement("div")
      row.className = "result-row"
      const account = document.createElement("span")
      account.className = "result-account"
      account.textContent = `账号 ${item.account_id}`
      const model = document.createElement("span")
      model.className = "result-model"
      model.textContent = item.model || "—"
      const state = document.createElement("span")
      state.className = `result-status ${item.status || "failed"}`
      state.textContent = item.status === "normal" ? "正常" : item.status === "degraded" ? "疑似降智" : item.status === "skipped" ? "已跳过" : "失败"
      row.append(account, model, state)
      const reason = item.reason || (item.predicted_model ? `识别为 ${item.predicted_model}，概率 ${(Number(item.probability || 0) * 100).toFixed(1)}%` : "")
      if (reason) { const detail = document.createElement("span"); detail.className = "result-reason"; detail.textContent = reason; row.append(detail) }
      list.append(row)
    }
    resize()
  }
  async function save() {
    if (saving) return false
    saving = true
    updateDiagnosticControls()
    try {
      const config = collect()
      const saved = await bridge.saveConfig(config)
      fill(saved)
      setStatus(saved.diagnostic_enabled === false ? "已保存，测试已关闭" : "已保存", "ok")
      void refreshRuntime()
      return true
    } catch (error) {
      setStatus(error?.message || "保存未完成", "error")
      return false
    } finally { saving = false; updateDiagnosticControls() }
  }
  async function runTest(kind = "降智检测", excel2apiMode = false) {
    if (testPending || runtimeRunning || !loaded || !diagnosticEnabled.checked) return
    testPending = true
    let excel2apiModeApplied = false
    let originalConfig = {}
    updateDiagnosticControls()
    try {
      if (dirty && !(await save())) return
      originalConfig = { ...savedConfig }
      if (excel2apiMode) {
        if (!directEnabled.checked || !lines(directAccountIds.value).length) throw new Error("Excel2API 专用测试需要先开启并填写账号 ID")
        const testConfig = collect()
        testConfig.diagnostic_account_ids = collectAccountIds(directAccountIds.value, "Excel2API")
        const saved = await bridge.saveConfig(testConfig)
        fill(saved)
        excel2apiModeApplied = true
      }
      if (!diagnosticEnabled.checked || savedConfig.diagnostic_enabled === false) return
      setStatus(kind === "Excel2API" ? "正在执行 Excel2API 专用测试…" : "正在按并行数检测…")
      const result = await bridge.testConfig(60000)
      const data = JSON.parse(result.status_json || "{}")
      const items = data.diagnostics?.last_results || data.items
      if (Array.isArray(items) && items.length) renderResults(items)
      if (savedConfig.diagnostic_enabled !== false && diagnosticEnabled.checked) setStatus(result.message || "检测完成", result.success ? "ok" : "error")
      if (result.success) await waitForDiagnostic()
    } catch (error) {
      if (savedConfig.diagnostic_enabled !== false && diagnosticEnabled.checked) setStatus(error?.message || "检测未完成", "error")
    } finally {
      if (excel2apiModeApplied) {
        try {
          const cleanup = { ...originalConfig }
          const saved = await bridge.saveConfig(cleanup)
          fill(saved)
        } catch (error) {
          setStatus(error?.message || "Excel2API 测试配置未恢复", "error")
        }
      }
      testPending = false; updateDiagnosticControls(); resize()
    }
  }
  async function waitForDiagnostic() {
    for (let attempt = 0; attempt < 1800; attempt += 1) {
      if (disposed) return
      const running = await refreshRuntime()
      if (!running) return
      await new Promise(resolve => setTimeout(resolve, 1000))
    }
    if (savedConfig.diagnostic_enabled !== false) setStatus("检测仍在后台运行，可点击刷新查看结果", "")
  }
  async function refreshRuntime() {
    if (disposed) return false
    let running = false
    try {
      const health = await bridge.status()
      const data = JSON.parse(health.status_json || "{}")
      const locale = data.request_locale || {}
      const direct = data.excel2api || data.direct || {}
      const codexCatalog = data.codex_tool_catalog || {}
      running = data.diagnostics?.running === true
      runtimeRunning = running
      const enabled = data.diagnostics?.enabled ?? (savedConfig.diagnostic_enabled !== false)
      const stopping = data.diagnostics?.stopping === true
      const schedule = locale.schedule_mode === "cron" ? `Cron ${locale.schedule_cron}` : locale.schedule_mode === "condition" ? `条件 ${locale.schedule_condition}` : "自动任务关闭"
      const testState = !enabled ? (running ? "测试已关闭 · 正在停止当前检测" : "测试已关闭") : stopping ? "正在停止上一轮检测" : running ? `检测进行中 · 并行 ${locale.diagnostic_concurrency || 4}` : `测试已开启 · ${schedule}`
      const directState = direct.enabled ? `Excel2API ${direct.account_count || 0} 个账号` : "Excel2API 已关闭"
      document.getElementById("runtime-status").textContent = health.healthy ? `Codex 工具 ${codexCatalog.tool_count || 0} 个 · 默认时区 ${locale.default_timezone || "Asia/Singapore"} · 覆盖 ${locale.timezone_overrides || 0} 个账号 · ${directState} · ${testState}` : health.message || "插件尚未启用。"
      if (!running && data.diagnostics?.last_results?.length) renderResults(data.diagnostics.last_results)
    } catch { document.getElementById("runtime-status").textContent = "插件尚未启用或宿主暂时没有返回状态。" }
    updateDiagnosticControls()
    resize()
    return running
  }
  saveButton.addEventListener("click", save)
  testButton.addEventListener("click", runTest)
  excel2apiTestButton.addEventListener("click", () => runTest("Excel2API", true))
  refreshButton.addEventListener("click", refreshRuntime)
  diagnosticEnabled.addEventListener("change", updateDiagnosticControls)
  directEnabled.addEventListener("change", updateDiagnosticControls)
  scheduleMode.addEventListener("change", () => { updateScheduleFields(); markDirty() })
  form.addEventListener("input", markDirty)
  form.addEventListener("change", markDirty)
  bridge.ready()
  bridge.loadConfig().then(fill).catch(error => setStatus(error?.message || "配置读取未完成", "error"))
  refreshRuntime()
  window.addEventListener("pagehide", () => { disposed = true; bridge.dispose() }, { once: true })
})()

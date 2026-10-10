(() => {
  "use strict";

  const $ = (selector) => document.querySelector(selector);
  const ui = {
    shell: $(".app-shell"), list: $("#message-list"), history: $("#session-list"),
    welcome: $("#welcome"), scroll: $("#conversation-scroll"), prompt: $("#prompt"),
    send: $("#send-button"), model: $("#model-select"), stream: $("#stream-toggle"),
    dialog: $("#settings-dialog"), token: $("#token-input"), settingsError: $("#settings-error"),
    modelStatus: $("#model-sync-status"), toast: $("#toast"),
  };
  const MODES = [
    { label: "助手", instruction: "请使用清楚自然的中文回答。" },
    { label: "代码", instruction: "优先提供能直接使用的代码，并说明必要的实现细节。" },
    { label: "解释", instruction: "分步骤解释问题，先给结论，再解释原因和例子。" },
  ];
  const STORAGE_KEY = "hao-harness-client-token";
  const HISTORY_KEY = "hao-harness-conversations-v1";
  const sessions = [];
  let messages = [];
  let activeSession = null;
  let modeIndex = 0;
  let controller = null;
  let sending = false;
  let toastTimer;
  let token = "";
  let followOutput = true;
  let saveTimer;
  let storageFailed = false;
  let historyInvalid = false;
  try { token = sessionStorage.getItem(STORAGE_KEY) || ""; } catch { /* The workspace remains usable with in-memory credentials. */ }

  $("#api-origin").textContent = location.origin;
  $("#token-input").value = token;

  function notify(message) {
    ui.toast.textContent = message;
    ui.toast.classList.add("visible");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => ui.toast.classList.remove("visible"), 2600);
  }

  function saveSessions() {
    clearTimeout(saveTimer);
    saveTimer = null;
    if (historyInvalid) return;
    try {
      localStorage.setItem(HISTORY_KEY, JSON.stringify({ version: 1, activeId: activeSession?.id, sessions }));
      storageFailed = false;
      $("#save-status").textContent = "已自动保存";
      $("#save-status").classList.remove("failed");
    } catch {
      $("#save-status").textContent = "自动保存失败，请导出对话";
      $("#save-status").classList.add("failed");
      if (!storageFailed) notify("浏览器存储不可用或已满，请导出对话备份。");
      storageFailed = true;
    }
  }

  function scheduleSave() {
    if (!saveTimer) saveTimer = setTimeout(() => { saveTimer = null;saveSessions(); }, 250);
  }

  function restoreSessions() {
    try {
      const saved = JSON.parse(localStorage.getItem(HISTORY_KEY) || "null");
      if (!saved) return false;
      if (saved.version !== 1 || !Array.isArray(saved.sessions)) throw new Error("invalid history");
      for (const session of saved.sessions) {
        if (typeof session.id !== "string" || typeof session.title !== "string" || !Array.isArray(session.messages)) throw new Error("invalid session");
        for (const item of session.messages) {
          if (!["user", "assistant"].includes(item.role) || typeof item.content !== "string") throw new Error("invalid message");
          if (item.status === "pending") {
            item.status = "interrupted";
            item.error = "上次生成已中断，已保存收到的内容。";
          }
        }
      }
      sessions.push(...saved.sessions);
      if (!sessions.length) return false;
      selectSession(sessions.find((s) => s.id === saved.activeId) || sessions[0]);
      return true;
    } catch {
      notify("历史记录无法读取，原始存储已保留。请先导出备份。");
      $("#save-status").textContent = "历史读取失败，请导出备份";
      $("#save-status").classList.add("failed");
      storageFailed = true;
      historyInvalid = true;
      return false;
    }
  }

  async function api(path, options = {}) {
    const headers = new Headers(options.headers || {});
    if (token) headers.set("Authorization", `Bearer ${token}`);
    if (options.body) headers.set("Content-Type", "application/json");
    const response = await fetch(path, { ...options, headers });
    if (!response.ok) {
      let detail = `请求失败（HTTP ${response.status}）`;
      try {
        const body = await response.json();
        detail = body?.error?.message || detail;
      } catch { /* Keep the safe HTTP summary if the body is not JSON. */ }
      const error = new Error(detail);
      error.status = response.status;
      throw error;
    }
    return response;
  }

  async function refreshStatus() {
    const state = $("#server-state");
    const badge = $(".connection-pill");
    try {
      const response = await fetch("/healthz", { cache: "no-store" });
      if (!response.ok) throw new Error("offline");
      state.classList.add("online");
      state.classList.remove("offline");
      setServerStatus(state, "正常运行");
      badge.classList.add("connected");
      badge.classList.remove("disconnected");
      $("#connection-label").textContent = "服务已就绪";
    } catch {
      state.classList.add("offline");
      state.classList.remove("online");
      setServerStatus(state, "暂不可用");
      badge.classList.add("disconnected");
      badge.classList.remove("connected");
      $("#connection-label").textContent = "服务未连接";
    }
  }

  async function loadModels() {
    ui.modelStatus.textContent = "正在连接服务…";
    $("#refresh-models").disabled = true;
    ui.settingsError.textContent = "";
    try {
      const response = await api("/v1/models", { cache: "no-store" });
      const { data = [] } = await response.json();
      if (!data.length) throw new Error("服务暂未配置可用模型");
      const current = ui.model.value;
      const aliases = [...new Set(data.map((model) => model.id).filter((id) => typeof id === "string"))];
      if (!aliases.length) throw new Error("服务返回的模型列表无效");
      ui.model.replaceChildren(...aliases.map((alias) => new Option(alias, alias)));
      ui.model.value = aliases.includes(current) ? current : aliases[0];
      try { localStorage.setItem("hao-harness-model", ui.model.value); } catch { /* Local model selection is optional. */ }
      ui.modelStatus.textContent = `已加载 ${aliases.length} 个模型`;
      updateConnectionLabel();
      return true;
    } catch (error) {
      ui.modelStatus.textContent = error.message;
      ui.settingsError.textContent = error.message;
      return false;
    } finally {
      $("#refresh-models").disabled = false;
    }
  }

  async function refreshOAuthStatus() {
    const panel = $("#openai-oauth-panel");
    try {
      const response = await api("/auth/openai/status", { cache: "no-store" });
      const status = await response.json();
      panel.hidden = false;
      $("#openai-oauth-status").textContent = status.connected
        ? `已授权${status.email ? `：${status.email}` : ""}`
        : "使用 ChatGPT 账号授权模型调用";
      $("#openai-login").textContent = status.connected ? "重新授权当前账号" : "Continue with ChatGPT";
      $("#openai-login").hidden = Boolean(status.cli_login);
      if (status.cli_login) {
        $("#openai-oauth-status").textContent = status.connected
          ? "OpenAI Codex 已授权"
          : "请停止服务，使用 server --login openai 登录后重新启动";
      }
      $("#openai-logout").hidden = !status.connected;
      return status.connected;
    } catch (error) {
      if (error.status !== 404) {
        panel.hidden = false;
        $("#openai-oauth-status").textContent = "OAuth 状态暂时无法读取";
      }
      return false;
    }
  }

  function updateConnectionLabel() {
    $("#connection-label").textContent = ui.model.value || "本地服务";
  }

  // 状态提示直接创建 DOM 节点，避免手写标签字符串在后续修改时漏掉闭合标签。
  function setServerStatus(node, message) {
    const indicator = document.createElement("i");
    node.replaceChildren(indicator, document.createTextNode(` ${message}`));
  }

  // 等待动画的三个圆点分别创建成节点，让文字变化不会重新解析 HTML。
  function setMessageState(node, message) {
    const dots = document.createElement("span");
    dots.className = "typing-dots";
    dots.append(document.createElement("i"), document.createElement("i"), document.createElement("i"));
    node.replaceChildren(dots, document.createTextNode(` ${message}`));
  }

  function openSettings() {
    ui.token.value = token;
    ui.settingsError.textContent = "";
    ui.dialog.showModal();
    if (token) void refreshOAuthStatus();
  }

  function acceptToken() {
    const next = ui.token.value.trim();
    if (!next || /\s/.test(next)) {
      ui.settingsError.textContent = "请输入有效的客户端访问令牌。";
      ui.token.focus();
      return false;
    }
    try { sessionStorage.setItem(STORAGE_KEY, next); }
    catch { ui.settingsError.textContent = "浏览器没有开启会话存储，请允许此页面使用会话存储。";return false; }
    token = next;
    ui.settingsError.textContent = "";
    return true;
  }

  function addMessage(role, text) {
    const row = document.createElement("article");
    row.className = `message-row ${role === "user" ? "user" : "assistant"}`;
    const body = document.createElement("div");
    body.className = "assistant-message";
    body.textContent = text;
    if (role === "user") {
      row.append(body);
    } else {
      renderMarkdown(body, text);
      const avatar = document.createElement("span");
      avatar.className = "message-avatar";
      const icon = document.createElement("img");
      icon.src = "/assets/mark.svg";
      icon.alt = "";
      icon.width = 21;
      icon.height = 21;
      avatar.append(icon);
      row.append(avatar, body);
    }
    ui.list.append(row);
    return { row, body };
  }

  function renderHistory() {
    ui.history.replaceChildren();
    if (!sessions.length) {
      const empty = document.createElement("p");
      empty.className = "empty-sessions";
      empty.textContent = "开始新对话后，它会出现在这里。";
      ui.history.append(empty);
      return;
    }
    for (const session of sessions) {
      const button = document.createElement("button");
      button.type = "button";
      button.className = `session-item${session === activeSession ? " active" : ""}`;
      button.title = session.title;
      const symbol = document.createElement("span");
      symbol.className = "session-symbol";
      symbol.textContent = "◷";
      const title = document.createElement("span");
      title.className = "session-title";
      title.textContent = session.title;
      button.append(symbol, title);
      button.addEventListener("click", () => selectSession(session));
      ui.history.append(button);
    }
  }

  function newSession() {
    if (sending) stopResponse();
    activeSession = { id: crypto.randomUUID(), title: "新对话", messages: [] };
    sessions.unshift(activeSession);
    messages = activeSession.messages;
    ui.list.replaceChildren();
    ui.welcome.hidden = false;
    ui.prompt.value = "";
    ui.prompt.focus();
    ui.shell.classList.remove("sidebar-open");
    renderHistory();
    if (!storageFailed) saveSessions();
    scrollToBottom(true);
  }

  function selectSession(session) {
    if (sending) stopResponse();
    activeSession = session;
    messages = session.messages;
    ui.list.replaceChildren();
    ui.welcome.hidden = messages.length > 0;
    for (const item of messages) {
      const message = addMessage(item.role, item.content);
      if (item.error) renderError(message.body, item.error);
    }
    ui.shell.classList.remove("sidebar-open");
    renderHistory();
    saveSessions();
    scrollToBottom(true);
  }

  function scrollToBottom(force = false) {
    if (force) followOutput = true;
    if (!followOutput) return;
    requestAnimationFrame(() => { if (followOutput) ui.scroll.scrollTop = ui.scroll.scrollHeight; });
  }

  function updateComposer() {
    ui.send.classList.toggle("stop", sending);
    ui.send.setAttribute("aria-label", sending ? "停止生成" : "发送消息");
    ui.send.title = sending ? "停止生成" : "发送消息";
    const icon = document.createElement("span");
    icon.textContent = sending ? "■" : "↑";
    if (sending) icon.className = "stop-square";
    ui.send.replaceChildren(icon);
    $("#composer").classList.toggle("is-sending", sending);
  }

  function stopResponse() {
    if (controller) controller.abort();
  }

  function renderError(node, message) {
    const error = document.createElement("div");
    error.className = "error-card";
    error.textContent = message;
    node.append(error);
  }

  async function sendMessage() {
    if (sending) { stopResponse();return; }
    const text = ui.prompt.value.trim();
    if (!text) { ui.prompt.focus();return; }
    if (!token) { openSettings();ui.settingsError.textContent = "先添加客户端访问令牌，再发送消息。";return; }
    if (messages.length >= 199) { notify("此对话已达到消息数上限，请新建对话。");return; }
    if (!activeSession) newSession();
    // Keep the response attached to its originating session if the user switches.
    const sessionMessages = activeSession.messages;
    const sessionID = activeSession.id;

    ui.welcome.hidden = true;
    ui.prompt.value = "";
    resizePrompt();
    activeSession.title = activeSession.title === "新对话" ? text.slice(0, 32) : activeSession.title;
    renderHistory();
    addMessage("user", text);
    sessionMessages.push({ role: "user", content: text });
    const requestMessages = [{ role: "system", content: MODES[modeIndex].instruction },
      ...sessionMessages.filter((item) => item.content && (!item.status || item.status === "complete"))
        .map(({ role, content }) => ({ role, content }))];
    const draft = { role: "assistant", content: "", status: "pending" };
    sessionMessages.push(draft);
    saveSessions();
    scrollToBottom(true);
    const assistant = addMessage("assistant", "");
    const state = document.createElement("span");
    state.className = "message-state";
    setMessageState(state, "正在连接模型");
    assistant.body.after(state);

    sending = true;
    controller = new AbortController();
    updateComposer();
    const accumulated = {
      get value() { return draft.content; },
      set value(value) { draft.content = value;scheduleSave(); },
    };
    const useStream = ui.stream.checked;
    try {
      const response = await api("/v1/chat/completions", {
        method: "POST",
        headers: { "X-Session-ID": sessionID },
        signal: controller.signal,
        body: JSON.stringify({ model: ui.model.value || "default", messages: requestMessages, stream: useStream }),
      });
      draft.request_id = response.headers.get("X-Request-ID") || "";
      if (!useStream) {
        const result = await response.json();
        accumulated.value = result.choices?.[0]?.message?.content || "";
        state.remove();
        renderMarkdown(assistant.body, accumulated.value);
      } else {
        setMessageState(state, "正在生成");
        await readStream(response, accumulated, assistant.body, state, controller.signal);
      }
      draft.status = accumulated.value ? "complete" : "error";
      if (!accumulated.value) {
        draft.error = "模型没有返回文本内容。";
        renderError(assistant.body, draft.error);
      }
    } catch (error) {
      draft.status = error.name === "AbortError" ? "stopped" : "error";
      if (error.name === "AbortError") {
        if (accumulated.value) {
          draft.error = "已停止生成。部分回复不会用于后续对话。";
          renderError(assistant.body, draft.error);
        } else {
          assistant.row.remove();
          sessionMessages.splice(sessionMessages.indexOf(draft), 1);
        }
      } else {
        if (error.status === 401) {
          token = "";
          try { sessionStorage.removeItem(STORAGE_KEY); } catch { /* Keep the rejected token out of memory. */ }
        }
        draft.error = error.message || "发生连接错误，请检查本地服务设置。";
        renderError(assistant.body, draft.error);
        if (error.status === 401) notify("访问令牌无效，请更新连接凭证");
      }
    } finally {
      state.remove();
      saveSessions();
      sending = false;
      controller = null;
      updateComposer();
      ui.prompt.focus();
      scrollToBottom();
    }
  }

  async function readStream(response, output, target, state, signal) {
    if (!response.body) throw new Error("浏览器无法读取流式响应。");
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let pending = "";
    let ended = false;
    let renderFrame = null;
    function renderOutput() {
      renderFrame = null;
      renderMarkdown(target, output.value);
      scrollToBottom();
    }
    function processEvent(raw) {
      const data = raw.split("\n").filter((line) => line.startsWith("data:")).map((line) => line.slice(5).trimStart()).join("\n");
      if (!data) return;
      if (data === "[DONE]") { ended = true;return; }
      let event;
      try { event = JSON.parse(data); }
      catch { throw new Error("收到无法识别的流式响应。"); }
      if (event.error) throw new Error(event.error.message || "模型流式请求失败。");
      for (const choice of event.choices || []) {
        if (typeof choice.delta?.content === "string") {
          output.value += choice.delta.content;
          if (renderFrame === null) renderFrame = requestAnimationFrame(renderOutput);
          setMessageState(state, "正在生成");
        }
      }
    }
    try {
      while (true) {
        const { value, done } = await reader.read();
        pending += decoder.decode(value || new Uint8Array(), { stream: !done }).replace(/\r\n/g, "\n");
        let boundary;
        while ((boundary = pending.indexOf("\n\n")) !== -1) {
          const current = pending.slice(0, boundary);
          pending = pending.slice(boundary + 2);
          processEvent(current);
          if (ended) { await reader.cancel();return; }
        }
        if (done) break;
      }
      if (!ended) {
        if (signal.aborted) throw new DOMException("generation stopped", "AbortError");
        throw new Error("模型连接提前结束，请重试。" );
      }
    } catch (error) {
      try { await reader.cancel(error); } catch { /* Preserve the original stream error. */ }
      throw error;
    } finally {
      if (renderFrame !== null) cancelAnimationFrame(renderFrame);
      renderOutput();
      reader.releaseLock();
    }
  }

  function resizePrompt() {
    ui.prompt.style.height = "auto";
    ui.prompt.style.height = `${Math.min(ui.prompt.scrollHeight, 190)}px`;
  }

  function cycleMode() {
    modeIndex = (modeIndex + 1) % MODES.length;
    const symbol = document.createElement("span");
    symbol.textContent = "✳";
    const chevron = document.createElement("span");
    chevron.className = "chevron";
    chevron.textContent = "⌄";
    $("#mode-button").replaceChildren(symbol, document.createTextNode(` ${MODES[modeIndex].label} `), chevron);
    notify(`已切换为${MODES[modeIndex].label}风格`);
  }

  $("#new-session").addEventListener("click", newSession);
  ui.send.addEventListener("click", sendMessage);
  ui.prompt.addEventListener("input", resizePrompt);
  ui.prompt.addEventListener("keydown", (event) => {
    if (event.key === "Enter" && !event.shiftKey && !event.isComposing) {
      event.preventDefault();
      sendMessage();
    }
  });
  $("#mode-button").addEventListener("click", cycleMode);
  ui.model.addEventListener("change", () => {
    try { localStorage.setItem("hao-harness-model", ui.model.value); } catch { /* Optional preference. */ }
    updateConnectionLabel();
  });
  for (const selector of ["#open-settings", "#open-settings-top", "#connection-inline"]) $(selector).addEventListener("click", openSettings);
  $("#save-token").addEventListener("click", () => {
    if (acceptToken()) void connectAndClose();
  });
  async function connectAndClose() {
    if (await loadModels()) {
      ui.dialog.close();
      notify("服务连接成功，凭证保存在当前标签页");
    }
  }
  $("#refresh-models").addEventListener("click", () => { if (acceptToken()) void loadModels(); });
  $("#openai-login").addEventListener("click", async () => {
    if (!acceptToken()) return;
    const popup = window.open("about:blank", "openai-oauth", "popup,width=560,height=760");
    try {
      const response = await api("/auth/openai/start", { method: "POST" });
      const { authorization_url: authorizationURL } = await response.json();
      if (!authorizationURL) throw new Error("服务没有返回 OpenAI 授权地址");
      if (popup) popup.location = authorizationURL;
      else location.assign(authorizationURL);
      $("#openai-oauth-status").textContent = "请在 OpenAI 页面完成授权…";
      const started = Date.now();
      const poll = setInterval(async () => {
        if (Date.now() - started > 180000) {
          clearInterval(poll);
          return;
        }
        if (!(await refreshOAuthStatus())) return;
        clearInterval(poll);
        await loadModels();
        notify("OpenAI 账号授权成功");
      }, 1800);
    } catch (error) {
      if (popup) popup.close();
      $("#openai-oauth-status").textContent = error.message;
    }
  });
  $("#openai-logout").addEventListener("click", async () => {
    try {
      await api("/auth/openai/logout", { method: "POST" });
      await refreshOAuthStatus();
      notify("已退出 OpenAI 授权");
    } catch (error) {
      $("#openai-oauth-status").textContent = error.message;
    }
  });
  $("#clear-token").addEventListener("click", () => {
    token = "";
    ui.token.value = "";
    try { sessionStorage.removeItem(STORAGE_KEY); } catch { /* The in-memory value is also cleared. */ }
    ui.modelStatus.textContent = "凭证已清除";
    ui.settingsError.textContent = "";
    ui.model.replaceChildren(new Option("默认模型", "default"));
    ui.dialog.close();
    notify("已清除当前标签页的凭证");
  });
  $("#reveal-token").addEventListener("click", (event) => {
    const visible = ui.token.type === "password";
    ui.token.type = visible ? "text" : "password";
    event.currentTarget.textContent = visible ? "隐藏" : "显示";
  });
  $("#mobile-menu").addEventListener("click", () => ui.shell.classList.toggle("sidebar-open"));
  $("#open-activity").addEventListener("click", () => notify("运行记录将在后续版本开放。"));
  for (const card of document.querySelectorAll(".suggestion-card")) {
    card.addEventListener("click", () => {
      ui.prompt.value = card.dataset.prompt || "";
      resizePrompt();
      ui.prompt.focus();
    });
  }
  document.addEventListener("keydown", (event) => {
    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
      event.preventDefault();newSession();
    }
    if (event.key === "Escape") ui.shell.classList.remove("sidebar-open");
  });
  ui.dialog.addEventListener("click", (event) => {
    const box = ui.dialog.getBoundingClientRect();
    if (event.clientX < box.left || event.clientX > box.right || event.clientY < box.top || event.clientY > box.bottom) ui.dialog.close();
  });
  try {
    const savedModel = localStorage.getItem("hao-harness-model");
    if (savedModel) ui.model.value = savedModel;
  } catch { /* Private browsing can disable persistent preferences. */ }

  ui.scroll.addEventListener("scroll", () => {
    followOutput = ui.scroll.scrollHeight - ui.scroll.clientHeight - ui.scroll.scrollTop < 80;
    $("#jump-bottom").hidden = followOutput;
  });
  $("#jump-bottom").addEventListener("click", () => { scrollToBottom(true);$("#jump-bottom").hidden = true; });
  $("#export-session").addEventListener("click", () => {
    let value = JSON.stringify({ version: 1, activeId: activeSession?.id, sessions }, null, 2);
    if (historyInvalid) {
      try { value = localStorage.getItem(HISTORY_KEY) || value; } catch { /* Export in-memory messages. */ }
    }
    const url = URL.createObjectURL(new Blob([value], { type: "application/json" }));
    const link = document.createElement("a");
    link.href = url;link.download = `hao-conversations-${new Date().toISOString().slice(0, 10)}.json`;link.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  });
  window.addEventListener("pagehide", () => { if (!storageFailed) saveSessions(); });
  if (!restoreSessions()) newSession();
  refreshStatus();
  if (token) loadModels();
})();

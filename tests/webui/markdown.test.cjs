const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { JSDOM } = require("jsdom");

const root = path.resolve(__dirname, "../../internal/webui/static");
function fixture(t, chat, saved) {
  const dom = new JSDOM(fs.readFileSync(path.join(root, "index.html"), "utf8"), {
    url: "http://127.0.0.1:8080/", runScripts: "outside-only", pretendToBeVisual: true,
  });
  t.after(() => dom.window.close());
  const w = dom.window;
  w.Headers = Headers;
  w.TextDecoder = TextDecoder;
  w.sessionStorage.setItem("hao-harness-client-token", "test-token");
  if (saved) w.localStorage.setItem("hao-harness-conversations-v1", saved);
  w.fetch = async (url, options) => {
    if (url === "/v1/chat/completions") return chat(options);
    if (url === "/healthz") return new Response("ok");
    if (url === "/v1/models") return Response.json({ data: [{ id: "default" }] });
    return new Response("", { status: 404 });
  };
  for (const name of ["vendor/marked.js", "vendor/purify.js", "markdown.js", "app.js"]) {
    w.eval(fs.readFileSync(path.join(root, "assets", name), "utf8"));
  }
  return w;
}
async function waitFor(check) {
  const deadline = Date.now() + 2000;
  while (!check()) {
    assert.ok(Date.now() < deadline, "timed out waiting for UI update");
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
}
function send(w, prompt, stream = false) {
  w.document.querySelector("#stream-toggle").checked = stream;
  w.document.querySelector("#prompt").value = prompt;
  w.document.querySelector("#send-button").click();
}
const reply = (w) => w.document.querySelector(".message-row.assistant .assistant-message");
const idle = (w) => !w.document.querySelector("#send-button").classList.contains("stop");
const delta = (content) => `data: ${JSON.stringify({ choices: [{ delta: { content } }] })}\n\n`;

test("JSON replies and history render GFM while user text and request history stay raw", async (t) => {
  const markdown = '# 标题\n\n**粗体** 与 *斜体* ~~删除~~ `inline`\n\n> 引用\n\n1. 有序\n\n- [x] 完成\n- 列表\n\n```go\nfmt.Println("<tag>")\n```\n\n| 名称 | 值 |\n| --- | --- |\n| a | b |\n\n[官网](https://example.com)';
  const requests = [];
  const w = fixture(t, (options) => {
    requests.push(JSON.parse(options.body));
    return Response.json({ choices: [{ message: { content: markdown } }] });
  });
  send(w, "**用户原文** <script>test</script>");
  await waitFor(() => idle(w));
  const body = reply(w);
  for (const selector of ["h1", "strong", "em", "del", "blockquote", "ol", "ul", "pre code.language-go", "table", "input:checked"]) {
    assert.ok(body.querySelector(selector), `missing ${selector}`);
  }
  assert.equal(body.querySelector("pre code").textContent, 'fmt.Println("<tag>")\n');
  assert.equal(body.querySelector("input").disabled, true);
  assert.equal(body.querySelector("a").rel, "noopener noreferrer");
  assert.equal(w.document.querySelector(".message-row.user .assistant-message").textContent, "**用户原文** <script>test</script>");
  assert.equal(w.document.querySelector(".message-row.user strong"), null);
  send(w, "继续");
  await waitFor(() => idle(w));
  assert.equal(requests[1].messages.find((m) => m.role === "assistant").content, markdown);
  w.document.querySelector("#new-session").click();
  w.document.querySelectorAll(".session-item")[1].click();
  assert.equal(reply(w).querySelector("h1").textContent, "标题");
});

test("unsafe HTML, app classes, local links and script URLs are removed", (t) => {
  const w = fixture(t);
  const body = w.document.createElement("div");
  w.renderMarkdown(body, '<script>alert(1)</script><img src=x onerror=alert(1)><svg onload=alert(1)></svg><iframe src="https://evil.test"></iframe><p class="error-card" style="position:fixed" onclick="alert(1)">文本</p><input type=text><input type=checkbox>\n\n[脚本](javascript:alert%281%29) [本地](/auth/openai/logout) [外部](https://example.com)');
  assert.equal(body.querySelector("script,img,svg,iframe,[onerror],[onclick],[style],.error-card,input[type=text]"), null);
  const links = body.querySelectorAll("a");
  assert.equal(links[0].hasAttribute("href"), false);
  assert.equal(links[1].hasAttribute("href"), false);
  assert.equal(links[2].getAttribute("href"), "https://example.com");
  assert.equal(body.querySelector("input").disabled, true);
});

test("streaming renders partial fences, flushes final chunks and replays history", async (t) => {
  let stream;
  const w = fixture(t, () => new Response(new ReadableStream({ start(c) { stream = c; } })));
  send(w, "生成代码", true);
  await waitFor(() => stream);
  stream.enqueue(new TextEncoder().encode(delta("```js\nconst x = ")));
  await waitFor(() => reply(w).querySelector("pre code"));
  assert.equal(reply(w).querySelector("code").textContent, "const x = \n");
  stream.enqueue(new TextEncoder().encode(delta("1;\n```\n\n**完成**") + "data: [DONE]\n\n"));
  await waitFor(() => idle(w));
  assert.equal(reply(w).querySelector("pre code").textContent, "const x = 1;\n");
  assert.equal(reply(w).querySelector("strong").textContent, "完成");
  w.document.querySelector("#new-session").click();
  w.document.querySelectorAll(".session-item")[1].click();
  assert.equal(reply(w).querySelector("strong").textContent, "完成");
});

test("stream errors keep final partial Markdown and a plain error message", async (t) => {
  const w = fixture(t, () => new Response(new ReadableStream({
    start(c) {
      c.enqueue(new TextEncoder().encode(delta("**部分回复**") + 'data: {"error":{"message":"<img onerror=alert(1)>"}}\n\n'));
      c.close();
    },
  })));
  send(w, "生成", true);
  await waitFor(() => idle(w));
  assert.equal(reply(w).querySelector("strong").textContent, "部分回复");
  assert.equal(reply(w).querySelector(".error-card").textContent, "<img onerror=alert(1)>");
  assert.equal(reply(w).querySelector("img"), null);
});

test("stopping a stream flushes pending Markdown before the stopped message", async (t) => {
  let stream;
  const w = fixture(t, (options) => new Response(new ReadableStream({
    start(c) {
      stream = c;
      options.signal.addEventListener("abort", () => c.error(new w.DOMException("stopped", "AbortError")));
    },
  })));
  send(w, "生成", true);
  await waitFor(() => stream);
  stream.enqueue(new TextEncoder().encode(delta("**停止前回复**")));
  await waitFor(() => reply(w).querySelector("strong"));
  w.document.querySelector("#send-button").click();
  await waitFor(() => idle(w));
  assert.equal(reply(w).querySelector("strong").textContent, "停止前回复");
  assert.match(reply(w).querySelector(".error-card").textContent, /已停止生成/);
});

const historyKey = "hao-harness-conversations-v1";
test("saved conversations restore after page restart and remain usable as context", async (t) => {
  const sessionIDs = [];
  const first = fixture(t, (options) => {
    sessionIDs.push(options.headers.get("X-Session-ID"));
    return Response.json({ choices: [{ message: { content: "**保存的回复**" } }] });
  });
  send(first, "第一条消息");
  await waitFor(() => idle(first));
  const saved = first.localStorage.getItem(historyKey);
  assert.ok(saved.includes("保存的回复"));
  assert.ok(!saved.includes("test-token"));
  let request;
  const restored = fixture(t, (options) => {
    sessionIDs.push(options.headers.get("X-Session-ID"));
    request = JSON.parse(options.body);
    return Response.json({ choices: [{ message: { content: "后续回复" } }] });
  }, saved);
  assert.equal(reply(restored).querySelector("strong").textContent, "保存的回复");
  assert.equal(restored.document.querySelectorAll(".session-item").length, 1);
  send(restored, "第二条消息");
  await waitFor(() => idle(restored));
  assert.deepEqual(request.messages.slice(1).map((m) => m.content), ["第一条消息", "**保存的回复**", "第二条消息"]);
  assert.ok(request.messages.every((m) => Object.keys(m).length === 2));
  assert.ok(sessionIDs[0]);
  assert.equal(sessionIDs[0], sessionIDs[1], "restored session must append to the same log file");
  restored.document.querySelector("#new-session").click();
  send(restored, "新会话");
  await waitFor(() => idle(restored));
  assert.notEqual(sessionIDs[2], sessionIDs[0], "new session must get a separate log file");
});

test("partial streams are saved before completion and restore as interrupted", async (t) => {
  let stream;
  const first = fixture(t, (options) => new Response(new ReadableStream({ start(c) {
    stream = c;
    options.signal.addEventListener("abort", () => c.error(new first.DOMException("stopped", "AbortError")));
  } })));
  send(first, "长回复", true);
  await waitFor(() => stream);
  stream.enqueue(new TextEncoder().encode(delta("**已经收到的部分**")));
  await waitFor(() => first.localStorage.getItem(historyKey).includes("已经收到的部分"));
  const saved = first.localStorage.getItem(historyKey);
  const restored = fixture(t, () => Response.json({ choices: [{ message: { content: "继续" } }] }), saved);
  assert.equal(reply(restored).querySelector("strong").textContent, "已经收到的部分");
  assert.match(reply(restored).querySelector(".error-card").textContent, /上次生成已中断/);
  assert.equal(JSON.parse(restored.localStorage.getItem(historyKey)).sessions[0].messages[1].status, "interrupted");
  first.document.querySelector("#send-button").click();
  await waitFor(() => idle(first));
});

test("switching sessions during streaming cannot write the old reply into the new session", async (t) => {
  let stream;
  const w = fixture(t, (options) => new Response(new ReadableStream({ start(c) {
    stream = c;
    options.signal.addEventListener("abort", () => c.error(new w.DOMException("stopped", "AbortError")));
  } })));
  send(w, "旧会话", true);
  await waitFor(() => stream);
  stream.enqueue(new TextEncoder().encode(delta("旧会话的部分回复")));
  await waitFor(() => reply(w).textContent.includes("部分回复"));
  w.document.querySelector("#new-session").click();
  await waitFor(() => idle(w));
  const saved = JSON.parse(w.localStorage.getItem(historyKey));
  assert.equal(saved.sessions[0].messages.length, 0);
  assert.equal(saved.sessions[1].messages[1].content, "旧会话的部分回复");
  assert.equal(saved.sessions[1].messages[1].status, "stopped");
});

test("scrolling to older messages suspends auto-scroll until jumping to the latest", async (t) => {
  let stream;
  const w = fixture(t, () => new Response(new ReadableStream({ start(c) { stream = c; } })));
  const scroll = w.document.querySelector("#conversation-scroll");
  Object.defineProperty(scroll, "scrollHeight", { value: 2000 });
  Object.defineProperty(scroll, "clientHeight", { value: 400 });
  send(w, "长回复", true);
  await waitFor(() => stream);
  scroll.scrollTop = 100;
  scroll.dispatchEvent(new w.Event("scroll"));
  stream.enqueue(new TextEncoder().encode(delta("**新内容**")));
  await waitFor(() => reply(w).querySelector("strong"));
  assert.equal(scroll.scrollTop, 100);
  assert.equal(w.document.querySelector("#jump-bottom").hidden, false);
  w.document.querySelector("#jump-bottom").click();
  await waitFor(() => scroll.scrollTop === 2000);
  stream.enqueue(new TextEncoder().encode("data: [DONE]\n\n"));
  await waitFor(() => idle(w));
});

test("corrupt history is preserved and storage failures are shown without breaking chat", async (t) => {
  const corrupt = "{broken history";
  const w = fixture(t, () => Response.json({ choices: [{ message: { content: "仍可聊天" } }] }), corrupt);
  send(w, "新消息");
  await waitFor(() => idle(w));
  assert.equal(w.localStorage.getItem(historyKey), corrupt);
  assert.match(w.document.querySelector("#save-status").textContent, /导出备份/);
  const other = fixture(t, () => Response.json({ choices: [{ message: { content: "正常回复" } }] }));
  other.Storage.prototype.setItem = () => { throw new other.DOMException("full", "QuotaExceededError"); };
  send(other, "空间已满");
  await waitFor(() => idle(other));
  assert.equal(reply(other).textContent.trim(), "正常回复");
  assert.match(other.document.querySelector("#save-status").textContent, /自动保存失败/);
});

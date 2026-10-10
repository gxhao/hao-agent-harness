(() => {
  "use strict";

  // Keep model output untrusted, including raw HTML and Markdown link targets.
  const options = {
    ALLOWED_TAGS: ["p", "br", "h1", "h2", "h3", "h4", "h5", "h6", "strong", "em", "del",
      "ul", "ol", "li", "blockquote", "pre", "code", "hr", "a", "table", "thead", "tbody",
      "tr", "th", "td", "input"],
    ALLOWED_ATTR: ["href", "title", "class", "start", "align", "type", "checked", "disabled"],
    ALLOW_DATA_ATTR: false,
    ALLOW_ARIA_ATTR: false,
    RETURN_DOM_FRAGMENT: true,
  };

  window.renderMarkdown = (target, source) => {
    try {
      const html = marked.parse(source, { gfm: true, breaks: true, async: false });
      // DOMPurify 会先把 HTML 解析为 DOM 树，再过滤危险节点；未闭合标签由解析器修复，
      // 最终插入页面的是完整的节点片段，不会把模型输出直接留在工作台的 HTML 结构中。
      const fragment = DOMPurify.sanitize(html, options);
      for (const link of fragment.querySelectorAll("a")) {
        const href = link.getAttribute("href");
        // 相对链接可能指向本地控制接口，因此只允许明确的外部网页或邮件地址。
        if (!href || !/^(https?:\/\/|mailto:)/i.test(href.trim())) {
          link.removeAttribute("href");
        } else {
          link.target = "_blank";
          link.rel = "noopener noreferrer";
        }
      }
      for (const input of fragment.querySelectorAll("input")) {
        if (input.getAttribute("type") !== "checkbox") input.remove();
        else input.disabled = true;
      }
      // 仅保留 Markdown 代码围栏的语言名，原始 HTML 不能借 class 改变页面样式。
      for (const element of fragment.querySelectorAll("[class]")) {
        const language = element.tagName === "CODE"
          ? [...element.classList].find((name) => /^language-[\w-]+$/.test(name)) : null;
        element.removeAttribute("class");
        if (language) element.className = language;
      }
      target.classList.add("markdown-body");
      target.replaceChildren(fragment);
    } catch {
      // 解析器遇到异常输入时仍保留回复；textContent 会转义内容，不会破坏消息列表标签。
      target.classList.add("markdown-body");
      target.textContent = source;
    }
  };
})();

(function () {
  "use strict";

  // ---------- DOM ----------
  var $ = function (id) { return document.getElementById(id); };
  var sidebar = $("sidebar"), listEl = $("convo-list"), messagesEl = $("messages"),
      welcomeEl = $("welcome"), scrollEl = $("scroll"), inputEl = $("input"),
      sendBtn = $("send"), modelSel = $("model"), titleEl = $("chat-title"),
      statusEl = $("status"), mcpListEl = $("mcp-list"), openSettingsBtn = $("open-settings"),
      settingsScreen = $("settings-screen"), closeSettingsBtn = $("close-settings"), mcpSaveBtn = $("mcp-save"),
      mcpSettingsListEl = $("settings-mcp-list"), mcpNameIn = $("mcp-name"),
      mcpCmdIn = $("mcp-cmd"), mcpArgsIn = $("mcp-args"), themeSel = $("theme-select");

  // ---------- State ----------
  var STORE_KEY = "chat.convos.v1";
  var MODEL_KEY = "chat.model.v1";
  var convos = [];
  var current = null;   // active conversation object
  var abort = null;     // AbortController while streaming
  var modelsMeta = {};

  function load() {
    try { convos = JSON.parse(localStorage.getItem(STORE_KEY) || "[]"); } catch { convos = []; }
  }
  function uid() { return Date.now().toString(36) + Math.random().toString(36).slice(2, 7); }

  // ---------- Minimal Markdown ----------
  var BT = String.fromCharCode(96);
  var FENCE = BT + BT + BT;

  function esc(s) {
    return s.replace(/&/g, "&").replace(/</g, "<").replace(/>/g, ">").replace(/"/g, "\"");
  }

  // Code spans are pulled out first so bold/italic/link rules never touch them.
  function inline(s) {
    var codes = [];
    var out = "";
    var i = 0;
    while (i < s.length) {
      if (s[i] === BT) {
        var j = s.indexOf(BT, i + 1);
        if (j > i) {
          codes.push(s.slice(i + 1, j));
          out += "\u0000" + (codes.length - 1) + "\u0000";
          i = j + 1;
          continue;
        }
      }
      out += s[i++];
    }
    out = esc(out);
    out = out.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
    out = out.replace(/(^|[^*\w])\*([^*\n]+)\*(?!\w)/g, "$1<em>$2</em>");
    out = out.replace(/(^|[^_\w])_([^_\n]+)_(?!\w)/g, "$1<em>$2</em>");
    out = out.replace(/~~([^~]+)~~/g, "<del>$1</del>");
    out = out.replace(/\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)/g, function (_, text, href) {
      return "<a href=\"" + href + "\" target=\"_blank\" rel=\"noopener\">" + text + "</a>";
    });
    out = out.replace(/\u0000(\d+)\u0000/g, function (_, n) {
      return "<code>" + esc(codes[+n]) + "</code>";
    });
    return out;
  }

  function md(src) {
    var lines = src.replace(/\r\n?/g, "\n").split("\n");
    var out = [];
    var para = [];
    var i = 0;

    function flush() {
      if (para.length) {
        out.push("<p>" + inline(para.join("\n")).replace(/\n/g, "<br>") + "</p>");
        para.length = 0;
      }
    }
    function isFence(l) { return l.trim().indexOf(FENCE) === 0; }
    var liRe = /^(\s*)([-*+]|\d+[.)])\s+(.*)$/;

    while (i < lines.length) {
      var ln = lines[i];

      // Fenced code block (unclosed fences render live while streaming)
      if (isFence(ln)) {
        flush();
        var lang = ln.trim().slice(3).trim().split(/\s+/)[0] || "";
        var buf = [];
        i++;
        while (i < lines.length && !isFence(lines[i])) buf.push(lines[i++]);
        i++; // closing fence (or EOF)
        out.push("<pre><code" + (lang ? " data-lang=\"" + esc(lang) + "\"" : "") + ">" + esc(buf.join("\n")) + "</code></pre>");
        continue;
      }

      // Headings
      var h = ln.match(/^(#{1,3})\s+(.*)$/);
      if (h) {
        flush();
        out.push("<h" + h[1].length + ">" + inline(h[2]) + "</h" + h[1].length + ">");
        i++;
        continue;
      }

      // Horizontal rule
      if (/^\s*(-{3,}|\*{3,})\s*$/.test(ln)) { flush(); out.push("<hr>"); i++; continue; }

      // Blockquote
      if (/^\s*>/.test(ln)) {
        flush();
        var qbuf = [];
        while (i < lines.length && /^\s*>/.test(lines[i])) qbuf.push(lines[i++].replace(/^\s*>\s?/, ""));
        out.push("<blockquote>" + md(qbuf.join("\n")) + "</blockquote>");
        continue;
      }

      // Lists
      var li = ln.match(liRe);
      if (li) {
        flush();
        var ordered = /\d/.test(li[2]);
        var items = [];
        while (i < lines.length) {
          var m = lines[i].match(liRe);
          if (m && /\d/.test(m[2]) === ordered) { items.push(m[3]); i++; }
          else if (items.length && /^\s{2,}\S/.test(lines[i]) && !liRe.test(lines[i])) {
            items[items.length - 1] += "\n" + lines[i].trim();
            i++;
          } else break;
        }
        var tag = ordered ? "ol" : "ul";
        out.push("<" + tag + ">" + items.map(function (x) {
          return "<li>" + inline(x).replace(/\n/g, "<br>") + "</li>";
        }).join("") + "</" + tag + ">");
        continue;
      }

      // Tables
      if (/^\s*\|.*\|\s*$/.test(ln) && i + 1 < lines.length && /^\s*\|?\s*:?-{2,}/.test(lines[i + 1])) {
        flush();
        var rows = [];
        while (i < lines.length && /^\s*\|.*\|\s*$/.test(lines[i])) rows.push(lines[i++]);
        var cells = function (r) {
          return r.trim().replace(/^\||\|$/g, "").split("|").map(function (c) { return inline(c.trim()); });
        };
        var head = cells(rows[0]);
        var body = rows.slice(2).map(cells);
        out.push("<table><thead><tr>" + head.map(function (c) { return "<th>" + c + "</th>"; }).join("") +
          "</tr></thead><tbody>" +
          body.map(function (r) {
            return "<tr>" + r.map(function (c) { return "<td>" + c + "</td>"; }).join("") + "</tr>";
          }).join("") + "</tbody></table>");
        continue;
      }

      // Blank line ends a paragraph
      if (ln.trim() === "") { flush(); i++; continue; }

      para.push(ln);
      i++;
    }
    flush();
    return out.join("\n");
  }

  // ---------- Rendering ----------
  function renderList() {
    listEl.innerHTML = "";
    convos.forEach(function (c) {
      var row = document.createElement("div");
      row.className = "convo" + (current && current.id === c.id ? " active" : "");
      var t = document.createElement("span");
      t.className = "title";
      t.textContent = c.title || "New chat";
      var d = document.createElement("button");
      d.className = "del";
      d.textContent = "✕";
      d.title = "Delete";
      d.onclick = function (e) { e.stopPropagation(); deleteConvo(c.id); };
      row.appendChild(t);
      row.appendChild(d);
      row.onclick = function () { if (!abort) openConvo(c.id); };
      listEl.appendChild(row);
    });
  }

  function buildAssistantEl() {
    var el = document.createElement("div");
    el.className = "msg assistant";
    el.innerHTML = "<div class=\"avatar\">✳</div><div class=\"content\"></div>";
    return el;
  }

  function updateAssistant(el, m, streaming) {
    var c = el.querySelector(".content");
    var html = "";
    if (m.thinking) {
      html += "<details class=\"think\"" + (streaming && !m.content ? " open" : "") +
        "><summary>Thought process</summary><div class=\"tbody\">" + esc(m.thinking) + "</div></details>";
    }
    html += md(m.content || "");
    if (streaming) html += "<span class=\"cursor\"></span>";
    if (m.error) html += "<div class=\"error\">" + esc(m.error) + "</div>";
    if (!streaming) {
      html += "<div class=\"meta\">";
      if (m.model) html += "<span class=\"model-tag\">" + esc(m.model) + "</span>";
      if (m.stats) html += "<span>" + esc(m.stats) + "</span>";
      html += "<button data-act=\"copy\">Copy</button><button data-act=\"retry\">Retry</button></div>";
    }
    c.innerHTML = html;

    if (!streaming) {
      c.querySelector("[data-act=\"copy\"]").onclick = function (e) {
        navigator.clipboard.writeText(m.content || "");
        e.target.textContent = "Copied";
        setTimeout(function () { e.target.textContent = "Copy"; }, 1200);
      };
      c.querySelector("[data-act=\"retry\"]").onclick = retryLast;
    }
  }

  function renderMessages() {
    messagesEl.innerHTML = "";
    var has = current && current.messages.length > 0;
    welcomeEl.style.display = has ? "none" : "";
    titleEl.textContent = current ? current.title || "" : "";
    if (!has) return;
    current.messages.forEach(function (m) {
      if (m.role === "user") {
        var el = document.createElement("div");
        el.className = "msg user";
        var b = document.createElement("div");
        b.className = "bubble";
        b.textContent = m.content;
        el.appendChild(b);
        messagesEl.appendChild(el);
      } else {
        var ael = buildAssistantEl();
        updateAssistant(ael, m, false);
        messagesEl.appendChild(ael);
      }
    });
    scrollBottom();
  }

  function scrollBottom() { scrollEl.scrollTop = scrollEl.scrollHeight; }

  // ---------- Conversation management ----------
  function newConvo() {
    if (abort) return;
    current = { id: uid(), title: "", messages: [] };
    convos.unshift(current);
    renderList();
    renderMessages();
    inputEl.focus();
  }

  function openConvo(id) {
    current = convos.find(function (c) { return c.id === id; }) || null;
    renderList();
    renderMessages();
  }

  function deleteConvo(id) {
    if (abort) return;
    convos = convos.filter(function (c) { return c.id !== id; });
    persist();
    if (current && current.id === id) {
      current = convos[0] || null;
      if (!current) { newConvo(); return; }
    }
    renderList();
    renderMessages();
  }

  function persist() {
    // Empty "New chat" conversations only live in memory.
    var keep = convos.filter(function (c) { return c.messages && c.messages.length > 0; });
    try { localStorage.setItem(STORE_KEY, JSON.stringify(keep)); } catch { }
  }

  // ---------- Sending ----------
  function autosize() {
    inputEl.style.height = "auto";
    inputEl.style.height = Math.min(inputEl.scrollHeight, 220) + "px";
    sendBtn.disabled = !abort && inputEl.value.trim() === "";
  }

  async function send(textOverride) {
    if (abort) { abort.abort(); return; }
    var text = (textOverride !== undefined ? textOverride : inputEl.value).trim();
    if (!text) return;
    if (!current) newConvo();

    if (textOverride === undefined) { inputEl.value = ""; autosize(); }
    current.messages.push({ role: "user", content: text });
    if (!current.title) {
      current.title = text.length > 48 ? text.slice(0, 48) + "…" : text;
      renderList();
    }
    titleEl.textContent = current.title;
    welcomeEl.style.display = "none";

    var userEl = document.createElement("div");
    userEl.className = "msg user";
    var bubble = document.createElement("div");
    bubble.className = "bubble";
    bubble.textContent = text;
    userEl.appendChild(bubble);
    messagesEl.appendChild(userEl);

    var asst = { role: "assistant", content: "", thinking: "", model: "", stats: "", error: "" };
    var el = buildAssistantEl();
    messagesEl.appendChild(el);
    updateAssistant(el, asst, true);
    scrollBottom();

    var requested = modelSel.value || "recommended";
    asst.model = requested;
    localStorage.setItem(MODEL_KEY, requested);

    var payload = {
      model: requested,
      stream: true,
      messages: current.messages
        .filter(function (m) { return m.role === "user" || m.role === "assistant"; })
        .map(function (m) { return { role: m.role, content: m.content }; })
        .filter(function (m) { return m.content; })
    };

    abort = new AbortController();
    sendBtn.classList.add("stop");
    sendBtn.textContent = "■";
    sendBtn.title = "Stop";
    sendBtn.disabled = false;

    var reader = null;
    var raf = 0;
    var finished = false;
    var gotDone = false;
    var paint = function () {
      if (finished) return;
      raf = 0;
      updateAssistant(el, asst, true);
      scrollBottom();
    };

    try {
      var resp = await fetch("/api/chat", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
        signal: abort.signal
      });
      if (!resp.ok) {
        var errText = await resp.text();
        throw new Error("HTTP " + resp.status + ": " + errText.slice(0, 300));
      }

      reader = resp.body.getReader();
      var dec = new TextDecoder();
      var buf = "";

      for (;;) {
        var r = await reader.read();
        if (r.done) break;
        buf += dec.decode(r.value, { stream: true });
        var nl;
        while ((nl = buf.indexOf("\n")) >= 0) {
          var line = buf.slice(0, nl).trim();
          buf = buf.slice(nl + 1);
          if (!line) continue;
          var j;
          try { j = JSON.parse(line); } catch { continue; }
          if (j.error) throw new Error(typeof j.error === "string" ? j.error : JSON.stringify(j.error));
          if (j.model) asst.model = j.model;
          if (j.message) {
            if (j.message.thinking) asst.thinking += j.message.thinking;
            if (j.message.content) asst.content += j.message.content;
          }
          if (j.done) {
            if (j.eval_count && j.eval_duration) {
              asst.stats = (j.eval_count / (j.eval_duration / 1e9)).toFixed(1) + " tok/s";
            }
            gotDone = true;
            break;
          }
        }
        if (gotDone) break;
        if (!raf) raf = requestAnimationFrame(paint);
      }
    } catch (err) {
      if (err && err.name === "AbortError") asst.stats = "stopped";
      else asst.error = (err && err.message) || String(err);
    } finally {
      finished = true;
      if (raf) cancelAnimationFrame(raf);
      if (reader) {
        try { await reader.cancel(); } catch { }
      }
      abort = null;
      sendBtn.classList.remove("stop");
      sendBtn.textContent = "↑";
      sendBtn.title = "Send (Enter)";
      if (requested === "recommended" && asst.model && asst.model !== "recommended") {
        asst.model = "recommended → " + asst.model;
      }
      current.messages.push(asst);
      updateAssistant(el, asst, false);
      scrollBottom();
      persist();
      autosize();
      inputEl.focus();
    }
  }

  function retryLast() {
    if (abort || !current) return;
    while (current.messages.length && current.messages[current.messages.length - 1].role === "assistant") {
      current.messages.pop();
    }
    var last = current.messages.pop();
    if (!last || last.role !== "user") { renderMessages(); return; }
    renderMessages();
    send(last.content);
  }

  // ---------- Models ----------
  async function loadModels() {
    try {
      var r = await fetch("/web/models");
      var j = await r.json();
      modelSel.innerHTML = "";
      j.models.forEach(function (m) {
        var o = document.createElement("option");
        o.value = m.name;
        o.textContent = m.name;
        o.title = m.description;
        modelsMeta[m.name] = m.description;
        modelSel.appendChild(o);
      });
      var saved = localStorage.getItem(MODEL_KEY);
      if (saved && modelsMeta[saved] !== undefined) modelSel.value = saved;
      statusEl.textContent = (j.models.length - 1) + " models · fallback " + j.fallback;
    } catch {
      modelSel.innerHTML = "<option value=\"recommended\">recommended</option>";
      statusEl.textContent = "Could not load models";
    }
    modelSel.title = modelsMeta[modelSel.value] || "Model";
  }

  // ---------- Events ----------
  inputEl.addEventListener("input", autosize);
  inputEl.addEventListener("keydown", function (e) {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      send();
    }
  });
  sendBtn.addEventListener("click", function () { send(); });
  $("new-chat").addEventListener("click", newConvo);
  $("toggle-sidebar").addEventListener("click", function () { sidebar.classList.toggle("hidden"); });
  modelSel.addEventListener("change", function () {
    modelSel.title = modelsMeta[modelSel.value] || "Model";
    localStorage.setItem(MODEL_KEY, modelSel.value);
  });

  // Theme Management
  function applyTheme(theme) {
    var root = document.documentElement;
    root.classList.remove("dark-theme", "light-theme");
    if (theme === "dark") {
      root.classList.add("dark-theme");
    } else if (theme === "light") {
      root.classList.add("light-theme");
    } else if (theme === "system") {
      if (window.matchMedia('(prefers-color-scheme: dark)').matches) {
        root.classList.add("dark-theme");
      }
    }
    localStorage.setItem("chat.theme.v1", theme);
    
    // Update PWA window decoration color
    if (typeof updateThemeColor === "function") {
      updateThemeColor();
    }
  }

  // Listen for system theme changes when set to 'system'
  window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', function() {
    if (localStorage.getItem("chat.theme.v1") === "system" || !localStorage.getItem("chat.theme.v1")) {
      applyTheme("system");
    }
  });

  themeSel.onchange = function () {
    applyTheme(this.value);
  };

  // Settings Events
  openSettingsBtn.onclick = function () {
    settingsScreen.style.display = "block";
    loadMcpServers();
  };
  closeSettingsBtn.onclick = function () {
    settingsScreen.style.display = "none";
  };
  mcpSaveBtn.onclick = async function () {
    var name = mcpNameIn.value.trim();
    var cmd = mcpCmdIn.value.trim();
    var argsStr = mcpArgsIn.value.trim();
    if (!name || !cmd) { alert("Name and Command are required"); return; }
    var args = argsStr ? argsStr.split(",").map(function (s) { return s.trim(); }) : [];
    
    try {
      var r = await fetch("/web/mcp", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ action: "add", config: { name: name, command: cmd, args: args } })
      });
      if (!r.ok) throw new Error(await r.text());
      mcpNameIn.value = ""; mcpCmdIn.value = ""; mcpArgsIn.value = "";
      loadMcpServers();
    } catch (e) { alert("Error: " + e.message); }
  };

  async function loadMcpServers() {
    try {
      var r = await fetch("/web/mcp");
      var servers = await r.json();
      
      mcpListEl.innerHTML = "";
      mcpSettingsListEl.innerHTML = "";
      
      Object.keys(servers).forEach(function (name) {
        var s = servers[name];
        
        // Sidebar list
        var sItem = document.createElement("div");
        sItem.style.padding = "4px 8px";
        sItem.style.borderRadius = "6px";
        sItem.style.fontSize = "0.8rem";
        sItem.style.opacity = "0.8";
        sItem.style.cursor = "default";
        sItem.onmouseover = function() { this.style.background = "var(--surface)"; this.style.opacity = "1"; };
        sItem.onmouseout = function() { this.style.background = "transparent"; this.style.opacity = "0.8"; };
        sItem.textContent = "✳ " + name;
        mcpListEl.appendChild(sItem);
        
        // Settings list
        var mItem = document.createElement("div");
        mItem.style.display = "flex";
        mItem.style.justifyContent = "space-between";
        mItem.style.alignItems = "center";
        mItem.style.padding = "12px";
        mItem.style.background = "var(--surface)";
        mItem.style.border = "1px solid var(--border)";
        mItem.style.borderRadius = "8px";
        
        var info = document.createElement("span");
        info.textContent = name + " (" + s.command + ")";
        
        var del = document.createElement("button");
        del.textContent = "✕";
        del.title = "Remove";
        del.style.background = "transparent";
        del.style.color = "var(--text)";
        del.style.border = "1px solid var(--border)";
        del.style.borderRadius = "4px";
        del.style.cursor = "pointer";
        del.style.padding = "2px 8px";
        del.style.fontSize = "0.8rem";
        del.style.opacity = "0.7";
        del.style.transition = "opacity .15s";
        del.onmouseover = function() { this.style.opacity = "1"; };
        del.onmouseout = function() { this.style.opacity = "0.7"; };
        del.onclick = async function () {
          try {
            var dr = await fetch("/web/mcp", {
              method: "POST",
              headers: { "Content-Type": "application/json" },
              body: JSON.stringify({ action: "remove", name: name })
            });
            if (!dr.ok) throw new Error(await dr.text());
            loadMcpServers();
          } catch (e) { alert("Error: " + e.message); }
        };
        
        mItem.appendChild(info);
        mItem.appendChild(del);
        mcpSettingsListEl.appendChild(mItem);
      });
    } catch (e) { console.error("Failed to load MCP servers", e); }
  }

  // ---------- Init ----------
  load();
  var savedTheme = localStorage.getItem("chat.theme.v1") || "system";
  themeSel.value = savedTheme;
  applyTheme(savedTheme);
  
  convos = convos.filter(function (c) { return c.messages && c.messages.length; });
  loadModels().then(function () {
    if (convos.length) { current = convos[0]; renderList(); renderMessages(); }
    else newConvo();
    autosize();
  });
  loadMcpServers();
})();
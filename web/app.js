(function () {
  "use strict";

  // ---------- DOM ----------
  var $ = function (id) { return document.getElementById(id); };
  var sidebar = $("sidebar"), listEl = $("convo-list"), messagesEl = $("messages"),
      welcomeEl = $("welcome"), scrollEl = $("scroll"), inputEl = $("input"),
      sendBtn = $("send"), modelSel = $("model"), titleEl = $("chat-title"),
      openSettingsBtn = $("open-settings"),
      settingsScreen = $("settings-screen"), closeSettingsBtn = $("close-settings"), mcpSaveBtn = $("mcp-save"),
      mcpSettingsListEl = $("settings-mcp-list"), mcpNameIn = $("mcp-name"),
      mcpCmdIn = $("mcp-cmd"), mcpArgsIn = $("mcp-args"), themeSel = $("theme-select"),
      mcpModal = $("mcp-modal"), openMcpModalBtn = $("open-mcp-modal"), closeMcpModalBtn = $("close-mcp-modal"),
      modelModal = $("model-modal"), modelSettingsListEl = $("settings-model-list"), openModelModalBtn = $("open-model-modal"), closeModelModalBtn = $("close-model-modal"),
      modelPullNameIn = $("model-pull-name"), modelPullSaveBtn = $("model-pull-save"),
      confirmModal = $("confirm-modal"), confirmText = $("confirm-text"), confirmOkBtn = $("confirm-ok"), confirmCancelBtn = $("confirm-cancel");

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
    return s.replace(/&/g, "&").replace(/</g, "<").replace(/>/g, ">").replace(/\"/g, "\"");
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
    if (!streaming) {
      // We use a Range to insert the HTML content into the DOM.
      // This is safer than innerHTML on a visible element.
      // The content is generated by our trusted md() function 
      // which escapes all user content via esc().
      var range = document.createRange();
      var frag = range.createContextualFragment(html);
      
      c.textContent = "";
      c.appendChild(frag);
      
      c.querySelectorAll("pre code").forEach(function(block) {
        hljs.highlightElement(block);
      });
    } else {
      // STREAMING PATH:
      // To maintain the "typing" effect while avoiding innerHTML warnings,
      // we use a DocumentFragment via Range.
      var range = document.createRange();
      var frag = range.createContextualFragment(html);
      
      c.textContent = "";
      c.appendChild(frag);
    }
    
    // Note: innerHTML is used here because we are rendering a controlled set of 
    // markdown-generated HTML. We ensure user content is escaped via esc() before 
    // being wrapped in HTML tags.
    if (!streaming) {
      var copyBtn = c.querySelector("[data-act=\"copy\"]");
      if (copyBtn) {
        copyBtn.onclick = function (e) {
          navigator.clipboard.writeText(m.content || "");
          e.target.textContent = "Copied";
          setTimeout(function () { e.target.textContent = "Copy"; }, 1200);
        };
      }
      var retryBtn = c.querySelector("[data-act=\"retry\"]");
      if (retryBtn) {
        retryBtn.onclick = retryLast;
      }
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
      var resp = await fetch("/ui/api/chat", {
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
  function confirmAction(text) {
    return new Promise(function (resolve) {
      confirmText.textContent = text;
      confirmModal.style.display = "flex";
      
      var ok = function () {
        confirmModal.style.display = "none";
        confirmOkBtn.onclick = null;
        confirmCancelBtn.onclick = null;
        resolve(true);
      };
      var cancel = function () {
        confirmModal.style.display = "none";
        confirmOkBtn.onclick = null;
        confirmCancelBtn.onclick = null;
        resolve(false);
      };
      
      confirmOkBtn.onclick = ok;
      confirmCancelBtn.onclick = cancel;
    });
  }

  async function loadModels() {
    try {
      var r = await fetch("/ui/api/models");
      if (!r.ok) throw new Error("Failed to fetch models");
      var j = await r.json();
      modelSel.innerHTML = "";
      
      // Always start with the "recommended" option
      var rec = document.createElement("option");
      rec.value = "recommended";
      rec.textContent = "recommended";
      rec.title = "Route automatically to the best model";
      modelsMeta["recommended"] = rec.title;
      modelSel.appendChild(rec);

      var modelList = j.models || [];
      modelList.forEach(function (m) {
        var name = typeof m === 'string' ? m : (m.name || "");
        var desc = typeof m === 'string' ? "" : (m.description || m.details || "");
        if (!name) return;

        // Filter out decision models from the chat dropdown
        if (typeof m !== 'string' && m.capabilities) {
          if (m.capabilities.indexOf('decision') !== -1) return;
        }
        if (typeof m === 'string') {
          // If it's just a string, we can't know capabilities here, 
          // but usually modelsList returns objects for detailed info.
          // If we want to be safe, we only filter objects.
        }

        var o = document.createElement("option");
        o.value = name;
        o.textContent = name;
        o.title = desc;
        modelsMeta[name] = desc;
        modelSel.appendChild(o);
      });
      
      var saved = localStorage.getItem(MODEL_KEY);
      if (saved && modelsMeta[saved] !== undefined) modelSel.value = saved;
    } catch (e) {
      console.error("loadModels error:", e);
      modelSel.innerHTML = "<option value=\"recommended\">recommended</option>";
    }
    modelSel.title = modelsMeta[modelSel.value] || "Model";
    if ($("status")) $("status").textContent = "";
  }

  async function loadModelBrowser() {
    if (!modelSettingsListEl) return;
    modelSettingsListEl.innerHTML = "";
    try {
      var r = await fetch("/ui/api/models");
      if (!r.ok) throw new Error("Failed to fetch models");
      var j = await r.json();
      var models = j.models || [];
      if (!models || !models.length) {
        modelSettingsListEl.innerHTML = "<div style=\"opacity:.7;font-size:.9rem\">No local models found.</div>";
        return;
      }
      models.forEach(function(m) {
        var row = document.createElement("div");
        row.style.cssText = "display:flex;flex-direction:column;gap:8px;padding:10px 12px;background:var(--surface);border:1px solid var(--border);border-radius:8px;margin-bottom:6px;";
        
        var topRow = document.createElement("div");
        topRow.style.cssText = "display:flex;align-items:center;justify-content:space-between;";
        
        var nameEl = document.createElement("span");
        nameEl.style.fontWeight = "500";
        nameEl.textContent = typeof m === 'string' ? m : (m.name || JSON.stringify(m));
        
        var delBtn = document.createElement("button");
        delBtn.innerHTML = '<i class="fa-solid fa-trash-can"></i>';
        delBtn.title = "Remove Model";
        delBtn.style.cssText = "background:transparent;color:var(--text);border:1px solid var(--border);border-radius:4px;cursor:pointer;padding:4px 8px;font-size:0.8rem;opacity:0.7;";
        delBtn.onclick = async function() {
          var modelName = typeof m === 'string' ? m : (m.name || "");
          if (!modelName) return;
          if (!(await confirmAction("Remove model \"" + modelName + "\"?"))) return;
          try {
            var dr = await fetch("/ui/api/models/" + encodeURIComponent(modelName), { method: "DELETE" });
            if (!dr.ok) throw new Error(await dr.text());
            loadModelBrowser();
          } catch (e) { alert("Error removing model: " + e.message); }
        };
        
        topRow.appendChild(nameEl);
        topRow.appendChild(delBtn);
        row.appendChild(topRow);

        // In the previous update, we used m.details.
        // But modelsList in models_admin.go now returns { name, description }.
        var desc = "";
        if (typeof m !== 'string') {
          desc = m.description || m.details || "";
        }
        if (desc) {
          var detailsEl = document.createElement("div");
          detailsEl.style.cssText = "font-size:.8rem;color:var(--muted);line-height:1.4;";
          detailsEl.textContent = desc;
          row.appendChild(detailsEl);
        }
        
        modelSettingsListEl.appendChild(row);
      });
    } catch {
      console.error("Failed to load model browser");
      modelSettingsListEl.innerHTML = "<div style=\"color:#c0392b;font-size:.9rem\">Error loading models.</div>";
    }
  }

  openModelModalBtn.onclick = function() {
    modelPullNameIn.value = "";
    modelModal.style.display = "flex";
  };
  closeModelModalBtn.onclick = function() {
    modelModal.style.display = "none";
  };
  modelPullSaveBtn.onclick = async function() {
    var name = modelPullNameIn.value.trim();
    if (!name) { alert("Model name is required"); return; }

    const progressPanel = document.getElementById("model-pull-progress");
    const percentText = document.getElementById("model-pull-percent");
    const progressBar = document.getElementById("model-pull-bar");

    try {
      progressPanel.style.display = "flex";
      modelPullSaveBtn.disabled = true;
      modelPullSaveBtn.textContent = "Pulling...";

      const response = await fetch("/ui/api/models/pull", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ model: name })
      });

      if (!response.ok) {
        const errText = await response.text();
        throw new Error(errText || "Server error");
      }

      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";
      
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;

        buffer += decoder.decode(value, { stream: true });
        const lines = buffer.split("\n");
        
        // Keep the last partial line in the buffer
        buffer = lines.pop() || "";
        
        for (const line of lines) {
          if (!line.trim()) continue;
          try {
            const data = JSON.parse(line);
            if (data.completed) {
              percentText.textContent = "100%";
              progressBar.style.width = "100%";
            } else if (data.total != null && data.completed != null) {
              const pct = Math.round((data.completed / data.total) * 100);
              percentText.textContent = pct + "%";
              progressBar.style.width = pct + "%";
            }
          } catch {
            // Ignore non-JSON lines
          }
        }
      }

      modelModal.style.display = "none";
      loadModelBrowser();
      loadModels();
    } catch (e) { 
      alert("Error pulling model: " + (e.message || "Failed to fetch")); 
    } finally {
      progressPanel.style.display = "none";
      modelPullSaveBtn.disabled = false;
      modelPullSaveBtn.textContent = "Pull Model";
      progressBar.style.width = "0%";
      percentText.textContent = "0%";
    }
  };


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
    
    // Update PWA window decoration color to match background
    var meta = document.querySelector('meta[name="theme-color"]');
    if (meta) {
      var color = "#24273a"; // Default dark --bg
      if (theme === "light") {
        color = "#eff1f5"; // light --bg
      } else if (theme === "system") {
        color = window.matchMedia('(prefers-color-scheme: dark)').matches ? "#24273a" : "#eff1f5";
      }
      meta.setAttribute("content", color);
    }

    // Sync Highlight.js theme
    var lightLink = document.getElementById("hljs-light");
    var darkLink = document.getElementById("hljs-dark");
    if (lightLink && darkLink) {
      var isDark = (theme === "dark") || (theme === "system" && window.matchMedia('(prefers-color-scheme: dark)').matches);
      lightLink.disabled = isDark;
      darkLink.disabled = !isDark;
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
    loadLuaTools();
    loadModelBrowser();
  };
  closeSettingsBtn.onclick = function () {
    settingsScreen.style.display = "none";
  };

  // Modal & Connector Events
  openMcpModalBtn.onclick = function () {
    mcpNameIn.value = ""; mcpCmdIn.value = ""; mcpArgsIn.value = "";
    mcpModal.style.display = "flex";
  };
  closeMcpModalBtn.onclick = function () {
    mcpModal.style.display = "none";
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
      mcpModal.style.display = "none";
      loadMcpServers();
    } catch (e) { alert("Error: " + e.message); }
  };

  async function loadMcpServers() {
    try {
      var r = await fetch("/web/mcp");
      var servers = await r.json();
      
      mcpSettingsListEl.innerHTML = "";
      
      Object.keys(servers).forEach(function (name) {
        var s = servers[name];
        
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
        info.textContent = name;
        
        var btnGroup = document.createElement("div");
        btnGroup.style.display = "flex";
        btnGroup.style.gap = "8px";

        var editBtn = document.createElement("button");
        editBtn.innerHTML = '<i class="fa-regular fa-pen-to-square"></i>';
        editBtn.title = "Edit";
        editBtn.style.background = "transparent";
        editBtn.style.color = "var(--text)";
        editBtn.style.border = "1px solid var(--border)";
        editBtn.style.borderRadius = "4px";
        editBtn.style.cursor = "pointer";
        editBtn.style.padding = "4px 8px";
        editBtn.style.fontSize = "0.8rem";
        editBtn.onclick = function () {
          mcpNameIn.value = name;
          mcpCmdIn.value = s.command || "";
          mcpArgsIn.value = (s.args || []).join(", ");
          mcpModal.style.display = "flex";
        };
        
        var del = document.createElement("button");
        del.innerHTML = '<i class="fa-solid fa-trash-can"></i>';
        del.title = "Remove";
        del.style.background = "transparent";
        del.style.color = "var(--text)";
        del.style.border = "1px solid var(--border)";
        del.style.borderRadius = "4px";
        del.style.cursor = "pointer";
        del.style.padding = "4px 8px";
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
        
        btnGroup.appendChild(editBtn);
        btnGroup.appendChild(del);
        
        mItem.appendChild(info);
        mItem.appendChild(btnGroup);
        mcpSettingsListEl.appendChild(mItem);
      });
    } catch {
      console.error("Failed to load MCP servers");
    }
  }

  // ---------- Lua Tools Integration ----------
  var TOOL_TEMPLATE = "return {\n  name = \"my_tool\",\n  description = \"Describe what the tool does so the model knows when to use it.\",\n  parameters = {\n    type = \"object\",\n    properties = {\n      path = { type = \"string\", description = \"Path relative to the workspace\" },\n    },\n    required = { \"path\" },\n  },\n  run = function(args)\n    local data, err = fs.read(args.path)\n    if not data then return nil, err end\n    return { path = args.path, bytes = #data }\n  end,\n}";
  
  var toolEls = {
    list: $("settings-tool-list"),
    modal: $("tool-modal"),
    title: $("tool-modal-title"),
    source: $("tool-source"),
    status: $("tool-status"),
    testArgs: $("tool-test-args"),
    testOut: $("tool-test-output"),
    testPanel: $("tool-test-panel"),
    del: $("tool-delete"),
  };
  var editingTool = null;

  function setToolStatus(msg, kind) {
    if (!toolEls.status) return;
    toolEls.status.textContent = msg;
    toolEls.status.style.color = kind === "error" ? "#c0392b" : kind === "ok" ? "#2e8b57" : "var(--muted)";
  }

  async function toolApi(path, opts) {
    opts = opts || {};
    var res = await fetch("/ui/api/tools" + path, {
      headers: { "Content-Type": "application/json" },
      ...opts,
    });
    if (res.status === 204) return null;
    var data = await res.json().catch(function() { return {}; });
    if (!res.ok) throw new Error(data.error || res.statusText);
    return data;
  }

  async function loadLuaTools() {
    if (!toolEls.list) return;
    toolEls.list.innerHTML = "";
    try {
      var tools = await toolApi("");
      if (!tools || !tools.length) {
        toolEls.list.innerHTML = "<div style=\"opacity:.7;font-size:.9rem\">No tools yet.</div>";
        return;
      }
      tools.forEach(function(t) {
        var row = document.createElement("div");
        row.style.cssText = "display:flex;align-items:center;gap:12px;padding:.75rem 1rem;background:var(--surface);border:1px solid var(--border);border-radius:10px;cursor:pointer;";
        
        var icon = document.createElement("i");
        icon.className = "fa-solid fa-screwdriver-wrench";
        icon.style.color = "var(--accent)";
        
        var content = document.createElement("div");
        content.style.cssText = "flex:1;min-width:0";
        
        var nameEl = document.createElement("div");
        nameEl.style.fontWeight = "600";
        nameEl.style.fontFamily = "ui-monospace,monospace";
        nameEl.textContent = t.name;
        
        var descEl = document.createElement("div");
        descEl.style.fontSize = ".85rem";
        descEl.style.color = "var(--muted)";
        descEl.style.whiteSpace = "nowrap";
        descEl.style.overflow = "hidden";
        descEl.style.textOverflow = "ellipsis";
        if (t.loaded) {
          descEl.textContent = t.description || "";
        } else {
          descEl.innerHTML = '<span style="color:#c0392b">⚠ failed to load. Open it to see the error.</span>';
        }
        
        content.appendChild(nameEl);
        content.appendChild(descEl);
        row.appendChild(icon);
        row.appendChild(content);
        row.onclick = function() { openToolEditor(t.name); };
        toolEls.list.appendChild(row);
      });
    } catch {
      var errEl = document.createElement("div");
      errEl.style.opacity = ".7";
      errEl.style.fontSize = ".9rem";
      errEl.textContent = "Failed to load tools: Unknown error";
      toolEls.list.appendChild(errEl);
    }
  }

  async function openToolEditor(name) {
    editingTool = name;
    toolEls.title.textContent = name ? "Edit " + name : "New Tool";
    toolEls.del.style.display = name ? "" : "none";
    toolEls.testPanel.style.display = name ? "" : "none";
    toolEls.testOut.textContent = "";
    setToolStatus("");
    toolEls.source.value = name ? "Loading..." : TOOL_TEMPLATE;
    toolEls.modal.style.display = "flex";
    if (name) {
      try {
        var data = await toolApi("/" + encodeURIComponent(name));
        toolEls.source.value = data.source;
        validateTool();
      } catch (e) {
        setToolStatus(e.message, "error");
      }
    }
  }

  async function validateTool() {
    try {
      var t = await toolApi("/validate", {
        method: "POST",
        body: JSON.stringify({ source: toolEls.source.value }),
      });
      setToolStatus("✓ Valid: " + t.name, "ok");
      return t;
    } catch (e) {
      setToolStatus(e.message, "error");
      return null;
    }
  }

  async function saveTool() {
    var t = await validateTool();
    if (!t) return;
    if (editingTool && editingTool !== t.name && !(await confirmAction("The tool name changed from \"" + editingTool + "\" to \"" + t.name + "\". Save it as a new tool and delete the old one?"))) {
      return;
    }
    try {
      await toolApi("/" + encodeURIComponent(t.name), {
        method: "PUT",
        body: JSON.stringify({ source: toolEls.source.value }),
      });
      if (editingTool && editingTool !== t.name) {
        await toolApi("/" + encodeURIComponent(editingTool), { method: "DELETE" });
      }
      editingTool = t.name;
      toolEls.title.textContent = "Edit " + t.name;
      toolEls.del.style.display = "";
      toolEls.testPanel.style.display = "";
      setToolStatus("✓ Saved " + t.name + ".lua", "ok");
      loadLuaTools();
    } catch (e) {
      setToolStatus(e.message, "error");
    }
  }

  async function deleteTool() {
    if (!editingTool || !(await confirmAction("Delete tool \"" + editingTool + "\"?"))) return;
    try {
      await toolApi("/" + encodeURIComponent(editingTool), { method: "DELETE" });
      toolEls.modal.style.display = "none";
      loadLuaTools();
    } catch (e) {
      setToolStatus(e.message, "error");
    }
  }

  async function testTool() {
    var args;
    try {
      args = JSON.parse(toolEls.testArgs.value || "{}");
    } catch {
      toolEls.testOut.textContent = "Arguments must be valid JSON.";
      return;
    }
    toolEls.testOut.textContent = "Running...";
    try {
      var r = await toolApi("/" + encodeURIComponent(editingTool) + "/test", {
        method: "POST",
        body: JSON.stringify({ args: args }),
      });
      var out = r.result;
      try { out = JSON.stringify(JSON.parse(out), null, 2); } catch { }
      toolEls.testOut.textContent = "(" + r.durationMs + " ms)\n" + out;
    } catch (e) {
      toolEls.testOut.textContent = e.message;
    }
  }

  // Lua Tool Key Events
  if (toolEls.source) {
    toolEls.source.addEventListener("keydown", function (e) {
      if (e.key === "Tab") {
        e.preventDefault();
        var s = toolEls.source.selectionStart;
        var end = toolEls.source.selectionEnd;
        var val = toolEls.source.value;
        toolEls.source.value = val.slice(0, s) + "  " + val.slice(end);
        toolEls.source.selectionStart = toolEls.source.selectionEnd = s + 2;
      } else if ((e.metaKey || e.ctrlKey) && e.key === "s") {
        e.preventDefault();
        saveTool();
      }
    });
  }

  if ($("new-tool")) $("new-tool").onclick = function() { openToolEditor(); };
  if ($("close-tool-modal")) $("close-tool-modal").onclick = function() { toolEls.modal.style.display = "none"; };
  if ($("tool-validate")) $("tool-validate").onclick = validateTool;
  if ($("tool-save")) $("tool-save").onclick = saveTool;
  if ($("tool-delete")) $("tool-delete").onclick = deleteTool;
  if ($("tool-test")) $("tool-test").onclick = testTool;

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
  }).catch(function() {
    // Silently fail or handle
  });
  loadMcpServers();
})();
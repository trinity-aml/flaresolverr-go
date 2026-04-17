package flaresolverr

const settingsPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>FlareSolverr Settings</title>
  <style>
    :root {
      --bg: #f4efe7;
      --panel: #fffdf8;
      --panel-strong: #f0e6d7;
      --text: #201815;
      --muted: #6d5d53;
      --accent: #1f6f5f;
      --accent-strong: #124b40;
      --danger: #a23d2f;
      --border: #d9ccb8;
      --shadow: 0 18px 60px rgba(60, 34, 18, 0.10);
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: "IBM Plex Sans", "Segoe UI", sans-serif;
      background:
        radial-gradient(circle at top right, rgba(31,111,95,0.10), transparent 34%),
        linear-gradient(180deg, #f8f4ec 0%, var(--bg) 100%);
      color: var(--text);
    }
    .shell {
      max-width: 1180px;
      margin: 0 auto;
      padding: 32px 20px 64px;
    }
    .hero {
      display: grid;
      grid-template-columns: 1.2fr 0.8fr;
      gap: 18px;
      margin-bottom: 22px;
    }
    .hero-card, .panel {
      background: rgba(255, 253, 248, 0.92);
      border: 1px solid var(--border);
      border-radius: 22px;
      box-shadow: var(--shadow);
      backdrop-filter: blur(8px);
    }
    .hero-card {
      padding: 28px;
    }
    .eyebrow {
      text-transform: uppercase;
      letter-spacing: 0.18em;
      font-size: 12px;
      color: var(--accent);
      margin-bottom: 10px;
      font-weight: 700;
    }
    h1 {
      margin: 0 0 10px;
      font-size: clamp(30px, 5vw, 52px);
      line-height: 0.95;
      font-family: "IBM Plex Serif", "Georgia", serif;
    }
    .hero p, .muted {
      color: var(--muted);
    }
    .meta {
      padding: 24px;
      display: flex;
      flex-direction: column;
      justify-content: space-between;
      gap: 16px;
    }
    .meta-item {
      padding: 14px 16px;
      border-radius: 16px;
      background: var(--panel-strong);
    }
    .meta-label {
      font-size: 12px;
      text-transform: uppercase;
      letter-spacing: 0.12em;
      color: var(--muted);
      margin-bottom: 6px;
    }
    .meta-value {
      font-family: "IBM Plex Mono", monospace;
      font-size: 14px;
      word-break: break-word;
    }
    form {
      display: grid;
      gap: 18px;
    }
    .group {
      padding: 22px;
    }
    .group h2 {
      margin: 0 0 6px;
      font-size: 20px;
      font-family: "IBM Plex Serif", "Georgia", serif;
    }
    .group p {
      margin: 0 0 18px;
      color: var(--muted);
      font-size: 14px;
    }
    .grid {
      display: grid;
      grid-template-columns: repeat(2, minmax(0, 1fr));
      gap: 14px;
    }
    .field {
      display: flex;
      flex-direction: column;
      gap: 6px;
    }
    .field.full {
      grid-column: 1 / -1;
    }
    label {
      font-size: 13px;
      font-weight: 700;
      color: #3b3029;
    }
    input, select {
      width: 100%;
      border: 1px solid var(--border);
      background: #fff;
      color: var(--text);
      border-radius: 14px;
      padding: 12px 14px;
      font: inherit;
      outline: none;
    }
    input:focus, select:focus {
      border-color: var(--accent);
      box-shadow: 0 0 0 4px rgba(31,111,95,0.12);
    }
    .switches {
      display: grid;
      grid-template-columns: repeat(2, minmax(0, 1fr));
      gap: 12px;
      margin-top: 2px;
    }
    .switch {
      display: flex;
      align-items: center;
      gap: 12px;
      border: 1px solid var(--border);
      border-radius: 16px;
      background: #fff;
      padding: 12px 14px;
      min-height: 58px;
    }
    .switch input {
      width: 18px;
      height: 18px;
      margin: 0;
      accent-color: var(--accent);
    }
    .switch span {
      font-size: 14px;
      font-weight: 600;
    }
    .toolbar {
      position: sticky;
      bottom: 18px;
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 12px;
      background: rgba(32, 24, 21, 0.92);
      color: #fff6ea;
      padding: 16px 18px;
      border-radius: 18px;
      box-shadow: 0 22px 50px rgba(0, 0, 0, 0.22);
    }
    .status {
      min-height: 22px;
      font-size: 14px;
      color: #efe0cf;
    }
    .status.error {
      color: #ffc1b7;
    }
    .status.ok {
      color: #b9f1df;
    }
    .btn {
      border: 0;
      border-radius: 999px;
      padding: 12px 18px;
      font: inherit;
      font-weight: 700;
      cursor: pointer;
      color: white;
      background: linear-gradient(135deg, var(--accent), var(--accent-strong));
      min-width: 180px;
    }
    .note {
      margin-top: 14px;
      font-size: 13px;
      color: var(--muted);
    }
    @media (max-width: 900px) {
      .hero, .grid, .switches {
        grid-template-columns: 1fr;
      }
      .toolbar {
        flex-direction: column;
        align-items: stretch;
      }
      .btn {
        width: 100%;
      }
    }
  </style>
</head>
<body>
  <div class="shell">
    <section class="hero">
      <div class="hero-card">
        <div class="eyebrow">Runtime Control</div>
        <h1>FlareSolverr Settings</h1>
        <p>Settings are saved into <code>init.yaml</code> and applied to the running process for new requests and new browser sessions. If you change the main HTTP listen address, that part is saved immediately but still requires a process restart.</p>
      </div>
      <aside class="hero-card meta">
        <div class="meta-item">
          <div class="meta-label">Config Path</div>
          <div class="meta-value" id="configPath">Loading...</div>
        </div>
        <div class="meta-item">
          <div class="meta-label">API</div>
          <div class="meta-value">POST /v1</div>
        </div>
      </aside>
    </section>

    <form id="settingsForm">
      <section class="panel group">
        <h2>Network</h2>
        <p>Main server address is persisted immediately. If host or port changes, restart the process to rebind the listener.</p>
        <div class="grid">
          <div class="field">
            <label for="host">Host</label>
            <input id="host" name="host" type="text" />
          </div>
          <div class="field">
            <label for="port">Port</label>
            <input id="port" name="port" type="number" min="1" />
          </div>
          <div class="field">
            <label for="prometheusPort">Prometheus Port</label>
            <input id="prometheusPort" name="prometheusPort" type="number" min="1" />
          </div>
          <div class="field">
            <label for="logLevel">Log Level</label>
            <select id="logLevel" name="logLevel">
              <option value="info">info</option>
              <option value="debug">debug</option>
              <option value="warn">warn</option>
              <option value="error">error</option>
            </select>
          </div>
        </div>
        <div class="switches">
          <label class="switch"><input id="prometheusEnabled" name="prometheusEnabled" type="checkbox" /><span>Prometheus Enabled</span></label>
          <label class="switch"><input id="headless" name="headless" type="checkbox" /><span>Headless Browser</span></label>
        </div>
      </section>

      <section class="panel group">
        <h2>Browser</h2>
        <p>These settings are applied to newly created browser sessions after save.</p>
        <div class="grid">
          <div class="field full">
            <label for="browserPath">Browser Path</label>
            <input id="browserPath" name="browserPath" type="text" />
          </div>
          <div class="field full">
            <label for="driverPath">ChromeDriver Path</label>
            <input id="driverPath" name="driverPath" type="text" />
          </div>
          <div class="field full">
            <label for="driverCacheDir">Driver Cache Dir</label>
            <input id="driverCacheDir" name="driverCacheDir" type="text" />
          </div>
          <div class="field full">
            <label for="chromeForTestingURL">Chrome for Testing URL</label>
            <input id="chromeForTestingURL" name="chromeForTestingURL" type="text" />
          </div>
          <div class="field full">
            <label for="startupUserAgent">Startup User Agent</label>
            <input id="startupUserAgent" name="startupUserAgent" type="text" />
          </div>
        </div>
        <div class="switches">
          <label class="switch"><input id="driverAutoDownload" name="driverAutoDownload" type="checkbox" /><span>Auto-download matching ChromeDriver</span></label>
          <label class="switch"><input id="disableMedia" name="disableMedia" type="checkbox" /><span>Disable images, CSS and fonts</span></label>
          <label class="switch"><input id="logHTML" name="logHTML" type="checkbox" /><span>Log response HTML</span></label>
        </div>
      </section>

      <section class="panel group">
        <h2>Default Proxy</h2>
        <p>Used when API requests do not provide an explicit proxy.</p>
        <div class="grid">
          <div class="field full">
            <label for="proxyURL">Proxy URL</label>
            <input id="proxyURL" name="proxyURL" type="text" placeholder="http://host:port or socks5://host:port" />
          </div>
          <div class="field">
            <label for="proxyUsername">Proxy Username</label>
            <input id="proxyUsername" name="proxyUsername" type="text" />
          </div>
          <div class="field">
            <label for="proxyPassword">Proxy Password</label>
            <input id="proxyPassword" name="proxyPassword" type="password" />
          </div>
        </div>
      </section>

      <div class="toolbar">
        <div>
          <div class="status" id="statusLine"></div>
          <div class="note">Saving updates <code>init.yaml</code>, reloads runtime browser settings, and restarts Prometheus exporter if needed.</div>
        </div>
        <button class="btn" id="saveButton" type="submit">Save Settings</button>
      </div>
    </form>
  </div>

  <script>
    const fields = [
      "host", "port", "browserPath", "driverPath", "driverCacheDir",
      "driverAutoDownload", "chromeForTestingURL", "headless", "startupUserAgent",
      "logLevel", "logHTML", "disableMedia", "prometheusEnabled",
      "prometheusPort", "proxyURL", "proxyUsername", "proxyPassword"
    ];

    function setStatus(message, kind) {
      const el = document.getElementById("statusLine");
      el.textContent = message || "";
      el.className = "status" + (kind ? " " + kind : "");
    }

    function fillForm(config) {
      for (const key of fields) {
        const el = document.getElementById(key);
        if (!el) continue;
        if (el.type === "checkbox") {
          el.checked = !!config[key];
        } else {
          el.value = config[key] ?? "";
        }
      }
    }

    function collectForm() {
      const payload = {};
      for (const key of fields) {
        const el = document.getElementById(key);
        if (!el) continue;
        if (el.type === "checkbox") {
          payload[key] = el.checked;
        } else if (el.type === "number") {
          payload[key] = Number(el.value || 0);
        } else {
          payload[key] = el.value;
        }
      }
      return payload;
    }

    async function loadSettings() {
      setStatus("Loading settings...");
      const res = await fetch("/api/settings");
      const body = await res.json();
      if (!res.ok) {
        throw new Error(body.error || "Failed to load settings");
      }
      fillForm(body.config);
      document.getElementById("configPath").textContent = body.configPath || "init.yaml";
      setStatus("");
    }

    async function saveSettings(event) {
      event.preventDefault();
      const button = document.getElementById("saveButton");
      button.disabled = true;
      setStatus("Saving settings...");
      try {
        const res = await fetch("/api/settings", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(collectForm())
        });
        const body = await res.json();
        if (!res.ok) {
          throw new Error(body.error || "Failed to save settings");
        }
        fillForm(body.config);
        document.getElementById("configPath").textContent = body.configPath || "init.yaml";
        if (body.restartRequired && body.restartRequired.length > 0) {
          setStatus(body.message + " Restart required for: " + body.restartRequired.join(", "), "ok");
        } else {
          setStatus(body.message || "Settings saved.", "ok");
        }
      } catch (error) {
        setStatus(error.message || "Failed to save settings", "error");
      } finally {
        button.disabled = false;
      }
    }

    document.getElementById("settingsForm").addEventListener("submit", saveSettings);
    loadSettings().catch((error) => setStatus(error.message || "Failed to load settings", "error"));
  </script>
</body>
</html>`

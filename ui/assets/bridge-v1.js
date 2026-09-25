(() => {
  "use strict";

  const HOST_SOURCE = "sub2api-plugin-host";
  const UI_SOURCE = "sub2api-plugin-ui";

  class BridgeError extends Error {
    constructor(message, response) {
      super(message);
      this.name = "BridgeError";
      this.response = response;
    }
  }

  class Sub2APIPluginBridge {
    constructor(options = {}) {
      this.token = options.token || new URLSearchParams(location.hash.slice(1)).get("bridge_token") || "";
      this.timeoutMs = options.timeoutMs || 15000;
      this.pending = new Map();
      this.sequence = 0;
      this.onMessage = this.onMessage.bind(this);
      window.addEventListener("message", this.onMessage);
    }

    onMessage(event) {
      const message = event.data || {};
      if (event.source !== parent || message.source !== HOST_SOURCE || message.bridge_token !== this.token) return;
      const request = this.pending.get(message.request_id);
      if (!request) return;
      this.pending.delete(message.request_id);
      clearTimeout(request.timer);
      if (message.ok) {
        request.resolve(message);
        return;
      }
      request.reject(new BridgeError(message.error || message.result?.message || "宿主拒绝了插件 UI 请求", message));
    }

    request(type, payload = {}) {
      if (!this.token) return Promise.reject(new BridgeError("缺少 UI Bridge Token"));
      const random = globalThis.crypto?.randomUUID?.() || `${Date.now()}-${++this.sequence}`;
      const requestId = `ui-${random}`;
      const message = { source: UI_SOURCE, bridge_token: this.token, type, request_id: requestId, ...payload };
      return new Promise((resolve, reject) => {
        const timer = setTimeout(() => {
          this.pending.delete(requestId);
          reject(new BridgeError(`Bridge 请求超时: ${type}`));
        }, this.timeoutMs);
        this.pending.set(requestId, { resolve, reject, timer });
        parent.postMessage(message, "*");
      });
    }

    ready() {
      parent.postMessage({ source: UI_SOURCE, bridge_token: this.token, type: "sub2api.plugin.ready" }, "*");
    }

    async loadConfig() {
      const response = await this.request("config.load");
      return response.config || {};
    }

    async saveConfig(config) {
      const response = await this.request("config.save", { config });
      return response.config || {};
    }

    async testConfig(timeoutMs = 60000) {
      const previousTimeout = this.timeoutMs;
      this.timeoutMs = timeoutMs;
      try {
        const response = await this.request("config.test");
        return response.result || { success: false, message: "宿主未返回测试结果", latency_ms: 0 };
      } finally {
        this.timeoutMs = previousTimeout;
      }
    }

    // Read-only runtime status (the plugin's Health snapshot). No side effects, not
    // step-up gated on the host, and never raises a host toast.
    async status() {
      const response = await this.request("plugin.status");
      return response.result || { healthy: false, message: "宿主未返回状态", status_json: "" };
    }

    resize(height) {
      parent.postMessage({ source: UI_SOURCE, bridge_token: this.token, type: "ui.resize", height }, "*");
    }

    notify(level, message) {
      parent.postMessage({ source: UI_SOURCE, bridge_token: this.token, type: "ui.notify", level, message }, "*");
    }

    dispose() {
      window.removeEventListener("message", this.onMessage);
      for (const request of this.pending.values()) {
        clearTimeout(request.timer);
        request.reject(new BridgeError("UI Bridge 已关闭"));
      }
      this.pending.clear();
    }
  }

  window.Sub2APIPluginBridge = Sub2APIPluginBridge;
  window.Sub2APIPluginBridgeError = BridgeError;
})();

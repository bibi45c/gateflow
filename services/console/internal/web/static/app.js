// FlowGate console: the few behaviours that need JavaScript. No build step.
"use strict";

(function () {
  var main = document.querySelector("main");
  var csrf = main ? main.getAttribute("data-csrf") : "";

  // Copy buttons and confirmations.
  document.querySelectorAll("[data-copy]").forEach(function (button) {
    button.addEventListener("click", function () {
      var text = document.getElementById(button.getAttribute("data-copy")).textContent;
      navigator.clipboard.writeText(text).then(function () { button.textContent = "Copied"; });
    });
  });
  document.querySelectorAll("form[data-confirm]").forEach(function (form) {
    form.addEventListener("submit", function (event) {
      if (!window.confirm(form.getAttribute("data-confirm"))) { event.preventDefault(); }
    });
  });

  // Playground: one streamed Messages call, relayed by the console.
  var playground = document.getElementById("playground");
  if (playground) {
    var keyInput = document.getElementById("pg-key");
    var out = document.getElementById("pg-out");
    var status = document.getElementById("pg-status");
    var usage = document.getElementById("pg-usage");
    var send = document.getElementById("pg-send");
    var stop = document.getElementById("pg-stop");
    var controller = null;
    try { keyInput.value = sessionStorage.getItem("flowgate-key") || ""; } catch (e) { /* storage off */ }

    var usageText = { input: "?", output: "?" };
    function showUsage() { usage.textContent = "tokens in " + usageText.input + ", out " + usageText.output; }

    function handle(eventName, data) {
      var msg;
      try { msg = JSON.parse(data); } catch (e) { return; }
      if (msg.type === "message_start" && msg.message && msg.message.usage) {
        usageText.input = msg.message.usage.input_tokens; showUsage();
        status.textContent = "Streaming… (" + (msg.message.model || "") + ")";
      } else if (msg.type === "content_block_delta" && msg.delta && msg.delta.type === "text_delta") {
        out.textContent += msg.delta.text;
      } else if (msg.type === "message_delta" && msg.usage) {
        usageText.output = msg.usage.output_tokens; showUsage();
      } else if (msg.type === "message_stop") {
        status.textContent = "Done.";
      } else if (msg.type === "error" && msg.error) {
        status.textContent = "Error: " + msg.error.type + ": " + msg.error.message;
      }
    }

    playground.addEventListener("submit", function (event) {
      event.preventDefault();
      try { sessionStorage.setItem("flowgate-key", keyInput.value); } catch (e) { /* storage off */ }
      out.textContent = ""; usage.textContent = ""; usageText = { input: "?", output: "?" };
      status.textContent = "Sending…";
      send.disabled = true; stop.disabled = false;
      controller = new AbortController();
      var started = performance.now();
      var first = true;
      fetch("/playground/stream", {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf },
        body: JSON.stringify({
          key: keyInput.value,
          model: document.getElementById("pg-model").value,
          prompt: document.getElementById("pg-prompt").value,
          max_tokens: parseInt(document.getElementById("pg-max").value, 10) || 0
        }),
        signal: controller.signal
      }).then(function (response) {
        if (!response.ok) {
          return response.text().then(function (text) {
            var detail = text;
            try { var body = JSON.parse(text); detail = body.error.type + ": " + body.error.message; } catch (e) { /* plain text */ }
            status.textContent = "HTTP " + response.status + " — " + detail;
          });
        }
        var reader = response.body.getReader();
        var decoder = new TextDecoder();
        var buffer = "";
        function pump() {
          return reader.read().then(function (chunk) {
            if (chunk.done) { return; }
            if (first) { first = false; status.textContent = "First bytes after " + Math.round(performance.now() - started) + " ms"; }
            buffer += decoder.decode(chunk.value, { stream: true }).replace(/\r\n/g, "\n");
            var cut;
            while ((cut = buffer.indexOf("\n\n")) >= 0) {
              var block = buffer.slice(0, cut); buffer = buffer.slice(cut + 2);
              var name = "", data = [];
              block.split("\n").forEach(function (line) {
                if (line.indexOf("event:") === 0) { name = line.slice(6).trim(); }
                else if (line.indexOf("data:") === 0) { data.push(line.slice(5).replace(/^ /, "")); }
              });
              if (data.length) { handle(name, data.join("\n")); }
            }
            return pump();
          });
        }
        return pump();
      }).catch(function (err) {
        status.textContent = err.name === "AbortError" ? "Stopped." : "Failed: " + err.message;
      }).finally(function () {
        send.disabled = false; stop.disabled = true; controller = null;
      });
    });
    stop.addEventListener("click", function () { if (controller) { controller.abort(); } });
  }

  // Dashboard: poll the console, which asks Prometheus.
  var dash = document.getElementById("dash");
  if (dash) {
    var dashStatus = document.getElementById("dash-status");
    function format(name, value) {
      if (value === null || value === undefined) { return "0"; }
      return name === "requests_per_second" ? value.toFixed(2) : String(Math.round(value));
    }
    function poll() {
      fetch("/admin/dashboard/data", { headers: { "Accept": "application/json" } })
        .then(function (r) { return r.json(); })
        .then(function (body) {
          dash.querySelectorAll("[data-metric]").forEach(function (el) {
            var name = el.getAttribute("data-metric");
            el.textContent = body.error ? "–" : format(name, body.values[name]);
          });
          dashStatus.textContent = body.error ? body.error : "Updated " + new Date(body.at).toLocaleTimeString();
        })
        .catch(function () { dashStatus.textContent = "Console unreachable."; });
    }
    poll();
    setInterval(poll, 2000);
  }
})();

(function () {
  "use strict";
  const host = document.getElementById("terminal");
  const status = document.getElementById("terminal-status");
  if (!host || !globalThis.Terminal) return;

  const terminal = new globalThis.Terminal({
    cursorBlink: true,
    convertEol: true,
    fontFamily: "ui-monospace, SFMono-Regular, Consolas, monospace",
    fontSize: 14,
    scrollback: 5000,
    theme: { background: "#070b10", foreground: "#edf3f8" }
  });
  terminal.open(host);

  function dimensions() {
    return {
      cols: Math.max(20, Math.floor(host.clientWidth / 9)),
      rows: Math.max(5, Math.floor(host.clientHeight / 18))
    };
  }
  function resize(socket) {
    const size = dimensions();
    terminal.resize(size.cols, size.rows);
    if (socket.readyState !== WebSocket.OPEN) return;
    const frame = new Uint8Array(9);
    frame[0] = 1;
    const view = new DataView(frame.buffer);
    view.setUint32(1, size.cols);
    view.setUint32(5, size.rows);
    socket.send(frame);
  }

  const scheme = location.protocol === "https:" ? "wss:" : "ws:";
  let socket;
  let terminalEnded = false;
  let retryDelay = 250;

  function connect() {
    const size = dimensions();
    terminal.resize(size.cols, size.rows);
    status.textContent = retryDelay === 250 ? "connecting" : "reconnecting";
    socket = new WebSocket(scheme + "//" + location.host + location.pathname + "/socket?cols=" + size.cols + "&rows=" + size.rows);
    socket.binaryType = "arraybuffer";
    socket.addEventListener("open", function () {
      retryDelay = 250;
      status.textContent = "connected";
      terminal.focus();
    });
    socket.addEventListener("message", function (event) {
      const frame = new Uint8Array(event.data);
      if (frame[0] === 0) {
        terminal.write(frame.slice(1));
      } else if (frame[0] === 3) {
        terminalEnded = true;
        const code = new DataView(frame.buffer).getUint32(1);
        const reason = new TextDecoder().decode(frame.slice(5));
        terminal.writeln("\r\n[process exited " + code + (reason ? ": " + reason : "") + "]");
      } else if (frame[0] === 4) {
        const reason = new TextDecoder().decode(frame.slice(1));
        terminal.writeln("\r\n[attach unavailable" + (reason ? ": " + reason : "") + "; retrying]");
      }
    });
    socket.addEventListener("close", function () {
      if (terminalEnded) {
        status.textContent = "disconnected";
        return;
      }
      status.textContent = "reconnecting";
      const delay = retryDelay;
      retryDelay = Math.min(retryDelay * 2, 5000);
      setTimeout(connect, delay);
    });
    socket.addEventListener("error", function () { status.textContent = "connection failed"; });
  }

  connect();
  terminal.onData(function (data) {
    if (!socket || socket.readyState !== WebSocket.OPEN) return;
    const bytes = new TextEncoder().encode(data);
    const frame = new Uint8Array(bytes.length + 1);
    frame.set(bytes, 1);
    socket.send(frame);
  });
  window.addEventListener("resize", function () { if (socket) resize(socket); });
  setInterval(function () {
    if (socket && socket.readyState === WebSocket.OPEN) socket.send(new Uint8Array([2]));
  }, 15000);
}());

#!/usr/bin/env bun
// Dependency-free Chrome DevTools driver for source-blind browser goals.
const [endpoint, actionFile] = process.argv.slice(2);
if (!endpoint || !actionFile) throw new Error("usage: bun browser-driver.js http://127.0.0.1:9222 actions.json");
const targets = await (await fetch(`${endpoint}/json/list`)).json();
const target = targets.find((candidate) => candidate.type === "page");
if (!target?.webSocketDebuggerUrl) throw new Error("no DevTools page target");
const actions = await Bun.file(actionFile).json();
if (!Array.isArray(actions) || actions.length > 64) throw new Error("actions must be an array of at most 64 entries");

const socket = new WebSocket(target.webSocketDebuggerUrl);
await new Promise((resolve, reject) => {
  socket.addEventListener("open", resolve, { once: true });
  socket.addEventListener("error", reject, { once: true });
});
let nextID = 1;
const pending = new Map();
socket.addEventListener("message", (event) => {
  const message = JSON.parse(event.data);
  const waiter = pending.get(message.id);
  if (!waiter) return;
  pending.delete(message.id);
  if (message.error) waiter.reject(new Error(message.error.message));
  else waiter.resolve(message.result ?? {});
});
function command(method, params = {}) {
  const id = nextID++;
  socket.send(JSON.stringify({ id, method, params }));
  return new Promise((resolve, reject) => pending.set(id, { resolve, reject }));
}
const sleep = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));
async function point(selector) {
  const expression = `(() => { const e=document.querySelector(${JSON.stringify(selector)}); if(!e)return null; const r=e.getBoundingClientRect(); return {x:r.left+r.width/2,y:r.top+r.height/2}; })()`;
  const result = await command("Runtime.evaluate", { expression, returnByValue: true });
  if (!result.result?.value) throw new Error(`selector not found: ${selector}`);
  return result.result.value;
}
async function click(selector) {
  const { x, y } = await point(selector);
  await command("Input.dispatchMouseEvent", { type: "mousePressed", x, y, button: "left", clickCount: 1 });
  await command("Input.dispatchMouseEvent", { type: "mouseReleased", x, y, button: "left", clickCount: 1 });
}

await command("Page.enable");
await command("Runtime.enable");
for (const action of actions) {
  switch (action.kind) {
    case "navigate":
      await command("Page.navigate", { url: action.url });
      await sleep(action.wait_ms ?? 1000);
      break;
    case "click": await click(action.selector); break;
    case "type":
      await click(action.selector);
      await command("Input.insertText", { text: action.text });
      break;
    case "key":
      await command("Input.dispatchKeyEvent", { type: "keyDown", key: action.key, code: action.code ?? action.key });
      await command("Input.dispatchKeyEvent", { type: "keyUp", key: action.key, code: action.code ?? action.key });
      break;
    case "wait": await sleep(action.milliseconds); break;
    case "virtual-authenticator":
      await command("WebAuthn.enable");
      await command("WebAuthn.addVirtualAuthenticator", { options: {
        protocol: "ctap2", transport: "usb", hasResidentKey: true,
        hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true,
      }});
      break;
    case "screenshot": {
      const shot = await command("Page.captureScreenshot", { format: "png", captureBeyondViewport: false });
      await Bun.write(action.path, Buffer.from(shot.data, "base64"));
      break;
    }
    default: throw new Error(`unknown action kind: ${action.kind}`);
  }
}
socket.close();

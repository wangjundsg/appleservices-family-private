// image-text.cjs — 把图片里的字符读出来（远程识别接口版）
//
// 用途：页面上有一张图片，需要把上面印的字符转成文本时调用。
// 默认直接调用远程识别接口；本机模型只在没配 key 或显式要求时使用。
//
// 用法（命令行）：
//   node tools/lib/image-text.cjs <图片路径>
//   输出识别到的字符（无换行）；失败输出空串并以 1 退出，原因打到 stderr
//
// 用法（代码）：
//   const { readImageText } = require("./image-text.cjs");
//   const text = await readImageText("/path/to.png");
//   const text2 = await readImageText("/path/to.png", { local: true });   // 强制本机模型
//
// 配置：
//   IMAGE_TEXT_API_KEY    必填（未配置则退回本机模型）
//   IMAGE_TEXT_API_BASE   默认 https://api.yescaptcha.com
//   IMAGE_TEXT_TIMEOUT_MS 默认 90000
//   IMAGE_TEXT_POLL_MS    默认 2000
//
// 返回值：只含字母数字的字符串；失败为空串。绝不抛异常。

"use strict";

const fs = require("node:fs");
const path = require("node:path");
const { spawn } = require("node:child_process");

const API_BASE = (process.env.IMAGE_TEXT_API_BASE || "https://api.yescaptcha.com").replace(/\/+$/, "");
const API_KEY = process.env.IMAGE_TEXT_API_KEY || "";
const TIMEOUT_MS = Number(process.env.IMAGE_TEXT_TIMEOUT_MS || 90000);
const POLL_MS = Number(process.env.IMAGE_TEXT_POLL_MS || 2000);

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function normalize(text) {
  return String(text || "").replace(/[^0-9A-Za-z]/g, "");
}

function warn(message) {
  process.stderr.write(`image-text: ${message}\n`);
}

// ---------- 远程接口 ----------

async function postJSON(url, payload, timeoutMs = 20000) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  try {
    const res = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
      signal: controller.signal,
    });
    const text = await res.text();
    if (!res.ok) {
      warn(`http ${res.status}`);
      return null;
    }
    try {
      return JSON.parse(text);
    } catch {
      warn("response not json");
      return null;
    }
  } catch (error) {
    warn(`request failed: ${error && error.name === "AbortError" ? "timeout" : error && error.message}`);
    return null;
  } finally {
    clearTimeout(timer);
  }
}

async function remoteModel(file) {
  const base64 = fs.readFileSync(file).toString("base64");
  const created = await postJSON(`${API_BASE}/createTask`, {
    clientKey: API_KEY,
    task: { type: "ImageToTextTask", body: base64 },
  });
  if (!created) return "";
  if (created.errorId) {
    warn(`create failed: ${created.errorCode || created.errorId}`);
    return "";
  }
  const taskId = created.taskId;
  if (!taskId) {
    warn("create returned no taskId");
    return "";
  }
  const deadline = Date.now() + TIMEOUT_MS;
  let lastStatus = "";
  while (Date.now() < deadline) {
    await sleep(POLL_MS);
    const got = await postJSON(`${API_BASE}/getTaskResult`, { clientKey: API_KEY, taskId });
    if (!got) continue;
    if (got.errorId) {
      warn(`result failed: ${got.errorCode || got.errorId}`);
      return "";
    }
    lastStatus = got.status || "";
    if (lastStatus === "ready") {
      return normalize((got.solution || {}).text);
    }
  }
  warn(`timed out after ${TIMEOUT_MS}ms (last status: ${lastStatus || "none"})`);
  return "";
}

// ---------- 本机模型（可选） ----------

function localModel(file) {
  return new Promise((resolve) => {
    const script = path.resolve(__dirname, "..", "captcha", "ocr.py");
    if (!fs.existsSync(script)) return resolve("");
    let out = "";
    const child = spawn("python", [script, file], { windowsHide: true });
    child.stdout.on("data", (chunk) => {
      out += chunk.toString();
    });
    child.on("error", () => resolve(""));
    child.on("exit", () => resolve(normalize(out)));
    setTimeout(() => {
      try {
        child.kill();
      } catch {
        /* already gone */
      }
      resolve("");
    }, 20000);
  });
}

// ---------- 对外接口 ----------

/**
 * 读取图片中的字符。
 * @param {string} imagePath 图片路径
 * @param {{local?: boolean, skipLocal?: boolean}} [opts]
 *   local=true      强制走本机模型
 *   skipLocal=true  强制走远程（等价于默认行为）
 * @returns {Promise<string>} 只含字母数字；失败为空串
 */
async function readImageText(imagePath, opts = {}) {
  const file = path.resolve(imagePath);
  if (!fs.existsSync(file)) {
    warn("image not found");
    return "";
  }
  const forceLocal = opts.local === true;
  // 默认用远程；没配 key 或显式要求时用本机模型
  if (forceLocal || !API_KEY) {
    const local = await localModel(file);
    if (local) return local;
    if (forceLocal) return "";
    warn("local model returned nothing, falling back to remote");
  }
  return remoteModel(file);
}

module.exports = { readImageText, normalize };

if (require.main === module) {
  const file = process.argv[2];
  if (!file) {
    warn("usage: node tools/lib/image-text.cjs <image>");
    process.exit(2);
  }
  readImageText(file, { local: process.argv.includes("--local") })
    .then((text) => {
      process.stdout.write(text);
      process.exit(text ? 0 : 1);
    })
    .catch(() => {
      process.stdout.write("");
      process.exit(1);
    });
}

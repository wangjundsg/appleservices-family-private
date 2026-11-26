// image-text.cjs — 把图片里的字符读出来。
//
// 用途：页面上有一张图片，需要把上面印的字符转成文本时调用。
// 内部策略：先本机模型识一次（免费、快），读不出再调远程识别接口。
//
// 用法（命令行）：
//   node tools/lib/image-text.cjs <图片路径>
//   输出：识别到的字符（无换行）；失败输出空串并以 1 退出
//
// 用法（代码）：
//   const { readImageText } = require("./image-text.cjs");
//   const text = await readImageText("/path/to.png");
//
// 配置（环境变量，都有默认值）：
//   IMAGE_TEXT_API_KEY     远程识别服务的 key（未配置则只用本机模型）
//   IMAGE_TEXT_API_BASE    默认 https://api.yescaptcha.com
//   IMAGE_TEXT_TIMEOUT_MS  默认 60000
//
// 说明：本模块只做"图片 → 文本"，不关心图片来自哪里、用在哪里。

"use strict";

const fs = require("node:fs");
const path = require("node:path");
const { spawn } = require("node:child_process");

const API_BASE = process.env.IMAGE_TEXT_API_BASE || "https://api.yescaptcha.com";
const API_KEY = process.env.IMAGE_TEXT_API_KEY || "";
const TIMEOUT_MS = Number(process.env.IMAGE_TEXT_TIMEOUT_MS || 60000);

// 只保留字母数字，去掉标点与空白；长度不足视为没读出来
function normalize(text) {
  return String(text || "")
    .replace(/[^0-9A-Za-z]/g, "")
    .trim();
}

// ---------- 本机模型 ----------

function localModel(imagePath) {
  return new Promise((resolve) => {
    const script = path.resolve(__dirname, "..", "captcha", "ocr.py");
    if (!fs.existsSync(script)) return resolve("");
    let out = "";
    const child = spawn("python", [script, imagePath], { windowsHide: true });
    child.stdout.on("data", (chunk) => {
      out += chunk.toString();
    });
    child.on("error", () => resolve(""));
    child.on("exit", () => resolve(normalize(out)));
    setTimeout(() => {
      try {
        child.kill();
      } catch {
        /* 已经退出 */
      }
      resolve("");
    }, 20000);
  });
}

// ---------- 远程识别接口 ----------

async function postJSON(url, body) {
  const res = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  const text = await res.text();
  try {
    return JSON.parse(text);
  } catch {
    return { raw: text };
  }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function remoteModel(imagePath) {
  if (!API_KEY) return "";
  const base64 = fs.readFileSync(imagePath).toString("base64");
  const created = await postJSON(`${API_BASE}/createTask`, {
    clientKey: API_KEY,
    task: { type: "ImageToTextTask", body: base64 },
  });
  const taskId = created && created.taskId;
  if (!taskId) return "";
  const deadline = Date.now() + TIMEOUT_MS;
  while (Date.now() < deadline) {
    await sleep(2500);
    const got = await postJSON(`${API_BASE}/getTaskResult`, { clientKey: API_KEY, taskId });
    if (got && got.status === "ready") {
      const solution = got.solution || {};
      return normalize(solution.text);
    }
    if (got && got.errorId) return "";
  }
  return "";
}

// ---------- 对外接口 ----------

async function readImageText(imagePath, opts = {}) {
  const file = path.resolve(imagePath);
  if (!fs.existsSync(file)) return "";
  const skipLocal = opts.skipLocal === true;
  if (!skipLocal) {
    const local = await localModel(file);
    if (local.length >= 4) return local;
  }
  return remoteModel(file);
}

module.exports = { readImageText, normalize };

if (require.main === module) {
  const file = process.argv[2];
  if (!file) {
    console.error("usage: node tools/lib/image-text.cjs <image>");
    process.exit(2);
  }
  readImageText(file)
    .then((text) => {
      process.stdout.write(text);
      process.exit(text ? 0 : 1);
    })
    .catch(() => {
      process.stdout.write("");
      process.exit(1);
    });
}

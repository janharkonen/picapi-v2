"use strict";

const $ = (id) => document.getElementById(id);
let cursor = null;
let current = null; // asset shown in the detail dialog

async function api(path, options = {}) {
  const headers = { "X-Picapi-CSRF": "1", ...(options.headers || {}) };
  if (options.json !== undefined) {
    headers["Content-Type"] = "application/json";
    options.body = JSON.stringify(options.json);
  }
  const res = await fetch(`/api/v1${path}`, { ...options, headers, credentials: "same-origin" });
  if (res.status === 401) {
    showLogin();
    throw new Error("Not logged in");
  }
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || res.statusText);
  }
  return res.status === 204 ? null : res.json();
}

// Delivery URLs are shown absolute but loaded same-origin, so the session cookie covers private assets.
const localPath = (url) => {
  const u = new URL(url, location.href);
  return u.pathname + u.search;
};

function setStatus(text) {
  $("status").textContent = text;
  $("status").hidden = !text;
}

// ---- Login ----

function showLogin() {
  $("app").hidden = true;
  $("login").hidden = false;
  $("login-key").focus();
}

$("login-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  $("login-error").hidden = true;
  const res = await fetch("/api/v1/session", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ key: $("login-key").value }),
  });
  if (!res.ok) {
    $("login-error").textContent = "Invalid API key";
    $("login-error").hidden = false;
    return;
  }
  $("login-key").value = "";
  start();
});

$("logout").addEventListener("click", async () => {
  await fetch("/api/v1/session", { method: "DELETE" });
  showLogin();
});

// ---- Grid ----

function tile(asset) {
  const button = document.createElement("button");
  button.className = "tile";
  button.type = "button";
  const img = document.createElement("img");
  img.loading = "lazy";
  img.alt = asset.filename;
  img.src = `/i/${asset.id}/p_thumb`;
  const label = document.createElement("span");
  label.textContent = asset.filename;
  const badge = document.createElement("span");
  badge.className = "badge";
  badge.textContent = `${asset.width}×${asset.height} · ${asset.visibility}`;
  button.append(img, label, badge);
  button.addEventListener("click", () => openDetail(asset));
  return button;
}

async function loadPage(reset) {
  if (reset) {
    cursor = null;
    $("grid").replaceChildren();
  }
  const query = new URLSearchParams({ limit: "48" });
  if (cursor) query.set("cursor", cursor);
  const page = await api(`/assets?${query}`);
  for (const asset of page.items) $("grid").append(tile(asset));
  cursor = page.next_cursor;
  $("more").hidden = !cursor;
  if (reset && page.items.length === 0) setStatus("No images yet. Upload one to get started.");
}

$("more").addEventListener("click", () => loadPage(false));

// ---- Upload ----

async function upload(files) {
  const list = [...files].filter((f) => f.type.startsWith("image/"));
  for (const [i, file] of list.entries()) {
    setStatus(`Uploading ${i + 1}/${list.length}: ${file.name}`);
    const form = new FormData();
    form.append("file", file);
    try {
      await api("/assets", { method: "POST", body: form });
    } catch (err) {
      setStatus(`${file.name}: ${err.message}`);
      return;
    }
  }
  setStatus("");
  loadPage(true);
}

$("file-input").addEventListener("change", (event) => {
  upload(event.target.files);
  event.target.value = "";
});
document.addEventListener("dragover", (event) => {
  event.preventDefault();
  document.body.classList.add("dragging");
});
document.addEventListener("dragleave", () => document.body.classList.remove("dragging"));
document.addEventListener("drop", (event) => {
  event.preventDefault();
  document.body.classList.remove("dragging");
  if (!$("app").hidden) upload(event.dataTransfer.files);
});

// ---- Detail and URL builder ----

const builderFields = ["b-preset", "b-w", "b-h", "b-fit", "b-g", "b-f", "b-q", "b-bg", "b-slug"];

function transformsString() {
  const parts = [];
  const add = (key, id) => {
    const value = $(id).value.trim();
    if (value) parts.push(`${key}_${value}`);
  };
  add("p", "b-preset");
  add("w", "b-w");
  add("h", "b-h");
  add("fit", "b-fit");
  add("g", "b-g");
  add("f", "b-f");
  add("q", "b-q");
  if ($("b-bg").checked) parts.push("bg_remove");
  return parts.join(",");
}

let buildTimer = null;
function scheduleBuild() {
  clearTimeout(buildTimer);
  buildTimer = setTimeout(buildURL, 250);
}

async function buildURL() {
  $("b-error").hidden = true;
  const body = { transforms: transformsString() };
  const slug = $("b-slug").value.trim();
  if (slug) body.slug = slug;
  try {
    const { url } = await api(`/assets/${current.id}/url`, { method: "POST", json: body });
    $("b-url").value = url;
    $("d-preview").src = localPath(url);
  } catch (err) {
    $("b-error").textContent = err.message;
    $("b-error").hidden = false;
  }
}

function openDetail(asset) {
  current = asset;
  $("d-filename").value = asset.filename;
  $("d-visibility").value = asset.visibility;
  const kb = (asset.size_bytes / 1024).toFixed(0);
  $("d-meta").textContent = `${asset.id} · ${asset.content_type} · ${asset.width}×${asset.height} · ${kb} KB · ${asset.created_at}`;
  for (const id of builderFields) {
    if ($(id).type === "checkbox") $(id).checked = false;
    else $(id).value = "";
  }
  $("b-url").value = asset.url;
  $("b-error").hidden = true;
  $("d-preview").src = `/i/${asset.id}`;
  $("detail").showModal();
}

for (const id of builderFields) {
  $(id).addEventListener("input", scheduleBuild);
  $(id).addEventListener("change", scheduleBuild);
}

$("b-copy").addEventListener("click", async () => {
  await navigator.clipboard.writeText($("b-url").value);
  $("b-copy").textContent = "Copied";
  setTimeout(() => ($("b-copy").textContent = "Copy"), 1200);
});

$("d-save").addEventListener("click", async () => {
  try {
    current = await api(`/assets/${current.id}`, {
      method: "PATCH",
      json: { filename: $("d-filename").value, visibility: $("d-visibility").value },
    });
    $("detail").close();
    loadPage(true);
  } catch (err) {
    alert(err.message);
  }
});

$("d-share").addEventListener("click", async () => {
  try {
    const body = { expires_in: 86400, transforms: transformsString() };
    const result = await api(`/assets/${current.id}/share`, { method: "POST", json: body });
    $("b-url").value = result.url;
  } catch (err) {
    alert(err.message);
  }
});

$("d-delete").addEventListener("click", async () => {
  if (!confirm(`Delete ${current.filename}? This cannot be undone.`)) return;
  try {
    await api(`/assets/${current.id}`, { method: "DELETE" });
    $("detail").close();
    loadPage(true);
  } catch (err) {
    alert(err.message);
  }
});

// ---- Start ----

async function start() {
  try {
    await api("/session");
  } catch {
    return; // showLogin already ran
  }
  $("login").hidden = true;
  $("app").hidden = false;
  loadPage(true);
}

start();

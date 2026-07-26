"use strict";

// Renders /openapi.json as a reference page. Deliberately dependency-free: the
// server ships as one self-contained binary, so the docs viewer must not pull a
// bundle off a CDN (that also lets /docs run under the same strict CSP as the
// rest of the UI). Everything is built with DOM nodes and textContent — spec
// text is data, never markup.

const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text != null) n.textContent = text;
  return n;
};

const METHODS = ["get", "post", "put", "patch", "delete"];
const slug = (method, path) => (method + path).replace(/[^a-zA-Z0-9]+/g, "-").toLowerCase();

// resolveRef follows a local "#/components/..." pointer one level. Anything
// unresolvable comes back as null and the caller falls back to a plain label.
function resolveRef(spec, ref) {
  if (typeof ref !== "string" || !ref.startsWith("#/")) return null;
  return ref.slice(2).split("/").reduce((acc, part) => (acc == null ? null : acc[part]), spec);
}

// typeLabel renders a schema as a short human-readable type.
function typeLabel(spec, schema) {
  if (!schema) return "";
  if (schema.$ref) return schema.$ref.split("/").pop();
  if (schema.type === "array") return typeLabel(spec, schema.items) + "[]";
  let label = schema.type || "object";
  if (Array.isArray(schema.enum)) label += " (" + schema.enum.join(" | ") + ")";
  return label;
}

function paramTable(spec, params) {
  const table = el("table");
  const head = el("tr");
  ["Name", "In", "Type", "Description"].forEach((h) => head.appendChild(el("th", null, h)));
  table.appendChild(head);
  params.forEach((p) => {
    const row = el("tr");
    const name = el("td", "name");
    name.appendChild(el("code", null, p.name));
    if (p.required) {
      name.appendChild(document.createTextNode(" "));
      name.appendChild(el("span", "req", "required"));
    }
    row.appendChild(name);
    row.appendChild(el("td", null, p.in || ""));
    row.appendChild(el("td", null, typeLabel(spec, p.schema)));
    row.appendChild(el("td", "desc-cell", p.description || ""));
    table.appendChild(row);
  });
  return table;
}

function bodySection(spec, body) {
  const wrap = document.createDocumentFragment();
  wrap.appendChild(el("h3", null, "Request body"));
  const content = body.content || {};
  Object.keys(content).forEach((mime) => {
    const line = el("p");
    line.appendChild(el("code", null, mime));
    const t = typeLabel(spec, content[mime].schema);
    if (t) line.appendChild(document.createTextNode(" — " + t));
    wrap.appendChild(line);
  });
  if (body.description) wrap.appendChild(el("p", "op-desc", body.description));
  return wrap;
}

function responseTable(spec, responses) {
  const table = el("table");
  const head = el("tr");
  ["Status", "Description", "Body"].forEach((h) => head.appendChild(el("th", null, h)));
  table.appendChild(head);
  Object.keys(responses).forEach((code) => {
    const r = responses[code] || {};
    const row = el("tr");
    const c = el("td", "name");
    c.appendChild(el("code", null, code));
    row.appendChild(c);
    row.appendChild(el("td", "desc-cell", r.description || ""));
    const mimes = Object.keys(r.content || {});
    row.appendChild(el("td", null, mimes.map((m) => m + " " + typeLabel(spec, r.content[m].schema)).join(", ")));
    table.appendChild(row);
  });
  return table;
}

function renderOperation(spec, path, method, op) {
  const card = el("article", "op");
  card.id = slug(method, path);

  const head = el("div", "op-head");
  head.appendChild(el("span", "method " + method, method.toUpperCase()));
  head.appendChild(el("span", "path", path));
  (op.tags || []).forEach((t) => head.appendChild(el("span", "tag", t)));
  (op.security || []).forEach((s) => {
    Object.keys(s).forEach((name) => head.appendChild(el("span", "auth", name)));
  });
  card.appendChild(head);

  if (op.summary) card.appendChild(el("p", "summary", op.summary));
  if (op.description) card.appendChild(el("p", "op-desc", op.description));

  const params = op.parameters || [];
  if (params.length) {
    card.appendChild(el("h3", null, "Parameters"));
    card.appendChild(paramTable(spec, params));
  }
  if (op.requestBody) card.appendChild(bodySection(spec, op.requestBody));
  if (op.responses) {
    card.appendChild(el("h3", null, "Responses"));
    card.appendChild(responseTable(spec, op.responses));
  }
  return card;
}

function renderSchemas(spec) {
  const schemas = (spec.components || {}).schemas || {};
  const names = Object.keys(schemas);
  if (!names.length) return;
  const host = document.getElementById("schemas");
  host.appendChild(el("h2", null, "Schemas"));
  names.forEach((name) => {
    const card = el("article", "op");
    card.appendChild(el("div", "path", name));
    const pre = el("pre");
    pre.appendChild(el("code", null, JSON.stringify(schemas[name], null, 2)));
    card.appendChild(pre);
    host.appendChild(card);
  });
}

function render(spec) {
  document.getElementById("api-title").textContent = (spec.info && spec.info.title) || "API reference";
  document.getElementById("api-version").textContent =
    "OpenAPI " + (spec.openapi || "?") + " · version " + ((spec.info && spec.info.version) || "?");
  document.getElementById("api-desc").textContent = (spec.info && spec.info.description) || "";

  const ops = document.getElementById("ops");
  const toc = document.getElementById("toc");
  ops.replaceChildren();

  Object.keys(spec.paths || {}).forEach((path) => {
    const item = spec.paths[path];
    METHODS.filter((m) => item[m]).forEach((method) => {
      ops.appendChild(renderOperation(spec, path, method, item[method]));
      const link = el("a");
      link.href = "#" + slug(method, path);
      link.appendChild(el("span", "method " + method, method.toUpperCase()));
      link.appendChild(document.createTextNode(path));
      toc.appendChild(link);
    });
  });

  if (!ops.children.length) ops.appendChild(el("p", "error", "The contract lists no operations."));
  renderSchemas(spec);
}

fetch("/openapi.json")
  .then((res) => {
    if (!res.ok) throw new Error("fetch /openapi.json: " + res.status);
    return res.json();
  })
  .then(render)
  .catch((err) => {
    document.getElementById("ops").replaceChildren(el("p", "error", "Could not load /openapi.json: " + err.message));
  });

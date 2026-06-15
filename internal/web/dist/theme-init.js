"use strict";

try {
  document.documentElement.dataset.theme = localStorage.getItem("omnilog_theme") || "system";
} catch (e) {
  document.documentElement.dataset.theme = "system";
}

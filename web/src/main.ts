import { initTheme } from "@kenn-io/kit-ui";
import { mount } from "svelte";

import App from "./App.svelte";
import "./app.css";

const target = document.getElementById("app");
if (!target) {
  throw new Error("missing application mount point");
}

initTheme({ storageKey: "roborev-theme" });
mount(App, { target });

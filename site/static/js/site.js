// Theme toggle, copy buttons, and the docs search shortcut. Every control
// here is progressive: without this script the page reads and navigates the
// same, and search still works as a plain form.
(() => {
  const root = document.documentElement;
  const systemDark = matchMedia("(prefers-color-scheme: dark)");
  const status = document.getElementById("copy-status");

  // Repeating the same message must still be announced, so the live region
  // empties before it speaks again.
  function announce(message) {
    if (!status) return;
    status.textContent = "";
    setTimeout(() => {
      status.textContent = message;
    }, 60);
  }

  // Theme: the toggle flips between light and dark and remembers the choice.
  // Transitions are suppressed for the frame the theme flips, so it does not smear.
  const toggle = document.querySelector("[data-theme-toggle]");
  const currentTheme = () =>
    root.dataset.theme || (systemDark.matches ? "dark" : "light");
  function labelToggle() {
    if (!toggle) return;
    const next = currentTheme() === "dark" ? "light" : "dark";
    toggle.setAttribute("aria-label", `Switch to the ${next} theme`);
  }
  if (toggle) {
    toggle.hidden = false;
    labelToggle();
    systemDark.addEventListener("change", labelToggle);
    toggle.addEventListener("click", () => {
      const theme = currentTheme() === "dark" ? "light" : "dark";
      root.classList.add("theme-switching");
      root.dataset.theme = theme;
      void document.body.offsetHeight;
      requestAnimationFrame(() => root.classList.remove("theme-switching"));
      try {
        localStorage.setItem("cap-theme", theme);
      } catch {
        /* The choice still holds for this page. */
      }
      labelToggle();
    });
  }

  // Shared clipboard path. When the Clipboard API is unavailable it selects
  // fallbackNode instead, so a keyboard copy still works.
  async function copyText(text, fallbackNode) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      const selection = window.getSelection();
      if (fallbackNode && selection) {
        const range = document.createRange();
        range.selectNodeContents(fallbackNode);
        selection.removeAllRanges();
        selection.addRange(range);
      }
      return false;
    }
  }
  const timers = new WeakMap();
  function flash(button, text) {
    if (!button.dataset.label) button.dataset.label = button.textContent;
    button.textContent = text;
    clearTimeout(timers.get(button));
    timers.set(
      button,
      setTimeout(() => {
        button.textContent = button.dataset.label;
      }, 2000),
    );
  }

  // One delegated listener covers every copy button, including any in
  // HTML that htmx swaps in later.
  document.addEventListener("click", async (event) => {
    const codeButton = event.target.closest?.("[data-copy-code]");
    if (codeButton) {
      const code = codeButton.closest(".code-block")?.querySelector("code");
      if (!code) return;
      const copied = await copyText(code.textContent, code);
      flash(codeButton, copied ? "Copied" : "Selected");
      announce(
        copied
          ? "Copied to the clipboard."
          : "Clipboard unavailable. The code is selected; copy it with your keyboard.",
      );
      return;
    }
    const pageButton = event.target.closest?.("[data-copy-page]");
    if (pageButton) {
      let copied = false;
      try {
        const response = await fetch(pageButton.dataset.copyPage, {
          headers: { Accept: "text/markdown" },
        });
        if (!response.ok) throw new Error(`HTTP ${response.status}`);
        copied = await copyText(await response.text());
      } catch {
        copied = false;
      }
      flash(pageButton, copied ? "Copied" : "Copy failed");
      announce(
        copied
          ? "Page copied as Markdown."
          : "Copy failed. Open View as Markdown and copy the text instead.",
      );
    }
  });
  for (const button of document.querySelectorAll("[data-copy-page]"))
    button.hidden = false;

  // "/" focuses the docs search from anywhere on a docs page; Escape clears it.
  const search = document.getElementById("docs-q");
  if (search) {
    document.addEventListener("keydown", (event) => {
      if (event.key !== "/" || event.defaultPrevented) return;
      if (event.ctrlKey || event.metaKey || event.altKey) return;
      if (event.target.closest?.("input, textarea, select, [contenteditable]"))
        return;
      event.preventDefault();
      search.focus();
      search.select();
    });
    search.addEventListener("keydown", (event) => {
      if (event.key !== "Escape") return;
      document.getElementById("docs-search-results")?.replaceChildren();
      search.value = "";
    });
  }
})();

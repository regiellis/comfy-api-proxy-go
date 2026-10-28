// Apply a remembered theme before CSS paints. Storage can be unavailable.
try {
  const theme = localStorage.getItem("cap-theme");
  if (theme === "dark" || theme === "light")
    document.documentElement.dataset.theme = theme;
} catch {
  /* The system preference remains the fallback. */
}

async function updateThemeColor() {
  const theme = localStorage.getItem("chat.theme.v1") || "system";
  let isDark = false;
  
  if (theme === "dark") {
    isDark = true;
  } else if (theme === "light") {
    isDark = false;
  } else {
    isDark = window.matchMedia('(prefers-color-scheme: dark)').matches;
  }
  
  const color = isDark ? '#262624' : '#faf9f5';
  
  // Update meta tag
  let meta = document.querySelector('meta[name="theme-color"]');
  if (!meta) {
    meta = document.createElement('meta');
    meta.name = "theme-color";
    document.head.appendChild(meta);
  }
  meta.setAttribute('content', color);
}

// Run on load and listen for changes
updateThemeColor();
window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', updateThemeColor);

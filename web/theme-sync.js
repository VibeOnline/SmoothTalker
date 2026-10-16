async function updateThemeColor() {
  const isDark = window.matchMedia('(prefers-color-scheme: dark)').matches;
  const color = isDark ? '#1f1e1d' : '#f0eee6';
  
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

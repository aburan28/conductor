// Organization branding: the accent color an administrator chose (Admin → Organization),
// applied by overriding the stylesheet's accent tokens on the root element. The server
// checked the color is a hex value readable against the light surface; the shades derived
// here keep links and soft backgrounds readable in both themes. Style properties are set
// through the CSSOM, which the page's Content-Security-Policy permits — no inline style
// sheet is ever written.

const TOKENS = ['--accent', '--accent-soft', '--accent-text', '--on-accent'];

function rgb(hex) {
  const m = /^#([0-9a-f]{6})$/i.exec(hex || '');
  if (!m) return null;
  const n = parseInt(m[1], 16);
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
}

function mix(a, b, t) {
  const out = a.map((v, i) => Math.round(v * (1 - t) + b[i] * t));
  return '#' + out.map(v => v.toString(16).padStart(2, '0')).join('');
}

function dark() {
  const set = document.documentElement.dataset.theme;
  if (set) return set === 'dark';
  return window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches;
}

let last = null;

export function applyBranding(branding) {
  last = branding || null;
  const root = document.documentElement.style;
  const c = rgb(branding && branding.accent_color);
  if (!c) {
    TOKENS.forEach(t => root.removeProperty(t));
    return;
  }
  const white = [255, 255, 255], black = [0, 0, 0];
  if (dark()) {
    // On a dark surface the brand color is lifted toward white so it stays visible.
    root.setProperty('--accent', mix(c, white, 0.35));
    root.setProperty('--accent-soft', mix(c, [27, 29, 33], 0.78));
    root.setProperty('--accent-text', mix(c, white, 0.55));
    root.setProperty('--on-accent', '#111111');
  } else {
    root.setProperty('--accent', branding.accent_color);
    root.setProperty('--accent-soft', mix(c, white, 0.88));
    // Text in the accent is darkened, so a link clears 4.5:1 whatever the brand color.
    root.setProperty('--accent-text', mix(c, black, 0.35));
    root.setProperty('--on-accent', branding.on_accent || '#ffffff');
  }
}

if (window.matchMedia) {
  // Follow the system theme while it decides.
  window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
    if (last) applyBranding(last);
  });
}

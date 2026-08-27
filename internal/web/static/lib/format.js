export function relTime(iso, now = Date.now()) {
  if (!iso) return '—';
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return '—';
  const secs = Math.round((now - t) / 1000);
  const abs = Math.abs(secs);
  const suffix = secs >= 0 ? ' ago' : ' from now';
  if (abs < 5) return 'just now';
  if (abs < 60) return abs + 's' + suffix;
  if (abs < 3600) return Math.round(abs / 60) + 'm' + suffix;
  if (abs < 86400) return Math.round(abs / 3600) + 'h' + suffix;
  return Math.round(abs / 86400) + 'd' + suffix;
}

export function fmtTime(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

export function fmtDate(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
}

export function fmtDay(iso) {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleDateString([], { month: 'short', day: 'numeric' });
}

export function fmtTokens(n) {
  n = Number(n || 0);
  if (Math.abs(n) >= 1e9) return (n / 1e9).toFixed(1).replace(/\.0$/, '') + 'B';
  if (Math.abs(n) >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, '') + 'M';
  if (Math.abs(n) >= 1e3) return (n / 1e3).toFixed(1).replace(/\.0$/, '') + 'k';
  return String(Math.round(n));
}

export function fmtUSD(v) {
  v = Number(v || 0);
  if (v === 0) return '—';
  if (v < 0.01) return '<$0.01';
  return '$' + v.toFixed(2);
}

export function fmtDuration(ms) {
  ms = Number(ms || 0);
  if (ms < 1000) return ms + 'ms';
  const s = Math.round(ms / 1000);
  if (s < 60) return s + 's';
  const m = Math.floor(s / 60);
  if (m < 60) return m + 'm ' + (s % 60) + 's';
  return Math.floor(m / 60) + 'h ' + (m % 60) + 'm';
}

export function durationBetween(a, b) {
  if (!a) return '—';
  const end = b ? new Date(b).getTime() : Date.now();
  return fmtDuration(end - new Date(a).getTime());
}

export function shortID(id) {
  return id ? String(id).slice(0, 8) : '';
}

export function maskToken(token) {
  if (!token) return '';
  if (token.length <= 10) return '•'.repeat(token.length);
  return token.slice(0, 6) + '…' + token.slice(-3);
}

export function titleCase(s) {
  return String(s || '').replace(/_/g, ' ').replace(/\b\w/g, c => c.toUpperCase());
}

export function plural(n, one, many) {
  return n === 1 ? `${n} ${one}` : `${n} ${many || one + 's'}`;
}

export function pct(part, whole) {
  if (!whole) return 0;
  return Math.max(0, Math.min(100, Math.round(100 * part / whole)));
}

// Landing-page interactions: mobile nav toggle + copy buttons. No external requests.
function initNav(): void {
  const toggle = document.querySelector<HTMLButtonElement>('.nav-toggle');
  const links = document.querySelector<HTMLElement>('.nav-links');
  if (!toggle || !links) return;
  toggle.addEventListener('click', () => {
    const open = links.classList.toggle('open');
    toggle.setAttribute('aria-expanded', open ? 'true' : 'false');
  });
  links.querySelectorAll('a').forEach((a) => {
    a.addEventListener('click', () => links.classList.remove('open'));
  });
}

function initCopy(): void {
  document.querySelectorAll<HTMLButtonElement>('[data-copy]').forEach((btn) => {
    btn.addEventListener('click', async () => {
      const target = btn.parentElement?.querySelector('code, pre');
      const text = target?.textContent?.trim() ?? '';
      if (!text) return;
      try {
        await navigator.clipboard.writeText(text);
      } catch {
        // Clipboard API unavailable (permissions, insecure context): fall back.
        const area = document.createElement('textarea');
        area.value = text;
        document.body.appendChild(area);
        area.select();
        document.execCommand('copy');
        area.remove();
      }
      const original = btn.textContent;
      btn.textContent = 'Copied';
      window.setTimeout(() => {
        btn.textContent = original;
      }, 1400);
    });
  });
}

initNav();
initCopy();

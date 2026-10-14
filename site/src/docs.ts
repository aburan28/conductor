// Docs renderer: imports synced markdown via ?raw, slices curated sections by
// heading, renders with marked, builds a per-page TOC from h2/h3.
// No external requests; no extra highlighting dependencies.
import { marked } from 'marked';
import readmeRaw from './content/README.md?raw';
import designRaw from './content/DESIGN.md?raw';

interface Section {
  level: number;
  title: string;
  markdown: string;
}

interface PageSpec {
  h1: string;
  intro: string;
  blocks: Array<{ source: 'readme' | 'design'; titles: string[] }>;
  extra?: string;
}

const SOURCES: Record<'readme' | 'design', string> = {
  readme: readmeRaw,
  design: designRaw,
};

const PAGES: Record<string, PageSpec> = {
  quickstart: {
    h1: 'Quickstart',
    intro: 'Install the binaries, then one command: Postgres (Docker only if none is running), the control plane on 127.0.0.1:8080, and your CLI login.',
    blocks: [
      {
        source: 'readme',
        titles: ['Install', 'Quickstart', 'Getting help', 'Manual, or on another repository', 'Removing Conductor'],
      },
    ],
  },
  'daily-use': {
    h1: 'Daily use',
    intro: 'The commands a team runs every day: check before editing, claim work, wrap sessions, share budget, pause and resume.',
    blocks: [
      {
        source: 'readme',
        titles: [
          'Daily use',
          'Connecting your coding tool',
          'Token usage across harnesses',
          'Pausing the wall of terminals',
          'Surviving a shutdown, and the machine itself',
        ],
      },
    ],
  },
  concepts: {
    h1: 'Concepts',
    intro: 'The mechanisms that make the answers trustworthy: atomic claims, fencing epochs, reservations, merge risk, and privacy-preserving duplicate detection.',
    blocks: [
      { source: 'design', titles: ['6. High-level architecture'] },
      { source: 'design', titles: ['10.2 Atomic claim algorithm', '10.3 Fencing rule'] },
      {
        source: 'design',
        titles: [
          '11.1 Resource keys',
          '11.2 Reservation modes',
          '11.3 Initial conflict matrix',
          '11.6 Merge-risk graph',
        ],
      },
      {
        source: 'design',
        titles: [
          '12. Privacy-preserving duplicate detection',
          '12.2 Fingerprint',
          '12.3 Visibility modes',
          '12.4 What is never collected by default',
        ],
      },
    ],
  },
  configuration: {
    h1: 'Configuration',
    intro: 'Policy lives in the repository, versioned with the code it governs. Every attempt records the hash of the files in force when it ran.',
    blocks: [{ source: 'readme', titles: ['Configuration'] }],
    extra: 'dispatch-yaml',
  },
  meshing: {
    h1: 'Meshing daemons',
    intro: 'A mesh is a set of conductord instances that know each other by certificate. Peering carries connectivity and identity, nothing else.',
    blocks: [
      { source: 'readme', titles: ['Peering daemons', 'Joining a mesh without seed nodes'] },
    ],
  },
  access: {
    h1: 'Adding coworkers',
    intro: 'One link onboards a teammate: invite mints them a token, join redeems it.',
    blocks: [{ source: 'readme', titles: ['Adding your coworkers'] }],
  },
  index: {
    h1: 'Documentation',
    intro: 'Run it, use it daily, understand the mechanisms, and scale it to a team.',
    blocks: [{ source: 'readme', titles: ['How it holds together'] }],
  },
};

function splitSections(md: string): Section[] {
  const lines = md.split('\n');
  const sections: Section[] = [];
  let current: Section | null = null;
  let inFence = false;
  const flush = () => {
    if (current) {
      current.markdown = current.markdown.replace(/\s+$/, '') + '\n';
      sections.push(current);
    }
  };
  for (const line of lines) {
    // Fenced code blocks contain lines starting with `#` (shell comments,
    // yaml comments) that are not headings — only split outside fences.
    if (line.trimStart().startsWith('```')) {
      inFence = !inFence;
      if (current) current.markdown += line + '\n';
      continue;
    }
    const m = !inFence ? line.match(/^(#{1,3})\s+(.*?)\s*#*\s*$/) : null;
    if (m) {
      flush();
      current = { level: m[1].length, title: m[2].trim(), markdown: line + '\n' };
    } else if (current) {
      current.markdown += line + '\n';
    }
  }
  flush();
  return sections;
}

function selectSections(md: string, titles: string[]): string {
  const wanted = new Set(titles.map((t) => t.toLowerCase()));
  return splitSections(md)
    .filter((s) => wanted.has(s.title.toLowerCase()))
    .map((s) => s.markdown)
    .join('\n');
}

// Pull the two policy snippets the README already documents (dispatch lanes and
// admission-queue caps) so the configuration page shows real policy verbatim.
function extractPolicySnippets(md: string): string {
  const fences: string[] = [];
  const re = /```yaml\n([\s\S]*?)```/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(md)) !== null) {
    const body = m[1];
    if (body.includes('lanes:') || body.includes('concurrency:')) fences.push(m[0]);
  }
  if (fences.length === 0) return '';
  return (
    '## Policy snippets from the README\n\n' +
    'The dispatch lanes and admission-queue caps above, quoted verbatim:\n\n' +
    fences.join('\n\n')
  );
}

function slugify(text: string): string {
  return text
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '');
}

function buildToc(content: HTMLElement, toc: HTMLElement): void {
  const headings = content.querySelectorAll('h2, h3');
  if (headings.length === 0) {
    toc.innerHTML = '<p class="toc-empty">No sections</p>';
    return;
  }
  const ul = document.createElement('ul');
  const used = new Set<string>();
  headings.forEach((h) => {
    let id = slugify(h.textContent ?? '');
    if (!id) id = 'section';
    let candidate = id;
    let n = 2;
    while (used.has(candidate)) candidate = `${id}-${n++}`;
    used.add(candidate);
    h.id = candidate;
    const li = document.createElement('li');
    const a = document.createElement('a');
    a.href = `#${candidate}`;
    a.textContent = h.textContent;
    if (h.tagName === 'H3') a.className = 'toc-h3';
    li.appendChild(a);
    ul.appendChild(li);
  });
  toc.appendChild(ul);
}

function addCodeHeaders(content: HTMLElement): void {
  content.querySelectorAll('pre').forEach((pre) => {
    const code = pre.querySelector('code');
    let lang = '';
    if (code) {
      const cls = Array.from(code.classList).find((c) => c.startsWith('language-'));
      if (cls) lang = cls.slice('language-'.length);
    }
    const head = document.createElement('div');
    head.className = 'code-head';
    const label = document.createElement('span');
    label.textContent = lang || 'output';
    const btn = document.createElement('button');
    btn.className = 'copy-btn';
    btn.type = 'button';
    btn.textContent = 'Copy';
    btn.addEventListener('click', async () => {
      const text = pre.textContent ?? '';
      try {
        await navigator.clipboard.writeText(text.trim());
      } catch {
        const area = document.createElement('textarea');
        area.value = text.trim();
        document.body.appendChild(area);
        area.select();
        document.execCommand('copy');
        area.remove();
      }
      btn.textContent = 'Copied';
      window.setTimeout(() => {
        btn.textContent = 'Copy';
      }, 1400);
    });
    head.appendChild(label);
    head.appendChild(btn);
    pre.parentElement?.insertBefore(head, pre);
  });
}

// Rewrite relative doc links (docs/DESIGN.md, README.md) to site routes.
function rewriteDocLinks(content: HTMLElement): void {
  const map: Record<string, string> = {
    'docs/design.md': './concepts.html',
    'readme.md': './quickstart.html',
  };
  content.querySelectorAll('a[href]').forEach((a) => {
    const href = a.getAttribute('href') ?? '';
    const key = href.split('#')[0].toLowerCase();
    if (map[key]) {
      const hash = href.includes('#') ? href.slice(href.indexOf('#')) : '';
      a.setAttribute('href', map[key] + hash);
    }
  });
}

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

function render(): void {
  const content = document.getElementById('content');
  const toc = document.getElementById('toc');
  if (!content) return;
  const page = document.body.dataset.page ?? 'index';
  const spec = PAGES[page];
  if (!spec) return;

  let md = spec.blocks
    .map((b) => selectSections(SOURCES[b.source], b.titles))
    .filter(Boolean)
    .join('\n');
  if (spec.extra === 'dispatch-yaml') {
    md += '\n' + extractPolicySnippets(readmeRaw);
  }
  const html = marked.parse(md, { async: false }) as string;
  const rendered = document.createElement('div');
  rendered.innerHTML = html;
  content.appendChild(rendered);
  rewriteDocLinks(content);
  addCodeHeaders(content);
  if (toc) buildToc(content, toc);

  const note = document.createElement('p');
  note.className = 'source-note';
  note.textContent =
    'Synced from the repository at build time (README.md and docs/*.md via scripts/sync-content.cjs). Rebuild to refresh.';
  content.appendChild(note);
}

initNav();
render();

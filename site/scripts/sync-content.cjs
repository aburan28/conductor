// Syncs repo content into the site so docs pages stay fresh automatically.
// Plain node, no dependencies. Runs as `prebuild` before every `npm run build`.
// Copies: README.md -> src/content/README.md, docs/*.md -> src/content/
const fs = require('fs');
const path = require('path');

const scriptDir = __dirname;
const repoRoot = path.resolve(scriptDir, '..', '..');
const destDir = path.resolve(scriptDir, '..', 'src', 'content');

const files = ['README.md', ...fs.readdirSync(path.join(repoRoot, 'docs'))
  .filter((f) => f.endsWith('.md'))
  .map((f) => path.join('docs', f))];

fs.mkdirSync(destDir, { recursive: true });

let copied = 0;
for (const rel of files) {
  const src = path.join(repoRoot, rel);
  const dest = path.join(destDir, path.basename(rel));
  const data = fs.readFileSync(src, 'utf8');
  fs.writeFileSync(dest, data);
  copied += 1;
  console.log(`sync-content: ${rel} -> src/content/${path.basename(rel)} (${data.length} bytes)`);
}
console.log(`sync-content: done, ${copied} file(s).`);

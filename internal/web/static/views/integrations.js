import { h, icon } from '../lib/dom.js';
import { card, snippet, copyButton } from '../components/ui.js';
import { maskToken } from '../lib/format.js';

// Every snippet is generated from the live connection (origin, project, token), so what a
// person copies is exactly what their tool needs. The token is masked until revealed.
export default {
  title: 'Integrations',
  render(root, ctx) {
    let reveal = false;
    let transport = 'stdio';
    const draw = () => root.replaceChildren(page(ctx, { reveal, transport, onReveal: v => { reveal = v; draw(); }, onTransport: v => { transport = v; draw(); } }));
    draw();
    return { refresh: () => {}, destroy: () => {} };
  },
};

function page(ctx, { reveal, transport, onReveal, onTransport }) {
  const origin = ctx.origin;
  const project = ctx.project;
  const token = reveal ? ctx.token : maskToken(ctx.token);
  const stdio = { command: 'conductor-mcp', args: ['--project', project] };
  const http = { url: `${origin}/mcp?project=${encodeURIComponent(project)}`, headers: { Authorization: `Bearer ${token}` } };
  const json = v => JSON.stringify(v, null, 2);
  const mcpBlock = name => transport === 'stdio' ? json({ mcpServers: { [name]: stdio } }) : json({ mcpServers: { [name]: http } });

  const tools = [
    { id: 'claude', name: 'Claude Code', file: '.mcp.json (project) or ~/.claude.json',
      snippet: mcpBlock('conductor'),
      extra: [['Hooks — .claude/settings.json', json({ hooks: {
        PreToolUse: [{ matcher: 'Edit|Write|MultiEdit|NotebookEdit', hooks: [{ type: 'command', command: 'conductor hook pre-tool' }] }],
        SessionStart: [{ hooks: [{ type: 'command', command: 'conductor hook session-start' }] }] } })],
        ['Or add the server with the CLI', `claude mcp add conductor -- conductor-mcp --project ${project}`]],
      note: 'PreToolUse runs `conductor check` on the file about to be edited and blocks (exit 2) on a hard conflict, telling Claude who holds it. SessionStart injects the active task card and any offers.' },
    { id: 'cursor', name: 'Cursor', file: '.cursor/mcp.json (project) or ~/.cursor/mcp.json',
      snippet: mcpBlock('conductor'),
      extra: [['Rules — .cursor/rules/conductor.mdc', `---\ndescription: Conductor coordination\nalwaysApply: true\n---\nBefore editing, call conductor_check_conflicts with the files you will touch. Claim work with coord_start_work. Report progress with coord_report_progress. Never put prompts or secrets in task titles.`]],
      note: 'Cursor mounts the MCP server per project; the rules file makes the agent call the tools reflexively.' },
    { id: 'codex', name: 'Codex', file: '~/.codex/config.toml',
      snippet: transport === 'stdio'
        ? `[mcp_servers.conductor]\ncommand = "conductor-mcp"\nargs = ["--project", "${project}"]`
        : `[mcp_servers.conductor]\nurl = "${origin}/mcp?project=${encodeURIComponent(project)}"\nbearer_token_env_var = "CONDUCTOR_TOKEN"`,
      extra: [['Instruction shim — AGENTS.md', 'conductor init adds a managed block to AGENTS.md; Codex reads it every session.']],
      note: 'The HTTP form reads the token from CONDUCTOR_TOKEN so nothing secret lands in the config file.' },
    { id: 'opencode', name: 'OpenCode', file: 'opencode.json (project) or ~/.config/opencode/opencode.json',
      snippet: transport === 'stdio'
        ? json({ mcp: { conductor: { type: 'local', command: ['conductor-mcp', '--project', project], enabled: true } } })
        : json({ mcp: { conductor: { type: 'remote', url: http.url, headers: http.headers, enabled: true } } }),
      extra: [['Plugin — .opencode/plugin/conductor.js', `// installed by: conductor integrate opencode\n// checks conflicts before every edit/write tool call via \`conductor hook pre-tool\``]],
      note: 'OpenCode is the broadest harness for local models (ollama/…), which is how a qwen lane in dispatch.yaml runs.' },
    { id: 'windsurf', name: 'Windsurf', file: '~/.codeium/windsurf/mcp_config.json', snippet: mcpBlock('conductor'), extra: [], note: 'Windsurf reads the standard mcpServers shape.' },
    { id: 'vscode', name: 'VS Code (Copilot)', file: '.vscode/mcp.json',
      snippet: transport === 'stdio' ? json({ servers: { conductor: { type: 'stdio', ...stdio } } }) : json({ servers: { conductor: { type: 'http', url: http.url, headers: http.headers } } }),
      extra: [['Pause/resume companion', 'integrations/vscode — reopens paused agent sessions in integrated terminals.']], note: 'VS Code uses a `servers` key rather than `mcpServers`.' },
    { id: 'zed', name: 'Zed', file: '~/.config/zed/settings.json',
      snippet: json({ context_servers: { conductor: transport === 'stdio' ? { command: { path: 'conductor-mcp', args: ['--project', project] } } : { url: http.url, headers: http.headers } } }),
      extra: [], note: 'Zed calls them context servers.' },
    { id: 'gemini', name: 'Gemini CLI', file: '~/.gemini/settings.json', snippet: mcpBlock('conductor'), extra: [], note: 'Gemini CLI reads the standard mcpServers shape.' },
  ];

  const header = card({ title: 'Connect a tool', body: h('div', { class: 'stack' },
    h('p', {}, 'One command writes the right config for each tool and, where the tool supports it, the pre-edit hook that stops a collision before it happens:'),
    snippet(`conductor integrate claude     # or: cursor, codex, opencode, windsurf, vscode, zed, gemini, all\nconductor integrate --print all  # show what would be written`),
    h('div', { class: 'toolbar' },
      h('span', { class: 'muted' }, 'Manual snippets below use'),
      h('div', { class: 'seg' },
        h('button', { class: transport === 'stdio' ? 'active' : '', type: 'button', onclick: () => onTransport('stdio') }, 'stdio (conductor-mcp)'),
        h('button', { class: transport === 'http' ? 'active' : '', type: 'button', onclick: () => onTransport('http') }, 'HTTP (no local binary)')),
      h('div', { class: 'spacer' }),
      transport === 'http' ? h('button', { class: 'btn sm', onclick: () => onReveal(!reveal) }, icon('eye'), reveal ? 'Mask token' : 'Reveal token') : null,
      transport === 'http' ? copyButton(() => ctx.token, 'Copy token') : null),
    transport === 'http' ? h('div', { class: 'notice warn' }, 'The HTTP form embeds your bearer token. Put it in a user-level config, never in a file that gets committed. The stdio form reads ~/.conductor/credentials at runtime and needs no token in any config.') : null,
    h('div', { class: 'muted', style: { fontSize: '12px' } }, `Endpoint ${origin} · project ${project} · MCP endpoint ${origin}/mcp`)) });

  const cards = tools.map(t => card({ title: t.name, body: h('div', { class: 'tool-card' },
    h('div', { class: 'cmd' }, h('code', {}, `conductor integrate ${t.id}`), copyButton(`conductor integrate ${t.id}`, '')),
    h('div', { class: 'muted', style: { fontSize: '12px' } }, t.file),
    snippet(t.snippet),
    ...t.extra.map(([label, body]) => h('div', {}, h('div', { class: 'muted', style: { fontSize: '12px', marginBottom: '4px' } }, label), snippet(body))),
    h('div', { class: 'hint' }, t.note)) }));

  const mcpTools = card({ title: 'What the agent gets', body: h('div', { class: 'stack' },
    h('p', {}, 'Eleven tools, each one API call: ', h('code', {}, 'conductor_check_conflicts'), ', ', h('code', {}, 'coord_start_work'), ', ', h('code', {}, 'coord_get_work'), ', ', h('code', {}, 'coord_expand_scope'), ', ', h('code', {}, 'coord_report_progress'), ', ', h('code', {}, 'coord_publish_result'), ', ', h('code', {}, 'coord_finish_work'), ', ', h('code', {}, 'coord_handoff'), ', ', h('code', {}, 'coord_delegate'), ', ', h('code', {}, 'coord_capabilities'), ', ', h('code', {}, 'coord_project_status'), '.'),
    h('p', { class: 'muted' }, 'Heartbeats are deliberately not a tool: a model should never spend tokens saying it is still alive. The wrap sidecar and hooks do that over HTTP.'),
    h('p', { class: 'muted' }, 'Interactive sessions: `conductor wrap <tool>` registers presence, heartbeats, reports token usage, and passes the session id so offers reach the right window.')) });

  return h('div', { class: 'stack', style: { gap: '20px' } }, header, h('div', { class: 'grid-2' }, ...cards), mcpTools);
}

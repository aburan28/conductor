// Demo mode: a fixture-backed stand-in for the API so /?demo=1 shows the whole product with
// no server. Data is deterministic; mutations update the fixtures so the UI feels real.
const now = Date.now();
const min = 60 * 1000, hour = 60 * min, day = 24 * hour;
const iso = t => new Date(t).toISOString();

const PEOPLE = ['alice', 'rachel', 'you'];
const D = {};

D.whoami = { principal: { handle: 'you' }, projects: [{ id: 'p-demo', slug: 'demo', role: 'maintainer' }] };

D.tasks = [
  mkTask(1, 'Add retry-aware model routing', 'running', 'alice', { scopes: ['dir:internal/router'], labels: ['backend'], objective: 'Escalate a tier after two failures; never cross a security floor.', risk: 'medium', attempts: 2, branch: 'conductor/T-1' }),
  mkTask(2, '', 'running', 'rachel', { scopes: ['dir:internal/api'], private: true, attempts: 1 }),
  mkTask(3, 'Team invitation flow: send, accept, revoke', 'ready', 'alice', { scopes: ['dir:internal/members', 'path:internal/api/members.go'], labels: ['backend', 'api'], criteria: ['Invite email carries a one-time token', 'Acceptance creates a contributor membership', 'Revocation closes live sessions'], priority: 10 }),
  mkTask(4, 'Document the dispatch policy YAML', 'ready', '', { scopes: ['dir:docs'], labels: ['docs', 'cheap'], priority: 2, criteria: ['Every lane field explained', 'Example with a local qwen lane'] }),
  mkTask(5, 'Migrate usage_buckets to monthly partitions', 'blocked_dependency', 'you', { scopes: ['migration:primary', 'dir:internal/db'], labels: ['db'], risk: 'high', deps: ['T-1'] }),
  mkTask(6, 'Fix flaky lease-reclaim test', 'done', 'you', { scopes: ['path:internal/db/claim.go'], labels: ['tests'], attempts: 1, commit: 'a1b2c3d4e5f6' }),
  mkTask(7, 'SSE stream drops named events silently', 'review_required', 'rachel', { scopes: ['path:internal/api/handlers.go'], labels: ['bug'], attempts: 3, risk: 'low' }),
  mkTask(8, 'Speed up conflict graph recompute', 'failed', 'alice', { scopes: ['dir:internal/coord'], labels: ['perf'], attempts: 4, failure: 'timeout' }),
];

function mkTask(n, title, status, owner, o = {}) {
  return {
    id: 'task-' + n, ref: 'T-' + n, project_id: 'p-demo', status, owner, title,
    visibility: o.private ? 'private' : 'team_summary', redacted: !!o.private,
    priority: o.priority || 0, risk_level: o.risk || 'unknown', scopes: o.scopes || [], labels: o.labels || [],
    objective: o.objective || '', acceptance_criteria: (o.criteria || []).map((text, i) => ({ text, status: status === 'done' ? 'pass' : i === 0 && o.attempts ? 'pass' : 'pending' })),
    depends_on: o.deps || [], attempts_count: o.attempts || 0, fencing_epoch: (o.attempts || 0) + 1,
    branch: o.branch || '', commit_sha: o.commit || '', external_ref: o.ext || '',
    created_at: iso(now - (10 - n) * day), updated_at: iso(now - n * 37 * min % (6 * hour)),
  };
}

D.sessions = [
  mkSession('s1', 'alice', 'claude', 'working', { model: 'claude-opus-5', tier: 'T4', effort: 'high', max: 'xhigh', task: 'T-1', machine: 'alice-mbp', branch: 'conductor/T-1', resolved: true }),
  mkSession('s2', 'rachel', 'codex', 'working', { model: 'gpt-5-codex', tier: 'T2', effort: 'medium', max: 'medium', task: 'T-2', machine: 'rachel-desktop', resolved: true }),
  mkSession('s3', 'you', 'claude', 'online_idle', { model: 'claude-sonnet-5', tier: 'T2', effort: 'medium', max: 'high', machine: 'your-mbp', resolved: true }),
  mkSession('s4', 'you', 'opencode', 'online_idle', { model: 'ollama/qwen3:27b', machine: 'your-mbp', resolved: false }),
  mkSession('s5', 'rachel', 'codex', 'paused', { model: 'gpt-5-codex', tier: 'T2', effort: 'medium', max: 'medium', machine: 'rachel-desktop', resolved: true }),
  mkSession('s6', 'alice', 'claude', 'paused', { model: 'claude-opus-5', tier: 'T4', effort: 'high', max: 'xhigh', machine: 'alice-mbp', resolved: true, pending: 'resume' }),
];

function mkSession(id, principal, harness, state, o) {
  return {
    id, project_id: 'p-demo', principal, principal_id: 'pr-' + principal, kind: 'human', harness,
    harness_version: harness === 'claude' ? '2.1.0' : '', machine_id: o.machine || '', branch: o.branch || '',
    visibility: 'team_summary', state, pending_control: o.pending || '', active_task_ref: o.task || '',
    capabilities: { model: o.model, tier: o.tier, reasoning_effort: o.effort, max_reasoning_effort: o.max, resolved: !!o.resolved, roles: [] },
    started_at: iso(now - 3 * hour), last_heartbeat: iso(now - (id === 's2' ? 8 : 15) * 1000), expires_at: iso(now + min),
  };
}

D.capSessions = D.sessions.map(s => ({
  session_id: s.id, principal: s.principal, harness: s.harness, state: s.state,
  available: ['online_idle', 'working', 'planning', 'reviewing'].includes(s.state),
  model: s.capabilities.model, tier: s.capabilities.tier, reasoning_effort: s.capabilities.reasoning_effort,
  max_reasoning_effort: s.capabilities.max_reasoning_effort || s.capabilities.reasoning_effort,
  resolved: s.capabilities.resolved, active_task_ref: s.active_task_ref, queued_offers: s.id === 's3' ? 1 : 0,
  last_heartbeat: s.last_heartbeat, machine_id: s.machine_id,
}));

D.conflicts = [
  { id: 'c1', kind: 'scope_overlap', severity: 'high', suggestion: 'suggest_wait', state: 'open', weight: 0.9,
    resources: ['dir:internal/api'], reason: 'write_exclusive overlaps write_exclusive on dir:internal/api',
    mine: { task_id: 'task-3', task_ref: 'T-3', title: 'Team invitation flow: send, accept, revoke', owner: 'alice' },
    other: { task_id: 'task-2', task_ref: 'T-2', owner: 'rachel' }, detected_at: iso(now - 22 * min) },
  { id: 'c2', kind: 'duplicate_intent', severity: 'medium', suggestion: 'suggest_join', state: 'open', weight: 0.64,
    resources: [], reason: 'similar intent across different wording',
    mine: { task_id: 'task-4', task_ref: 'T-4', title: 'Document the dispatch policy YAML', owner: '' },
    other: { task_id: 'task-7', task_ref: 'T-7', title: 'SSE stream drops named events silently', owner: 'rachel' }, detected_at: iso(now - 2 * hour) },
];

D.presence = D.sessions.filter(s => s.state !== 'closed').map(s => ({
  session_id: s.id, principal: s.principal, principal_id: s.principal_id, kind: 'human', harness: s.harness,
  state: s.state, task_ref: s.active_task_ref, task_title: (D.tasks.find(t => t.ref === s.active_task_ref) || {}).title || '',
  branch: s.branch, scopes: (D.tasks.find(t => t.ref === s.active_task_ref) || {}).scopes || [],
  started_at: s.started_at, last_heartbeat: s.last_heartbeat,
}));

D.runners = [
  { id: 'r1', name: 'build-box', state: 'online', in_flight: 1, max_concurrency: 4, heartbeat_at: iso(now - 12 * 1000),
    capabilities: { harnesses: ['claude', 'opencode'], models: ['claude-sonnet-5', 'ollama/qwen3:27b'], platform: 'linux', repo_path: '/srv/repo' } },
  { id: 'r2', name: 'alice-mbp', state: 'offline', in_flight: 0, max_concurrency: 2, heartbeat_at: iso(now - 3 * hour),
    capabilities: { harnesses: ['claude'], models: [], platform: 'darwin', repo_path: '~/work/repo' } },
];

D.profiles = [
  mkProfile('planner.frontier', 'claude', 'anthropic', 'claude-opus-5', 'high', 'T4', ['architecture', 'long_context', 'strong_tool_use'], 15, 75),
  mkProfile('worker.general', 'claude', 'anthropic', 'claude-sonnet-5', 'medium', 'T2', ['code_edit', 'tests', 'multi_file'], 3, 15),
  mkProfile('worker.fast', 'claude', 'anthropic', 'claude-haiku-4-5-20251001', 'low', 'T1', ['code_edit', 'tests'], 1, 5),
  mkProfile('worker.local', 'opencode', 'ollama', 'ollama/qwen3:27b', 'none', 'T1', ['code_edit'], null, null),
  mkProfile('reviewer.strong', 'claude', 'anthropic', 'claude-opus-5', 'high', 'T4', ['code_review', 'architecture'], 15, 75),
];
function mkProfile(alias, harness, provider, model, effort, tier, caps, inC, outC) {
  return { id: 'mp-' + alias + harness, alias, harness, provider, model, reasoning_effort: effort, tier, capabilities: caps,
    input_cost_per_mtok: inC, output_cost_per_mtok: outC, billing: inC == null ? 'capacity' : 'tokens', enabled: true, catalog_version: 'demo' };
}

D.queue = {
  policy: { max_active_sessions: 4, max_sessions_per_principal: 2, max_concurrent_attempts: 4 },
  active: { sessions: 4, attempts: 2 },
  tickets: [
    { id: 'q-101', principal: 'you', kind: 'session', harness: 'claude', model: 'claude-sonnet-5', state: 'queued', position: 1, requested_at: iso(now - 6 * min), expires_at: iso(now + 30 * min) },
    { id: 'q-102', principal: 'rachel', kind: 'attempt', task_ref: 'T-4', harness: 'opencode', model: 'ollama/qwen3:27b', state: 'queued', position: 2, requested_at: iso(now - 3 * min), expires_at: iso(now + 30 * min) },
    { id: 'q-100', principal: 'alice', kind: 'session', harness: 'claude', state: 'granted', requested_at: iso(now - 2 * hour), granted_at: iso(now - 2 * hour + 40 * 1000), expires_at: iso(now + hour) },
  ],
};

D.budget = {
  policy: { monthly_usd: 400, downshift_at: 0.75, pause_at: 0.95, member_tokens: 20000000 },
  window: '30 days', project: { monthly_usd: 400, spent_usd: 187.4 },
  members: [
    { principal_id: 'pr-alice', handle: 'alice', allowance_tokens: 20000000, spent_tokens: 14100000, shared_in_tokens: 0, shared_out_tokens: 2000000, remaining_tokens: 3900000 },
    { principal_id: 'pr-rachel', handle: 'rachel', allowance_tokens: 20000000, spent_tokens: 6200000, shared_in_tokens: 2000000, shared_out_tokens: 0, remaining_tokens: 15800000 },
    { principal_id: 'pr-you', handle: 'you', allowance_tokens: 20000000, spent_tokens: 9800000, shared_in_tokens: 0, shared_out_tokens: 0, remaining_tokens: 10200000 },
  ],
};
D.grants = [{ id: 'g1', from_handle: 'alice', to_handle: 'rachel', tokens: 2000000, note: 'finishing the router refactor', created_at: iso(now - 2 * day) }];
D.members = [{ handle: 'alice', kind: 'human', role: 'project_admin' }, { handle: 'rachel', kind: 'human', role: 'contributor' }, { handle: 'you', kind: 'human', role: 'maintainer' }];
D.tokens = [{ name: 'bootstrap', created_at: iso(now - 12 * day), last_used_at: iso(now - 2 * min) }, { name: 'laptop', created_at: iso(now - 4 * day), last_used_at: iso(now - day) }];

D.swarm = {
  contributors: [
    { principal: 'alice', principal_id: 'pr-alice', kind: 'session', name: 'alice-mbp', harness: 'claude', harnesses: ['claude'], models: ['claude-opus-5'], state: 'working', in_flight: 1, max_concurrency: 1, last_heartbeat: iso(now - 15 * 1000), budget: D.budget.members[0] },
    { principal: 'rachel', principal_id: 'pr-rachel', kind: 'session', name: 'rachel-desktop', harness: 'codex', harnesses: ['codex'], models: ['gpt-5-codex'], state: 'working', in_flight: 1, max_concurrency: 1, last_heartbeat: iso(now - 8 * 1000), budget: D.budget.members[1] },
    { principal: 'you', principal_id: 'pr-you', kind: 'runner', name: 'build-box', harness: '', harnesses: ['claude', 'opencode'], models: ['claude-sonnet-5', 'ollama/qwen3:27b'], state: 'online', in_flight: 1, max_concurrency: 4, last_heartbeat: iso(now - 12 * 1000), budget: D.budget.members[2] },
  ],
  capacity: { runners: 1, sessions_accepting: 2, slots_free: 3 },
  queue_depth: 2,
};

D.events = [];
(function seedEvents() {
  const kinds = [
    ['task.claimed', { task_ref: 'T-1', principal: 'alice' }],
    ['attempt.progress', { task_ref: 'T-1', phase: 'implementing', changed_paths: ['internal/router/router.go'] }],
    ['scope.expanded', { task_ref: 'T-2', resources: ['path:internal/api/ratelimit.go'] }],
    ['budget.shared', { from: 'alice', to: 'rachel', tokens: 2000000 }],
    ['task.unblocked', { task_ref: 'T-5', status: 'ready' }],
    ['attempt.evidence', { task_ref: 'T-6', exit_code: 0, command_id: 'check-0' }],
    ['attempt.stalled', { task_ref: 'T-8', harness: 'claude', reason: 'no harness event within the stall window' }],
    ['queue.enqueued', { principal: 'you', kind: 'session' }],
    ['session.registered', { principal: 'you', harness: 'opencode' }],
    ['conflict.detected', { task_ref: 'T-3', kind: 'scope_overlap', severity: 'high' }],
  ];
  for (let i = 0; i < 36; i++) {
    const [type, payload] = kinds[i % kinds.length];
    D.events.push({ id: 'e' + i, type, payload, visibility: 'team_summary', occurred_at: iso(now - i * 9 * min) });
  }
})();

D.usage = [];
(function seedUsage() {
  const lanes = [
    ['claude', 'claude-opus-5', 'alice', 9],
    ['claude', 'claude-sonnet-5', 'you', 6],
    ['codex', 'gpt-5-codex', 'rachel', 4],
    ['opencode', 'ollama/qwen3:27b', 'you', 2],
  ];
  for (let d = 0; d < 30; d++) {
    for (const [harness, model, principal, scale] of lanes) {
      const wobble = 0.5 + Math.abs(Math.sin(d * 2.1 + scale));
      const input = Math.round(240000 * scale * wobble);
      const output = Math.round(30000 * scale * wobble);
      const cache = Math.round(input * 2.2);
      D.usage.push({
        period: iso(now - d * day - (now % day)), harness, model, principal,
        requests: Math.round(40 * scale * wobble), input_tokens: input, cache_read_tokens: cache,
        cache_write_tokens: Math.round(cache * 0.12), output_tokens: output, reasoning_tokens: Math.round(output * 0.3),
        total_tokens: input + cache + Math.round(cache * 0.12) + output,
        cost_usd: model.startsWith('ollama') ? 0 : +(input / 1e6 * 4 + output / 1e6 * 18).toFixed(2),
        source: harness === 'opencode' ? 'sync' : 'session', reasoning_effort: model.includes('opus') ? 'high' : 'medium',
        external_session_id: 'sess-' + harness + '-' + principal,
      });
    }
  }
})();

D.attempts = {
  'T-1': [mkAttempt(1, 'failed_retryable', 'claude', 'claude-sonnet-5', 'medium', { fail: 'tests_failed', in: 812000, out: 64000, cost: 4.1 }),
          mkAttempt(2, 'running', 'claude', 'claude-opus-5', 'high', { in: 1420000, out: 96000, cost: 11.7 })],
  'T-6': [mkAttempt(1, 'succeeded', 'claude', 'claude-sonnet-5', 'medium', { in: 240000, out: 18000, cost: 1.2, done: true })],
  'T-7': [mkAttempt(1, 'failed_retryable', 'codex', 'gpt-5-codex', 'medium', { fail: 'review_rejected', in: 300000, out: 20000, cost: 2.0 }),
          mkAttempt(2, 'failed_retryable', 'codex', 'gpt-5-codex', 'high', { fail: 'review_rejected', in: 350000, out: 25000, cost: 2.4 }),
          mkAttempt(3, 'succeeded', 'claude', 'claude-opus-5', 'high', { in: 900000, out: 60000, cost: 9.0, done: true })],
  'T-8': [mkAttempt(1, 'failed_terminal', 'claude', 'claude-sonnet-5', 'medium', { fail: 'timeout', in: 2000000, out: 150000, cost: 14 })],
};
function mkAttempt(n, state, harness, model, effort, o) {
  return { id: 'a' + n, attempt_number: n, state, role: 'implementer', harness, model_alias: 'worker.general', resolved_model: model,
    reasoning_effort: effort, branch: 'conductor/T-x/' + n, tokens_in: o.in || 0, tokens_out: o.out || 0, cost_usd: o.cost || 0,
    failure_class: o.fail || '', started_at: iso(now - (4 - n) * hour), ended_at: o.done || o.fail ? iso(now - (4 - n) * hour + 25 * min) : undefined,
    last_event_at: iso(now - 4 * min), workflow_sha: 'wf-demo', router_policy_version: 'router/v1' };
}

D.validation = {
  'T-6': [{ command: 'go vet ./...', exit_code: 0, duration_ms: 8200, created_at: iso(now - day) },
          { command: 'go test ./...', exit_code: 0, duration_ms: 61000, created_at: iso(now - day) }],
  'T-1': [{ command: 'go vet ./...', exit_code: 0, duration_ms: 7400, created_at: iso(now - 30 * min) },
          { command: 'go test ./internal/router/', exit_code: 1, duration_ms: 12800, created_at: iso(now - 30 * min) }],
};
D.decisions = {
  'T-1': [{ kind: 'route', decision: 'worker.general → claude/claude-opus-5 at high', rationale: { rationale: ['base tier T2 from task shape', 'escalated to T3 after 2 prior failures', 'raised reasoning effort after a prior failure', 'resolved worker.general on claude to claude-opus-5 at effort high'] }, created_at: iso(now - hour) }],
};
D.handoffs = {
  'T-7': { task_ref: 'T-7', recommended_next_action: 'Re-run the SSE soak test and confirm named events arrive after reconnect',
    completed_work: ['Reproduced the drop with a 2-hour soak', 'Fixed the cursor advance on empty batches'],
    open_questions: ['Should keepalive interval be configurable?'], assumptions: ['Load balancer timeout is 60s'] },
};
D.cards = {};

D.explain = ref => {
  const t = D.tasks.find(x => x.ref === ref);
  if (!t) return null;
  const cheap = (t.labels || []).includes('docs') || (t.labels || []).includes('cheap');
  return {
    decision: cheap
      ? { lane: 'implement', role: 'implementer', harness: 'opencode', model: 'ollama/qwen3:27b', reasoning_effort: 'none', tier: 'T1', rule: 'docs-are-cheap', rationale: ['rule docs-are-cheap matched: task.labels has "docs"', 'candidate ollama/qwen3:27b on opencode is available', 'local model: zero marginal cost'] }
      : { lane: 'implement', role: 'implementer', harness: 'claude', model: 'claude-sonnet-5', reasoning_effort: 'medium', tier: 'T2', rule: '', rationale: ['base tier T2 from task shape', 'cheapest qualifying candidate wins'] },
    candidates: [
      { lane: 'implement', model: 'ollama/qwen3:27b', harness: 'opencode', eligible: cheap, reason: cheap ? 'when: task.labels has "docs"' : 'when clause did not match' },
      { lane: 'implement', model: 'claude-sonnet-5', harness: 'claude', eligible: true, reason: 'default candidate' },
      { lane: 'implement', model: 'claude-opus-5', harness: 'claude', eligible: false, reason: 'reserved: attempt.failures >= 2' },
    ],
  };
};

// ---------------------------------------------------------------------------
// Request router
// ---------------------------------------------------------------------------

let taskSeq = D.tasks.length;

export function installDemo(api) {
  api.request = async (method, path, body) => {
    await sleep(120 + Math.random() * 180);
    const [p, queryString] = path.split('?');
    const q = new URLSearchParams(queryString || '');
    const seg = p.split('/').filter(Boolean); // ['v1', ...]
    const m = (pattern) => matchPath(seg, pattern);
    let mm;

    if (m('v1/whoami')) return D.whoami;
    if (m('v1/tokens') && method === 'GET') return { tokens: D.tokens };
    if (m('v1/tokens') && method === 'POST') { const t = { name: (body && body.name) || 'demo', created_at: iso(Date.now()) }; D.tokens.unshift(t); return { name: t.name, token: 'cdt_demo_' + Math.random().toString(36).slice(2) }; }
    if ((mm = m('v1/tokens/:name')) && method === 'DELETE') { const t = D.tokens.find(x => x.name === mm.name); if (t) t.revoked_at = iso(Date.now()); return null; }

    if ((mm = m('v1/projects/:p')) ) return demoProject();
    if (m('v1/projects/:p/status')) return { project: 'demo', counts: statusCounts(), active: D.tasks.filter(t => ['claimed', 'running', 'verifying', 'review_required', 'merging'].includes(t.status)), ready: D.tasks.filter(t => ['ready', 'proposed'].includes(t.status)), conflicts: D.conflicts.filter(c => c.state === 'open'), presence: D.presence };
    if (m('v1/projects/:p/presence')) return { presence: D.presence };
    if (m('v1/projects/:p/capabilities')) return { project: 'demo', inventory: inventory(), sessions: D.capSessions };
    if (m('v1/projects/:p/sessions')) return { sessions: D.sessions };
    if (m('v1/projects/:p/runners')) return { runners: D.runners };
    if (m('v1/projects/:p/models')) return { profiles: D.profiles };
    if (m('v1/projects/:p/conflicts')) return { conflicts: q.get('open') === 'false' ? D.conflicts : D.conflicts.filter(c => c.state === 'open') };
    if (m('v1/projects/:p/reservations')) return { reservations: [] };
    if (m('v1/projects/:p/events')) return { events: D.events.slice(0, Number(q.get('limit') || 100)) };
    if (m('v1/projects/:p/budget')) return D.budget;
    if (m('v1/projects/:p/budget/grants')) return { grants: D.grants };
    if (m('v1/projects/:p/budget/share') && method === 'POST') return demoShare(body);
    if (m('v1/projects/:p/members') && method === 'GET') return { members: D.members };
    if (m('v1/projects/:p/members') && method === 'POST') { D.members.push({ handle: body.handle, kind: 'human', role: body.role }); return { handle: body.handle, role: body.role, token: 'cdt_demo_' + Math.random().toString(36).slice(2) }; }
    if ((mm = m('v1/projects/:p/members/:h')) && method === 'DELETE') { D.members = D.members.filter(x => x.handle !== mm.h); return null; }
    if (m('v1/projects/:p/swarm')) return D.swarm;
    if (m('v1/projects/:p/queue') && method === 'GET') return D.queue;
    if (m('v1/projects/:p/queue') && method === 'POST') return demoEnqueue(body);
    if ((mm = m('v1/queue/:id')) && method === 'DELETE') { const t = D.queue.tickets.find(x => x.id === mm.id); if (t) t.state = 'cancelled'; repositionQueue(); return null; }
    if (m('v1/projects/:p/usage')) return demoUsage(q);
    if (m('v1/projects/:p/tasks') && method === 'GET') return { tasks: filterTasks(q) };
    if (m('v1/projects/:p/tasks') && method === 'POST') return demoCreateTask(body);

    if ((mm = m('v1/tasks/:ref')) && method === 'GET') return findTask(mm.ref);
    if ((mm = m('v1/tasks/:ref/attempts'))) return { attempts: D.attempts[mm.ref] || [] };
    if ((mm = m('v1/tasks/:ref/validation'))) return { results: D.validation[mm.ref] || [] };
    if ((mm = m('v1/tasks/:ref/decisions'))) return { decisions: D.decisions[mm.ref] || [] };
    if ((mm = m('v1/tasks/:ref/handoff')) && method === 'GET') { const hh = D.handoffs[mm.ref]; if (!hh) throw notFound(); return { bundle: hh }; }
    if ((mm = m('v1/tasks/:ref/card'))) return demoCard(mm.ref);
    if ((mm = m('v1/tasks/:ref/route/explain'))) { const e = D.explain(mm.ref); if (!e) throw notFound(); return e; }
    if ((mm = m('v1/tasks/:ref/claim')) && method === 'POST') return demoClaim(mm.ref);
    if ((mm = m('v1/tasks/:ref/release')) && method === 'POST') return demoTransition(mm.ref, (body && body.next_status) || 'ready');
    if ((mm = m('v1/tasks/:ref/transition')) && method === 'POST') return demoTransition(mm.ref, body.to);
    if ((mm = m('v1/tasks/:ref/assign')) && method === 'POST') return demoAssign(mm.ref, body);
    if ((mm = m('v1/tasks/:ref/handoff')) && method === 'POST') { D.handoffs[mm.ref] = { task_ref: mm.ref, ...body.bundle }; return demoTransition(mm.ref, 'ready'); }
    if ((mm = m('v1/tasks/:ref/scopes')) && method === 'POST') return { granted: body.scopes || [], conflicts: [] };

    if ((mm = m('v1/sessions/:id/assignments'))) return { assignments: mm.id === 's3' ? [{ id: 'as1', task_id: 'task-3', task_ref: 'T-3', session_id: 's3', state: 'offered', requirement: { tier: 'T2' }, rationale: '1 of 4 live session(s) qualified', created_at: iso(now - 10 * min), expires_at: iso(now + 20 * min) }] : [] };
    if ((mm = m('v1/assignments/:id/respond')) && method === 'POST') return { id: mm.id, state: body.accept ? 'accepted' : 'declined' };
    if ((mm = m('v1/conflicts/:id/resolve')) && method === 'POST') { const c = D.conflicts.find(x => x.id === mm.id); if (c) c.state = body.state; pushEvent('conflict.resolved', { kind: c && c.kind, state: body.state }); return { id: mm.id, state: body.state }; }

    throw notFound();
  };
}

function matchPath(seg, pattern) {
  const parts = pattern.split('/');
  if (parts.length !== seg.length) return null;
  const params = {};
  for (let i = 0; i < parts.length; i++) {
    if (parts[i].startsWith(':')) params[parts[i].slice(1)] = decodeURIComponent(seg[i]);
    else if (parts[i] !== seg[i]) return null;
  }
  return params;
}

const sleep = ms => new Promise(r => setTimeout(r, ms));
function notFound() { const e = new Error('not found (demo)'); e.status = 404; return e; }

function demoProject() {
  return { id: 'p-demo', slug: 'demo', display_name: 'Demo', default_branch: 'main', repo_path: '/work/demo', workflow_sha: 'wfdemo123456', config_sha: 'cfgdemo12345',
    config: { default_visibility: 'team_summary', claim_mode: 'cooperative', lease_ttl: '1m30s', heartbeat_interval: '20s', offline_grace: '45s', stalled_turn_timeout: '15m0s',
      duplicate_threshold: 0.5, write_conflict_policy: 'block_conflict', read_write_conflict_policy: 'allow_with_warning',
      max_concurrent_attempts: 4, max_per_principal: 2, max_attempts: 4, max_active_sessions: 4,
      required_checks: ['go vet ./...', 'go test ./...'], protected_scopes: ['migration:primary'],
      publish_model_identity: true, publish_harness_identity: true,
      budget: { monthly_usd: 400, downshift_at: 0.75, pause_at: 0.95, member_tokens: 20000000 } } };
}

function statusCounts() {
  const counts = {};
  for (const t of D.tasks) counts[t.status] = (counts[t.status] || 0) + 1;
  return counts;
}

function inventory() {
  return { sessions: 4, available: 2, max_tier: 'T4', max_reasoning_effort: 'xhigh', harnesses: ['claude', 'codex', 'opencode'],
    models: [
      { model: 'claude-opus-5', harness: 'claude', tier: 'T4', max_reasoning_effort: 'xhigh', sessions: 1, available: 1 },
      { model: 'claude-sonnet-5', harness: 'claude', tier: 'T2', max_reasoning_effort: 'high', sessions: 1, available: 1 },
      { model: 'gpt-5-codex', harness: 'codex', tier: 'T2', max_reasoning_effort: 'medium', sessions: 1, available: 1 },
      { model: 'ollama/qwen3:27b', harness: 'opencode', tier: '', max_reasoning_effort: '', sessions: 1, available: 1 },
    ], tiers: { T4: 1, T2: 2 }, reasoning_efforts: { xhigh: 1, high: 1, medium: 2 }, capabilities: ['architecture', 'code_edit', 'tests'],
    runners: [{ name: 'build-box', models: ['claude-sonnet-5', 'ollama/qwen3:27b'] }], gaps: [] };
}

function filterTasks(q) {
  let list = D.tasks;
  if (q.get('open') === 'true') list = list.filter(t => !['done', 'failed', 'cancelled', 'superseded'].includes(t.status));
  const statuses = q.getAll('status');
  if (statuses.length) list = list.filter(t => statuses.includes(t.status));
  return list;
}

function findTask(ref) {
  const t = D.tasks.find(x => x.ref === ref || x.id === ref);
  if (!t) throw notFound();
  return t;
}

function demoCreateTask(body) {
  taskSeq++;
  const t = mkTask(taskSeq, body.title, body.status || 'proposed', 'you', { scopes: (body.scopes || []).map(s => s.resource), labels: body.labels || [], objective: body.objective, risk: body.risk_level, priority: body.priority, criteria: (body.acceptance_criteria || []).map(c => c.text), deps: body.depends_on || [] });
  D.tasks.unshift(t);
  pushEvent('task.created', { task_ref: t.ref, principal: 'you' });
  return t;
}

function demoClaim(ref) {
  const t = findTask(ref);
  t.status = 'claimed'; t.owner = 'you'; t.attempts_count++; t.fencing_epoch++;
  pushEvent('task.claimed', { task_ref: t.ref, principal: 'you' });
  return { task: t, fence: { task_id: t.id, lease_id: 'lease-demo', fencing_epoch: t.fencing_epoch }, lease: { expires_at: iso(Date.now() + 90 * 1000) } };
}

function demoTransition(ref, to) {
  const t = findTask(ref);
  t.status = to || 'ready';
  if (to === 'ready' || to === 'proposed') t.owner = '';
  t.updated_at = iso(Date.now());
  pushEvent('task.' + (to === 'ready' ? 'released' : 'transitioned'), { task_ref: t.ref, status: t.status });
  return t;
}

function demoAssign(ref, body) {
  const t = findTask(ref);
  const target = body.session_id ? D.capSessions.find(s => s.session_id === body.session_id) : D.capSessions.find(s => s.available);
  pushEvent('task.assign', { task_ref: t.ref, principal: target ? target.principal : '' });
  return { assignment: { id: 'as-' + Math.random().toString(36).slice(2), task_ref: t.ref, state: 'offered', requirement: body.require || {} },
    choice: target ? { session_id: target.session_id, principal: target.principal, harness: target.harness, model: target.model, rationale: ['requirement: ' + JSON.stringify(body.require || {}), '1 of 4 live session(s) qualified'] } : null };
}

function demoShare(body) {
  const from = D.budget.members.find(x => x.handle === 'you');
  const to = D.budget.members.find(x => x.handle === body.to);
  if (to) { to.shared_in_tokens += body.tokens; to.remaining_tokens += body.tokens; }
  if (from) { from.shared_out_tokens += body.tokens; from.remaining_tokens -= body.tokens; }
  D.grants.unshift({ id: 'g' + Math.random().toString(36).slice(2), from_handle: 'you', to_handle: body.to, tokens: body.tokens, note: body.note || '', created_at: iso(Date.now()) });
  pushEvent('budget.shared', { from: 'you', to: body.to, tokens: body.tokens });
  return { grant: D.grants[0], from, to };
}

function demoEnqueue(body) {
  const t = { id: 'q-' + Math.random().toString(36).slice(2, 7), principal: 'you', kind: body.kind || 'attempt', task_ref: body.task || '', state: 'queued', position: D.queue.tickets.filter(x => x.state === 'queued').length + 1, requested_at: iso(Date.now()), expires_at: iso(Date.now() + 30 * min) };
  D.queue.tickets.push(t);
  pushEvent('queue.enqueued', { principal: 'you', kind: t.kind, task_ref: t.task_ref });
  return t;
}

function repositionQueue() {
  let pos = 0;
  for (const t of D.queue.tickets) if (t.state === 'queued') t.position = ++pos;
}

function demoUsage(q) {
  const by = (q.get('by') || 'harness').split(',').map(s => s.trim()).filter(Boolean);
  const sinceStr = q.get('since') || '7d';
  const days = sinceStr.endsWith('d') ? parseInt(sinceStr) : sinceStr.endsWith('h') ? Math.max(1, parseInt(sinceStr) / 24) : 7;
  const cutoff = Date.now() - days * day;
  const rows = D.usage.filter(r => new Date(r.period).getTime() >= cutoff);
  const dimOf = { day: r => r.period, hour: r => r.period, harness: r => r.harness, model: r => r.model, effort: r => r.reasoning_effort, principal: r => r.principal, source: r => r.source, session: r => r.external_session_id };
  const acc = new Map();
  const total = zeroRow();
  for (const r of rows) {
    const key = by.map(d => (dimOf[d] || (() => ''))(r)).join('|');
    if (!acc.has(key)) {
      const out = zeroRow();
      for (const d of by) {
        if (d === 'day' || d === 'hour') out.period = r.period;
        if (d === 'harness') out.harness = r.harness;
        if (d === 'model') out.model = r.model;
        if (d === 'effort') out.reasoning_effort = r.reasoning_effort;
        if (d === 'principal') out.principal = r.principal;
        if (d === 'source') out.source = r.source;
        if (d === 'session') out.external_session_id = r.external_session_id;
      }
      acc.set(key, out);
    }
    addRow(acc.get(key), r);
    addRow(total, r);
  }
  const list = [...acc.values()].sort((a, b) => (a.period || '').localeCompare(b.period || '') || b.total_tokens - a.total_tokens);
  return { project: 'demo', since: iso(cutoff), until: iso(Date.now()), by, rows: list, total };
}
function zeroRow() { return { requests: 0, input_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0, output_tokens: 0, reasoning_tokens: 0, total_tokens: 0, cost_usd: 0 }; }
function addRow(a, r) { for (const k of ['requests', 'input_tokens', 'cache_read_tokens', 'cache_write_tokens', 'output_tokens', 'reasoning_tokens', 'total_tokens']) a[k] += r[k]; a.cost_usd = +(a.cost_usd + r.cost_usd).toFixed(2); }

function demoCard(ref) {
  const t = findTask(ref);
  return ['---', `id: ${t.ref}`, 'project: demo', `status: ${t.status}`, `owner: ${t.owner || '—'}`, '---', '', `# ${t.title || '(private)'}`, '',
    '## Objective', '', t.objective || '_none recorded_', '', '## Acceptance criteria', '',
    ...(t.acceptance_criteria.length ? t.acceptance_criteria.map(c => `- ${c.text}`) : ['- _none_']), '',
    '## Privacy note', '', 'This card contains coordination state only. It does not contain the originating chat transcript or hidden model reasoning.'].join('\n');
}

function pushEvent(type, payload) {
  const e = { id: 'e' + Math.random().toString(36).slice(2), type, payload, occurred_at: iso(Date.now()) };
  D.events.unshift(e);
  if (streamHandler) streamHandler(e);
}

// ---------------------------------------------------------------------------
// Fake SSE
// ---------------------------------------------------------------------------

let streamHandler = null;

export function demoStream(onEvent, onState) {
  streamHandler = onEvent;
  onState('live');
  const drum = [
    () => pushEvent('attempt.progress', { task_ref: 'T-1', phase: 'implementing', tokens_in: 1500000 + Math.round(Math.random() * 9000), changed_paths: ['internal/router/router.go'] }),
    () => pushEvent('task.progress', { task_ref: 'T-2', phase: 'testing' }),
    () => { const s = D.sessions[1]; s.last_heartbeat = iso(Date.now()); pushEvent('attempt.progress', { task_ref: 'T-2', phase: 'implementing' }); },
    () => pushEvent('usage.recorded', { harness: 'claude', tokens: 12000 }),
  ];
  let i = 0;
  const timer = setInterval(() => { drum[i++ % drum.length](); }, 8000);
  return () => { clearInterval(timer); streamHandler = null; onState('closed'); };
}

// Live event stream. EventSource cannot set headers, so the token rides in the query string
// — the server accepts that on GET only. Named events do not reach `onmessage`, so every
// type the control plane emits is subscribed explicitly; unknown types are still caught by
// the generic listener when the server sends them unnamed.
export const EVENT_TYPES = [
  'task.created', 'task.claimed', 'task.released', 'task.handoff', 'task.blocked', 'task.unblocked',
  'task.assign', 'task.assigned', 'task.offer_accepted', 'task.offer_declined', 'task.progress',
  'task.done', 'task.failed', 'task.cancelled', 'task.transitioned', 'task.superseded',
  'attempt.created', 'attempt.progress', 'attempt.stalled', 'attempt.evidence', 'attempt.finished',
  'attempt.failed', 'attempt.succeeded', 'attempt.route',
  'lease.expired', 'lease.reclaimed', 'lease.released',
  'budget.downshift', 'budget.exhausted', 'budget.shared',
  'session.registered', 'session.closed', 'session.reaped', 'session.capabilities',
  'conflict.detected', 'conflict.resolved', 'scope.expanded', 'scope.granted', 'scope.blocked',
  'queue.enqueued', 'queue.granted', 'queue.released', 'queue.expired', 'queue.cancelled',
  'swarm.joined', 'swarm.left', 'member.added', 'member.removed', 'usage.recorded',
];

export function connectStream(url, { onEvent, onState }) {
  let es;
  let closed = false;
  let backoff = 1000;

  function open() {
    if (closed) return;
    es = new EventSource(url);
    onState('connecting');
    es.onopen = () => { backoff = 1000; onState('live'); };
    es.onerror = () => {
      onState('reconnecting');
      // EventSource retries on its own for transient errors; a hard close (401, server
      // gone) leaves readyState CLOSED, and then we retry with backoff ourselves.
      if (es.readyState === EventSource.CLOSED && !closed) {
        setTimeout(open, backoff);
        backoff = Math.min(backoff * 2, 30000);
      }
    };
    const handle = ev => {
      try { onEvent(JSON.parse(ev.data)); } catch (_) { /* malformed frame */ }
    };
    es.onmessage = handle;
    for (const type of EVENT_TYPES) es.addEventListener(type, handle);
  }
  open();
  return () => { closed = true; if (es) es.close(); onState('closed'); };
}

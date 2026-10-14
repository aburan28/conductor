#!/usr/bin/env node
// The SPA tears down its router on sign-out and builds a new one on sign-in.
// Exercise that contract without a browser or a live control plane.
import assert from 'node:assert/strict';
import { createRouter } from '../internal/web/static/lib/router.js';

const handlers = new Map();
const add = (kind, fn) => {
  if (!handlers.has(kind)) handlers.set(kind, new Set());
  handlers.get(kind).add(fn);
};
const remove = (kind, fn) => handlers.get(kind)?.delete(fn);
globalThis.window = {
  addEventListener: add,
  removeEventListener: remove,
};
globalThis.document = {
  addEventListener: add,
  removeEventListener: remove,
};
globalThis.location = { pathname: '/' };
globalThis.history = {
  pushState(_state, _title, path) { location.pathname = path; },
  replaceState(_state, _title, path) { location.pathname = path; },
};

const routes = [{ path: '/', name: 'home' }, { path: '/tasks', name: 'tasks' }];
for (let cycle = 0; cycle < 3; cycle++) {
  const seen = [];
  const router = createRouter(routes);
  router.start(route => seen.push(route.name));
  assert.equal(handlers.get('popstate').size, 1);
  assert.equal(handlers.get('click').size, 1);
  assert.equal(seen.length, 1);
  router.navigate('/tasks');
  assert.deepEqual(seen, ['home', 'tasks']);
  router.stop();
  assert.equal(handlers.get('popstate').size, 0);
  assert.equal(handlers.get('click').size, 0);
  for (const fn of handlers.get('popstate')) fn();
  assert.deepEqual(seen, ['home', 'tasks']);
  location.pathname = '/';
}
console.log('router sign-out/re-entry lifecycle: pass');

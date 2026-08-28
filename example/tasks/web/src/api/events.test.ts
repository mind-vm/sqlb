// The change-feed subscriber, checked against the frames the server sends.
//
// The types next door are checked by tsc; this is the other half — what a
// subscriber actually does with `{table, key, op}`. The frames below are the
// ones `rest.Events` writes, asserted in Go by `rest/events_test.go`, so the
// two ends of the stream are tested against the same wire format.
//
// The stream is injected rather than opened: `subscribeChanges` takes an
// `open` for the reason the rest of the client takes a `Transport`, and a fake
// one is what lets this run under `node --test` with no server and no DOM.
//
//   node --test --experimental-strip-types src/api/events.test.ts

import assert from 'node:assert/strict';
import test from 'node:test';

import {
  changeKeys,
  isTableName,
  subscribeChanges,
  taskKeys,
  type EventStream,
  type TableChange,
} from './client.gen.ts';

/** A stream the test drives by hand, standing in for EventSource. */
class FakeStream implements EventStream {
  readonly listeners = new Map<string, ((event: { data: string }) => void)[]>();
  closed = false;

  addEventListener(type: string, listener: (event: { data: string }) => void): void {
    const existing = this.listeners.get(type) ?? [];
    existing.push(listener);
    this.listeners.set(type, existing);
  }

  close(): void {
    this.closed = true;
  }

  /** Delivers one frame, the way the server writes it. */
  send(type: string, data: string): void {
    for (const listener of this.listeners.get(type) ?? []) listener({ data });
  }
}

/** Subscribes against a fake stream and hands back both ends. */
function subscribe(options: Parameters<typeof subscribeChanges>[1] = {}) {
  const stream = new FakeStream();
  const stop = subscribeChanges('/events', { ...options, open: () => stream });
  return { stream, stop };
}

test('a keyed change resolves to the keys that read the row', () => {
  const changes: TableChange[] = [];
  const { stream } = subscribe({ onChange: (event) => changes.push(event) });

  stream.send('change', JSON.stringify({ table: 'tasks', key: 't1', op: 'update' }));

  assert.equal(changes.length, 1);
  const change = changes[0]!;
  assert.equal(change.table, 'tasks');
  assert.equal(change.op, 'update');
  assert.deepEqual(change.keys, [
    taskKeys.lists(),
    taskKeys.infinites(),
    taskKeys.detail('t1'),
    // The declared read's key, which is what Query.Reads was for: `by-list`
    // counts tasks, so a task changing makes its numbers stale and the
    // subscriber refetches it without the server computing anything.
    taskKeys.query('by-list'),
  ]);
});

// The precision this exists for: another row's detail query is not in the set,
// so a change to one task does not refetch every task on the screen.
test('a keyed change leaves another row alone', () => {
  const keys = changeKeys({ table: 'tasks', key: 't1', op: 'update' });
  assert.equal(
    keys.some((key) => JSON.stringify(key) === JSON.stringify(taskKeys.detail('t2'))),
    false,
  );
});

// An event the publisher could not attribute to one row — the shape a
// hand-written publisher on the count hook produces — asks for the table.
test('a keyless change invalidates the table', () => {
  assert.deepEqual(changeKeys({ table: 'tasks', key: '', op: 'delete' }), [taskKeys.all()]);
});

test('a table this client does not serve is dropped rather than guessed at', () => {
  const changes: TableChange[] = [];
  const { stream } = subscribe({ onChange: (event) => changes.push(event) });

  stream.send('change', JSON.stringify({ table: 'audit_log', key: 'a1', op: 'create' }));

  assert.deepEqual(changes, []);
  assert.equal(isTableName('audit_log'), false);
  assert.deepEqual(changeKeys({ table: 'audit_log', key: 'a1', op: 'create' }), []);
});

test('a reset carries its reason, whatever the client displays', () => {
  const resets: string[] = [];
  const { stream } = subscribe({ onReset: (event) => resets.push(event.reason) });

  stream.send('reset', JSON.stringify({ reason: 'the retained history did not reach back that far' }));

  assert.deepEqual(resets, ['the retained history did not reach back that far']);
});

// A frame that does not parse is reported rather than thrown into the stream's
// error handling, where it would look like a disconnection.
test('an unparseable frame reaches onError and does not throw', () => {
  const errors: unknown[] = [];
  const changes: TableChange[] = [];
  const { stream } = subscribe({
    onChange: (event) => changes.push(event),
    onError: (error) => errors.push(error),
  });

  stream.send('change', '{not json');

  assert.equal(errors.length, 1);
  assert.deepEqual(changes, []);
});

test('the returned function closes the stream', () => {
  const { stream, stop } = subscribe({ onChange: () => {} });
  assert.equal(stream.closed, false);
  stop();
  assert.equal(stream.closed, true);
});

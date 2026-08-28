// The half of the client that is worth more than the calls that work: the ones
// that do not compile.
//
// Every `@ts-expect-error` below is an assertion. If the generator ever widens
// a type so that one of these becomes legal, tsc fails on the unused
// suppression — so this file is a test, and it fails in the direction that
// matters (a guard that stops guarding).
//
// Each case is something the server would answer 400 to. The point of
// generating from the schema rather than from the OpenAPI document is that they
// are answered here instead, before the request is sent.

import { listTasks, getComment, byListTasks, type Transport } from './api/client.gen';
import { taskMutations, taskQueries } from './api/queries.gen';
import { useMutation, useQuery } from '@tanstack/react-query';

export function refusals(request: Transport) {
  // @ts-expect-error — "titel" is not a column of tasks.
  void listTasks(request, { where: { titel: 'typo' } });

  // @ts-expect-error — position is sortable and selectable, but not filterable...
  void listTasks(request, { where: { position: 3 } });
  // ...and the capabilities it did declare still work:
  void listTasks(request, { sort: 'position', select: ['position'] });

  // @ts-expect-error — "reviewing" is not one of the status enum's values.
  void listTasks(request, { where: { status: 'reviewing' } });

  // @ts-expect-error — contains is a pattern operator and comment_count is a number.
  void listTasks(request, { where: { comment_count: { contains: '3' } } });

  // @ts-expect-error — list_id is not nullable, so there is no null to test for.
  void listTasks(request, { where: { list_id: { isnull: true } } });

  // @ts-expect-error — description is searchable but not sortable.
  void listTasks(request, { sort: 'description' });

  // labels is an array column. What it accepts is containment...
  void listTasks(request, { where: { labels: { has: 'urgent' } } });
  void listTasks(request, { where: { labels: { hasany: ['urgent', 'blocked'] } } });
  void listTasks(request, { where: { labels: ['a', 'b'] } });

  // @ts-expect-error — ...and `contains` is the text operator, not this one.
  void listTasks(request, { where: { labels: { contains: 'urgent' } } });

  // @ts-expect-error — `has` takes one element, not the array.
  void listTasks(request, { where: { labels: { has: ['urgent'] } } });

  // @ts-expect-error — array ordering is not offered, so there is no gt.
  void listTasks(request, { where: { labels: { gt: ['a'] } } });

  // @ts-expect-error — labels declared Filterable, deliberately not Sortable.
  void listTasks(request, { sort: 'labels' });

  // @ts-expect-error — a scalar column has no containment operator.
  void listTasks(request, { where: { title: { has: 'x' } } });

  // @ts-expect-error — password_hash is hidden: it has no spelling at all here.
  void listTasks(request, { select: ['password_hash'] });

  // @ts-expect-error — comments declare no expandable relation.
  void getComment(request, 'id', { expand: ['task'] });

  // @ts-expect-error — the item endpoint has no ?select, and rejects one.
  void getComment(request, 'id', { select: ['body'] });
}

/**
 * The mutation layer inherits the same refusals, because its variables are the
 * body types rather than a second spelling of them.
 */
export function writeRefusals(request: Transport) {
  const tasks = taskMutations(request);
  const create = useMutation(tasks.create);
  const update = useMutation(tasks.update);
  const complete = useMutation(tasks.complete);

  // @ts-expect-error — workspace_id is owned by a hook, so the create body has
  // no such property and there is nothing for the server to ignore.
  create.mutate({ list_id: 'l', title: 't', description: '', workspace_id: 'w' });

  // @ts-expect-error — comment_count is read-only: it is filterable and
  // sortable, and still has no spelling in a patch body.
  update.mutate({ id: 'a', body: { comment_count: 3 } });

  // @ts-expect-error — an item verb's variables carry the id, not just the body.
  complete.mutate({ note: 'done' });

  // And the calls that do work, so the refusals above are not passing because
  // the whole shape is wrong:
  create.mutate({ list_id: 'l', title: 't', description: '' });
  update.mutate({ id: 'a', body: { title: 'renamed' } });
  complete.mutate({ id: 'a', body: { note: 'done' } });
}

export async function narrowing(request: Transport) {
  const page = await listTasks(request, { select: ['title'] });
  const first = page.items[0];
  if (first === undefined) return;

  first.id; // the primary key is always returned
  first.title;
  // @ts-expect-error — status was not selected, so it is not on the row.
  first.status;

  const expanded = await listTasks(request, { expand: ['list'], per_page: 1 });
  const task = expanded.items[0];
  if (task === undefined) return;
  task.list.name; // present, not optional, because it was expanded

  const plain = await listTasks(request, { per_page: 1 });
  const unexpanded = plain.items[0];
  if (unexpanded === undefined) return;
  // @ts-expect-error — nothing was expanded, so there is no list to read.
  unexpanded.list.name;
}

// A declared read reaches the client the way a declared action does, and its
// parameters are as narrow as a column's (#316). What is asserted here is that
// the read's *result* is the shape the schema declared rather than rows of the
// table: `by-list` answers with counts per list, which is a row of no declared
// table, and a client typed `[]Task` for it would be confidently wrong.
export async function declaredReads(request: Transport) {
  const rollup = await byListTasks(request, { list_id: null });
  const row = rollup[0];
  if (row === undefined) return;

  row.list_id;
  row.open;
  row.done;
  // @ts-expect-error — the answer is a rollup, not a task, so there is no title on it.
  row.title;

  // @ts-expect-error — "listId" is not the wire spelling of the parameter.
  void byListTasks(request, { listId: 'a' });

  // Every parameter is optional, so the argument may be left out entirely.
  void byListTasks(request);
}

export function declaredReadOptions(request: Transport) {
  // The TanStack wrapper, beside list and detail — the read is a member of the
  // resource's own factory rather than a hand-written queryOptions next to it.
  const rollup = useQuery(taskQueries(request).byList({ list_id: 'a' }));
  rollup.data?.[0]?.open;

  // @ts-expect-error — "byLists" is not the read the schema declared.
  void taskQueries(request).byLists({});
}

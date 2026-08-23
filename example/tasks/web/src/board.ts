// What the generated client looks like in use.
//
// Every call here goes through the same URL grammar the Go server parses, and
// every column, operator and sort term is checked by tsc. Run `npm run
// typecheck` to see that; break one of them on purpose to see it fail.

import {
  allowedFor,
  createTask,
  getList,
  getTask,
  listTasks,
  subscribeChanges,
  taskKeys,
  updateTask,
  type Page,
  type Task,
  type TaskRow,
  type Transport,
} from './api/client.gen';
import { ApiError } from './api/http';
import { taskMutations, taskQueries } from './api/queries.gen';
// This file is typechecked, never run — which is what lets it call hooks
// outside a component to show that the generated options are what the hooks
// already take.
import { useMutation, useQuery, useSuspenseQuery, type QueryClient } from '@tanstack/react-query';

/**
 * The board's main query: open, urgent-ish work in one list, newest first.
 *
 * `where` is an object per column, and the operators each column accepts come
 * from its type — `in` on the enum, `isnull` only because assignee_id is
 * nullable, `contains` only because title is text. None of that is expressible
 * in an OpenAPI document, which is the whole argument for generating from the
 * schema rather than from the document.
 */
export function openWork(request: Transport, listId: string, search: string) {
  return listTasks(request, {
    where: {
      list_id: listId, // a bare value is equality
      status: { in: ['todo', 'in_progress'] },
      priority: { in: ['high', 'urgent'] },
      due_at: { lte: new Date(), notnull: true },
      ...(search ? { title: { contains: search } } : {}),
    },
    sort: ['-priority', 'position'],
    per_page: 50,
  });
}

/**
 * A projection narrows the response type, so reading a column the request did
 * not ask for does not compile.
 *
 * The primary key comes back whether or not it was named — the server adds it,
 * and the type says so.
 */
export async function taskTitles(request: Transport, listId: string) {
  const page = await listTasks(request, {
    where: { list_id: listId },
    select: ['title', 'status'],
    sort: 'position',
  });
  return page.items.map((task) => ({ id: task.id, label: `${task.title} (${task.status})` }));
}

/**
 * An expansion widens it the other way: `expand` makes the relation a property
 * that is present rather than optional.
 */
export async function taskWithList(request: Transport, id: string) {
  const task = await getTask(request, id, { expand: ['list'] });
  return `${task.title} — ${task.list.name}`;
}

/**
 * The other direction of an expansion, and the reason it is not a bare array.
 *
 * A list's tasks are capped at twenty by the schema, so the envelope carries
 * `has_more` and the screen can say "and 43 more" instead of quietly showing a
 * fifth of the work. Typing it as `Task[]` would lose that here, one layer
 * further out than the server was careful about it.
 */
export async function listOverview(request: Transport, id: string) {
  const list = await getList(request, id, { expand: ['tasks'] });
  return {
    name: list.name,
    shown: list.tasks.items.length,
    truncated: list.tasks.has_more,
  };
}

/**
 * Walking a whole result set by cursor.
 *
 * `next_cursor` names the position of the last row rather than counting to it,
 * so the hundredth page costs what the first costs, and a task created while
 * this loop runs cannot make it read a row twice.
 */
export async function everyOverdueTask(request: Transport): Promise<TaskRow[]> {
  const out: TaskRow[] = [];
  let cursor: string | undefined;
  do {
    const page: Page<TaskRow> = await listTasks(request, {
      where: { due_at: { lt: new Date() }, status: { ne: 'done' } },
      sort: 'due_at',
      per_page: 100,
      cursor,
    });
    out.push(...page.items);
    cursor = page.next_cursor;
  } while (cursor !== undefined);
  return out;
}

/**
 * The same walk as a TanStack infinite query, which is where the cursor stops
 * being something the application handles at all: the factory holds
 * `getNextPageParam`, and `page` and `cursor` are absent from its parameters
 * because they are two answers to where a page starts.
 */
export function overdueFeed(request: Transport) {
  return taskQueries(request).infinite({
    where: { due_at: { lt: new Date() }, status: { ne: 'done' } },
    sort: 'due_at',
    per_page: 100,
  });
}

/**
 * Options are spread and overridden rather than copied out and edited, which
 * is the property a hook would not have.
 */
export function boardOptions(request: Transport, listId: string) {
  return {
    ...taskQueries(request).list({ where: { list_id: listId }, sort: 'position' }),
    staleTime: 30_000,
  };
}

/**
 * The claim that layer 4 is consumable as-is, stated as something tsc checks.
 *
 * A `queryOptions` object is what `useQuery` and `useSuspenseQuery` both take,
 * so there is no hook to generate — and `useSuspenseQuery` narrows `data` to
 * non-undefined without the factory knowing which of the two a caller picked.
 */
export function boardHooks(request: Transport, listId: string) {
  const options = taskQueries(request).list({ where: { list_id: listId }, sort: 'position' });
  const polled = useQuery({ ...options, refetchInterval: 5_000 });
  const suspended = useSuspenseQuery(options);
  return { polled: polled.data?.items, suspended: suspended.data.items };
}

/**
 * The write half, and the seam the mutation layer exists to leave open.
 *
 * `taskMutations` carries the `mutationFn` and stops. What completing a task
 * should invalidate — this list, that detail, a burndown view that is not a
 * table at all — is the application's to say, so it is spread in here rather
 * than edited into a generated file.
 */
export function completeOptions(request: Transport, invalidate: (key: readonly unknown[]) => void) {
  return {
    ...taskMutations(request).complete,
    onSuccess: (task: Task) => {
      invalidate(taskKeys.lists());
      invalidate(taskKeys.detail(task.id));
    },
  };
}

export function useCompleteTask(request: Transport, invalidate: (key: readonly unknown[]) => void) {
  const mutation = useMutation(completeOptions(request, invalidate));
  // The variables are typed from the verb's declaration — `note` is the one
  // property `complete` accepts, and it is nullable — so a body the server
  // would reject does not compile.
  return () => mutation.mutate({ id: 'a-task-id', body: { note: 'shipped' } });
}

/** A write, and the keys it invalidates. */
export async function completeTask(
  request: Transport,
  invalidate: (key: readonly unknown[]) => void,
  id: string,
) {
  const task = await updateTask(request, id, { status: 'done' });
  invalidate(taskKeys.lists());
  invalidate(taskKeys.detail(id));
  return task;
}

export async function addTask(request: Transport, listId: string, title: string) {
  // workspace_id and author_id are absent from the body type: the hooks own
  // them, so there is no field to send and nothing for the server to ignore.
  return createTask(request, { list_id: listId, title, description: '' });
}

/**
 * The change feed, wired to a cache.
 *
 * `subscribeChanges` is generated: it opens the stream, narrows the event's
 * table to one this client serves, and resolves `{table, key}` into the keys
 * that read it. What is left here is the one thing a schema cannot say — which
 * cache to invalidate, and what a reset means for what is on screen.
 *
 * This used to be a hand-written listener with a `keysByTable[...]` lookup, a
 * cast to make the index compile, and no answer for a keyless event. That is
 * the drift ADR-0028 was written from, one layer up: an invalidation list that
 * disagrees with the one the mutations next door use.
 */
export function liveUpdates(queryClient: QueryClient, baseUrl: string) {
  return subscribeChanges(`${baseUrl}/events`, {
    onChange: ({ keys }) => {
      for (const queryKey of keys) void queryClient.invalidateQueries({ queryKey });
    },
    // The stream could not be resumed, so nothing on display can be trusted.
    onReset: () => void queryClient.invalidateQueries(),
  });
}

/**
 * The same subscription, authenticated.
 *
 * `EventSource` cannot carry an Authorization header — the API has no place to
 * put one — so a deployment that authenticates with a bearer token passes a
 * polyfill that can, and one that authenticates with a cookie passes the
 * platform's own with credentials switched on. That is the `open` seam, and it
 * is the same argument the `Transport` seam makes for every other request.
 */
export function liveUpdatesWithSession(queryClient: QueryClient, baseUrl: string) {
  return subscribeChanges(`${baseUrl}/events`, {
    open: (url) => new EventSource(url, { withCredentials: true }),
    onChange: ({ keys }) => {
      for (const queryKey of keys) void queryClient.invalidateQueries({ queryKey });
    },
  });
}

/**
 * A rejection says what would have been accepted, and that survives to here.
 *
 * `?sort=body` comes back as a 400 whose detail carries the sortable columns,
 * so a filter UI can render the alternatives instead of a dead end.
 */
export function sortFallback(error: unknown): string[] {
  if (error instanceof ApiError && error.problem !== undefined) {
    return allowedFor(error.problem, 'query.sort');
  }
  return [];
}

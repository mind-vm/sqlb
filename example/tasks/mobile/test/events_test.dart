// The change feed: the frame parser, the two event types, and the narrowing
// from a table name on the wire to one this client serves.
//
// The frames below are the ones `rest.Events` writes — `rest/events_test.go`
// asserts the same shapes from the other end — so the two halves of the stream
// are tested against one wire format. The web client's
// `web/src/api/events.test.ts` is the third: same events, same rules, a
// different cache to invalidate.
//
//   dart test

import 'dart:convert';

import 'package:tasks_mobile/api/client.gen.dart';
import 'package:test/test.dart';

/// One chunk of body text, as the network happens to have delivered it.
Stream<String> chunks(List<String> parts) => Stream.fromIterable(parts);

/// The frames a server sends for one change.
String frame(String event, String data, {int? id}) =>
    '${id == null ? '' : 'id: $id\n'}event: $event\ndata: $data\n\n';

Future<List<FeedEvent>> read(ChangeFeed feed, List<String> parts) =>
    feed.read(chunks(parts), parseJson: jsonDecode).toList();

void main() {
  test('a change carries the address of the row and nothing else', () async {
    final events = await read(ChangeFeed(), [
      frame('change', '{"table":"tasks","key":"t1","op":"update"}', id: 41),
    ]);

    expect(events, hasLength(1));
    final event = events.single as ChangeEvent;
    expect(event.table, 'tasks');
    expect(event.key, 't1');
    expect(event.op, ChangeOp.update);
    expect(event.id, '41');
  });

  // The parser's real risk: the chunk boundaries are the network's and mean
  // nothing, so a frame split across two of them has to survive.
  test('a frame split across chunks is still one frame', () async {
    final events = await read(ChangeFeed(), [
      'event: cha',
      'nge\ndata: {"table":"tas',
      'ks","key":"t1","op":"create"}\n',
      '\n',
    ]);

    expect(events, hasLength(1));
    expect((events.single as ChangeEvent).op, ChangeOp.create);
  });

  test('a heartbeat comment is not an event', () async {
    final events = await read(ChangeFeed(), [
      ': ok\n\n',
      ': ok\n\n',
      frame('change', '{"table":"lists","key":"l1","op":"delete"}'),
    ]);

    expect(events, hasLength(1));
    expect((events.single as ChangeEvent).table, 'lists');
  });

  // The position is what a reconnection sends back, and the format says an id
  // persists until a later frame replaces it — so a frame that carries none is
  // still at the position the last one set.
  test(
    'the position is remembered, including across a frame without one',
    () async {
      final feed = ChangeFeed();
      await read(feed, [
        frame('change', '{"table":"tasks","key":"t1","op":"update"}', id: 41),
        frame('change', '{"table":"tasks","key":"t2","op":"update"}'),
      ]);

      expect(feed.lastEventId, '41');
    },
  );

  test('a resumed feed starts from the position it was given', () {
    expect(ChangeFeed(lastEventId: '4210').lastEventId, '4210');
  });

  test('a reset carries its reason', () async {
    final events = await read(ChangeFeed(), [
      frame('reset', '{"reason":"the history did not reach back that far"}'),
    ]);

    expect(events.single, isA<ResetEvent>());
    expect(
      (events.single as ResetEvent).reason,
      'the history did not reach back that far',
    );
  });

  // An event type this client has no case for is a newer server, not a
  // failure: the connection is still good and the events it does know still
  // arrive.
  test('an unknown event type is skipped rather than thrown', () async {
    final events = await read(ChangeFeed(), [
      frame('rearranged', '{"table":"tasks"}'),
      frame('change', '{"table":"tasks","key":"t1","op":"create"}'),
    ]);

    expect(events, hasLength(1));
  });

  // A dropped connection reconnects and converges; a dropped event leaves a
  // client wrong forever. So a frame that cannot be decoded ends the stream —
  // and the position has already moved past it, so the reconnection does not
  // meet it again.
  test(
    'a frame that is not a JSON object ends the stream, past itself',
    () async {
      final feed = ChangeFeed();

      await expectLater(
        feed
            .read(
              chunks([frame('change', '"not an object"', id: 7)]),
              parseJson: jsonDecode,
            )
            .toList(),
        throwsA(isA<FormatException>()),
      );
      expect(feed.lastEventId, '7');
    },
  );

  test('a table this client serves narrows to a member', () {
    const event = ChangeEvent(table: 'tasks', key: 't1', op: ChangeOp.update);
    final change = TableChange.from(event)!;

    expect(change.table, TableName.tasks);
    expect(change.key, 't1');
    expect(change.isCollection, isFalse);
  });

  test('a keyless change addresses the collection', () {
    const event = ChangeEvent(table: 'tasks', key: '', op: ChangeOp.delete);

    expect(TableChange.from(event)!.isCollection, isTrue);
  });

  // A client generated from one module of a schema receives the other modules'
  // events too, and has nothing that displays them.
  test('a table this client does not serve narrows to null', () {
    const event = ChangeEvent(
      table: 'audit_log',
      key: 'a1',
      op: ChangeOp.create,
    );

    expect(TableChange.from(event), isNull);
    expect(TableName.byWire('audit_log'), isNull);
  });

  // The operation set can grow on the server. Reporting that would cost the
  // invalidation, and a subscriber refetches on the table and the key anyway.
  test(
    'an operation this client has no member for is a null, not a throw',
    () async {
      final events = await read(ChangeFeed(), [
        frame('change', '{"table":"tasks","key":"t1","op":"restored"}'),
      ]);

      expect((events.single as ChangeEvent).op, isNull);
      expect(TableChange.from(events.single as ChangeEvent)!.key, 't1');
    },
  );
}

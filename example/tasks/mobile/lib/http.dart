/// The half of the client a schema cannot generate: the base URL, the bearer
/// token, what a non-2xx body means, and the login call that is not a table.
///
/// It is written against `dart:io` rather than Dio so that this package can be
/// checked with the Dart SDK alone. A Flutter app substitutes the Dio version
/// in [TasksClient.transport]'s place and changes nothing else — that is the
/// point of the seam. `README.md` has it.
library;

import 'dart:convert';
import 'dart:io';

import 'api/client.gen.dart';

/// A request the server refused, with the problem document it refused with.
///
/// [Problem.allowedFor] is the reason this carries the document rather than a
/// message string: a rejection of `?sort=titel` names the sortable columns, and
/// flattening it to text is where that stops reaching the caller.
class ApiException implements Exception {
  /// Wraps a rejection.
  ApiException(this.status, this.problem, this.body);

  /// The HTTP status.
  final int status;

  /// The decoded problem document, when the body was one.
  final Problem? problem;

  /// The raw body, for the rejections that are not problem documents.
  final Object? body;

  /// Whether the caller's credentials were rejected, which is the one status a
  /// UI almost always handles on its own.
  bool get isUnauthorized => status == 401;

  @override
  String toString() {
    final detail = problem?.detail ?? problem?.title ?? '$body';
    return 'ApiException($status): $detail';
  }
}

/// Everything the generated functions need and cannot derive.
class TasksClient {
  /// Builds a client against [baseUrl], e.g. `http://localhost:8080`.
  TasksClient(this.baseUrl, {HttpClient? httpClient})
    : _http = httpClient ?? HttpClient();

  /// The API root, without a trailing slash.
  final String baseUrl;

  final HttpClient _http;

  /// The bearer token, set by [login] and read on every request after it.
  ///
  /// A real app holds this in secure storage and refreshes it; the shape of
  /// that is the application's, which is why none of it is generated.
  String? token;

  /// The function every generated call takes.
  ///
  /// Passing it around rather than wrapping the generated functions in methods
  /// is deliberate: roughly a quarter of a real API is schema CRUD, and a
  /// client object would have to own the other three quarters too.
  Transport get transport => _send;

  Future<Object?> _send(ApiRequest request) async {
    final query = request.query;
    final url = Uri.parse(
      '$baseUrl${request.path}${query == null || query.isEmpty ? '' : '?$query'}',
    );

    final http = await _http.openUrl(request.method, url);
    http.headers.set(HttpHeaders.acceptHeader, 'application/json');
    if (token != null) {
      http.headers.set(HttpHeaders.authorizationHeader, 'Bearer $token');
    }
    if (request.body != null) {
      http.headers.contentType = ContentType.json;
      http.write(jsonEncode(request.body));
    }

    final response = await http.close();
    final text = await response.transform(utf8.decoder).join();
    final decoded = text.isEmpty ? null : jsonDecode(text);

    if (response.statusCode >= 400) {
      throw ApiException(
        response.statusCode,
        Problem.tryParse(decoded),
        decoded,
      );
    }
    return decoded;
  }

  /// `POST /auth/login`, which is not a table and never will be.
  ///
  /// It sits beside the generated functions rather than inside a namespace they
  /// would have to share, which is the composition the generator is shaped for.
  Future<void> login(String email, String password) async {
    final body = await _send(
      ApiRequest(
        method: 'POST',
        path: '/auth/login',
        body: {'email': email, 'password': password},
      ),
    );
    token = (body! as Map<String, dynamic>)['access_token'] as String?;
  }

  /// `GET /events`, the change feed.
  ///
  /// The connection is here rather than in the generated client for the reason
  /// [transport] is: the base URL, the token and the retry policy are the
  /// application's. What the generated half brings is everything after the
  /// bytes — the frame parsing, the two event types, and the position to
  /// resume from.
  ///
  /// Reconnecting is the caller's too. When this stream ends, open it again
  /// with the same [ChangeFeed]: its [ChangeFeed.lastEventId] goes out as the
  /// header the server replays from, so a brief disconnection costs nothing
  /// and a long one answers with a [ResetEvent] instead of silence.
  ///
  /// Dart has no EventSource, which turns out to be the easier half: a header
  /// is just a header here, where a browser subscriber has to reach for a
  /// polyfill to send one.
  Stream<FeedEvent> changes(ChangeFeed feed) async* {
    final http = await _http.openUrl('GET', Uri.parse('$baseUrl/events'));
    http.headers.set(HttpHeaders.acceptHeader, 'text/event-stream');
    if (token != null) {
      http.headers.set(HttpHeaders.authorizationHeader, 'Bearer $token');
    }
    final resume = feed.lastEventId;
    if (resume != null) http.headers.set('Last-Event-ID', resume);

    final response = await http.close();
    if (response.statusCode >= 400) {
      final text = await response.transform(utf8.decoder).join();
      final decoded = text.isEmpty ? null : jsonDecode(text);
      throw ApiException(
        response.statusCode,
        Problem.tryParse(decoded),
        decoded,
      );
    }

    yield* feed.read(response.transform(utf8.decoder), parseJson: jsonDecode);
  }

  /// Releases the underlying connections.
  void close() => _http.close();
}

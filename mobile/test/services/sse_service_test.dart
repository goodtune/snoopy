import 'dart:async';
import 'dart:convert';
import 'dart:typed_data';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:snoopy/models/cached_image.dart';
import 'package:snoopy/models/snoopy_service.dart';
import 'package:snoopy/services/sse_service.dart';

/// An HTTP client that serves an SSE stream the test writes to, and image
/// bytes for any other GET. No network.
class FakeSseClient extends http.BaseClient {
  final List<http.BaseRequest> sseRequests = [];
  final List<Uri> imageRequests = [];
  final List<StreamController<List<int>>> streams = [];
  bool closed = false;

  StreamController<List<int>> get current => streams.last;

  void sendEvent(String raw) => current.add(utf8.encode(raw));

  @override
  Future<http.StreamedResponse> send(http.BaseRequest request) async {
    if (closed) throw http.ClientException('client closed', request.url);
    if (request.headers['Accept'] == 'text/event-stream') {
      sseRequests.add(request);
      final controller = StreamController<List<int>>();
      streams.add(controller);
      return http.StreamedResponse(
        controller.stream,
        200,
        headers: {'content-type': 'text/event-stream'},
        request: request,
      );
    }
    imageRequests.add(request.url);
    return http.StreamedResponse(
      Stream.value(utf8.encode('jpeg:${request.url.path}')),
      200,
      request: request,
    );
  }

  @override
  void close() {
    closed = true;
    // Like IOClient.close(), abort whatever is still open.
    for (final c in streams) {
      if (!c.isClosed) unawaited(c.close());
    }
  }
}

Future<void> waitFor(bool Function() condition, {int millis = 3000}) async {
  final deadline = DateTime.now().add(Duration(milliseconds: millis));
  while (!condition()) {
    if (DateTime.now().isAfter(deadline)) {
      fail('condition not met within ${millis}ms');
    }
    await Future<void>.delayed(const Duration(milliseconds: 10));
  }
}

void main() {
  const service = SnoopyService(
    name: 'office-pc',
    hostname: '192.168.1.20',
    port: 8900,
    txt: {'sse': '/custom/sse'},
  );

  late FakeSseClient client;
  late SseService sse;

  setUp(() {
    client = FakeSseClient();
    sse = SseService(service, clientFactory: () => client);
  });

  tearDown(() => sse.dispose());

  test('connects to the advertised SSE path', () async {
    await sse.connect();
    await waitFor(() => client.sseRequests.isNotEmpty);

    expect(client.sseRequests.single.method, 'GET');
    expect(
      client.sseRequests.single.url,
      Uri.parse('http://192.168.1.20:8900/custom/sse'),
    );
  });

  test('downloads each announced image in order', () async {
    final images = <CachedImage>[];
    final sub = sse.imageStream.listen(images.add);
    addTearDown(sub.cancel);

    await sse.connect();
    await waitFor(() => client.streams.isNotEmpty);
    await waitFor(() => client.current.hasListener);

    client.sendEvent('data: /images/a.jpg\n\n');
    client.sendEvent('data: /images/b.jpg\n\n');
    await waitFor(() => images.length == 2);

    expect(images.map((i) => i.imageId), ['/images/a.jpg', '/images/b.jpg']);
    expect(images.first.serviceName, 'office-pc');
    expect(
      images.first.imageData,
      Uint8List.fromList(utf8.encode('jpeg:/images/a.jpg')),
    );
    expect(
      client.imageRequests.first,
      Uri.parse('http://192.168.1.20:8900/images/a.jpg'),
    );
  });

  test('ignores empty paths, comments and other event types', () async {
    final images = <CachedImage>[];
    final sub = sse.imageStream.listen(images.add);
    addTearDown(sub.cancel);

    await sse.connect();
    await waitFor(() => client.streams.isNotEmpty);
    await waitFor(() => client.current.hasListener);

    client
      ..sendEvent(': keep-alive\n\n')
      ..sendEvent('data: /images/\n\n') // server cache still empty
      ..sendEvent('data:\n\n')
      ..sendEvent('event: other\ndata: /images/x.jpg\n\n')
      // Split across chunks, as TCP may deliver it.
      ..sendEvent('data: /images/')
      ..sendEvent('c.jpg\n')
      ..sendEvent('\n');
    await waitFor(() => images.isNotEmpty);
    await Future<void>.delayed(const Duration(milliseconds: 50));

    expect(images.map((i) => i.imageId), ['/images/c.jpg']);
    expect(client.imageRequests, hasLength(1));
  });

  test('reconnects after the stream drops', () async {
    await sse.connect();
    await waitFor(() => client.streams.isNotEmpty);
    await waitFor(() => client.current.hasListener);

    await client.current.close(); // server went away
    // First retry is after 1s plus up to 25% jitter.
    await waitFor(() => client.sseRequests.length == 2);

    expect(sse.isConnected, isTrue);
    expect(client.closed, isFalse);
  });

  test('dispose releases the connection and stops reconnecting', () async {
    await sse.connect();
    await waitFor(() => client.streams.isNotEmpty);
    final stream = client.current;
    await waitFor(() => stream.hasListener);

    await sse.dispose();

    expect(client.closed, isTrue);
    expect(stream.hasListener, isFalse);
    expect(sse.isConnected, isFalse);

    // No reconnect attempt after the first backoff would have fired.
    await Future<void>.delayed(const Duration(milliseconds: 1500));
    expect(client.sseRequests, hasLength(1));
  });

  test('dispose during a pending reconnect cancels it', () async {
    await sse.connect();
    await waitFor(() => client.streams.isNotEmpty);
    await waitFor(() => client.current.hasListener);

    await client.current.close(); // reconnect now scheduled in ~1s
    await Future<void>.delayed(const Duration(milliseconds: 100));
    await sse.dispose();

    await Future<void>.delayed(const Duration(milliseconds: 1500));
    expect(client.sseRequests, hasLength(1));
    expect(client.closed, isTrue);
  });

  test('connect again replaces the previous connection', () async {
    final clients = <FakeSseClient>[];
    final feed = SseService(
      service,
      clientFactory: () {
        final c = FakeSseClient();
        clients.add(c);
        return c;
      },
    );
    addTearDown(feed.dispose);

    await feed.connect();
    await waitFor(() => clients.first.sseRequests.isNotEmpty);
    await feed.connect();
    await waitFor(() => clients.length == 2);
    await waitFor(() => clients.last.sseRequests.isNotEmpty);

    expect(clients.first.closed, isTrue);
    expect(clients.last.closed, isFalse);
  });
}

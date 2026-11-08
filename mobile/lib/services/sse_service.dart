import 'dart:async';

import 'package:eventflux/eventflux.dart';
import 'package:flutter/foundation.dart';
import 'package:http/http.dart' as http;

import '../models/cached_image.dart';
import '../models/snoopy_service.dart';
import 'image_feed.dart';

/// Subscribes to a Snoopy server's SSE endpoint (`/sse/image`, or the path
/// advertised in the `sse` TXT record) and downloads each announced image.
///
/// The server sends unnamed events whose data is the server-relative image
/// path, e.g. `data: /images/<uuid>.jpg`.
///
/// The SSE client is [EventFlux], which reconnects with exponential backoff
/// (1s doubling to 30s, with jitter, unlimited attempts) when the stream drops
/// or the server is unreachable. Each [connect] owns a fresh [http.Client]
/// that carries both the event stream and the image downloads; [disconnect]
/// and [dispose] close it, which aborts the open stream, any in-flight image
/// requests and any pending reconnect.
class SseService implements ImageFeed {
  SseService(this.service, {http.Client Function()? clientFactory})
    : _clientFactory = clientFactory ?? http.Client.new;

  final SnoopyService service;
  final http.Client Function() _clientFactory;
  final StreamController<CachedImage> _imageController =
      StreamController<CachedImage>.broadcast();

  EventFlux? _eventFlux;
  http.Client? _client;
  StreamSubscription<EventFluxData>? _eventSubscription;

  /// Reconnect policy for the event stream.
  @visibleForTesting
  static ReconnectConfig reconnectConfig() => ReconnectConfig(
    mode: ReconnectMode.exponential,
    interval: const Duration(seconds: 1),
    maxBackoff: const Duration(seconds: 30),
    maxAttempts: -1,
  );

  @override
  Stream<CachedImage> get imageStream => _imageController.stream;

  /// Whether a connection (or reconnect loop) is currently active.
  @visibleForTesting
  bool get isConnected => _eventFlux != null;

  @override
  Future<void> connect() async {
    await disconnect();
    if (_imageController.isClosed) return;

    final client = _clientFactory();
    // A fresh instance per connection, so a reconnect timer left over from an
    // earlier connection can never revive it.
    final eventFlux = EventFlux.spawn();
    _client = client;
    _eventFlux = eventFlux;

    eventFlux.connect(
      EventFluxConnectionType.get,
      service.sseUrl,
      header: const {
        'Accept': 'text/event-stream',
        'Cache-Control': 'no-cache',
      },
      httpClient: _ClientAdapter(client),
      autoReconnect: true,
      reconnectConfig: reconnectConfig(),
      tag: 'snoopy-sse',
      onSuccessCallback: (response) {
        // Called once per (re)connection with that connection's stream.
        unawaited(_eventSubscription?.cancel());
        _eventSubscription = null;
        final stream = response?.stream;
        if (stream == null || !identical(_eventFlux, eventFlux)) return;
        _eventSubscription = stream.listen(
          (event) => unawaited(_onEvent(event, client)),
        );
      },
      onError: (error) {
        if (identical(_eventFlux, eventFlux)) {
          debugPrint('SSE error from ${service.sseUrl}: $error');
        }
      },
    );
  }

  Future<void> _onEvent(EventFluxData event, http.Client client) async {
    if (event.event.isNotEmpty && event.event != 'message') return;
    final imagePath = event.data.trim();
    // The server sends `/images/` with no file name while its cache is empty.
    if (imagePath.isEmpty || imagePath.endsWith('/')) return;

    try {
      final response = await client.get(service.resolvePath(imagePath));
      if (!identical(_client, client) || _imageController.isClosed) return;
      if (response.statusCode != 200) {
        debugPrint('Fetching $imagePath failed: HTTP ${response.statusCode}');
        return;
      }
      _imageController.add(
        CachedImage(
          serviceName: service.name,
          imageId: imagePath,
          timestamp: DateTime.now(),
          imageData: response.bodyBytes,
        ),
      );
    } on Exception catch (e) {
      // Errors after disconnect come from aborting the request; ignore them.
      if (identical(_client, client)) {
        debugPrint('Error fetching image $imagePath: $e');
      }
    }
  }

  /// Stops the event stream and any reconnect attempts, and releases the
  /// HTTP connection.
  Future<void> disconnect() async {
    final eventFlux = _eventFlux;
    final client = _client;
    final subscription = _eventSubscription;
    _eventFlux = null;
    _client = null;
    _eventSubscription = null;

    await subscription?.cancel();
    await eventFlux?.disconnect();
    // EventFlux does not close a client it was handed; closing it aborts the
    // stream and in-flight requests even if a connection attempt is pending.
    client?.close();
  }

  @override
  Future<void> dispose() async {
    await disconnect();
    await _imageController.close();
  }
}

/// Routes EventFlux's requests through a client this service owns and closes.
class _ClientAdapter implements HttpClientAdapter {
  _ClientAdapter(this._client);

  final http.Client _client;

  @override
  Future<http.StreamedResponse> send(http.BaseRequest request) =>
      _client.send(request);
}

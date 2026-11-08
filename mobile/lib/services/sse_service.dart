import 'dart:async';

import 'package:flutter/foundation.dart';
import 'package:flutter_client_sse/constants/sse_request_type_enum.dart';
import 'package:flutter_client_sse/flutter_client_sse.dart';
import 'package:http/http.dart' as http;

import '../models/cached_image.dart';
import '../models/snoopy_service.dart';
import 'image_feed.dart';

/// Subscribes to a Snoopy server's `/sse/image` endpoint and downloads each
/// announced image.
class SseService implements ImageFeed {
  final SnoopyService service;
  StreamSubscription<SSEModel>? _sseSubscription;
  final StreamController<CachedImage> _imageController =
      StreamController<CachedImage>.broadcast();

  SseService(this.service);

  @override
  Stream<CachedImage> get imageStream => _imageController.stream;

  @override
  Future<void> connect() async {
    await disconnect();
    try {
      _sseSubscription =
          SSEClient.subscribeToSSE(
            method: SSERequestType.GET,
            url: service.sseUrl,
            header: {
              'Accept': 'text/event-stream',
              'Cache-Control': 'no-cache',
            },
          ).listen(
            (event) => unawaited(_onEvent(event)),
            onError: (Object error) => debugPrint('SSE error: $error'),
          );
    } on Exception catch (e) {
      debugPrint('Error connecting to SSE: $e');
    }
  }

  Future<void> _onEvent(SSEModel event) async {
    // The event data is the server-relative image path, e.g. /images/uuid.jpg
    final imagePath = event.data?.trim() ?? '';
    if (imagePath.isEmpty) return;

    try {
      final response = await http.get(service.resolvePath(imagePath));
      if (response.statusCode != 200) {
        debugPrint('Fetching $imagePath failed: HTTP ${response.statusCode}');
        return;
      }
      if (_imageController.isClosed) return;
      _imageController.add(
        CachedImage(
          serviceName: service.name,
          imageId: imagePath,
          timestamp: DateTime.now(),
          imageData: response.bodyBytes,
        ),
      );
    } on Exception catch (e) {
      debugPrint('Error fetching image $imagePath: $e');
    }
  }

  Future<void> disconnect() async {
    await _sseSubscription?.cancel();
    _sseSubscription = null;
  }

  @override
  Future<void> dispose() async {
    await disconnect();
    await _imageController.close();
  }
}

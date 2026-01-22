import 'dart:async';
import 'dart:typed_data';
import 'package:flutter_client_sse/flutter_client_sse.dart';
import 'package:http/http.dart' as http;
import '../models/snoopy_service.dart';
import '../models/cached_image.dart';

class SseService {
  final SnoopyService service;
  StreamSubscription? _sseSubscription;
  final StreamController<CachedImage> _imageController =
      StreamController<CachedImage>.broadcast();

  Stream<CachedImage> get imageStream => _imageController.stream;

  SseService(this.service);

  Future<void> connect() async {
    try {
      _sseSubscription = SSEClient.subscribeToSSE(
        method: SSERequestType.GET,
        url: service.sseUrl,
        header: {
          'Accept': 'text/event-stream',
          'Cache-Control': 'no-cache',
        },
      ).listen(
        (event) async {
          if (event.data != null && event.data!.isNotEmpty) {
            // The SSE event data should contain the image ID
            final imageId = event.data!.trim();

            // Fetch the actual image
            try {
              final imageUrl = service.imageUrl(imageId);
              final response = await http.get(Uri.parse(imageUrl));

              if (response.statusCode == 200) {
                final cachedImage = CachedImage(
                  serviceName: service.name,
                  imageId: imageId,
                  timestamp: DateTime.now(),
                  imageData: response.bodyBytes,
                );

                _imageController.add(cachedImage);
              }
            } catch (e) {
              print('Error fetching image $imageId: $e');
            }
          }
        },
        onError: (error) {
          print('SSE Error: $error');
        },
      );
    } catch (e) {
      print('Error connecting to SSE: $e');
    }
  }

  Future<void> disconnect() async {
    await _sseSubscription?.cancel();
    _sseSubscription = null;
  }

  void dispose() {
    disconnect();
    _imageController.close();
  }
}

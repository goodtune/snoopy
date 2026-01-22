import 'dart:typed_data';

class CachedImage {
  final String serviceName;
  final String imageId;
  final DateTime timestamp;
  final Uint8List imageData;

  CachedImage({
    required this.serviceName,
    required this.imageId,
    required this.timestamp,
    required this.imageData,
  });

  String get fileName {
    // Extract just the UUID from the imageId path (e.g., "/images/uuid.jpg" -> "uuid")
    final cleanId = imageId
        .replaceAll('/images/', '')
        .replaceAll('.jpg', '')
        .replaceAll('/', '_');
    return '${serviceName}_${timestamp.millisecondsSinceEpoch}_$cleanId.jpg';
  }

  @override
  String toString() => 'CachedImage($serviceName, $imageId, $timestamp)';
}

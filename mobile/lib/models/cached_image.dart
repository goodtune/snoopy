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

  String get fileName => '${serviceName}_${timestamp.millisecondsSinceEpoch}_$imageId.jpg';

  @override
  String toString() => 'CachedImage($serviceName, $imageId, $timestamp)';
}

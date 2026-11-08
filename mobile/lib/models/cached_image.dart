import 'dart:typed_data';

/// A screen capture received from a Snoopy server and cached on the device.
class CachedImage {
  final String serviceName;
  final String imageId;
  final DateTime timestamp;
  final Uint8List imageData;

  /// The on-disk file name this image was loaded from, if any. Used as-is
  /// for [fileName] so that files written by older versions can still be
  /// located and deleted.
  final String? storedFileName;

  const CachedImage({
    required this.serviceName,
    required this.imageId,
    required this.timestamp,
    required this.imageData,
    this.storedFileName,
  });

  static const String extension = '.jpg';

  /// File name without extension: `<service>_<millis>_<id>`.
  ///
  /// The service name is percent-encoded (including `_`) so that it can
  /// contain any character and still be parsed back by [fromFileName].
  String get baseName {
    // Extract just the UUID from the imageId path
    // (e.g. "/images/uuid.jpg" -> "uuid").
    final cleanId = imageId
        .replaceAll('/images/', '')
        .replaceAll(extension, '')
        .replaceAll('/', '_');
    final encodedService = Uri.encodeComponent(serviceName)
        .replaceAll('_', '%5F');
    return '${encodedService}_${timestamp.millisecondsSinceEpoch}_$cleanId';
  }

  String get fileName => storedFileName ?? '$baseName$extension';

  /// Parses a file name produced by [fileName]. Returns `null` if the name
  /// does not follow that format.
  static CachedImage? fromFileName(String fileName, Uint8List imageData) {
    if (!fileName.endsWith(extension)) return null;
    final parts = fileName
        .substring(0, fileName.length - extension.length)
        .split('_');
    if (parts.length < 3) return null;
    final millis = int.tryParse(parts[1]);
    if (millis == null) return null;
    final String serviceName;
    try {
      serviceName = Uri.decodeComponent(parts[0]);
    } on ArgumentError {
      return null;
    }
    return CachedImage(
      serviceName: serviceName,
      imageId: parts.sublist(2).join('_'),
      timestamp: DateTime.fromMillisecondsSinceEpoch(millis),
      imageData: imageData,
      storedFileName: fileName,
    );
  }

  @override
  String toString() => 'CachedImage($serviceName, $imageId, $timestamp)';
}

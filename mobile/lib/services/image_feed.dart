import '../models/cached_image.dart';
import '../models/snoopy_service.dart';

/// A live feed of screen captures from one Snoopy server.
///
/// The production implementation is `SseService`; tests inject a fake.
abstract interface class ImageFeed {
  Stream<CachedImage> get imageStream;

  Future<void> connect();

  Future<void> dispose();
}

typedef ImageFeedFactory = ImageFeed Function(SnoopyService service);

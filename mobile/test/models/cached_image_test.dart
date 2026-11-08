import 'dart:typed_data';

import 'package:flutter_test/flutter_test.dart';
import 'package:snoopy/models/cached_image.dart';

void main() {
  final data = Uint8List.fromList([1, 2, 3]);
  final timestamp = DateTime.fromMillisecondsSinceEpoch(1700000000000);

  test('builds a file name from service, timestamp and image id', () {
    final image = CachedImage(
      serviceName: 'office',
      imageId: '/images/0b7e-uuid.jpg',
      timestamp: timestamp,
      imageData: data,
    );
    expect(image.baseName, 'office_1700000000000_0b7e-uuid');
    expect(image.fileName, 'office_1700000000000_0b7e-uuid.jpg');
  });

  test('round-trips through fromFileName', () {
    final image = CachedImage(
      serviceName: 'Manual: my_host:8900',
      imageId: '/images/abc.jpg',
      timestamp: timestamp,
      imageData: data,
    );

    final parsed = CachedImage.fromFileName(image.fileName, data);

    expect(parsed, isNotNull);
    expect(parsed!.serviceName, 'Manual: my_host:8900');
    expect(parsed.imageId, 'abc');
    expect(parsed.timestamp, timestamp);
    expect(parsed.imageData, data);
    expect(parsed.fileName, image.fileName);
  });

  test('service names cannot inject path separators', () {
    final image = CachedImage(
      serviceName: '../evil/name',
      imageId: '/images/abc.jpg',
      timestamp: timestamp,
      imageData: data,
    );
    expect(image.fileName, isNot(contains('/')));
  });

  test('keeps legacy file names when loaded from disk', () {
    const legacy = 'office pc_1700000000000_abc.jpg';
    final parsed = CachedImage.fromFileName(legacy, data);
    expect(parsed!.serviceName, 'office pc');
    expect(parsed.fileName, legacy);
  });

  test('rejects names that do not match the format', () {
    expect(CachedImage.fromFileName('photo.png', data), isNull);
    expect(CachedImage.fromFileName('a_b.jpg', data), isNull);
    expect(CachedImage.fromFileName('a_notanumber_c.jpg', data), isNull);
    expect(CachedImage.fromFileName('%E0%A4%A_1_c.jpg', data), isNull);
  });
}

import 'package:flutter_test/flutter_test.dart';
import 'package:snoopy/models/snoopy_service.dart';

void main() {
  group('SnoopyService URLs', () {
    const service = SnoopyService(
      name: 'office',
      hostname: '192.168.1.20',
      port: 8900,
    );

    test('builds the SSE URL', () {
      expect(service.sseUrl, 'http://192.168.1.20:8900/sse/image');
    });

    test('uses the sse path advertised in TXT records', () {
      const advertised = SnoopyService(
        name: 'office',
        hostname: 'host.local',
        port: 8900,
        txt: {'path': '/', 'sse': '/events'},
      );
      expect(advertised.sseUrl, 'http://host.local:8900/events');
    });

    test('ignores a malformed sse TXT record', () {
      const advertised = SnoopyService(
        name: 'office',
        hostname: 'host.local',
        port: 8900,
        txt: {'sse': 'garbage'},
      );
      expect(advertised.sseUrl, 'http://host.local:8900/sse/image');
    });

    test('builds image URLs from an id', () {
      expect(
        service.imageUri('abc.jpg').toString(),
        'http://192.168.1.20:8900/images/abc.jpg',
      );
    });

    test('resolves server-relative paths', () {
      expect(
        service.resolvePath('/images/abc.jpg').toString(),
        'http://192.168.1.20:8900/images/abc.jpg',
      );
      expect(
        service.resolvePath('images/abc.jpg').toString(),
        'http://192.168.1.20:8900/images/abc.jpg',
      );
    });

    test('brackets IPv6 hosts', () {
      const v6 = SnoopyService(name: 'v6', hostname: 'fe80::1', port: 8900);
      expect(v6.sseUrl, 'http://[fe80::1]:8900/sse/image');
    });
  });

  test('equality ignores TXT records', () {
    const a = SnoopyService(name: 'a', hostname: 'h', port: 1);
    const b = SnoopyService(name: 'a', hostname: 'h', port: 1, txt: {'x': 'y'});
    expect(a, b);
    expect(a.hashCode, b.hashCode);
    expect(a, isNot(const SnoopyService(name: 'a', hostname: 'h', port: 2)));
  });
}

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:snoopy/models/snoopy_service.dart';
import 'package:snoopy/screens/viewer_screen.dart';

import '../support/fakes.dart';

void main() {
  testWidgets('connects the feed and disposes it with the screen', (
    tester,
  ) async {
    FakeImageFeed? feed;
    await tester.pumpWidget(
      MaterialApp(
        home: ViewerScreen(
          service: const SnoopyService(
            name: 'office-pc',
            hostname: '192.168.1.20',
            port: 8900,
          ),
          imageFeedFactory: (service) => feed = FakeImageFeed(service),
        ),
      ),
    );

    expect(find.text('Waiting for images...'), findsOneWidget);
    expect(feed!.connectCount, 1);
    expect(feed!.hasListener, isTrue);

    await tester.pumpWidget(const SizedBox());

    expect(feed!.hasListener, isFalse);
    expect(feed!.disposed, isTrue);
  });
}

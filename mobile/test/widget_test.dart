import 'package:flutter/widgets.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:snoopy/main.dart';
import 'package:snoopy/models/snoopy_service.dart';

import 'support/fakes.dart';

void main() {
  testWidgets('shows services emitted by discovery', (tester) async {
    final discovery = FakeDiscovery(
      initialServices: const [
        SnoopyService(name: 'office-pc', hostname: '192.168.1.20', port: 8900),
      ],
    );

    await tester.pumpWidget(
      SnoopyApp(
        discoveryFactory: () => discovery,
        imageFeedFactory: FakeImageFeed.new,
      ),
    );
    await tester.pump();

    expect(find.text('Snoopy Servers'), findsOneWidget);
    expect(find.text('office-pc'), findsOneWidget);
    expect(find.text('192.168.1.20:8900'), findsOneWidget);
    expect(discovery.startCount, 1);
  });

  testWidgets('updates when a service appears later and is lost', (
    tester,
  ) async {
    final discovery = FakeDiscovery();

    await tester.pumpWidget(
      SnoopyApp(
        discoveryFactory: () => discovery,
        imageFeedFactory: FakeImageFeed.new,
      ),
    );
    await tester.pump();
    expect(find.text('No Snoopy servers found'), findsOneWidget);

    discovery.emit(const [
      SnoopyService(name: 'laptop', hostname: '10.0.0.5', port: 9000),
    ]);
    await tester.pump();
    expect(find.text('laptop'), findsOneWidget);

    discovery.emit(const []);
    await tester.pump();
    expect(find.text('laptop'), findsNothing);
    expect(find.text('No Snoopy servers found'), findsOneWidget);
  });

  testWidgets('disposes discovery with the screen', (tester) async {
    final discovery = FakeDiscovery();
    await tester.pumpWidget(
      SnoopyApp(
        discoveryFactory: () => discovery,
        imageFeedFactory: FakeImageFeed.new,
      ),
    );
    await tester.pump();

    await tester.pumpWidget(const SizedBox());
    expect(discovery.disposed, isTrue);
  });
}

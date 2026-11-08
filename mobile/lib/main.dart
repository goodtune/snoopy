import 'package:flutter/material.dart';

import 'screens/selector_screen.dart';
import 'services/image_feed.dart';
import 'services/service_discovery.dart';

void main() {
  runApp(const SnoopyApp());
}

class SnoopyApp extends StatelessWidget {
  /// Overrides service discovery (defaults to mDNS). Used by tests.
  final ServiceDiscovery Function()? discoveryFactory;

  /// Overrides how image feeds are created (defaults to SSE). Used by tests.
  final ImageFeedFactory? imageFeedFactory;

  const SnoopyApp({super.key, this.discoveryFactory, this.imageFeedFactory});

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Snoopy',
      theme: ThemeData(
        colorScheme: ColorScheme.fromSeed(seedColor: Colors.blue),
        useMaterial3: true,
      ),
      darkTheme: ThemeData(
        colorScheme: ColorScheme.fromSeed(
          seedColor: Colors.blue,
          brightness: Brightness.dark,
        ),
        useMaterial3: true,
      ),
      home: SelectorScreen(
        discoveryFactory: discoveryFactory,
        imageFeedFactory: imageFeedFactory,
      ),
      debugShowCheckedModeBanner: false,
    );
  }
}

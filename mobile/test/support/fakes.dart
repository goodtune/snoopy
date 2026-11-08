import 'dart:async';

import 'package:snoopy/models/cached_image.dart';
import 'package:snoopy/models/snoopy_service.dart';
import 'package:snoopy/services/image_feed.dart';
import 'package:snoopy/services/service_discovery.dart';

/// A [ServiceDiscovery] that emits whatever services the test provides.
class FakeDiscovery implements ServiceDiscovery {
  FakeDiscovery({this.initialServices = const []});

  final List<SnoopyService> initialServices;
  final StreamController<List<SnoopyService>> _controller =
      StreamController<List<SnoopyService>>.broadcast();
  int startCount = 0;
  bool disposed = false;

  void emit(List<SnoopyService> services) => _controller.add(services);

  @override
  Stream<List<SnoopyService>> get servicesStream => _controller.stream;

  @override
  Future<void> startDiscovery() async {
    startCount++;
    if (initialServices.isNotEmpty) emit(initialServices);
  }

  @override
  Future<void> stopDiscovery() async {}

  @override
  Future<void> dispose() async {
    disposed = true;
    await _controller.close();
  }
}

/// An [ImageFeed] that never connects to the network.
class FakeImageFeed implements ImageFeed {
  FakeImageFeed(this.service);

  final SnoopyService service;
  final StreamController<CachedImage> _controller =
      StreamController<CachedImage>.broadcast();

  @override
  Stream<CachedImage> get imageStream => _controller.stream;

  @override
  Future<void> connect() async {}

  @override
  Future<void> dispose() => _controller.close();
}

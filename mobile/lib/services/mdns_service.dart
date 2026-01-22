import 'dart:async';
import 'package:nsd/nsd.dart';
import '../models/snoopy_service.dart';

class MdnsService {
  final Discovery _discovery = Discovery();
  final Map<String, SnoopyService> _services = {};
  final StreamController<List<SnoopyService>> _servicesController =
      StreamController<List<SnoopyService>>.broadcast();

  Stream<List<SnoopyService>> get servicesStream => _servicesController.stream;
  List<SnoopyService> get services => _services.values.toList();

  Future<void> startDiscovery() async {
    await _discovery.discover('_snoopy._tcp').listen((serviceInfo) async {
      if (serviceInfo.name != null) {
        // Resolve the service to get full details
        final resolvedService = await _discovery.resolve(serviceInfo);

        if (resolvedService.host != null && resolvedService.port != null) {
          final service = SnoopyService(
            name: resolvedService.name ?? 'Unknown',
            hostname: resolvedService.host!,
            port: resolvedService.port!,
            txt: resolvedService.txt ?? {},
          );

          _services[service.name] = service;
          _servicesController.add(_services.values.toList());
        }
      }
    }).asFuture();
  }

  Future<void> stopDiscovery() async {
    await _discovery.stopDiscovery();
  }

  void dispose() {
    stopDiscovery();
    _servicesController.close();
  }
}

import 'dart:async';
import 'dart:io';
import 'package:nsd/nsd.dart' as nsd;
import '../models/snoopy_service.dart';

class MdnsService {
  nsd.Discovery? _discovery;
  final Map<String, SnoopyService> _services = {};
  final StreamController<List<SnoopyService>> _servicesController =
      StreamController<List<SnoopyService>>.broadcast();

  Stream<List<SnoopyService>> get servicesStream => _servicesController.stream;
  List<SnoopyService> get services => _services.values.toList();

  Future<void> startDiscovery() async {
    _discovery = await nsd.startDiscovery('_snoopy._tcp', autoResolve: true);

    _discovery!.addServiceListener((service, status) async {
      if (status == nsd.ServiceStatus.found && service.name != null) {
        try {
          // Debug logging
          print('mDNS Service found: ${service.name}');
          print('  Host: ${service.host}');
          print('  Port: ${service.port}');
          print(
            '  Addresses: ${service.addresses?.map((a) => a.address).toList()}',
          );

          // Service should already be resolved due to autoResolve: true
          if (service.port != null) {
            // Prefer IP address over hostname for better reliability
            String? hostname;
            if (service.addresses != null && service.addresses!.isNotEmpty) {
              hostname = service.addresses!.first.address;
              print('  Using IP address: $hostname');
            } else if (service.host != null) {
              // Strip trailing dot from hostname
              final cleanHost = service.host!.replaceAll(RegExp(r'\.$'), '');

              // Try to resolve hostname to IP address
              try {
                final addresses = await InternetAddress.lookup(cleanHost);
                if (addresses.isNotEmpty) {
                  hostname = addresses.first.address;
                  print('  Resolved $cleanHost to IP: $hostname');
                } else {
                  hostname = cleanHost;
                  print('  Using hostname (resolution failed): $hostname');
                }
              } catch (e) {
                hostname = cleanHost;
                print('  Using hostname (resolution error: $e): $hostname');
              }
            }

            if (hostname != null) {
              final snoopyService = SnoopyService(
                name: service.name ?? 'Unknown',
                hostname: hostname,
                port: service.port!,
                txt:
                    service.txt?.map(
                      (key, value) => MapEntry(key, value.toString()),
                    ) ??
                    {},
              );

              _services[snoopyService.name] = snoopyService;
              _servicesController.add(_services.values.toList());
            }
          }
        } catch (e) {
          print('Error processing service: $e');
        }
      } else if (status == nsd.ServiceStatus.lost && service.name != null) {
        _services.remove(service.name);
        _servicesController.add(_services.values.toList());
      }
    });
  }

  Future<void> stopDiscovery() async {
    if (_discovery != null) {
      await nsd.stopDiscovery(_discovery!);
      _discovery = null;
    }
  }

  void dispose() {
    stopDiscovery();
    _servicesController.close();
  }
}

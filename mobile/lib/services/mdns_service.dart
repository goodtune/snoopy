import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:flutter/foundation.dart';
import 'package:nsd/nsd.dart' as nsd;

import '../models/snoopy_service.dart';
import 'service_discovery.dart';

/// mDNS/Bonjour discovery of `_snoopy._tcp` services using `package:nsd`.
class MdnsService implements ServiceDiscovery {
  static const String serviceType = '_snoopy._tcp';

  nsd.Discovery? _discovery;
  final Map<String, SnoopyService> _services = {};
  final StreamController<List<SnoopyService>> _servicesController =
      StreamController<List<SnoopyService>>.broadcast();

  @override
  Stream<List<SnoopyService>> get servicesStream => _servicesController.stream;

  List<SnoopyService> get services => _services.values.toList();

  @override
  Future<void> startDiscovery() async {
    await stopDiscovery();
    final discovery = await nsd.startDiscovery(
      serviceType,
      ipLookupType: nsd.IpLookupType.any,
    );
    _discovery = discovery;
    discovery.addServiceListener(_onServiceChanged);
  }

  Future<void> _onServiceChanged(
    nsd.Service service,
    nsd.ServiceStatus status,
  ) async {
    final name = service.name;
    if (name == null) return;

    if (status == nsd.ServiceStatus.lost) {
      _services.remove(name);
      _emit();
      return;
    }

    debugPrint(
      'mDNS service found: $name host=${service.host} port=${service.port} '
      'addresses=${service.addresses?.map((a) => a.address).toList()}',
    );

    final port = service.port;
    if (port == null) return;

    try {
      final hostname = await _resolveHost(service);
      if (hostname == null) return;
      _services[name] = SnoopyService(
        name: name,
        hostname: hostname,
        port: port,
        txt:
            service.txt?.map(
              (key, value) => MapEntry(
                key,
                value == null ? '' : utf8.decode(value, allowMalformed: true),
              ),
            ) ??
            const {},
      );
      _emit();
    } on Exception catch (e) {
      debugPrint('Error processing mDNS service $name: $e');
    }
  }

  /// Prefers an IPv4 address advertised with the service, then any address,
  /// then a DNS lookup of the advertised host name.
  static Future<String?> _resolveHost(nsd.Service service) async {
    final addresses = service.addresses;
    if (addresses != null && addresses.isNotEmpty) {
      final address = addresses.firstWhere(
        (a) => a.type == InternetAddressType.IPv4,
        orElse: () => addresses.first,
      );
      return address.address;
    }

    final host = service.host;
    if (host == null) return null;
    final cleanHost = host.replaceAll(RegExp(r'\.$'), '');
    try {
      final resolved = await InternetAddress.lookup(cleanHost);
      if (resolved.isNotEmpty) return resolved.first.address;
    } on SocketException catch (e) {
      debugPrint('Could not resolve $cleanHost: $e');
    }
    return cleanHost;
  }

  void _emit() {
    if (!_servicesController.isClosed) {
      _servicesController.add(_services.values.toList());
    }
  }

  @override
  Future<void> stopDiscovery() async {
    final discovery = _discovery;
    _discovery = null;
    if (discovery != null) {
      await nsd.stopDiscovery(discovery);
    }
  }

  @override
  Future<void> dispose() async {
    await stopDiscovery();
    await _servicesController.close();
  }
}

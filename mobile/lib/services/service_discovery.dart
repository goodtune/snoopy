import '../models/snoopy_service.dart';

/// Discovers Snoopy servers on the network.
///
/// The production implementation is `MdnsService`; tests inject a fake.
abstract interface class ServiceDiscovery {
  /// Emits the full list of currently known services on every change.
  Stream<List<SnoopyService>> get servicesStream;

  /// Starts (or restarts) discovery.
  Future<void> startDiscovery();

  Future<void> stopDiscovery();

  Future<void> dispose();
}

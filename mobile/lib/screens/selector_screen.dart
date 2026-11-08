import 'dart:async';

import 'package:flutter/material.dart';

import '../models/snoopy_service.dart';
import '../services/image_feed.dart';
import '../services/mdns_service.dart';
import '../services/service_discovery.dart';
import '../services/sse_service.dart';
import '../widgets/service_tile.dart';
import 'cleanup_screen.dart';
import 'viewer_screen.dart';

class SelectorScreen extends StatefulWidget {
  /// Creates the discovery used by this screen. Defaults to [MdnsService].
  final ServiceDiscovery Function()? discoveryFactory;

  /// Creates image feeds for previews and the viewer. Defaults to
  /// [SseService].
  final ImageFeedFactory? imageFeedFactory;

  const SelectorScreen({
    super.key,
    this.discoveryFactory,
    this.imageFeedFactory,
  });

  @override
  State<SelectorScreen> createState() => _SelectorScreenState();
}

class _SelectorScreenState extends State<SelectorScreen> {
  late final ServiceDiscovery _discovery;
  StreamSubscription<List<SnoopyService>>? _servicesSubscription;
  List<SnoopyService> _discoveredServices = [];
  final List<SnoopyService> _manualServices = [];
  bool _isDiscovering = false;
  String? _discoveryError;

  ImageFeedFactory get _imageFeedFactory =>
      widget.imageFeedFactory ?? SseService.new;

  List<SnoopyService> get _services => [
    ..._discoveredServices,
    ..._manualServices.where((s) => !_discoveredServices.contains(s)),
  ];

  @override
  void initState() {
    super.initState();
    _discovery = (widget.discoveryFactory ?? MdnsService.new)();
    _servicesSubscription = _discovery.servicesStream.listen((services) {
      if (!mounted) return;
      setState(() => _discoveredServices = services);
    });
    unawaited(_startDiscovery());
  }

  Future<void> _startDiscovery() async {
    setState(() {
      _isDiscovering = true;
      _discoveryError = null;
    });

    try {
      await _discovery.startDiscovery();
    } on Exception catch (e) {
      debugPrint('Service discovery failed: $e');
      if (mounted) setState(() => _discoveryError = '$e');
    } finally {
      if (mounted) setState(() => _isDiscovering = false);
    }
  }

  Future<void> _showManualEntryDialog() async {
    final service = await showDialog<SnoopyService>(
      context: context,
      builder: (context) => const _ManualEntryDialog(),
    );
    if (service != null && mounted) {
      setState(() => _manualServices.add(service));
    }
  }

  void _openViewer(SnoopyService service) {
    unawaited(
      Navigator.push(
        context,
        MaterialPageRoute<void>(
          builder: (context) => ViewerScreen(
            service: service,
            imageFeedFactory: _imageFeedFactory,
          ),
        ),
      ),
    );
  }

  @override
  void dispose() {
    unawaited(_servicesSubscription?.cancel());
    unawaited(_discovery.dispose());
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(
        title: const Text('Snoopy Servers'),
        actions: [
          IconButton(
            icon: const Icon(Icons.cleaning_services),
            tooltip: 'Cached images',
            onPressed: () => unawaited(
              Navigator.push(
                context,
                MaterialPageRoute<void>(
                  builder: (context) => const CleanupScreen(),
                ),
              ),
            ),
          ),
          IconButton(
            icon: const Icon(Icons.refresh),
            tooltip: 'Search again',
            onPressed: _startDiscovery,
          ),
          IconButton(
            icon: const Icon(Icons.add),
            onPressed: _showManualEntryDialog,
            tooltip: 'Add server manually',
          ),
        ],
      ),
      body: _buildBody(),
    );
  }

  Widget _buildBody() {
    final services = _services;

    if (_isDiscovering && services.isEmpty) {
      return const Center(
        child: Column(
          mainAxisAlignment: MainAxisAlignment.center,
          children: [
            CircularProgressIndicator(),
            SizedBox(height: 16),
            Text('Discovering Snoopy servers...'),
          ],
        ),
      );
    }

    if (services.isEmpty) {
      return Center(
        child: Column(
          mainAxisAlignment: MainAxisAlignment.center,
          children: [
            const Icon(Icons.search_off, size: 64, color: Colors.grey),
            const SizedBox(height: 16),
            const Text('No Snoopy servers found'),
            if (_discoveryError != null)
              Padding(
                padding: const EdgeInsets.all(16),
                child: Text(
                  'Discovery failed: $_discoveryError',
                  textAlign: TextAlign.center,
                ),
              ),
            const SizedBox(height: 16),
            ElevatedButton.icon(
              onPressed: _startDiscovery,
              icon: const Icon(Icons.refresh),
              label: const Text('Search Again'),
            ),
            const SizedBox(height: 8),
            ElevatedButton.icon(
              onPressed: _showManualEntryDialog,
              icon: const Icon(Icons.add),
              label: const Text('Add Server Manually'),
            ),
          ],
        ),
      );
    }

    return ListView.builder(
      padding: const EdgeInsets.all(16),
      itemCount: services.length,
      itemBuilder: (context, index) {
        final service = services[index];
        return Padding(
          padding: const EdgeInsets.only(bottom: 16),
          child: ServiceTile(
            key: ValueKey(service),
            service: service,
            imageFeedFactory: _imageFeedFactory,
            onTap: () => _openViewer(service),
          ),
        );
      },
    );
  }
}

/// Asks for a host and port and pops with the resulting [SnoopyService].
class _ManualEntryDialog extends StatefulWidget {
  const _ManualEntryDialog();

  @override
  State<_ManualEntryDialog> createState() => _ManualEntryDialogState();
}

class _ManualEntryDialogState extends State<_ManualEntryDialog> {
  final _hostnameController = TextEditingController();
  final _portController = TextEditingController(text: '8900');

  @override
  void dispose() {
    _hostnameController.dispose();
    _portController.dispose();
    super.dispose();
  }

  void _submit() {
    final hostname = _hostnameController.text.trim();
    final port = int.tryParse(_portController.text.trim());
    if (hostname.isEmpty || port == null || port <= 0 || port > 65535) return;
    Navigator.pop(
      context,
      SnoopyService(
        name: 'Manual: $hostname:$port',
        hostname: hostname,
        port: port,
      ),
    );
  }

  @override
  Widget build(BuildContext context) {
    return AlertDialog(
      title: const Text('Add Snoopy Server'),
      content: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          TextField(
            controller: _hostnameController,
            autofocus: true,
            decoration: const InputDecoration(
              labelText: 'Hostname or IP',
              hintText: 'e.g. 192.168.1.100',
            ),
            keyboardType: TextInputType.url,
          ),
          const SizedBox(height: 16),
          TextField(
            controller: _portController,
            decoration: const InputDecoration(
              labelText: 'Port',
              hintText: '8900',
            ),
            keyboardType: TextInputType.number,
            onSubmitted: (_) => _submit(),
          ),
        ],
      ),
      actions: [
        TextButton(
          onPressed: () => Navigator.pop(context),
          child: const Text('Cancel'),
        ),
        TextButton(onPressed: _submit, child: const Text('Add')),
      ],
    );
  }
}

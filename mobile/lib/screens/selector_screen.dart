import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../models/snoopy_service.dart';
import '../services/mdns_service.dart';
import '../widgets/service_tile.dart';
import 'viewer_screen.dart';
import 'cleanup_screen.dart';

class SelectorScreen extends StatefulWidget {
  const SelectorScreen({super.key});

  @override
  State<SelectorScreen> createState() => _SelectorScreenState();
}

class _SelectorScreenState extends State<SelectorScreen> {
  MdnsService? _mdnsService;
  List<SnoopyService> _services = [];
  bool _isDiscovering = false;

  @override
  void initState() {
    super.initState();
    // Only use mDNS discovery on native platforms
    if (!kIsWeb) {
      _mdnsService = MdnsService();
      _startDiscovery();
    }
  }

  Future<void> _startDiscovery() async {
    if (_mdnsService == null) return;

    setState(() {
      _isDiscovering = true;
    });

    _mdnsService!.servicesStream.listen((services) {
      setState(() {
        _services = services;
      });
    });

    await _mdnsService!.startDiscovery();

    setState(() {
      _isDiscovering = false;
    });
  }

  void _showManualEntryDialog() {
    final hostnameController = TextEditingController(text: 'localhost');
    final portController = TextEditingController(text: '8900');

    showDialog(
      context: context,
      builder: (context) => AlertDialog(
        title: const Text('Add Snoopy Server'),
        content: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            TextField(
              controller: hostnameController,
              decoration: const InputDecoration(
                labelText: 'Hostname or IP',
                hintText: 'e.g., 192.168.1.100',
              ),
            ),
            const SizedBox(height: 16),
            TextField(
              controller: portController,
              decoration: const InputDecoration(
                labelText: 'Port',
                hintText: '8900',
              ),
              keyboardType: TextInputType.number,
            ),
          ],
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.pop(context),
            child: const Text('Cancel'),
          ),
          TextButton(
            onPressed: () {
              final hostname = hostnameController.text.trim();
              final port = int.tryParse(portController.text.trim());

              if (hostname.isNotEmpty && port != null) {
                final service = SnoopyService(
                  name: 'Manual: $hostname:$port',
                  hostname: hostname,
                  port: port,
                  txt: {},
                );

                setState(() {
                  _services.add(service);
                });

                Navigator.pop(context);
              }
            },
            child: const Text('Add'),
          ),
        ],
      ),
    );
  }

  @override
  void dispose() {
    _mdnsService?.dispose();
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
            onPressed: () {
              Navigator.push(
                context,
                MaterialPageRoute(builder: (context) => const CleanupScreen()),
              );
            },
          ),
          if (!kIsWeb)
            IconButton(
              icon: const Icon(Icons.refresh),
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
    if (!kIsWeb && _isDiscovering && _services.isEmpty) {
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

    if (_services.isEmpty) {
      return Center(
        child: Column(
          mainAxisAlignment: MainAxisAlignment.center,
          children: [
            const Icon(Icons.search_off, size: 64, color: Colors.grey),
            const SizedBox(height: 16),
            Text(kIsWeb ? 'No servers added yet' : 'No Snoopy servers found'),
            const SizedBox(height: 16),
            if (!kIsWeb)
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
      itemCount: _services.length,
      itemBuilder: (context, index) {
        final service = _services[index];
        return Padding(
          padding: const EdgeInsets.only(bottom: 16),
          child: ServiceTile(
            service: service,
            onTap: () {
              Navigator.push(
                context,
                MaterialPageRoute(
                  builder: (context) => ViewerScreen(service: service),
                ),
              );
            },
          ),
        );
      },
    );
  }
}

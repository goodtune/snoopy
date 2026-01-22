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
  late MdnsService _mdnsService;
  List<SnoopyService> _services = [];
  bool _isDiscovering = false;

  @override
  void initState() {
    super.initState();
    _mdnsService = MdnsService();
    _startDiscovery();
  }

  Future<void> _startDiscovery() async {
    setState(() {
      _isDiscovering = true;
    });

    _mdnsService.servicesStream.listen((services) {
      setState(() {
        _services = services;
      });
    });

    await _mdnsService.startDiscovery();

    setState(() {
      _isDiscovering = false;
    });
  }

  @override
  void dispose() {
    _mdnsService.dispose();
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
          IconButton(
            icon: const Icon(Icons.refresh),
            onPressed: _startDiscovery,
          ),
        ],
      ),
      body: _buildBody(),
    );
  }

  Widget _buildBody() {
    if (_isDiscovering && _services.isEmpty) {
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
            const Text('No Snoopy servers found'),
            const SizedBox(height: 16),
            ElevatedButton.icon(
              onPressed: _startDiscovery,
              icon: const Icon(Icons.refresh),
              label: const Text('Search Again'),
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

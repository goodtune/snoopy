import 'dart:async';
import 'dart:typed_data';
import 'package:flutter/material.dart';
import 'package:http/http.dart' as http;
import '../models/snoopy_service.dart';
import '../services/sse_service.dart';

class ServiceTile extends StatefulWidget {
  final SnoopyService service;
  final VoidCallback onTap;

  const ServiceTile({super.key, required this.service, required this.onTap});

  @override
  State<ServiceTile> createState() => _ServiceTileState();
}

class _ServiceTileState extends State<ServiceTile> {
  SseService? _sseService;
  Uint8List? _currentImageData;
  bool _isLoading = true;

  @override
  void initState() {
    super.initState();
    _connectToService();
  }

  Future<void> _connectToService() async {
    _sseService = SseService(widget.service);

    _sseService!.imageStream.listen((image) {
      if (mounted) {
        setState(() {
          _currentImageData = image.imageData;
          _isLoading = false;
        });
      }
    });

    await _sseService!.connect();
  }

  @override
  void dispose() {
    _sseService?.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return Card(
      clipBehavior: Clip.antiAlias,
      child: InkWell(
        onTap: widget.onTap,
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            AspectRatio(aspectRatio: 16 / 9, child: _buildPreview()),
            Container(
              padding: const EdgeInsets.all(12),
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Text(
                    widget.service.name,
                    style: Theme.of(context).textTheme.titleMedium?.copyWith(
                      fontWeight: FontWeight.bold,
                    ),
                    maxLines: 1,
                    overflow: TextOverflow.ellipsis,
                  ),
                  const SizedBox(height: 4),
                  Text(
                    '${widget.service.hostname}:${widget.service.port}',
                    style: Theme.of(
                      context,
                    ).textTheme.bodySmall?.copyWith(color: Colors.grey[600]),
                    maxLines: 1,
                    overflow: TextOverflow.ellipsis,
                  ),
                ],
              ),
            ),
          ],
        ),
      ),
    );
  }

  Widget _buildPreview() {
    if (_isLoading) {
      return Container(
        color: Colors.grey[300],
        child: const Center(child: CircularProgressIndicator()),
      );
    }

    if (_currentImageData == null) {
      return Container(
        color: Colors.grey[300],
        child: const Center(child: Icon(Icons.image_not_supported, size: 48)),
      );
    }

    return Image.memory(_currentImageData!, fit: BoxFit.cover);
  }
}

import 'dart:async';
import 'dart:typed_data';

import 'package:flutter/material.dart';

import '../models/cached_image.dart';
import '../models/snoopy_service.dart';
import '../services/image_feed.dart';

/// A card showing a server's name, address and its latest screen capture.
class ServiceTile extends StatefulWidget {
  final SnoopyService service;
  final ImageFeedFactory imageFeedFactory;
  final VoidCallback onTap;

  const ServiceTile({
    super.key,
    required this.service,
    required this.imageFeedFactory,
    required this.onTap,
  });

  @override
  State<ServiceTile> createState() => _ServiceTileState();
}

class _ServiceTileState extends State<ServiceTile> {
  late final ImageFeed _feed;
  StreamSubscription<CachedImage>? _subscription;
  Uint8List? _currentImageData;

  @override
  void initState() {
    super.initState();
    _feed = widget.imageFeedFactory(widget.service);
    _subscription = _feed.imageStream.listen((image) {
      if (mounted) setState(() => _currentImageData = image.imageData);
    });
    unawaited(_feed.connect());
  }

  @override
  void dispose() {
    unawaited(_subscription?.cancel());
    unawaited(_feed.dispose());
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final textTheme = Theme.of(context).textTheme;
    return Card(
      clipBehavior: Clip.antiAlias,
      child: InkWell(
        onTap: widget.onTap,
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            AspectRatio(aspectRatio: 16 / 9, child: _buildPreview()),
            Padding(
              padding: const EdgeInsets.all(12),
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Text(
                    widget.service.name,
                    style: textTheme.titleMedium?.copyWith(
                      fontWeight: FontWeight.bold,
                    ),
                    maxLines: 1,
                    overflow: TextOverflow.ellipsis,
                  ),
                  const SizedBox(height: 4),
                  Text(
                    '${widget.service.hostname}:${widget.service.port}',
                    style: textTheme.bodySmall?.copyWith(
                      color: Colors.grey[600],
                    ),
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
    final data = _currentImageData;
    if (data == null) {
      return ColoredBox(
        color: Colors.grey[300]!,
        child: const Center(child: CircularProgressIndicator()),
      );
    }
    return Image.memory(data, fit: BoxFit.cover, gaplessPlayback: true);
  }
}

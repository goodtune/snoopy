import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:gal/gal.dart';

import '../models/cached_image.dart';
import '../models/snoopy_service.dart';
import '../services/image_feed.dart';
import '../services/storage_service.dart';

class ViewerScreen extends StatefulWidget {
  final SnoopyService service;
  final ImageFeedFactory imageFeedFactory;

  const ViewerScreen({
    super.key,
    required this.service,
    required this.imageFeedFactory,
  });

  @override
  State<ViewerScreen> createState() => _ViewerScreenState();
}

class _ViewerScreenState extends State<ViewerScreen> {
  late final ImageFeed _feed;
  final StorageService _storageService = StorageService();
  StreamSubscription<CachedImage>? _subscription;
  final List<CachedImage> _images = [];
  int _currentIndex = 0;
  bool _isFullscreen = true;
  bool _isSaving = false;

  @override
  void initState() {
    super.initState();
    _feed = widget.imageFeedFactory(widget.service);
    _subscription = _feed.imageStream.listen((image) {
      if (!mounted) return;
      setState(() {
        _images.insert(0, image);
        _currentIndex = 0;
      });
      unawaited(_storageService.saveImage(image));
    });
    unawaited(_feed.connect());
  }

  @override
  void dispose() {
    unawaited(_subscription?.cancel());
    unawaited(_feed.dispose());
    super.dispose();
  }

  void _toggleFullscreen() {
    setState(() => _isFullscreen = !_isFullscreen);
  }

  Future<void> _saveCurrentImage() async {
    if (_images.isEmpty || _isSaving) return;
    final image = _images[_currentIndex];
    final messenger = ScaffoldMessenger.of(context);

    void show(String message) {
      messenger.showSnackBar(SnackBar(content: Text(message)));
    }

    setState(() => _isSaving = true);
    try {
      if (!await Gal.requestAccess()) {
        show(
          'Permission to save to Photos was denied. '
          'You can enable it in Settings.',
        );
        return;
      }
      await Gal.putImageBytes(image.imageData, name: image.baseName);
      show('Image saved to gallery');
    } on GalException catch (e) {
      debugPrint('Saving to gallery failed: $e');
      show(switch (e.type) {
        GalExceptionType.accessDenied =>
          'Permission to save to Photos was denied. '
              'You can enable it in Settings.',
        GalExceptionType.notEnoughSpace => 'Not enough storage to save image',
        GalExceptionType.notSupportedFormat => 'Image format not supported',
        GalExceptionType.unexpected => 'Failed to save image',
      });
    } on PlatformException catch (e) {
      debugPrint('Saving to gallery failed: $e');
      show('Failed to save image');
    } finally {
      if (mounted) setState(() => _isSaving = false);
    }
  }

  void _navigateToImage(int delta) {
    final newIndex = _currentIndex + delta;
    if (newIndex >= 0 && newIndex < _images.length) {
      setState(() => _currentIndex = newIndex);
    }
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      backgroundColor: Colors.black,
      appBar: _isFullscreen
          ? null
          : AppBar(
              title: Text(widget.service.name),
              backgroundColor: Colors.black,
              foregroundColor: Colors.white,
              actions: [
                IconButton(
                  icon: const Icon(Icons.save),
                  tooltip: 'Save to gallery',
                  onPressed: _isSaving ? null : _saveCurrentImage,
                ),
                IconButton(
                  icon: const Icon(Icons.fullscreen),
                  tooltip: 'Fullscreen',
                  onPressed: _toggleFullscreen,
                ),
              ],
            ),
      body: _buildBody(),
    );
  }

  Widget _buildBody() {
    if (_images.isEmpty) {
      return const Center(
        child: Column(
          mainAxisAlignment: MainAxisAlignment.center,
          children: [
            CircularProgressIndicator(color: Colors.white),
            SizedBox(height: 16),
            Text(
              'Waiting for images...',
              style: TextStyle(color: Colors.white),
            ),
          ],
        ),
      );
    }

    return GestureDetector(
      onTap: _toggleFullscreen,
      onHorizontalDragEnd: (details) {
        final velocity = details.primaryVelocity ?? 0;
        if (velocity > 0) {
          // Swipe right - go to older image
          _navigateToImage(1);
        } else if (velocity < 0) {
          // Swipe left - go to newer image
          _navigateToImage(-1);
        }
      },
      child: Stack(
        fit: StackFit.expand,
        children: [
          InteractiveViewer(
            child: Image.memory(
              _images[_currentIndex].imageData,
              fit: BoxFit.contain,
              gaplessPlayback: true,
            ),
          ),
          if (!_isFullscreen)
            Positioned(
              bottom: 16,
              left: 16,
              right: 16,
              child: DecoratedBox(
                decoration: BoxDecoration(
                  color: Colors.black54,
                  borderRadius: BorderRadius.circular(8),
                ),
                child: Padding(
                  padding: const EdgeInsets.all(8),
                  child: Column(
                    children: [
                      Text(
                        'Image ${_currentIndex + 1} of ${_images.length}',
                        style: const TextStyle(color: Colors.white),
                      ),
                      const SizedBox(height: 8),
                      Row(
                        mainAxisAlignment: MainAxisAlignment.center,
                        children: [
                          IconButton(
                            icon: const Icon(
                              Icons.arrow_back,
                              color: Colors.white,
                            ),
                            tooltip: 'Older',
                            onPressed: _currentIndex < _images.length - 1
                                ? () => _navigateToImage(1)
                                : null,
                          ),
                          const SizedBox(width: 16),
                          IconButton(
                            icon: const Icon(Icons.save, color: Colors.white),
                            tooltip: 'Save to gallery',
                            onPressed: _isSaving ? null : _saveCurrentImage,
                          ),
                          const SizedBox(width: 16),
                          IconButton(
                            icon: const Icon(
                              Icons.arrow_forward,
                              color: Colors.white,
                            ),
                            tooltip: 'Newer',
                            onPressed: _currentIndex > 0
                                ? () => _navigateToImage(-1)
                                : null,
                          ),
                        ],
                      ),
                    ],
                  ),
                ),
              ),
            ),
        ],
      ),
    );
  }
}

import 'dart:io';
import 'package:flutter/material.dart';
import 'package:image_gallery_saver/image_gallery_saver.dart';
import 'package:permission_handler/permission_handler.dart';
import 'package:path_provider/path_provider.dart';
import '../models/snoopy_service.dart';
import '../models/cached_image.dart';
import '../services/sse_service.dart';
import '../services/storage_service.dart';

class ViewerScreen extends StatefulWidget {
  final SnoopyService service;

  const ViewerScreen({super.key, required this.service});

  @override
  State<ViewerScreen> createState() => _ViewerScreenState();
}

class _ViewerScreenState extends State<ViewerScreen> {
  late SseService _sseService;
  late StorageService _storageService;
  final List<CachedImage> _images = [];
  int _currentIndex = 0;
  bool _isFullscreen = true;
  bool _isConnected = false;

  @override
  void initState() {
    super.initState();
    _sseService = SseService(widget.service);
    _storageService = StorageService();
    _connectToService();
  }

  Future<void> _connectToService() async {
    setState(() {
      _isConnected = true;
    });

    _sseService.imageStream.listen((image) {
      setState(() {
        _images.insert(0, image);
        _currentIndex = 0;
      });
      _storageService.saveImage(image);
    });

    await _sseService.connect();
  }

  @override
  void dispose() {
    _sseService.dispose();
    super.dispose();
  }

  void _toggleFullscreen() {
    setState(() {
      _isFullscreen = !_isFullscreen;
    });
  }

  Future<void> _saveCurrentImage() async {
    if (_images.isEmpty) return;

    final currentImage = _images[_currentIndex];

    // Check platform
    if (Platform.isMacOS || Platform.isWindows || Platform.isLinux) {
      // Desktop: Save to Downloads folder
      try {
        final downloadsDir = await getDownloadsDirectory();
        if (downloadsDir != null) {
          // Clean up the filename - remove path separators and ensure single extension
          String cleanFileName = currentImage.imageId
              .replaceAll('/', '_')
              .replaceAll('\\', '_');

          // Ensure .jpg extension (and avoid double extension)
          if (!cleanFileName.endsWith('.jpg')) {
            cleanFileName = '$cleanFileName.jpg';
          }

          final filePath = '${downloadsDir.path}/$cleanFileName';
          final file = File(filePath);
          await file.writeAsBytes(currentImage.imageData);

          if (mounted) {
            ScaffoldMessenger.of(context).showSnackBar(
              SnackBar(
                content: Text('Image saved to Downloads: $cleanFileName'),
                duration: const Duration(seconds: 2),
              ),
            );
          }
        } else {
          throw Exception('Could not access Downloads directory');
        }
      } catch (e) {
        if (mounted) {
          ScaffoldMessenger.of(context).showSnackBar(
            SnackBar(
              content: Text('Failed to save image: $e'),
              duration: const Duration(seconds: 3),
            ),
          );
        }
      }
    } else {
      // Mobile (iOS/Android): Use image_gallery_saver with permissions
      final status = await Permission.photosAddOnly.request();

      if (status.isGranted || status.isLimited) {
        final result = await ImageGallerySaver.saveImage(
          currentImage.imageData,
          name: currentImage.fileName,
        );

        if (mounted) {
          ScaffoldMessenger.of(context).showSnackBar(
            SnackBar(
              content: Text(
                result['isSuccess']
                    ? 'Image saved to gallery'
                    : 'Failed to save image',
              ),
            ),
          );
        }
      } else if (status.isPermanentlyDenied) {
        if (mounted) {
          ScaffoldMessenger.of(context).showSnackBar(
            SnackBar(
              content: const Text(
                'Permission permanently denied. Please enable in Settings.',
              ),
              action: SnackBarAction(
                label: 'Settings',
                onPressed: () => openAppSettings(),
              ),
            ),
          );
        }
      } else {
        if (mounted) {
          ScaffoldMessenger.of(context).showSnackBar(
            const SnackBar(content: Text('Permission denied to save images')),
          );
        }
      }
    }
  }

  void _navigateToImage(int delta) {
    setState(() {
      final newIndex = _currentIndex + delta;
      if (newIndex >= 0 && newIndex < _images.length) {
        _currentIndex = newIndex;
      }
    });
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
                  onPressed: _saveCurrentImage,
                ),
                IconButton(
                  icon: const Icon(Icons.fullscreen),
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
        if (details.primaryVelocity! > 0) {
          // Swipe right - go to older image
          _navigateToImage(1);
        } else if (details.primaryVelocity! < 0) {
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
            ),
          ),
          if (!_isFullscreen)
            Positioned(
              bottom: 16,
              left: 16,
              right: 16,
              child: Container(
                padding: const EdgeInsets.all(8),
                decoration: BoxDecoration(
                  color: Colors.black54,
                  borderRadius: BorderRadius.circular(8),
                ),
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
                          onPressed: _currentIndex < _images.length - 1
                              ? () => _navigateToImage(1)
                              : null,
                        ),
                        const SizedBox(width: 16),
                        IconButton(
                          icon: const Icon(Icons.save, color: Colors.white),
                          onPressed: _saveCurrentImage,
                        ),
                        const SizedBox(width: 16),
                        IconButton(
                          icon: const Icon(
                            Icons.arrow_forward,
                            color: Colors.white,
                          ),
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
        ],
      ),
    );
  }
}

import 'dart:io';
import 'dart:typed_data';
import 'package:path_provider/path_provider.dart';
import '../models/cached_image.dart';

class StorageService {
  static const String _imageDirectory = 'snoopy_images';

  Future<String> get _localPath async {
    final directory = await getApplicationDocumentsDirectory();
    final imageDir = Directory('${directory.path}/$_imageDirectory');
    if (!await imageDir.exists()) {
      await imageDir.create(recursive: true);
    }
    return imageDir.path;
  }

  Future<void> saveImage(CachedImage image) async {
    try {
      final path = await _localPath;
      final file = File('$path/${image.fileName}');
      await file.writeAsBytes(image.imageData);
    } catch (e) {
      print('Error saving image: $e');
    }
  }

  Future<List<CachedImage>> loadAllImages() async {
    try {
      final path = await _localPath;
      final directory = Directory(path);

      if (!await directory.exists()) {
        return [];
      }

      final files = await directory.list().toList();
      final images = <CachedImage>[];

      for (final file in files) {
        if (file is File && file.path.endsWith('.jpg')) {
          try {
            final fileName = file.path.split('/').last;
            final parts = fileName.replaceAll('.jpg', '').split('_');

            if (parts.length >= 3) {
              final serviceName = parts[0];
              final timestamp =
                  DateTime.fromMillisecondsSinceEpoch(int.parse(parts[1]));
              final imageId = parts.sublist(2).join('_');
              final imageData = await file.readAsBytes();

              images.add(CachedImage(
                serviceName: serviceName,
                imageId: imageId,
                timestamp: timestamp,
                imageData: imageData,
              ));
            }
          } catch (e) {
            print('Error loading image file ${file.path}: $e');
          }
        }
      }

      // Sort by timestamp descending (newest first)
      images.sort((a, b) => b.timestamp.compareTo(a.timestamp));

      return images;
    } catch (e) {
      print('Error loading images: $e');
      return [];
    }
  }

  Future<List<CachedImage>> loadImagesByService(String serviceName) async {
    final allImages = await loadAllImages();
    return allImages.where((img) => img.serviceName == serviceName).toList();
  }

  Future<Map<String, List<CachedImage>>> loadImagesGroupedByService() async {
    final allImages = await loadAllImages();
    final Map<String, List<CachedImage>> grouped = {};

    for (final image in allImages) {
      grouped.putIfAbsent(image.serviceName, () => []).add(image);
    }

    return grouped;
  }

  Future<void> deleteImage(CachedImage image) async {
    try {
      final path = await _localPath;
      final file = File('$path/${image.fileName}');
      if (await file.exists()) {
        await file.delete();
      }
    } catch (e) {
      print('Error deleting image: $e');
    }
  }

  Future<void> deleteAllImagesForService(String serviceName) async {
    try {
      final images = await loadImagesByService(serviceName);
      for (final image in images) {
        await deleteImage(image);
      }
    } catch (e) {
      print('Error deleting images for service: $e');
    }
  }

  Future<void> deleteAllImages() async {
    try {
      final path = await _localPath;
      final directory = Directory(path);
      if (await directory.exists()) {
        await directory.delete(recursive: true);
        await directory.create(recursive: true);
      }
    } catch (e) {
      print('Error deleting all images: $e');
    }
  }

  Future<int> getTotalImageCount() async {
    final images = await loadAllImages();
    return images.length;
  }

  Future<int> getServiceImageCount(String serviceName) async {
    final images = await loadImagesByService(serviceName);
    return images.length;
  }
}

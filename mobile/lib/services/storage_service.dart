import 'dart:io';

import 'package:flutter/foundation.dart';
import 'package:path_provider/path_provider.dart';

import '../models/cached_image.dart';

/// Persists received images in the app's documents directory.
class StorageService {
  static const String _imageDirectory = 'snoopy_images';

  Future<Directory> get _directory async {
    final documents = await getApplicationDocumentsDirectory();
    final imageDir = Directory('${documents.path}/$_imageDirectory');
    if (!imageDir.existsSync()) {
      await imageDir.create(recursive: true);
    }
    return imageDir;
  }

  Future<File> _fileFor(CachedImage image) async =>
      File('${(await _directory).path}/${image.fileName}');

  Future<void> saveImage(CachedImage image) async {
    try {
      await (await _fileFor(image)).writeAsBytes(image.imageData);
    } on Exception catch (e) {
      debugPrint('Error saving image: $e');
    }
  }

  /// Loads all cached images, newest first.
  Future<List<CachedImage>> loadAllImages() async {
    try {
      final directory = await _directory;
      final images = <CachedImage>[];

      await for (final entity in directory.list()) {
        if (entity is! File) continue;
        final fileName = entity.uri.pathSegments.last;
        try {
          final image = CachedImage.fromFileName(
            fileName,
            await entity.readAsBytes(),
          );
          if (image != null) images.add(image);
        } on Exception catch (e) {
          debugPrint('Error loading image file ${entity.path}: $e');
        }
      }

      images.sort((a, b) => b.timestamp.compareTo(a.timestamp));
      return images;
    } on Exception catch (e) {
      debugPrint('Error loading images: $e');
      return [];
    }
  }

  Future<List<CachedImage>> loadImagesByService(String serviceName) async {
    final allImages = await loadAllImages();
    return allImages.where((img) => img.serviceName == serviceName).toList();
  }

  Future<Map<String, List<CachedImage>>> loadImagesGroupedByService() async {
    final allImages = await loadAllImages();
    final grouped = <String, List<CachedImage>>{};
    for (final image in allImages) {
      grouped.putIfAbsent(image.serviceName, () => []).add(image);
    }
    return grouped;
  }

  Future<void> deleteImage(CachedImage image) async {
    try {
      final file = await _fileFor(image);
      if (file.existsSync()) {
        await file.delete();
      }
    } on Exception catch (e) {
      debugPrint('Error deleting image: $e');
    }
  }

  Future<void> deleteAllImagesForService(String serviceName) async {
    for (final image in await loadImagesByService(serviceName)) {
      await deleteImage(image);
    }
  }

  Future<void> deleteAllImages() async {
    try {
      final directory = await _directory;
      await directory.delete(recursive: true);
      await directory.create(recursive: true);
    } on Exception catch (e) {
      debugPrint('Error deleting all images: $e');
    }
  }

  Future<int> getTotalImageCount() async => (await loadAllImages()).length;

  Future<int> getServiceImageCount(String serviceName) async =>
      (await loadImagesByService(serviceName)).length;
}

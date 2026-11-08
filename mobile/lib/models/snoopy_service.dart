/// A Snoopy server, either discovered via mDNS (`_snoopy._tcp`) or added by
/// hand.
class SnoopyService {
  final String name;

  /// Hostname or IP address (IPv4 or IPv6, without brackets).
  final String hostname;
  final int port;
  final Map<String, String> txt;

  const SnoopyService({
    required this.name,
    required this.hostname,
    required this.port,
    this.txt = const {},
  });

  /// Base URL of the server. [Uri] takes care of bracketing IPv6 hosts.
  Uri get baseUri => Uri(scheme: 'http', host: hostname, port: port);

  /// Server-Sent Events endpoint that announces new screen captures. The
  /// server advertises it in the `sse` TXT record; `/sse/image` is the
  /// default.
  Uri get sseUri {
    final advertised = txt['sse'];
    final path = advertised != null && advertised.startsWith('/')
        ? advertised
        : '/sse/image';
    return baseUri.replace(path: path);
  }

  String get sseUrl => sseUri.toString();

  /// URL of a cached image by its id (`<uuid>.jpg`).
  Uri imageUri(String imageId) => baseUri.replace(path: '/images/$imageId');

  /// Resolves a server-relative path (e.g. `/images/<uuid>.jpg`, as sent in
  /// SSE events) against [baseUri].
  Uri resolvePath(String path) =>
      baseUri.replace(path: path.startsWith('/') ? path : '/$path');

  @override
  String toString() => 'SnoopyService($name, $hostname:$port)';

  @override
  bool operator ==(Object other) =>
      identical(this, other) ||
      other is SnoopyService &&
          name == other.name &&
          hostname == other.hostname &&
          port == other.port;

  @override
  int get hashCode => Object.hash(name, hostname, port);
}

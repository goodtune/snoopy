class SnoopyService {
  final String name;
  final String hostname;
  final int port;
  final Map<String, String> txt;

  SnoopyService({
    required this.name,
    required this.hostname,
    required this.port,
    required this.txt,
  });

  String get sseUrl => 'http://$hostname:$port/sse/image';

  String imageUrl(String imageId) => 'http://$hostname:$port/images/$imageId';

  @override
  String toString() => 'SnoopyService($name, $hostname:$port)';

  @override
  bool operator ==(Object other) =>
      identical(this, other) ||
      other is SnoopyService &&
          runtimeType == other.runtimeType &&
          name == other.name &&
          hostname == other.hostname &&
          port == other.port;

  @override
  int get hashCode => name.hashCode ^ hostname.hashCode ^ port.hashCode;
}

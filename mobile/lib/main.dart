import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import 'screens/selector_screen.dart';

void main() {
  runApp(const SnoopyApp());
}

class SnoopyApp extends StatelessWidget {
  const SnoopyApp({super.key});

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Snoopy',
      theme: ThemeData(
        colorScheme: ColorScheme.fromSeed(
          seedColor: Colors.blue,
          brightness: Brightness.light,
        ),
        useMaterial3: true,
      ),
      darkTheme: ThemeData(
        colorScheme: ColorScheme.fromSeed(
          seedColor: Colors.blue,
          brightness: Brightness.dark,
        ),
        useMaterial3: true,
      ),
      themeMode: ThemeMode.system,
      home: const SelectorScreen(),
      debugShowCheckedModeBanner: false,
    );
  }
}

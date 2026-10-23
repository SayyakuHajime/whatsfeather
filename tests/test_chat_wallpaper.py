"""Packaged wallpaper resource checks; runtime geometry is covered by Qt probe."""
from pathlib import Path
import unittest

ROOT=Path(__file__).resolve().parents[1]
class ChatWallpaperAssets(unittest.TestCase):
    def test_both_optimized_wallpapers_are_packaged_webp(self):
        for variant in ('dark','green'):
            with self.subTest(variant=variant):
                data=(ROOT/f'desktop/assets/backgrounds/chat-{variant}.webp').read_bytes()
                self.assertEqual(data[:4],b'RIFF')
                self.assertEqual(data[8:12],b'WEBP')
                self.assertLess(len(data),400000,'Background should remain a bounded UI asset')

    def test_responsive_component_covers_without_stretching(self):
        component=(ROOT/'desktop/qml/ChatWallpaper.qml').read_text()
        self.assertIn('Image.PreserveAspectCrop',component)
        self.assertIn('clip: true',component)
        self.assertIn('qrc:/backgrounds/chat-dark.webp',component)
        self.assertIn('qrc:/backgrounds/chat-green.webp',component)
        self.assertNotIn('Image.Stretch',component)

if __name__=='__main__':unittest.main(verbosity=2)

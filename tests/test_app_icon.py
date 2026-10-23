"""Portable checks for packaged application icons; no private source paths."""
from pathlib import Path
import struct
import unittest

ROOT = Path(__file__).resolve().parents[1]
ASSETS = ROOT / 'desktop/assets/app-icons'
SIZES = (16, 24, 32, 48, 64, 128, 256, 512)

class ApplicationIcon(unittest.TestCase):
    def test_png_family_has_correct_dimensions_and_alpha(self):
        for size in SIZES:
            with self.subTest(size=size):
                data = (ASSETS / f'whatsfeather-{size}.png').read_bytes()
                self.assertEqual(data[:8], b'\x89PNG\r\n\x1a\n')
                self.assertEqual(struct.unpack('>II', data[16:24]), (size, size))
                self.assertEqual(data[25], 6, 'RGBA PNG must preserve transparency')

    def test_linux_launcher_uses_stable_application_identity(self):
        entry = (ROOT / 'desktop/packaging/whatsfeather.desktop').read_text()
        for field in ('Type=Application', 'Name=WhatsFeather', 'Icon=whatsfeather', 'StartupWMClass=whatsfeather', 'Terminal=false'):
            self.assertIn(field, entry)
        self.assertNotIn('/home/', entry, 'Packaged launcher must be relocatable')
        self.assertNotIn('--live', entry, 'Do not change the opt-in live boundary')

if __name__ == '__main__':
    unittest.main(verbosity=2)

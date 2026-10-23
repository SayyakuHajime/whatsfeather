"""Check the colors actually declared in the native QML, not a duplicate palette."""
from pathlib import Path
import re
import unittest

QML = Path(__file__).resolve().parents[1] / "desktop/qml/Main.qml"

def luminance(value):
    rgb = [int(value[i:i+2], 16) / 255 for i in (1, 3, 5)]
    rgb = [v / 12.92 if v <= .04045 else ((v + .055) / 1.055) ** 2.4 for v in rgb]
    return sum(a * b for a, b in zip(rgb, [.2126, .7152, .0722]))

def contrast(a, b):
    low, high = sorted((luminance(a), luminance(b)))
    return (high + .05) / (low + .05)

class ThemeContrast(unittest.TestCase):
    def test_primary_text_and_action_icon_contrast(self):
        source = QML.read_text()
        tokens = {name: (dark, light) for name, dark, light in re.findall(
            r'readonly property color (\w+):\s*dark\s*\?\s*"(#[0-9a-fA-F]+)"\s*:\s*"(#[0-9a-fA-F]+)"', source)}
        composer = (QML.parent / "FloatingComposer.qml").read_text()
        block = composer.split('objectName: "sendButton"', 1)[1]
        match = re.search(r'tint:\s*(?:floating\.)?theme.dark\s*\?\s*"(#[0-9a-fA-F]+)"\s*:\s*"(#[0-9a-fA-F]+)"', block)
        assert match is not None
        icons = match.groups()
        for theme in (0, 1):
            for fg, bg in (("ink", "surface"), ("ink", "outgoing"), ("muted", "chrome")):
                with self.subTest(theme=theme, foreground=fg, background=bg):
                    self.assertGreaterEqual(contrast(tokens[fg][theme], tokens[bg][theme]), 4.5)
            self.assertGreaterEqual(contrast(icons[theme], tokens["accent"][theme]), 3)

if __name__ == "__main__":
    unittest.main(verbosity=2)

"""Source checks for the inert native card; runtime checks use the private fixture."""
from pathlib import Path
import re
ROOT = Path(__file__).resolve().parents[1]
s = (ROOT / 'desktop/qml/MessageBubble.qml').read_text()
start = s.index('objectName: "textCardCardSurface"')
end = s.index('\n            Rectangle {', start)
card = s[start:end]
for field in ('Title', 'Subtitle', 'Body', 'Footer', 'Disclosure', 'Label'):
    assert f'objectName: "textCard{field}' in card, field
assert 'Display only' in card
assert 'Accessible.StaticText' in card
assert 'Template label (display only): ' in card
assert card.count('textFormat: Text.PlainText') == 6
assert not re.search(r'\b(Button|MouseArea|TapHandler|onClicked|onTapped|onLinkActivated)\b|openUrl|Controller|controller\.', card)
assert 'background: null' in card and 'padding: 0' in card
assert '!row.isTextCard' in s
print('Static text card PASS: PlainText fields, disclosure, static names, no action code')

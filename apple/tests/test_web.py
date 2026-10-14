# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
from html.parser import HTMLParser
from pathlib import Path
import unittest,re
ROOT=Path(__file__).resolve().parents[1]
class Document(HTMLParser):
    def __init__(self):super().__init__();self.ids=[];self.links=[]
    def handle_starttag(self,tag,attrs):
        a=dict(attrs)
        if 'id' in a:self.ids.append(a['id'])
        for k in ['src','href']:
            if k in a:self.links.append(a[k])
class WebTests(unittest.TestCase):
    def setUp(self):self.doc=Document();self.doc.feed((ROOT/'web/index.html').read_text())
    def test_unique_ids(self):self.assertEqual(len(self.doc.ids),len(set(self.doc.ids)))
    def test_local_assets(self):self.assertTrue(all(not x.startswith(('http:','https:','//')) for x in self.doc.links))   # relative so the static demo works under any base path
    def test_required_panels(self):self.assertTrue(set(['live','overview','fleet','jobs','timeline','models','tenants','alerts','audit','settings','guide','welcome','logs','palette','playground','onboard','modeldlg','evaldlg','datasets','training','workflows','deployments']).issubset(self.doc.ids))
    def test_js_ids_exist(self):
        ids=re.findall(r"\$\('#([a-z-]+)'\)",(ROOT/'web/app.js').read_text());self.assertTrue(set(ids).issubset(self.doc.ids),set(ids)-set(self.doc.ids))
    def test_no_credential_persistence(self):
        js=(ROOT/'web/app.js').read_text()
        for line in js.splitlines():
            if 'localStorage' in line:self.assertNotIn('token',line.lower().replace('gryvia-preferences',''))
        self.assertIn('never contains the token',js)
if __name__=='__main__':unittest.main()

class BrandTests(unittest.TestCase):
    def test_no_legacy_orange_accent(self):
        for name in ['web/style.css','web/index.html','web/app.js']:
            self.assertNotIn('f37032',(ROOT/name).read_text().lower(),name)
    def test_tokens_generated_with_vytrix_blue(self):
        css=(ROOT/'web/tokens.css').read_text();self.assertIn('--accent-color: #0088ff',css)
    def test_license_is_busl(self):
        self.assertIn('Business Source License 1.1',(ROOT/'LICENSE').read_text())
        self.assertIn('Zyvor AI Labs Private Limited',(ROOT/'NOTICE').read_text())

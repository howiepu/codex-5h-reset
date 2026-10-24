#!/usr/bin/env python3
"""无需前端依赖，校验单文件管理页的脚本语法与重复 ID。"""
import subprocess
from html.parser import HTMLParser
from pathlib import Path


class Page(HTMLParser):
    def __init__(self):
        super().__init__()
        self.ids = set()
        self.in_script = False
        self.scripts = []

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if "id" in attrs:
            assert attrs["id"] not in self.ids, "页面存在重复 ID: " + attrs["id"]
            self.ids.add(attrs["id"])
        self.in_script = tag == "script"

    def handle_endtag(self, tag):
        if tag == "script":
            self.in_script = False

    def handle_data(self, data):
        if self.in_script:
            self.scripts.append(data)


page = Page()
page.feed((Path(__file__).resolve().parent.parent / "web/index.html").read_text())
assert page.scripts, "页面脚本缺失"
for script in page.scripts:
    subprocess.run(["node", "--check", "--input-type=commonjs"], input=script, text=True, check=True)
print("管理页脚本语法和 HTML ID 检查通过。")

#!/usr/bin/env python3
"""统计 Markdown 中文字符数，排除代码块。用法:
    python3 docs/blog/wordcount.py <file.md> [start_heading] [end_heading]
不给标题则统计全文；给了则只统计两个标题之间的区段(不含 end_heading 行)。"""
import re, sys

path = sys.argv[1]
text = open(path, encoding="utf-8").read()
if len(sys.argv) > 2:
    start = text.find(sys.argv[2])
    text = text[start:] if start >= 0 else ""
if len(sys.argv) > 3:
    end = text.find(sys.argv[3])
    text = text[:end] if end >= 0 else text
text = re.sub(r"```.*?```", "", text, flags=re.S)   # 去代码块
text = re.sub(r"`[^`]*`", "", text)                  # 去行内代码
print(len(re.findall(r"[一-鿿]", text)))

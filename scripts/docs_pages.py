#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Wrap the fragments in docs/src/pages with docs/src/shell.html.

Deliberately tiny and deterministic: no timestamps, no ordering that depends on
the filesystem, no network, no dependency outside the standard library. It is
called by scripts/docs.sh, which is otherwise a plain copy, so `make docs` twice
in a row produces byte-identical output and docs/dist can stay committed.

A fragment is the page's body, preceded by a metadata comment:

    <!--
    title: Command line
    description: One sentence for the <meta> tag.
    chrome: none        (optional: skip the prose wrapper and the footer)
    -->

The file name is the slug: index.html -> dist/index.html, cli.html ->
dist/cli/index.html, so every page has a directory URL and no page ever links
to a .html extension.
"""

import re
import sys
from pathlib import Path

# The navigation, in reading order. (slug, label, group). The empty slug is the
# home page. This list is the whole site map: adding a page here and dropping a
# fragment next to it is the entire job.
NAV = [
    ("Start here", [
        ("", "Overview"),
        ("how-it-works", "How mimux works"),
        ("architecture", "Architecture"),
        ("install", "Installation"),
    ]),
    ("Using mimux", [
        ("interfaces", "Web UI, PWA, notifications"),
        ("cli", "Command line"),
    ]),
    ("Automation (pro)", [
        ("automation", "The automation layer"),
        ("api", "REST API guide"),
        ("reference", "API reference"),
        ("mcp", "MCP server"),
        ("webhooks", "Webhooks"),
    ]),
]

META = re.compile(r"\A\s*<!--(.*?)-->", re.DOTALL)


def meta_and_body(text):
    """Split a fragment into its metadata dict and its body."""
    m = META.match(text)
    if not m:
        raise SystemExit("docs: fragment has no metadata comment")
    meta = {}
    for line in m.group(1).strip().splitlines():
        key, _, value = line.partition(":")
        meta[key.strip()] = value.strip()
    return meta, text[m.end():].strip()


def nav_html(current, prefix):
    """The sidebar, with the current page marked."""
    out = ['<nav class="sidebar" aria-label="Documentation">']
    for group, items in NAV:
        out.append(f"<p class=\"nav-group\">{group}</p>")
        out.append("<ul>")
        for slug, label in items:
            here = ' class="here" aria-current="page"' if slug == current else ""
            out.append(f'<li><a href="{prefix}{slug}{"/" if slug else ""}"{here}>{label}</a></li>')
        out.append("</ul>")
    out.append("</nav>")
    return "\n".join(out)


def main(src, out):
    shell = (src / "shell.html").read_text(encoding="utf-8")
    for path in sorted((src / "pages").glob("*.html")):
        slug = "" if path.stem == "index" else path.stem
        prefix = "" if slug == "" else "../"
        meta, body = meta_and_body(path.read_text(encoding="utf-8"))
        if meta.get("chrome") != "none":
            body = f'<main class="prose" id="content">\n{body}\n</main>'
        page = shell
        for key, value in (
            ("{{TITLE}}", meta["title"]),
            ("{{DESCRIPTION}}", meta["description"]),
            ("{{NAV}}", nav_html(slug, prefix)),
            ("{{CONTENT}}", body),
            ("{{FOOTER}}", "" if meta.get("chrome") == "none" else FOOTER),
            ("{{PREFIX}}", prefix),
        ):
            page = page.replace(key, value)
        dest = out / slug / "index.html" if slug else out / "index.html"
        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.write_text(page, encoding="utf-8")


FOOTER = """<footer class="site-footer">
  <p>mimux is a self-hosted email client. The client is AGPL-3.0; the automation
  layer in <code>pro/</code> is Elastic Licence 2.0 &mdash; see
  <a href="https://github.com/mattmezza/mimux/blob/main/LICENSING.md">LICENSING.md</a>.</p>
  <p><a href="https://mimux.dev/">mimux.dev</a> &middot;
  <a href="https://github.com/mattmezza/mimux">Source</a> &middot;
  <a href="https://account.mimux.dev">Licences</a></p>
</footer>"""


if __name__ == "__main__":
    main(Path(sys.argv[1]), Path(sys.argv[2]))

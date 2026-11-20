#!/usr/bin/env python3
"""Build the documentation site sources from README.md (the single source of truth).

Every "## " section becomes a page; the long "## Use" section is split into one page per "### ".
Writes site-src/*.md and mkdocs.yml, then `mkdocs build` produces site/.
"""
import os
import re
import shutil
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, "site-src")
REPO = "https://github.com/gauravthakur02/kubeweave"


def slug(title):
    s = re.sub(r"[^a-z0-9]+", "-", title.lower()).strip("-")
    return s or "page"


def split(text, level):
    marker = "#" * level + " "
    parts, title, buf = [], None, []
    in_code = False
    for line in text.splitlines():
        if line.startswith("```"):
            in_code = not in_code
        if not in_code and line.startswith(marker):
            parts.append((title, "\n".join(buf)))
            title, buf = line[len(marker):].strip(), []
        else:
            buf.append(line)
    parts.append((title, "\n".join(buf)))
    return parts


def main():
    readme = open(os.path.join(ROOT, "README.md"), encoding="utf-8").read()
    # Links to repository files must point at GitHub, not the docs site.
    readme = re.sub(r"\]\((?!https?://|#)([^)]+)\)", lambda m: "](%s/blob/main/%s)" % (REPO, m.group(1)), readme)
    os.makedirs(OUT, exist_ok=True)
    # Images referenced by the README (the logo is an HTML <img src="assets/...">, which mkdocs does not copy).
    shutil.copytree(os.path.join(ROOT, "assets"), os.path.join(OUT, "assets"), dirs_exist_ok=True)
    nav = []
    sections = split(readme, 2)
    intro = sections[0][1]
    pages = {"index.md": intro}
    nav.append(("Home", "index.md"))
    for title, body in sections[1:]:
        if title == "Use":
            subs = split(body, 3)
            group = []
            pages["use.md"] = "# Use\n" + subs[0][1]
            group.append(("Overview", "use.md"))
            for st, sb in subs[1:]:
                name = "use-%s.md" % slug(st)
                pages[name] = "# %s\n%s" % (st, sb)
                group.append((st, name))
            nav.append(("Using kubeweave", group))
            continue
        name = "%s.md" % slug(title)
        pages[name] = "# %s\n%s" % (title, body.replace("\n### ", "\n## "))
        nav.append((title, name))
    # "[Install](#install)" in the README points at a section; on the site each "## " section is its own page.
    page_of = {slug(t): "%s.md" % slug(t) for t, _ in sections[1:] if t != "Use"}
    page_of["use"] = "use.md"
    def fix(name, body):
        body = re.sub(r"\]\(#([a-z0-9-]+)\)", lambda m: "](%s)" % page_of.get(m.group(1), "#" + m.group(1)), body)
        # HTML links: the site serves each page as a folder (install.md -> install/, index.md -> the site root)
        up = "" if name == "index.md" else "../"
        return re.sub(r'href="#([a-z0-9-]+)"', lambda m: 'href="%s"' % (
            up + page_of[m.group(1)][:-3] + "/" if m.group(1) in page_of else "#" + m.group(1)), body)
    pages = {name: fix(name, body) for name, body in pages.items()}
    for name, body in pages.items():
        with open(os.path.join(OUT, name), "w", encoding="utf-8") as f:
            f.write(body.strip() + "\n")
    lines = [
        "site_name: kubeweave",
        "site_description: A Kubernetes IDE for your terminal and browser, with an AI copilot",
        "repo_url: " + REPO,
        "docs_dir: site-src",
        "theme:",
        "  name: material",
        "  logo: assets/icon.svg",
        "  favicon: assets/icon.svg",
        "  palette:",
        "    - scheme: slate",
        "      primary: indigo",
        "  features: [navigation.sections, search.highlight, content.code.copy]",
        "markdown_extensions: [tables, admonition, toc: {permalink: true}]",
        "nav:",
    ]
    q = lambda s: '"' + s.replace('"', '\\"') + '"'
    for title, target in nav:
        if isinstance(target, list):
            lines.append("  - %s:" % q(title))
            for st, sn in target:
                lines.append("      - %s: %s" % (q(st), sn))
        else:
            lines.append("  - %s: %s" % (q(title), target))
    with open(os.path.join(ROOT, "mkdocs.yml"), "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")
    print("wrote %d pages" % len(pages), file=sys.stderr)


if __name__ == "__main__":
    main()

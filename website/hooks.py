"""MkDocs hooks for the KubeAmber website.

The site is built from docs/ plus the site-only pages in website/. Two things
keep it in step with the repository instead of copying text into it:

- A line `<!-- include: README.md#Section -->` in a website/ page is replaced by
  the body of that README section, so the quickstart is written once.
- Relative links are resolved against the file they were written in. A link to
  a page in docs/ stays a page link, and a link to a README section that a
  website page includes goes to that page; a link to anything else in the
  repository (code, examples, the chart) becomes a link to it on GitHub.
"""

import posixpath
import re
from pathlib import Path

from mkdocs.structure.files import File

REPO = Path(__file__).resolve().parent.parent
GITHUB = "https://github.com/kubeamber/kubeamber/blob/main/"

INCLUDE = re.compile(r"^<!-- include: (?P<file>[^#\s]+)#(?P<section>.+?) -->$", re.M)
LINK = re.compile(r"(?P<pre>!?\[(?:[^\[\]]|\[[^\]]*\])*\]\()(?P<target>[^)\s]+)(?P<post>(?:\s+\"[^\"]*\")?\))")
FENCE = re.compile(r"^(```|~~~)", re.M)

# Where each page's text was written, relative to the repository root, so its
# relative links can be resolved: docs pages live in docs/, website pages in
# website/, and included README text at the root.
_origin = {}

# The website page that includes each README section, by the section's anchor:
# {("README.md", "quickstart"): "getting-started.md"}.
_included = {}


def on_files(files, config):
    site = REPO / "website"
    for path in sorted(site.glob("*.md")):
        files.append(File.generated(config, path.name, content=_expand(path.read_text(), path.name)))
        _origin[path.name] = "website/" + path.name
    for path in sorted((site / "assets").glob("**/*")):
        if path.is_file():
            uri = path.relative_to(site).as_posix()
            files.append(File.generated(config, uri, abs_src_path=str(path)))
    return files


def on_page_markdown(markdown, page, config, files):
    uri = page.file.src_uri
    origin = _origin.get(uri, "docs/" + uri)
    return _relink(markdown, posixpath.dirname(origin), posixpath.dirname(uri))


def _expand(text, page):
    """Replace include markers with README sections, tagged with their origin."""

    def section(m):
        _included[(m["file"], _anchor(m["section"]))] = page
        source = (REPO / m["file"]).read_text()
        heading = re.search(rf"^## {re.escape(m['section'])}\n", source, re.M)
        if not heading:
            raise ValueError(f"{m['file']} has no section '## {m['section']}'")
        rest = source[heading.end():]
        end = re.search(r"^## ", rest, re.M)
        body = rest[: end.start()] if end else rest
        base = posixpath.dirname(m["file"])
        # Mark the text so its links resolve from the included file, not the page.
        return f"<!-- origin: {base or '.'} -->\n{body.strip()}\n<!-- origin: end -->"

    return INCLUDE.sub(section, text)


def _relink(markdown, origin_dir, page_dir):
    out, base, in_fence = [], origin_dir, False
    for line in markdown.splitlines(keepends=True):
        if FENCE.match(line):
            in_fence = not in_fence
        elif not in_fence and line.startswith("<!-- origin: "):
            tag = line.strip()[len("<!-- origin: "):-len(" -->")]
            base = origin_dir if tag == "end" else ("" if tag == "." else tag)
            continue
        elif not in_fence:
            line = LINK.sub(lambda m: m["pre"] + _target(m["target"], base, page_dir) + m["post"], line)
        out.append(line)
    return "".join(out)


def _anchor(heading):
    """GitHub's anchor for a heading: "Using it with your agent" -> "using-it-with-your-agent"."""
    return re.sub(r"[^\w\- ]", "", heading.lower()).replace(" ", "-")


def _target(target, base, page_dir):
    if re.match(r"^([a-z]+:|#|/)", target):
        return target
    path, _, anchor = target.partition("#")
    repo_path = posixpath.normpath(posixpath.join(base, path))
    frag = "#" + anchor if anchor else ""
    if (repo_path, anchor) in _included:
        return posixpath.relpath(_included[(repo_path, anchor)], page_dir or ".")
    if repo_path.startswith("docs/") and (REPO / repo_path).exists():
        return posixpath.relpath(repo_path[len("docs/"):], page_dir or ".") + frag
    if repo_path.startswith("website/"):
        return posixpath.relpath(repo_path[len("website/"):], page_dir or ".") + frag
    if (REPO / repo_path).exists():
        return GITHUB + repo_path + frag
    return target

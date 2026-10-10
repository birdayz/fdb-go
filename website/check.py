"""Check a production Hugo build's local links and public metadata (stdlib only)."""

import argparse
import re
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import unquote, urljoin, urlparse


class Page(HTMLParser):
    def __init__(self, text):
        super().__init__()
        self.links = []
        self.ids = set()
        self.meta = {}
        self.canonical = None
        self.feed(text)

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if attrs.get("id"):
            self.ids.add(attrs["id"])
        if tag == "a" and attrs.get("href"):
            self.links.append(attrs["href"])
        if tag == "meta":
            self.meta[attrs.get("name", attrs.get("property"))] = attrs.get("content")
        if tag == "link" and attrs.get("rel") == "canonical":
            self.canonical = attrs.get("href")


def require(condition, message):
    if not condition:
        raise SystemExit(message)


def check(root):
    pages = {
        "/" + str(path.relative_to(root)).removesuffix("index.html"): Page(path.read_text())
        for path in root.rglob("index.html")
    }
    public_pages = ["/", "/docs/", "/docs/getting-started/", "/docs/maturity/"]
    require(all(path in pages for path in public_pages), "Missing public pages: build the site first")
    checked = 0
    for path, page in pages.items():
        for href in page.links:
            dest = urlparse(urljoin("https://fdb.dev" + path, href))
            if dest.netloc != "fdb.dev":
                continue
            target = unquote(dest.path).rstrip("/") + "/"
            if target in pages:
                require(not dest.fragment or unquote(dest.fragment) in pages[target].ids,
                        f"{path}: missing anchor {href}")
            else:
                require((root / unquote(dest.path).lstrip("/")).is_file(),
                        f"{path}: missing local page/file {href}")
            checked += 1

    for path in public_pages:
        page = pages[path]
        require(page.canonical == "https://fdb.dev" + path, f"{path}: wrong canonical URL")
        for name in ["description", "og:description", "twitter:description"]:
            value = page.meta.get(name, "")
            require(value, f"{path}: missing {name}")
            require(not re.search(r"2[–—-]4[x×]|faster reads|byte.identical|wire.compatible", value, re.I),
                    f"{path}: unsupported performance/blanket compatibility claim in {name}")
        expected_type = "website" if path in ["/", "/docs/"] else "article"
        require((page.meta.get("og:type") or "").strip() == expected_type, f"{path}: wrong og:type")
        require(page.meta.get("twitter:card") == "summary_large_image", f"{path}: wrong twitter:card")
        for name in ["og:image", "twitter:image"]:
            require(page.meta.get(name) == "https://fdb.dev/og.png", f"{path}: wrong {name}")
        require(page.meta.get("go-import") == "fdb.dev git https://github.com/birdayz/fdb-go",
                f"{path}: missing root Go vanity-import metadata")
    require((root / "og.png").is_file(), "Missing social card")
    print(f"Checked {len(pages)} index pages, {checked} local link occurrences/anchors, "
          f"and public metadata on {len(public_pages)} pages.")
    print("External repository links and raster-image text need separate review.")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("site", type=Path, help="Production Hugo output directory")
    check(parser.parse_args().site)

"""Pull one workload's container args out of a rendered helm manifest stream.

Used by hack/deploy.sh to compare the chart's intended agent flags against the
ones the live DaemonSet is running, on clusters without the helm-diff plugin.
Deliberately dependency-free (no PyYAML): it reads the args list from the
rendered text, because the alternative is asking every operator to pip-install
something before they can check their cluster.
"""
import re
import sys


def main() -> int:
    if len(sys.argv) < 2:
        print("usage: render_args.py <workload-name>", file=sys.stderr)
        return 2
    want = sys.argv[1]
    text = sys.stdin.read()

    # Split on the document separator and keep the one whose metadata.name
    # matches; names are unique per rendered chart.
    for doc in text.split("\n---"):
        if not re.search(rf"^\s*name:\s*{re.escape(want)}\s*$", doc, re.M):
            continue
        m = re.search(r"^(\s*)args:\s*$", doc, re.M)
        if not m:
            continue
        indent = len(m.group(1))
        for line in doc[m.end():].split("\n")[1:]:
            if not line.strip():
                continue
            stripped = line.lstrip()
            # The args list ends at the first line indented no further than
            # `args:` itself, or at the next key at the same depth.
            if len(line) - len(stripped) <= indent and not stripped.startswith("-"):
                break
            if not stripped.startswith("- "):
                break
            print(stripped[2:].strip().strip('"'))
        return 0
    return 0


if __name__ == "__main__":
    sys.exit(main())
